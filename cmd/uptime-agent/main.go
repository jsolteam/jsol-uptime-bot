package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jsolteam/jsol-uptime-bot/internal/agent"
	"github.com/jsolteam/jsol-uptime-bot/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		slog.Error("укажите путь к конфигурации")
		os.Exit(2)
	}
	c, e := config.Load(os.Args[1])
	if e != nil {
		slog.Error("конфигурация", "error", e)
		os.Exit(1)
	}
	listener, e := net.Listen("tcp", c.Agent.Listen)
	if e != nil {
		slog.Error("не удалось открыть порт агента", "error", e)
		os.Exit(1)
	}
	s := &http.Server{Addr: c.Agent.Listen, Handler: agent.Handler(strings.TrimSpace(os.Getenv(c.Agent.TokenEnv))), ReadHeaderTimeout: 3 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdownCtx)
	}()
	e = s.Serve(listener)
	if e != nil && e != http.ErrServerClosed {
		slog.Error("агент", "error", e)
		os.Exit(1)
	}
}
