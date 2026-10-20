package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jsolteam/jsol-uptime-bot/internal/agent"
	"github.com/jsolteam/jsol-uptime-bot/internal/cluster"
	"github.com/jsolteam/jsol-uptime-bot/internal/config"
	"github.com/jsolteam/jsol-uptime-bot/internal/monitor"
	"github.com/jsolteam/jsol-uptime-bot/internal/storage"
	"github.com/jsolteam/jsol-uptime-bot/internal/telegram"
)

func main() {
	if len(os.Args) < 2 {
		slog.Error("укажите путь к YAML-конфигурации")
		os.Exit(2)
	}
	cfg, err := config.Load(os.Args[1])
	if err != nil {
		slog.Error("конфигурация", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	token := strings.TrimSpace(os.Getenv(cfg.Agent.TokenEnv))
	agentServer := &http.Server{Addr: cfg.Agent.Listen, Handler: agent.Handler(token), ReadHeaderTimeout: 3 * time.Second}
	go func() {
		if e := agentServer.ListenAndServe(); e != nil && !errors.Is(e, http.ErrServerClosed) {
			slog.Error("агент", "error", e)
		}
	}()
	if strings.TrimSpace(os.Getenv(cfg.Telegram.TokenEnv)) == "" {
		slog.Info("локальный агент запущен", "node", cfg.Node.ID)
		<-ctx.Done()
		_ = agentServer.Shutdown(context.Background())
		return
	}
	db, e := storage.Open(cfg.Storage.SQLitePath)
	if e != nil {
		slog.Error("SQLite", "error", e)
		os.Exit(1)
	}
	defer db.Close()
	go db.RunBackups(ctx, cfg.Storage.BackupDir, time.Duration(cfg.Storage.BackupInterval), cfg.Storage.BackupRetentionDays)
	if e = db.ProvisionAdmins(ctx, cfg.Admins); e != nil {
		slog.Error("не удалось загрузить администраторов", "error", e)
		os.Exit(1)
	}
	servers := make([]storage.Server, 0, len(cfg.Servers))
	for _, server := range cfg.Servers {
		servers = append(servers, storage.Server{ID: server.ID, Name: server.Name, Address: server.Address, Priority: server.Priority, AgentURL: server.AgentURL})
	}
	if e = db.SyncServers(ctx, servers); e != nil {
		slog.Error("не удалось сохранить конфигурацию серверов", "error", e)
		os.Exit(1)
	}
	bot, e := telegram.New(os.Getenv(cfg.Telegram.TokenEnv), cfg, db)
	if e != nil {
		slog.Error("Telegram", "error", e)
		os.Exit(1)
	}
	redisURL := strings.TrimSpace(os.Getenv(cfg.Coordination.RedisURLEnv))
	if cfg.Coordination.RedisURLEnv == "" {
		redisURL = strings.TrimSpace(os.Getenv("REDIS_URL"))
	}
	if redisURL == "" {
		slog.Error("для координатора лидера задайте coordination.redis_url_env и REDIS_URL")
		os.Exit(1)
	}
	lease, e := cluster.New(redisURL, "jsol-uptime:leader", cfg.Node.ID, cfg.Node.Priority, time.Duration(cfg.Coordination.Lease), time.Duration(cfg.Coordination.Heartbeat))
	if e != nil {
		slog.Error("Redis coordinator", "error", e)
		os.Exit(1)
	}
	defer lease.Close()

	tick := time.NewTicker(time.Duration(cfg.Coordination.Heartbeat))
	defer tick.Stop()
	var leaderCancel context.CancelFunc
	var leader bool
	mon := monitor.New(cfg, db)
	mon.OnEvent = bot.Notify
	for {
		select {
		case <-ctx.Done():
			if leaderCancel != nil {
				leaderCancel()
			}
			rctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if leader {
				_ = lease.Release(rctx)
			}
			cancel()
			_ = agentServer.Shutdown(context.Background())
			return
		case <-tick.C:
			cctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Monitoring.Timeout))
			if err = lease.Register(cctx); err != nil {
				slog.Warn("не удалось обновить регистрацию узла", "error", err)
				if leader {
					leaderCancel()
					leaderCancel = nil
					leader = false
					slog.Warn("мониторинг остановлен: потеряна связь с координатором")
					bot.Notify("Мониторинг остановлен: связь с координатором потеряна")
				}
				cancel()
				continue
			}
			if leader {
				err = lease.Renew(cctx)
				if err != nil {
					leaderCancel()
					leaderCancel = nil
					leader = false
					slog.Warn("лидерская аренда потеряна", "error", err)
					_ = lease.Release(cctx)
					_ = db.Event(cctx, "", "leader", "Узел уступил роль ведущего: "+cfg.Node.ID)
					bot.Notify("Ведущий узел переключается")
				}
			} else {
				var acquired bool
				acquired, err = lease.Acquire(cctx)
				if err == nil && acquired {
					lctx, lcancel := context.WithCancel(ctx)
					leaderCancel = lcancel
					leader = true
					slog.Info("узел стал ведущим", "node", cfg.Node.ID)
					_ = db.Event(cctx, "", "leader", "Узел стал ведущим: "+cfg.Node.ID)
					bot.Notify("Ведущий узел: " + cfg.Node.ID)
					go mon.Run(lctx, nil)
					go bot.Run(lctx)
					go bot.RunDailyReports(lctx, lease.ClaimOnce)
				}
			}
			cancel()
			if err != nil {
				slog.Warn("координатор недоступен", "error", err)
			}
		}
	}
}
