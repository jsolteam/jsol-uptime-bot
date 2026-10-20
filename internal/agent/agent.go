package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

type Metrics struct {
	CPU         float64   `json:"cpu_percent"`
	RAMUsed     uint64    `json:"ram_used_bytes"`
	RAMTotal    uint64    `json:"ram_total_bytes"`
	DiskUsed    uint64    `json:"disk_used_bytes"`
	DiskTotal   uint64    `json:"disk_total_bytes"`
	Uptime      uint64    `json:"uptime_seconds"`
	CollectedAt time.Time `json:"collected_at"`
}

func Collect(ctx context.Context) (Metrics, error) {
	var m Metrics
	p, e := cpu.PercentWithContext(ctx, time.Second, false)
	if e != nil {
		return m, e
	}
	v, e := mem.VirtualMemoryWithContext(ctx)
	if e != nil {
		return m, e
	}
	d, e := disk.UsageWithContext(ctx, "/")
	if e != nil {
		return m, e
	}
	h, e := host.InfoWithContext(ctx)
	if e != nil {
		return m, e
	}
	if len(p) > 0 {
		m.CPU = p[0]
	}
	m.RAMUsed = v.Used
	m.RAMTotal = v.Total
	m.DiskUsed = d.Used
	m.DiskTotal = d.Total
	m.Uptime = h.Uptime
	m.CollectedAt = time.Now().UTC()
	return m, nil
}
func Handler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if token == "" || !strings.EqualFold(r.Header.Get("Authorization"), "Bearer "+token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		m, e := Collect(r.Context())
		if e != nil {
			http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
	})
	return mux
}
