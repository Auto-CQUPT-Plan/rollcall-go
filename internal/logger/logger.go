package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
	colorBold   = "\033[1m"
)

type PrettyHandler struct {
	w     io.Writer
	level slog.Leveler
	mu    sync.Mutex
	attrs []slog.Attr
	group string
}

func NewPrettyHandler(w io.Writer, level slog.Leveler) *PrettyHandler {
	return &PrettyHandler{w: w, level: level}
}

func (h *PrettyHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *PrettyHandler) Handle(_ context.Context, r slog.Record) error {
	timeStr := r.Time.Format("15:04:05")

	var levelStr string
	switch {
	case r.Level >= slog.LevelError:
		levelStr = colorRed + colorBold + "ERROR" + colorReset
	case r.Level >= slog.LevelWarn:
		levelStr = colorYellow + " WARN" + colorReset
	case r.Level >= slog.LevelInfo:
		levelStr = colorGreen + " INFO" + colorReset
	default:
		levelStr = colorGray + "DEBUG" + colorReset
	}

	// Build attrs string
	var attrsStr string
	allAttrs := make([]slog.Attr, 0, len(h.attrs)+int(r.NumAttrs()))
	allAttrs = append(allAttrs, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		allAttrs = append(allAttrs, a)
		return true
	})

	// Filter out noisy attrs, keep only useful ones
	for _, a := range allAttrs {
		if a.Key == "component" {
			continue // component is already implied by the message
		}
		attrsStr += fmt.Sprintf("  %s%s=%s%s", colorGray, a.Key, a.Value.String(), colorReset)
	}

	line := fmt.Sprintf("%s%s%s %s %s▸%s %s%s\n",
		colorGray, timeStr, colorReset,
		levelStr,
		colorCyan, colorReset,
		r.Message,
		attrsStr,
	)

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write([]byte(line))
	return err
}

func (h *PrettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs), len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	newAttrs = append(newAttrs, attrs...)
	return &PrettyHandler{w: h.w, level: h.level, attrs: newAttrs, group: h.group}
}

func (h *PrettyHandler) WithGroup(name string) slog.Handler {
	return &PrettyHandler{w: h.w, level: h.level, attrs: h.attrs, group: name}
}

// Setup initializes the global slog logger with the pretty handler and deduplication.
func Setup(w io.Writer) {
	handler := NewPrettyHandler(w, slog.LevelInfo)
	dedupHandler := NewDeduplicatingHandler(handler)
	slog.SetDefault(slog.New(dedupHandler))
}

// FormatDuration formats a duration in a human-friendly way.
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%.0fm%.0fs", d.Minutes(), float64(d.Nanoseconds()%int64(time.Minute))/float64(time.Second))
}
