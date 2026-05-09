package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/center"
	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/logger"
)

func main() {
	logger.Setup(os.Stdout)

	slog.Info("Center Server 启动中")

	srv := center.NewServer()

	httpServer := &http.Server{
		Addr:         ":8081",
		Handler:      srv.Router(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("HTTP 服务已启动", "地址", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("服务异常", "error", err)
			os.Exit(1)
		}
	}()

	sig := <-sigCh
	slog.Info("收到信号，正在关闭...", "signal", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)

	slog.Info("Center Server 已停止")
}
