package main

// api.go（T009）：唯讀 JSON API（G1）與 Prometheus /metrics 暴露（G2，僅觀測）。
// 兩個端點預設皆綁 127.0.0.1；對外暴露需使用者明確修改設定並自負代理層責任。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"slo-sentinel/internal/store"
)

// metricsRegistry 執行緒安全的簡易指標集（Prometheus text 格式輸出）。
type metricsRegistry struct {
	mu     sync.RWMutex
	values map[string]float64
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{values: map[string]float64{}}
}

// Set 覆寫指標值（nameWithLabels 含完整 label 部分，如 `x{sensor="a"}`）。
func (m *metricsRegistry) Set(nameWithLabels string, v float64) {
	m.mu.Lock()
	m.values[nameWithLabels] = v
	m.mu.Unlock()
}

// renderText 渲染為 Prometheus text exposition format。
func (m *metricsRegistry) renderText() []byte {
	keys := make([]string, 0, len(m.values))
	for k := range m.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s %v\n", k, m.values[k])
	}
	return []byte(b.String())
}

// serveMetrics 啟動 /metrics HTTP 端點（直推中心定案：僅供觀測，非告警輸入）。
func serveMetrics(addr string, reg *metricsRegistry) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(reg.renderText())
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	return srv.ListenAndServe()
}

// readAPI 提供 UI/查詢用唯讀 JSON 端點。
type readAPI struct {
	d *daemon
}

func (a *readAPI) serve(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status.json", a.statusJSON)
	srv := &http.Server{Addr: addr, Handler: mux}
	return srv.ListenAndServe()
}

func (a *readAPI) statusJSON(w http.ResponseWriter, r *http.Request) {
	states := map[string]storeSensorStateJSON{}
	a.d.eachState(func(st store.SensorState) {
		states[st.SensorID] = storeSensorStateJSON{
			SensorID: st.SensorID, State: st.State,
			LastValue: st.LastValue,
			UpdatedAt: st.UpdatedAt.Format(time.RFC3339Nano),
		}
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"states": states})
}

// storeSensorStateJSON 為對外的 JSON 形狀（與 store 內部結構解耦）。
type storeSensorStateJSON struct {
	SensorID  string  `json:"sensor_id"`
	State     string  `json:"state"`
	LastValue float64 `json:"last_value"`
	UpdatedAt string  `json:"updated_at"`
}
