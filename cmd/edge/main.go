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
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := config.Load(); err != nil {
		slog.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	slog.Info("Starting Edge Server", "client_id", config.ClientID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Initialize components
	lmsClient := lms.NewClient()
	defer lmsClient.Close()

	// Try initial login / session check
	slog.Info("Checking LMS session...")
	if _, err := lmsClient.GetRollcalls(ctx); err != nil {
		slog.Warn("Initial rollcall check failed (will retry)", "error", err)
	}

	poller := edge.NewPoller(lmsClient)
	wsClient := edge.NewWSClient(lmsClient, poller)
	poller.SetSendFunc(wsClient.SendToCenter)
	server := edge.NewServer(lmsClient, wsClient, poller)

	// Start background goroutines
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Panic in poller", "panic", r)
			}
		}()
		poller.Run(ctx)
	}()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Panic in ws_client", "panic", r)
			}
		}()
		wsClient.Run(ctx)
	}()

	// Start HTTP server if configured
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
			slog.Info("HTTP server listening", "addr", addr)
			if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
				slog.Error("HTTP server error", "error", err)
			}
		}()

		// Wait for signal
		sig := <-sigCh
		slog.Info("Received signal, shutting down...", "signal", sig)
		cancel()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		httpServer.Shutdown(shutdownCtx)
	} else {
		// No HTTP, just wait for signal
		sig := <-sigCh
		slog.Info("Received signal, shutting down...", "signal", sig)
		cancel()
	}

	slog.Info("Edge Server stopped")
}
