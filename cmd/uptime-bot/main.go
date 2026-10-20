package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
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
	listener, err := net.Listen("tcp", cfg.Agent.Listen)
	if err != nil {
		slog.Error("не удалось открыть порт агента", "error", err)
		os.Exit(1)
	}
	if cfg.Agent.TLSCertFile != "" {
		if _, err = tls.LoadX509KeyPair(cfg.Agent.TLSCertFile, cfg.Agent.TLSKeyFile); err != nil {
			slog.Error("сертификат TLS агента", "error", err)
			os.Exit(1)
		}
	}
	var nodeStatus atomic.Value
	var leaderActive atomic.Bool
	var pollingActive atomic.Bool
	var pollingBlocked atomic.Bool
	nodeStatus.Store(agent.NodeStatus{NodeID: cfg.Node.ID, Priority: cfg.Node.Priority})
	agentServer := &http.Server{Addr: cfg.Agent.Listen, Handler: agent.Handler(token, func() agent.NodeStatus {
		status := nodeStatus.Load().(agent.NodeStatus)
		status.Polling = pollingActive.Load()
		return status
	}), ReadHeaderTimeout: 3 * time.Second}
	go func() {
		var e error
		if cfg.Agent.TLSCertFile != "" {
			e = agentServer.ServeTLS(listener, cfg.Agent.TLSCertFile, cfg.Agent.TLSKeyFile)
		} else {
			e = agentServer.Serve(listener)
		}
		if e != nil && !errors.Is(e, http.ErrServerClosed) {
			slog.Error("агент", "error", e)
		}
	}()
	if strings.TrimSpace(os.Getenv(cfg.Telegram.TokenEnv)) == "" {
		slog.Info("локальный агент запущен", "node", cfg.Node.ID)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = agentServer.Shutdown(shutdownCtx)
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
	elector := cluster.New(cfg, token)

	tick := time.NewTicker(time.Duration(cfg.Coordination.Heartbeat))
	defer tick.Stop()
	var leaderCancel context.CancelFunc
	var leader bool
	pollingFailure := make(chan error, 1)
	internetFailures, internetSuccesses := 0, 0
	telegramFailures, telegramSuccesses := 0, 0
	telegramSuppressedUntil := time.Time{}
	mon := monitor.New(cfg, db)
	mon.SetLocalStatus(func() agent.NodeStatus { return nodeStatus.Load().(agent.NodeStatus) })
	mon.OnEvent = func(message string) {
		if leaderActive.Load() {
			bot.Notify(message)
		}
	}
	go mon.Run(ctx, nil)
	readinessCtx, readinessCancel := context.WithTimeout(ctx, 2*time.Duration(cfg.Monitoring.Timeout))
	nodeStatus.Store(cluster.CheckReadiness(readinessCtx, cfg))
	readinessCancel()
	for {
		select {
		case <-ctx.Done():
			if leaderCancel != nil {
				leaderCancel()
			}
			leaderActive.Store(false)
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = agentServer.Shutdown(shutdownCtx)
			cancel()
			return
		case pollingErr := <-pollingFailure:
			if leader && leaderCancel != nil {
				leaderCancel()
				leaderCancel = nil
				leader = false
				leaderActive.Store(false)
				telegramSuppressedUntil = time.Now().Add(3 * time.Duration(cfg.Coordination.Heartbeat))
				status := nodeStatus.Load().(agent.NodeStatus)
				status.Telegram = false
				status.Ready = false
				status.CheckedAt = time.Now().UTC()
				nodeStatus.Store(status)
				slog.Warn("Telegram polling недоступен или занят; роль ведущего уступлена", "error", pollingErr)
				_ = db.Event(ctx, "", "leader", "Узел уступил роль Telegram-ведущего: polling недоступен или занят")
			}
		case <-tick.C:
			if !telegramSuppressedUntil.IsZero() && !time.Now().Before(telegramSuppressedUntil) {
				pollingBlocked.Store(false)
			}
			probeCtx, cancelProbe := context.WithTimeout(ctx, 2*time.Duration(cfg.Monitoring.Timeout))
			status := cluster.CheckReadiness(probeCtx, cfg)
			cancelProbe()
			previous := nodeStatus.Load().(agent.NodeStatus)
			suppressed := time.Now().Before(telegramSuppressedUntil)
			if suppressed {
				status.Telegram = false
			}
			status.Internet = stableSignal(status.Internet, previous.Internet, &internetFailures, &internetSuccesses, cfg.Monitoring.FailureThreshold, cfg.Monitoring.RecoveryThreshold)
			status.Telegram = stableSignal(status.Telegram, previous.Telegram, &telegramFailures, &telegramSuccesses, cfg.Monitoring.FailureThreshold, cfg.Monitoring.RecoveryThreshold)
			if suppressed {
				telegramFailures = cfg.Monitoring.FailureThreshold
				telegramSuccesses = 0
				status.Telegram = false
			}
			status.Ready = status.Internet && status.Telegram
			status.Polling = pollingActive.Load()
			nodeStatus.Store(status)
			electionCtx, cancelElection := context.WithTimeout(ctx, time.Duration(cfg.Monitoring.Timeout))
			leaderID, shouldLead, electionErr := elector.Leader(electionCtx, status)
			cancelElection()
			if electionErr != nil {
				slog.Warn("ошибка проверки узлов кластера", "error", electionErr)
			}
			if leader && !shouldLead {
				leaderCancel()
				leaderCancel = nil
				leader = false
				leaderActive.Store(false)
				slog.Info("узел уступил роль ведущего", "node", cfg.Node.ID, "leader", leaderID)
				_ = db.Event(ctx, "", "leader", "Узел уступил роль ведущего: "+cfg.Node.ID+"; новый кандидат: "+leaderID)
			}
			if !leader && shouldLead && !pollingActive.Load() && !pollingBlocked.Load() {
				lctx, lcancel := context.WithCancel(ctx)
				leaderCancel = lcancel
				leader = true
				leaderActive.Store(true)
				slog.Info("узел выбран ведущим", "node", cfg.Node.ID)
				_ = db.Event(ctx, "", "leader", "Узел стал ведущим: "+cfg.Node.ID)
				bot.Notify("Ведущий узел: " + cfg.Node.ID)
				pollingActive.Store(true)
				go func() {
					defer pollingActive.Store(false)
					if err := bot.Run(lctx); err != nil {
						pollingBlocked.Store(true)
						select {
						case pollingFailure <- err:
						default:
						}
					}
				}()
				go bot.RunDailyReports(lctx, func(ctx context.Context, date string) (bool, error) {
					return db.ClaimOnce(ctx, "daily-report:"+date)
				})
			}
		}
	}
}

func stableSignal(current, previous bool, failures, successes *int, failureThreshold, recoveryThreshold int) bool {
	if current {
		*failures = 0
		*successes++
		return previous || *successes >= recoveryThreshold
	}
	*successes = 0
	*failures++
	return previous && *failures < failureThreshold
}
