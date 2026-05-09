package edge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/config"
	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/lms"
)

type WSClient struct {
	conn         *websocket.Conn
	mu           sync.Mutex
	lmsClient    *lms.Client
	poller       *Poller
	invalidCache sync.Map // key -> expireTime (time.Time)
	log          *slog.Logger
}

func NewWSClient(lmsClient *lms.Client, poller *Poller) *WSClient {
	return &WSClient{
		lmsClient: lmsClient,
		poller:    poller,
		log:       slog.With("component", "ws_client"),
	}
}

// Run connects to the center server and processes messages with exponential backoff reconnection.
func (w *WSClient) Run(ctx context.Context) {
	if config.Cfg.CenterServerURL == "" {
		w.log.Info("未配置 Center 服务器，WebSocket 已禁用")
		return
	}

	backoff := time.Second
	maxBackoff := 60 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := w.connectAndProcess(ctx)
		if ctx.Err() != nil {
			return
		}

		w.log.Warn("WebSocket 断开，正在重连...", "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, maxBackoff)
	}
}

func (w *WSClient) connectAndProcess(ctx context.Context) error {
	w.log.Info("正在连接 Center 服务器", "地址", config.Cfg.CenterServerURL)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.DialContext(ctx, config.Cfg.CenterServerURL, nil)
	if err != nil {
		return err
	}

	w.mu.Lock()
	w.conn = conn
	w.mu.Unlock()

	defer func() {
		conn.Close()
		w.mu.Lock()
		w.conn = nil
		w.mu.Unlock()
	}()

	// Send register message
	reg := map[string]interface{}{
		"type":      "register",
		"client_id": config.ClientID,
		"secret":    config.Cfg.CenterServerSecret,
	}
	if err := conn.WriteJSON(reg); err != nil {
		return err
	}
	w.log.Info("已注册到 Center 服务器")

	// [Fix #5] Trigger poll immediately after registration to sync tasks
	w.poller.TriggerPoll()

	// Message receive loop
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}

		go w.handleMessage(ctx, msg)
	}
}

func (w *WSClient) handleMessage(ctx context.Context, msg map[string]interface{}) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("消息处理异常", "panic", r)
		}
	}()

	msgType, _ := msg["type"].(string)
	if msgType != "rollcall_share" {
		return
	}

	rollcallType, _ := msg["rollcall_type"].(string)
	fromClientID, _ := msg["from_client_id"].(string)

	if config.PauseSharedRollcall.Load() {
		w.log.Info("共享签到已暂停，忽略", "类型", rollcallType)
		return
	}

	switch rollcallType {
	case "qr":
		w.handleQRShare(ctx, msg, fromClientID)
	case "number":
		w.handleNumberShare(ctx, msg, fromClientID)
	}
}

func (w *WSClient) handleQRShare(ctx context.Context, msg map[string]interface{}, fromClientID string) {
	rawQR, _ := msg["rollcall_qr_data"].(string)
	cacheKey := "qr:" + rawQR

	if w.isInInvalidCache(cacheKey) {
		w.log.Info("跳过 QR 签到（已缓存为无效）", "raw", rawQR)
		return
	}

	qrData := ExtractQRData(rawQR)
	if qrData == "" {
		w.log.Warn("收到无效的 QR 数据", "raw", rawQR)
		w.addToInvalidCache(cacheKey)
		return
	}

	rollcalls, err := w.lmsClient.GetRollcalls(ctx)
	if err != nil {
		w.log.Error("获取签到列表失败（QR 共享）", "error", err)
		return
	}

	// [Fix #3] Try ALL qr+absent rollcalls, not just the first one
	tried := false
	success := false
	for _, r := range rollcalls {
		if r.Source == "qr" && r.Status == "absent" {
			tried = true
			result := w.lmsClient.DoCheckin(ctx, r.RollcallID, "qr", map[string]interface{}{
				"data": qrData,
			})
			if result.Success {
				success = true
				w.log.Info("QR 共享签到成功", "rollcall_id", r.RollcallID)
			}
		}
	}

	// [Fix #4] Only cache as invalid if we tried and ALL failed
	if tried && !success {
		w.addToInvalidCache(cacheKey)
	}

	// Only send verification if we actually tried (matching Python behavior)
	if !tried {
		return
	}

	w.poller.TriggerPoll()

	w.SendToCenter(map[string]interface{}{
		"type":             "rollcall_share_verification",
		"from_client_id":   fromClientID,
		"client_id":        config.ClientID,
		"rollcall_type":    "qr",
		"rollcall_qr_data": qrData,
		"valid":            success,
		"timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})
}

func (w *WSClient) handleNumberShare(ctx context.Context, msg map[string]interface{}, fromClientID string) {
	rollcallID := int(msg["rollcall_id"].(float64))
	number := int(msg["rollcall_number"].(float64))
	courseTitle, _ := msg["course_title"].(string)
	courseLocation, _ := msg["course_location"].(string)

	// [Fix #1] Use fmt.Sprintf for cache key, not string(rune())
	cacheKey := fmt.Sprintf("num:%d:%d", rollcallID, number)

	if w.isInInvalidCache(cacheKey) {
		w.log.Info("跳过数字签到（已缓存为无效）", "rollcall_id", rollcallID, "number", number)
		return
	}

	rollcalls, err := w.lmsClient.GetRollcalls(ctx)
	if err != nil {
		w.log.Error("获取签到列表失败（数字共享）", "error", err)
		return
	}

	tried := false
	valid := false
	for _, r := range rollcalls {
		if r.RollcallID == rollcallID && r.Status == "absent" {
			tried = true
			// [Fix #2] Send numberCode as string, matching Python: str(c_num)
			result := w.lmsClient.DoCheckin(ctx, r.RollcallID, "number", map[string]interface{}{
				"numberCode": fmt.Sprintf("%d", number),
			})
			if result.Success {
				valid = true
				w.log.Info("数字共享签到成功", "rollcall_id", rollcallID)
			} else {
				w.addToInvalidCache(cacheKey)
				w.log.Warn("数字共享签到失败", "rollcall_id", rollcallID, "error", result.ErrorCode)
			}
			break
		}
	}

	if !tried {
		return
	}

	w.poller.TriggerPoll()

	w.SendToCenter(map[string]interface{}{
		"type":            "rollcall_share_verification",
		"from_client_id":  fromClientID,
		"client_id":       config.ClientID,
		"rollcall_type":   "number",
		"course_title":    courseTitle,
		"course_location": courseLocation,
		"rollcall_id":     rollcallID,
		"rollcall_number": number,
		"valid":           valid,
		"timestamp":       time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})
}

// SendToCenter sends a JSON message to the center server.
func (w *WSClient) SendToCenter(msg map[string]interface{}) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn == nil {
		return
	}

	data, err := json.Marshal(msg)
	if err != nil {
		w.log.Error("消息序列化失败", "error", err)
		return
	}

	if err := w.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		w.log.Warn("发送到 Center 失败", "error", err)
	}
}

func (w *WSClient) isInInvalidCache(key string) bool {
	if v, ok := w.invalidCache.Load(key); ok {
		expire := v.(time.Time)
		if time.Now().Before(expire) {
			return true
		}
		w.invalidCache.Delete(key)
	}
	return false
}

func (w *WSClient) addToInvalidCache(key string) {
	w.invalidCache.Store(key, time.Now().Add(24*time.Hour))
}
