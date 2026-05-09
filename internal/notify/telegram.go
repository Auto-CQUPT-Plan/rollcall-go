package notify

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/config"
)

// Send sends a Telegram message asynchronously. Silent if unconfigured.
func Send(msg string) {
	if config.Cfg.TGBotToken == "" || config.Cfg.TGChatID == "" {
		return
	}
	go func() {
		if err := sendTelegram(config.Cfg.TGBotToken, config.Cfg.TGChatID, msg); err != nil {
			slog.Debug("TG 通知发送失败", "error", err)
		}
	}()
}

// Sendf sends a formatted Telegram message.
func Sendf(format string, args ...interface{}) {
	Send(fmt.Sprintf(format, args...))
}

func sendTelegram(token, chatID, text string) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).PostForm(apiURL, url.Values{
		"chat_id":    {chatID},
		"text":       {text},
		"parse_mode": {"HTML"},
	})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("telegram API status %d", resp.StatusCode)
	}
	return nil
}
