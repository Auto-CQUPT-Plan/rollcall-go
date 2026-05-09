package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/config"
	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/edge"
	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/lms"
	"github.com/Auto-CQUPT-Plan/rollcall-go/internal/logger"
)

func main() {
	logger.Setup(os.Stdout)

	if err := config.Load(); err != nil {
		slog.Error("配置加载失败", "error", err)
		os.Exit(1)
	}

	slog.Info("Edge Server 启动中", "client_id", config.ClientID[:8])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	lmsClient := lms.NewClient()
	defer lmsClient.Close()

	slog.Info("正在检查 LMS 会话...")
	if _, err := lmsClient.GetRollcalls(ctx); err != nil {
		slog.Warn("初始签到检查失败（稍后重试）", "error", err)
	}

	poller := edge.NewPoller(lmsClient)
	wsClient := edge.NewWSClient(lmsClient, poller)
	poller.SetSendFunc(wsClient.SendToCenter)
	server := edge.NewServer(lmsClient, wsClient, poller)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("轮询器崩溃", "panic", r)
			}
		}()
		poller.Run(ctx)
	}()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("WebSocket 客户端崩溃", "panic", r)
			}
		}()
		wsClient.Run(ctx)
	}()

	if config.Cfg.HTTPPort != nil {
		addr := fmt.Sprintf(":%d", *config.Cfg.HTTPPort)
		httpServer := &http.Server{
			Addr:         addr,
			Handler:      server.Router(),
			ReadTimeout:  15 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  60 * time.Second,
		}

		go func() {
			slog.Info("HTTP 服务已启动", "地址", addr)
			if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
				slog.Error("HTTP 服务异常", "error", err)
			}
		}()

		sig := <-sigCh
		slog.Info("收到信号，正在关闭...", "signal", sig)
		cancel()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		httpServer.Shutdown(shutdownCtx)
	} else {
		sig := <-sigCh
		slog.Info("收到信号，正在关闭...", "signal", sig)
		cancel()
	}

	slog.Info("Edge Server 已停止")
}
