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
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	slog.Info("Starting Center Server")

	srv := center.NewServer()

	httpServer := &http.Server{
		Addr:         ":8081",
		Handler:      srv.Router(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // WebSocket needs no write timeout
		IdleTimeout:  120 * time.Second,
	}

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("Center HTTP server listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("Center server error", "error", err)
			os.Exit(1)
		}
	}()

	sig := <-sigCh
	slog.Info("Received signal, shutting down...", "signal", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)

	slog.Info("Center Server stopped")
}
