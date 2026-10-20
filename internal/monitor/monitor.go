package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jsolteam/jsol-uptime-bot/internal/agent"
	"github.com/jsolteam/jsol-uptime-bot/internal/config"
	"github.com/jsolteam/jsol-uptime-bot/internal/storage"
	probing "github.com/prometheus-community/pro-bing"
)

type Probe struct {
	Server                                           config.Server
	Metrics                                          agent.Metrics
	Status, Internet, AgentStatus, PingStatus, Error string
	CheckedAt                                        time.Time
}
type Monitor struct {
	cfg                                 config.Config
	store                               *storage.Store
	client                              *http.Client
	failures, successes                 map[string]int
	states, internetStates, agentStates map[string]string
	agentDownSince                      map[string]time.Time
	localStatus                         func() agent.NodeStatus
	OnEvent                             func(string)
}

func New(c config.Config, s *storage.Store) *Monitor {
	return &Monitor{cfg: c, store: s, client: &http.Client{Timeout: time.Duration(c.Monitoring.Timeout)}, failures: map[string]int{}, successes: map[string]int{}, states: map[string]string{}, internetStates: map[string]string{}, agentStates: map[string]string{}, agentDownSince: map[string]time.Time{}}
}

func (m *Monitor) SetLocalStatus(status func() agent.NodeStatus) { m.localStatus = status }
func (m *Monitor) event(ctx context.Context, id, kind, msg string) {
	if err := m.store.Event(ctx, id, kind, msg); err != nil {
		slog.Error("не удалось записать событие", "server", id, "error", err)
	}
	if m.OnEvent != nil {
		m.OnEvent(msg)
	}
}
func (m *Monitor) Check(ctx context.Context, s config.Server) Probe {
	p := Probe{Server: s, CheckedAt: time.Now().UTC(), Status: "UNKNOWN", Internet: "UNKNOWN", AgentStatus: "AGENT_UNAVAILABLE"}
	if s.ID == m.cfg.Node.ID {
		p.Status = "UP"
		p.PingStatus = "LOCAL"
		metrics, err := agent.Collect(ctx)
		if err != nil {
			return m.finish(ctx, p, err)
		}
		p.Metrics = metrics
		p.AgentStatus = "AVAILABLE"
		if m.localStatus != nil {
			if m.localStatus().Internet {
				p.Metrics.Internet = "INTERNET_OK"
			} else {
				p.Metrics.Internet = "NO_INTERNET"
			}
		}
		return m.finish(ctx, p, nil)
	}
	type pingResult struct {
		reachable bool
		err       error
	}
	pingDone := make(chan pingResult, 1)
	go func() {
		reachable, err := pingHost(ctx, s.Address, time.Duration(m.cfg.Monitoring.Timeout))
		pingDone <- pingResult{reachable: reachable, err: err}
	}()
	token := strings.TrimSpace(os.Getenv(m.cfg.Agent.TokenEnv))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.AgentURL, "/")+"/metrics", nil)
	if err == nil && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if err == nil {
		resp, e := m.client.Do(req)
		if e == nil {
			if resp.StatusCode == http.StatusOK {
				decodeErr := json.NewDecoder(resp.Body).Decode(&p.Metrics)
				resp.Body.Close()
				if decodeErr == nil {
					p.AgentStatus = "AVAILABLE"
				} else {
					err = decodeErr
				}
			} else {
				err = fmt.Errorf("agent HTTP %d", resp.StatusCode)
				resp.Body.Close()
			}
		} else {
			err = e
		}
	}
	var ping pingResult
	select {
	case ping = <-pingDone:
	case <-ctx.Done():
		ping.err = ctx.Err()
	}
	reachable, pingErr := ping.reachable, ping.err
	if pingErr != nil {
		p.PingStatus = "UNKNOWN"
	} else if reachable {
		p.PingStatus = "REACHABLE"
	} else {
		p.PingStatus = "NO_REPLY"
	}
	if p.AgentStatus != "AVAILABLE" {
		host, port := s.Address, "8080"
		if parsed, _, e := net.SplitHostPort(host); e == nil {
			host = parsed
		}
		if u, e := url.Parse(s.AgentURL); e == nil && u.Port() != "" {
			port = u.Port()
		}
		d := net.Dialer{Timeout: time.Duration(m.cfg.Monitoring.Timeout)}
		conn, e := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if reachable || e == nil {
			if conn != nil {
				_ = conn.Close()
			}
			p.Status = "UP"
			m.failures[s.ID] = 0
		} else if pingErr == nil {
			m.failures[s.ID]++
			m.successes[s.ID] = 0
			if m.failures[s.ID] >= m.cfg.Monitoring.FailureThreshold {
				p.Status = "DOWN"
			}
		} else {
			p.Status = "UNKNOWN"
		}
	} else {
		m.failures[s.ID] = 0
		m.successes[s.ID]++
		if m.states[s.ID] == "DOWN" && m.successes[s.ID] < m.cfg.Monitoring.RecoveryThreshold {
			p.Status = "DOWN"
		} else {
			p.Status = "UP"
		}
	}
	return m.finish(ctx, p, err)
}

func (m *Monitor) finish(ctx context.Context, p Probe, err error) Probe {
	s := p.Server
	if err != nil {
		p.Error = err.Error()
	}
	if p.AgentStatus == "AVAILABLE" {
		delete(m.agentDownSince, s.ID)
	} else if _, ok := m.agentDownSince[s.ID]; !ok {
		m.agentDownSince[s.ID] = p.CheckedAt
	}
	if p.AgentStatus == "AVAILABLE" {
		if p.Metrics.Internet == "INTERNET_OK" || p.Metrics.Internet == "NO_INTERNET" {
			p.Internet = p.Metrics.Internet
		}
	}
	if p.AgentStatus != "AVAILABLE" && m.cfg.Monitoring.AgentFailureThreshold > 0 && time.Since(m.agentDownSince[s.ID]) >= time.Duration(m.cfg.Monitoring.AgentFailureThreshold) && m.agentStates[s.ID] != "AGENT_UNAVAILABLE" {
		m.agentStates[s.ID] = "AGENT_UNAVAILABLE"
		m.event(ctx, s.ID, "agent", "Агент мониторинга недоступен: "+p.Error)
	} else if p.AgentStatus == "AVAILABLE" && m.agentStates[s.ID] == "AGENT_UNAVAILABLE" {
		m.agentStates[s.ID] = "AVAILABLE"
		m.event(ctx, s.ID, "agent", "Агент мониторинга восстановлен")
	}
	if old := m.states[s.ID]; old != "" && old != p.Status {
		kind := "status"
		message := fmt.Sprintf("%s: %s → %s", s.Name, old, p.Status)
		if p.Status == "DOWN" {
			kind = "down"
			message = "🔴 Сервер недоступен: " + s.Name
		} else if old == "DOWN" && p.Status == "UP" {
			kind = "recovery"
			message = "🟢 Сервер восстановлен: " + s.Name
		}
		m.event(ctx, s.ID, kind, message)
	}
	m.states[s.ID] = p.Status
	if old := m.internetStates[s.ID]; p.Internet != "UNKNOWN" && old != "" && old != p.Internet {
		msg := fmt.Sprintf("%s: интернет %s", s.Name, p.Internet)
		if p.Internet == "NO_INTERNET" {
			msg = "⚠️ Нет доступа в интернет: " + s.Name
		} else {
			msg = "✅ Доступ в интернет восстановлен: " + s.Name
		}
		m.event(ctx, s.ID, "internet", msg)
	}
	if p.Internet != "UNKNOWN" {
		m.internetStates[s.ID] = p.Internet
	}
	if e := m.store.Save(ctx, storage.Sample{ServerID: s.ID, Status: p.Status, Internet: p.Internet, AgentStatus: p.AgentStatus, PingStatus: p.PingStatus, Error: p.Error, CPU: p.Metrics.CPU, RAMUsed: float64(p.Metrics.RAMUsed), RAMTotal: float64(p.Metrics.RAMTotal), DiskUsed: float64(p.Metrics.DiskUsed), DiskTotal: float64(p.Metrics.DiskTotal), Uptime: p.Metrics.Uptime, CheckedAt: p.CheckedAt}); e != nil {
		slog.Error("не удалось сохранить измерение", "server", s.ID, "error", e)
	}
	return p
}

func pingHost(ctx context.Context, address string, timeout time.Duration) (bool, error) {
	host := strings.Trim(address, "[]")
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	pinger, err := probing.NewPinger(host)
	if err != nil {
		return false, err
	}
	pinger.SetPrivileged(true)
	pinger.Count = 1
	pinger.Timeout = timeout
	pinger.Interval = 200 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- pinger.Run() }()
	select {
	case err := <-done:
		if err != nil {
			return false, err
		}
		return pinger.Statistics().PacketsRecv > 0, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
func (m *Monitor) Run(ctx context.Context, report func([]Probe)) {
	ticker := time.NewTicker(time.Duration(m.cfg.Monitoring.Interval))
	defer ticker.Stop()
	for {
		probes := make([]Probe, 0, len(m.cfg.Servers))
		for _, s := range m.cfg.Servers {
			c, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.Monitoring.Timeout))
			probes = append(probes, m.Check(c, s))
			cancel()
		}
		if report != nil {
			report(probes)
		}
		if err := m.store.Prune(ctx, m.cfg.Monitoring.RetentionDays); err != nil {
			slog.Error("ошибка очистки истории", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
