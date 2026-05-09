package edge

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/config"
	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/lms"
)

type Server struct {
	lmsClient *lms.Client
	wsClient  *WSClient
	poller    *Poller
	log       *slog.Logger
}

func NewServer(lmsClient *lms.Client, wsClient *WSClient, poller *Poller) *Server {
	return &Server{
		lmsClient: lmsClient,
		wsClient:  wsClient,
		poller:    poller,
		log:       slog.With("component", "edge_http"),
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Get("/health", s.handleHealth)
	r.Get("/rollcalls", s.handleGetRollcalls)
	r.Get("/pause_shared", s.handleGetPause)
	r.Post("/pause_shared", s.handleSetPause)
	r.Post("/rollcall/{rollcallID}/qr", s.handleQRCheckin)
	r.Post("/rollcall/{rollcallID}/number", s.handleNumberCheckin)
	r.Post("/rollcall/{rollcallID}/location", s.handleLocationCheckin)
	r.Post("/rollcallqr", s.handleBatchQRCheckin)

	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok", "client_id": config.ClientID})
}

func (s *Server) handleGetRollcalls(w http.ResponseWriter, r *http.Request) {
	rollcalls, err := s.lmsClient.GetRollcalls(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, rollcalls)
}

func (s *Server) handleGetPause(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"pause": config.PauseSharedRollcall.Load()})
}

func (s *Server) handleSetPause(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Pause bool `json:"pause"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}
	config.PauseSharedRollcall.Store(body.Pause)
	writeJSON(w, 200, map[string]interface{}{"message": "success", "pause": body.Pause})
}

func (s *Server) handleQRCheckin(w http.ResponseWriter, r *http.Request) {
	rollcallID, err := strconv.Atoi(chi.URLParam(r, "rollcallID"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid rollcall_id"})
		return
	}

	var body struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	qrData := ExtractQRData(body.Data)
	if qrData == "" {
		writeJSON(w, 400, map[string]string{"error": "invalid or expired QR data"})
		return
	}

	result := s.lmsClient.DoCheckin(r.Context(), rollcallID, "qr", map[string]interface{}{
		"data": qrData,
	})

	if result.Success {
		// Notify center
		go s.wsClient.SendToCenter(map[string]interface{}{
			"type":             "rollcall_success",
			"client_id":        config.ClientID,
			"rollcall_type":    "qr",
			"rollcall_data":    qrData,
			"timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		})
		s.poller.TriggerPoll()
		writeJSON(w, 200, map[string]string{"message": "success"})
	} else {
		writeJSON(w, 400, map[string]string{"error": result.ErrorCode})
	}
}

func (s *Server) handleNumberCheckin(w http.ResponseWriter, r *http.Request) {
	rollcallID, err := strconv.Atoi(chi.URLParam(r, "rollcallID"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid rollcall_id"})
		return
	}

	var body struct {
		NumberCode interface{} `json:"numberCode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	result := s.lmsClient.DoCheckin(r.Context(), rollcallID, "number", map[string]interface{}{
		"numberCode": body.NumberCode,
	})

	if result.Success {
		// Get rollcall info for center message
		rollcalls, _ := s.lmsClient.GetRollcalls(r.Context())
		var courseTitle string
		var courseLocation interface{}
		for _, rc := range rollcalls {
			if rc.RollcallID == rollcallID {
				courseTitle = rc.CourseTitle
				courseLocation = s.poller.getCourseLocationForRollcall(rc)
				break
			}
		}

		go s.wsClient.SendToCenter(map[string]interface{}{
			"type":            "rollcall_success",
			"client_id":       config.ClientID,
			"rollcall_type":   "number",
			"course_title":    courseTitle,
			"course_location": courseLocation,
			"rollcall_id":     rollcallID,
			"rollcall_number": body.NumberCode,
			"timestamp":       time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		})
		s.poller.TriggerPoll()
		writeJSON(w, 200, map[string]string{"message": "success"})
	} else {
		writeJSON(w, 400, map[string]string{"error": result.ErrorCode})
	}
}

func (s *Server) handleLocationCheckin(w http.ResponseWriter, r *http.Request) {
	rollcallID, err := strconv.Atoi(chi.URLParam(r, "rollcallID"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid rollcall_id"})
		return
	}

	var body struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	result := s.lmsClient.DoCheckin(r.Context(), rollcallID, "radar", map[string]interface{}{
		"lat": body.Lat,
		"lon": body.Lon,
	})

	if result.Success {
		s.poller.TriggerPoll()
		writeJSON(w, 200, map[string]string{"message": "success"})
	} else {
		writeJSON(w, 400, map[string]string{"error": result.ErrorCode})
	}
}

func (s *Server) handleBatchQRCheckin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	qrData := ExtractQRData(body.Data)
	if qrData == "" {
		writeJSON(w, 400, map[string]string{"error": "invalid or expired QR data"})
		return
	}

	rollcalls, err := s.lmsClient.GetRollcalls(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	type checkinResult struct {
		RollcallID int    `json:"rollcall_id"`
		Status     string `json:"status"`
		Error      string `json:"error,omitempty"`
	}

	var results []checkinResult
	for _, rc := range rollcalls {
		if rc.Source != "qr" || rc.Status != "absent" {
			continue
		}

		res := s.lmsClient.DoCheckin(r.Context(), rc.RollcallID, "qr", map[string]interface{}{
			"data": qrData,
		})

		cr := checkinResult{RollcallID: rc.RollcallID}
		if res.Success {
			cr.Status = "success"
			go s.wsClient.SendToCenter(map[string]interface{}{
				"type":             "rollcall_success",
				"client_id":        config.ClientID,
				"rollcall_type":    "qr",
				"rollcall_data":    qrData,
				"timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
			})
			s.poller.TriggerPoll()
		} else {
			cr.Status = "failed"
			cr.Error = res.ErrorCode
		}
		results = append(results, cr)
	}

	writeJSON(w, 200, map[string]interface{}{"results": results})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Fprintf(w, `{"error":"encode error"}`)
	}
}
