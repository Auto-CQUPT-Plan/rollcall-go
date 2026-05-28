package logger

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type DeduplicatingHandler struct {
	handler   slog.Handler
	mu        sync.Mutex
	lastKey   string
	lastCount int
	attrs     []slog.Attr
	group     string
}

func NewDeduplicatingHandler(handler slog.Handler) *DeduplicatingHandler {
	return &DeduplicatingHandler{
		handler: handler,
	}
}

func (h *DeduplicatingHandler) Enabled(_ context.Context, level slog.Level) bool {
	return h.handler.Enabled(context.Background(), level)
}

func (h *DeduplicatingHandler) Handle(_ context.Context, r slog.Record) error {
	// 构建去重键：消息+级别+属性
	key := h.buildDedupeKey(r)

	h.mu.Lock()
	defer h.mu.Unlock()

	// 如果与上一条相同，增加计数
	if key == h.lastKey {
		h.lastCount++
		return nil
	}

	// 如果与上一条不同
	// 先打印上一条（如果有过重复）
	if h.lastCount > 0 {
		// 创建上一条的副本并添加计数
		lastRecord := h.createRecordWithCount(h.lastKey, h.lastCount)
		if lastRecord != nil {
			h.handler.Handle(context.Background(), *lastRecord)
		}
	}

	// 更新状态
	h.lastKey = key
	h.lastCount = 1

	// 打印当前日志
	return h.handler.Handle(context.Background(), r)
}

func (h *DeduplicatingHandler) buildDedupeKey(r slog.Record) string {
	// 包含：级别、消息
	key := fmt.Sprintf("%s|%s", r.Level.String(), r.Message)

	// 添加组件和关键属性
	attrs := make([]slog.Attr, 0)
	r.Attrs(func(a slog.Attr) bool {
		// 排除 Count 属性本身
		if a.Key != "Count" {
			attrs = append(attrs, a)
		}
		return true
	})

	// 按字母顺序排序属性以确保一致性
	for _, a := range attrs {
		if a.Key == "component" {
			key += "|" + a.Key + "=" + a.Value.String()
			break
		}
	}

	return key
}

func (h *DeduplicatingHandler) createRecordWithCount(key string, count int) *slog.Record {
	// 从key中提取级别和消息
	parts := strings.SplitN(key, "|", 2)
	if len(parts) < 2 {
		return nil
	}

	var level slog.Level
	switch parts[0] {
	case "ERROR":
		level = slog.LevelError
	case "WARN":
		level = slog.LevelWarn
	case "INFO":
		level = slog.LevelInfo
	default:
		level = slog.LevelDebug
	}

	// 从key中提取原始消息（去掉 Count 部分）
	msg := parts[1]
	msg = strings.TrimSuffix(msg, fmt.Sprintf(" (Count: %d)", count))

	// 创建新记录，添加 Count 属性
	r := &slog.Record{
		Time:    time.Now(),
		Level:   level,
		Message: msg,
	}
	r.AddAttrs(slog.Int("Count", count))

	return r
}

func (h *DeduplicatingHandler) Flush() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// 程序退出时，打印最后一条重复日志（如果有）
	if h.lastCount > 1 {
		record := h.createRecordWithCount(h.lastKey, h.lastCount)
		if record != nil {
			h.handler.Handle(context.Background(), *record)
		}
	}

	return nil
}

func (h *DeduplicatingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs), len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	newAttrs = append(newAttrs, attrs...)
	return &DeduplicatingHandler{
		handler:   h.handler.WithAttrs(attrs),
		lastKey:   h.lastKey,
		lastCount: h.lastCount,
		attrs:     newAttrs,
		group:     h.group,
	}
}

func (h *DeduplicatingHandler) WithGroup(name string) slog.Handler {
	return &DeduplicatingHandler{
		handler:   h.handler.WithGroup(name),
		lastKey:   h.lastKey,
		lastCount: h.lastCount,
		attrs:     h.attrs,
		group:     name,
	}
}
