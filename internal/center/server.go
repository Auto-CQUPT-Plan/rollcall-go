package center

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	manager *ConnectionManager
	state   *SharedState
	log     *slog.Logger

	// Config
	secret                   string
	externalSecretController string
}

func NewServer() *Server {
	secret := os.Getenv("CENTER_SECRET")
	extCtrl := os.Getenv("EXTERNAL_SECRET_CONTROLLER")

	return &Server{
		manager: NewConnectionManager(),
		state:   NewSharedState(),
		log:     slog.With("component", "center"),
		secret:  secret,
		externalSecretController: extCtrl,
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Get("/api/rollcall/", s.handleIndex)
	r.Post("/api/rollcall/qr", s.handleQR)
	r.Get("/api/rollcall/status", s.handleStatus)
	r.HandleFunc("/api/rollcall/ws", s.handleWS)
	r.HandleFunc("/api/rollcall/ws/status", s.handleWSStatus)
	r.Get("/health", s.handleHealth)

	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"status":    "ok",
		"connected": s.manager.Count(),
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"name": "CQUPT-Rollcall"})
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid body"})
		return
	}

	if s.state.UpdateQRData(body.Data) {
		s.manager.Broadcast(map[string]interface{}{
			"type":             "rollcall_share",
			"from_client_id":   "http_api",
			"rollcall_type":    "qr",
			"rollcall_qr_data": body.Data,
			"timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	writeJSON(w, 200, map[string]interface{}{
		"message":   "success",
		"latest_qr": s.state.GetCurrentQR(),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.getStatusPayload())
}

func (s *Server) getStatusPayload() map[string]interface{} {
	ids := s.manager.ActiveClientIDs()
	return map[string]interface{}{
		"remaining_seconds": s.state.GetRemainingSeconds(),
		"current_qr":       s.state.GetCurrentQR(),
		"connected_edges":  len(ids),
		"uncheckin_edges":  s.state.UncheckinCount(),
	}
}

// handleWS is the main WebSocket endpoint for edge clients.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Error("WebSocket 升级失败", "error", err)
		return
	}
	defer conn.Close()

	// Wait for register message
	clientID, err := s.handleRegister(r.Context(), conn)
	if err != nil {
		s.log.Warn("注册失败", "error", err)
		conn.WriteJSON(map[string]interface{}{
			"type":    "error",
			"message": err.Error(),
		})
		return
	}

	s.manager.Connect(conn, clientID)
	defer func() {
		s.manager.Disconnect(conn)
		if !s.manager.IsClientConnected(clientID) {
			s.state.RemoveClient(clientID)
		}
	}()

	// Message loop
	for {
		_, msgData, err := conn.ReadMessage()
		if err != nil {
			s.log.Info("客户端读取错误", "client_id", clientID, "error", err)
			return
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(msgData, &msg); err != nil {
			continue
		}

		s.handleClientMessage(conn, clientID, msg)
	}
}

func (s *Server) handleRegister(ctx context.Context, conn *websocket.Conn) (string, error) {
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	var msg map[string]interface{}
	if err := conn.ReadJSON(&msg); err != nil {
		return "", fmt.Errorf("read register message: %w", err)
	}

	msgType, _ := msg["type"].(string)
	if msgType != "register" {
		return "", fmt.Errorf("expected register, got %s", msgType)
	}

	clientID, _ := msg["client_id"].(string)
	secret, _ := msg["secret"].(string)

	if !s.verifySecret(secret, clientID) {
		return "", fmt.Errorf("invalid secret")
	}

	s.log.Info("客户端已注册", "client_id", clientID)
	return clientID, nil
}

func (s *Server) verifySecret(secret, clientID string) bool {
	if s.externalSecretController != "" {
		client := &http.Client{Timeout: 10 * time.Second}
		url := fmt.Sprintf("%s?secret=%s&uuid=%s", s.externalSecretController, secret, clientID)
		resp, err := client.Get(url)
		if err != nil {
			s.log.Error("外部密钥验证失败", "error", err)
			return false
		}
		defer resp.Body.Close()
		var buf [32]byte
		n, _ := resp.Body.Read(buf[:])
		return string(buf[:n]) == "success"
	}

	if s.secret == "" {
		return true
	}
	return secret == s.secret
}

func (s *Server) handleClientMessage(conn *websocket.Conn, clientID string, msg map[string]interface{}) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("消息处理异常", "client_id", clientID, "panic", r)
		}
	}()

	msgType, _ := msg["type"].(string)

	switch msgType {
	case "rollcall_tasks":
		s.handleRollcallTasks(conn, clientID, msg)
	case "rollcall_success":
		s.handleRollcallSuccess(clientID, msg)
	case "rollcall_share_verification":
		s.handleRollcallSuccess(clientID, msg)
	}
}

func (s *Server) handleRollcallTasks(conn *websocket.Conn, clientID string, msg map[string]interface{}) {
	// [Fix #7] Handle rollcall_qr true/false correctly — remove from sets when false
	rollcallQR, _ := msg["rollcall_qr"].(bool)
	s.state.SetQRNeedingClient(clientID, rollcallQR)

	if rollcallQR {
		// If we have a valid QR, send it back immediately
		currentQR := s.state.GetCurrentQR()
		if currentQR != "" {
			// [Fix #8] Use manager.WriteJSON for per-conn write mutex
			s.manager.WriteJSON(conn, map[string]interface{}{
				"type":             "rollcall_share",
				"from_client_id":   "center",
				"rollcall_type":    "qr",
				"rollcall_qr_data": currentQR,
				"timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
			})
		}
	}

	// Number handling
	numbers, _ := msg["rollcall_number"].([]interface{})
	for _, item := range numbers {
		numMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		ridVal, ok := numMap["rollcall_id"].(float64)
		if !ok {
			continue
		}
		rollcallID := int(ridVal)
		courseTitle, _ := numMap["course_title"].(string)
		var courseLocation *string
		if loc, ok := numMap["course_location"].(string); ok {
			courseLocation = &loc
		}

		s.state.GetOrCreateNumberTask(rollcallID, courseTitle, courseLocation)

		// If we already have the number, send it back
		task := s.state.GetNumberTask(rollcallID)
		if task != nil && task.RollcallNumber != nil {
			// [Fix #8] Use manager.WriteJSON for per-conn write mutex
			s.manager.WriteJSON(conn, map[string]interface{}{
				"type":            "rollcall_share",
				"from_client_id":  "center",
				"rollcall_type":   "number",
				"course_title":    task.CourseTitle,
				"course_location": task.CourseLocation,
				"rollcall_id":     rollcallID,
				"rollcall_number": *task.RollcallNumber,
				"timestamp":       time.Now().UTC().Format("2006-01-02T15:04:05Z"),
			})
		}
	}
}

func (s *Server) handleRollcallSuccess(clientID string, msg map[string]interface{}) {
	rollcallType, _ := msg["rollcall_type"].(string)
	senderID, _ := msg["client_id"].(string)
	if senderID == "" {
		senderID = clientID
	}

	switch rollcallType {
	case "qr":
		// [Fix #6] Accept both "rollcall_data" and "rollcall_qr_data" (Python center does this)
		qrData, _ := msg["rollcall_data"].(string)
		if qrData == "" {
			qrData, _ = msg["rollcall_qr_data"].(string)
		}

		updated := s.state.UpdateQRData(qrData)
		if updated {
			s.state.AddQRSuccessClient(senderID)
			s.manager.Broadcast(map[string]interface{}{
				"type":             "rollcall_share",
				"from_client_id":   senderID,
				"rollcall_type":    "qr",
				"rollcall_qr_data": qrData,
				"timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
			})
		} else if s.state.MatchesCurrentQR(qrData) {
			// Same QR (not newer) — still mark sender as successful
			s.state.AddQRSuccessClient(senderID)
		}

	case "number":
		ridVal, ok := msg["rollcall_id"].(float64)
		if !ok {
			return
		}
		rollcallID := int(ridVal)
		numVal, ok := msg["rollcall_number"].(float64)
		if !ok {
			return
		}
		number := int(numVal)
		courseTitle, _ := msg["course_title"].(string)
		var courseLocation *string
		if loc, ok := msg["course_location"].(string); ok {
			courseLocation = &loc
		}

		// Update or create the number task
		s.state.GetOrCreateNumberTask(rollcallID, courseTitle, courseLocation)
		s.state.UpdateNumberTaskAnswer(rollcallID, number)

		s.manager.Broadcast(map[string]interface{}{
			"type":            "rollcall_share",
			"from_client_id":  senderID,
			"rollcall_type":   "number",
			"course_title":    courseTitle,
			"course_location": courseLocation,
			"rollcall_id":     rollcallID,
			"rollcall_number": number,
			"timestamp":       time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
}

// handleWSStatus sends status updates every second.
func (s *Server) handleWSStatus(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Error("状态 WebSocket 升级失败", "error", err)
		return
	}
	defer conn.Close()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if err := conn.WriteJSON(s.getStatusPayload()); err != nil {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
