package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jsolteam/jsol-uptime-bot/internal/agent"
	"github.com/jsolteam/jsol-uptime-bot/internal/config"
)

type Elector struct {
	config config.Config
	client *http.Client
	token  string
}

func New(c config.Config, token string) *Elector {
	return &Elector{config: c, token: token, client: &http.Client{Timeout: time.Duration(c.Monitoring.Timeout)}}
}

func CheckReadiness(ctx context.Context, c config.Config) agent.NodeStatus {
	status := agent.NodeStatus{NodeID: c.Node.ID, Priority: c.Node.Priority, CheckedAt: time.Now().UTC()}
	status.Internet = internetAvailable(ctx, c.Monitoring.InternetChecks, time.Duration(c.Monitoring.Timeout))
	status.Telegram = endpointAvailable(ctx, "https://api.telegram.org", time.Duration(c.Monitoring.Timeout))
	status.Ready = status.Internet && status.Telegram
	return status
}

func internetAvailable(ctx context.Context, targets []string, timeout time.Duration) bool {
	if len(targets) == 0 {
		return false
	}
	type result struct{ ok bool }
	results := make(chan result, len(targets))
	for _, target := range targets {
		go func(target string) {
			parsed, err := url.Parse(target)
			if err != nil || parsed.Hostname() == "" {
				results <- result{}
				return
			}
			checkCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			hosts, err := net.DefaultResolver.LookupHost(checkCtx, parsed.Hostname())
			if err != nil || len(hosts) == 0 {
				results <- result{}
				return
			}
			results <- result{ok: endpointAvailable(checkCtx, target, timeout)}
		}(target)
	}
	ok := 0
	for range targets {
		select {
		case r := <-results:
			if r.ok {
				ok++
			}
		case <-ctx.Done():
			return false
		}
	}
	return ok >= len(targets)/2+1
}

func endpointAvailable(ctx context.Context, endpoint string, timeout time.Duration) bool {
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < http.StatusInternalServerError
}

func (e *Elector) Leader(ctx context.Context, local agent.NodeStatus) (string, bool, error) {
	type candidate struct {
		id       string
		priority int
		ready    bool
		polling  bool
	}
	var reachable []candidate
	type response struct {
		server config.Server
		status agent.NodeStatus
		ok     bool
	}
	responses := make(chan response, len(e.config.Servers))
	requests := 0
	maxAge := 3 * time.Duration(e.config.Coordination.Heartbeat)
	probeCycle := 2*time.Duration(e.config.Monitoring.Timeout) + time.Duration(e.config.Coordination.Heartbeat)
	if probeCycle > maxAge {
		maxAge = probeCycle
	}
	for _, server := range e.config.Servers {
		if server.ID == e.config.Node.ID {
			responses <- response{server: server, status: local, ok: true}
			continue
		}
		requests++
		go func(server config.Server) {
			endpoint := strings.TrimRight(server.AgentURL, "/") + "/cluster/status"
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if err != nil {
				responses <- response{server: server}
				return
			}
			req.Header.Set("Authorization", "Bearer "+e.token)
			resp, err := e.client.Do(req)
			if err != nil {
				responses <- response{server: server}
				return
			}
			defer resp.Body.Close()
			var status agent.NodeStatus
			if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&status) != nil || status.NodeID != server.ID {
				responses <- response{server: server}
				return
			}
			responses <- response{server: server, status: status, ok: true}
		}(server)
	}
	if local.NodeID == e.config.Node.ID && local.CheckedAt.IsZero() {
		return "", false, fmt.Errorf("локальное состояние узла ещё не готово")
	}
	for i := 0; i < requests+1; i++ {
		select {
		case result := <-responses:
			if !result.ok {
				continue
			}
			status := result.status
			if time.Since(status.CheckedAt) > maxAge {
				continue
			}
			reachable = append(reachable, candidate{id: result.server.ID, priority: result.server.Priority, ready: status.Ready, polling: status.Polling})
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	}
	quorum := len(e.config.Servers)/2 + 1
	if len(reachable) < quorum {
		return "", false, nil
	}
	var eligible []candidate
	for _, node := range reachable {
		if node.ready {
			eligible = append(eligible, node)
		}
	}
	if len(eligible) == 0 {
		return "", false, nil
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].priority == eligible[j].priority {
			return eligible[i].id < eligible[j].id
		}
		return eligible[i].priority < eligible[j].priority
	})
	leader := eligible[0].id
	for _, node := range reachable {
		if node.id != e.config.Node.ID && node.polling {
			return node.id, false, nil
		}
	}
	return leader, leader == e.config.Node.ID, nil
}

func (e *Elector) Describe(ctx context.Context, local agent.NodeStatus) string {
	leader, active, err := e.Leader(ctx, local)
	if err != nil {
		return fmt.Sprintf("ошибка выбора ведущего: %v", err)
	}
	if leader == "" {
		return "нет кворума доступных узлов"
	}
	if active {
		return "узел выбран ведущим"
	}
	return "ведущий узел: " + leader
}
