package main

// api.go（T009）：唯讀 JSON API（G1）與 Prometheus /metrics 暴露（G2，僅觀測）。
// 兩個端點預設皆綁 127.0.0.1；對外暴露需使用者明確修改設定並自負代理層責任。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"slo-sentinel/internal/billing"
	"slo-sentinel/internal/cost"
	"slo-sentinel/internal/store"
	"slo-sentinel/internal/waste"
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
	mux.HandleFunc("/api/accuracy", a.accuracyJSON)
	mux.HandleFunc("/api/slo/", a.sloDetail)
	mux.HandleFunc("/api/cost", a.costJSON)
	mux.HandleFunc("/api/waste", a.wasteJSON)
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

// accuracyJSON 彙整各感測的預測紀錄筆數與最近 ETA（/accuracy 頁面資料源）。
func (a *readAPI) accuracyJSON(w http.ResponseWriter, r *http.Request) {
	since := time.Now().UTC().Add(-7 * 24 * time.Hour)
	out := []map[string]any{}
	for _, sr := range a.d.sensors {
		preds, err := a.d.st.ListPredictions(sr.id, since)
		if err != nil {
			continue
		}
		var lastEta *float64
		if len(preds) > 0 {
			lastEta = preds[len(preds)-1].EtaAggressive
		}
		out = append(out, map[string]any{
			"sensor_id": sr.id, "predictions": len(preds),
			"last_eta_aggressive_sec": lastEta,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"since": since.Format(time.RFC3339), "sensors": out})
}

// sloDetail 回傳單一感測的狀態＋預測預測歷史（預測 vs 實際曲線資料源）。
func (a *readAPI) sloDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/slo/")
	if id == "" {
		http.Error(w, "missing sensor id", http.StatusBadRequest)
		return
	}
	st, err := a.d.st.GetState(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	preds, err := a.d.st.ListPredictions(id, time.Now().UTC().Add(-time.Duration(days)*24*time.Hour))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"sensor_id": id, "state": st, "predictions": preds,
	})
}

// costJSON 月度成本現況與推估（F11–F13；未設定帳務來源時回 enabled:false）。
func (a *readAPI) costJSON(w http.ResponseWriter, r *http.Request) {
	if a.d.billingSrc == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"enabled": false,
			"hint":    "設定 AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY 或 ALICLOUD_ACCESS_KEY_ID/ALICLOUD_ACCESS_KEY_SECRET 以啟用",
		})
		return
	}
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	spends, err := a.d.billingSrc.DailySpend(r.Context(), billing.Filter{}, monthStart, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var mtd float64
	for _, sp := range spends {
		mtd += sp.CostUSD
	}
	dElapsed := now.Day()
	rates := cost.EstimateRates(mtd, dElapsed, recentTail(spends, 7))
	eom := cost.ProjectEOM(mtd, now, rates)
	resp := map[string]any{
		"enabled":           true,
		"confirmed_through": lastConfirmed(spends),
		"mtd_usd":           mtd,
		"eom_projection": map[string]float64{
			"aggressive": eom.Aggressive, "conservative": eom.Conservative,
		},
		"daily_spends": spends,
	}
	// estimate 模式（§D.0 主路徑）：與 actual 並存，差異供校準對照
	if a.d.pricer != nil && len(a.d.costMap) > 0 {
		est := cost.EstimateSpend(r.Context(), a.d.estimateLines(), a.d.pricer)
		resp["estimate"] = est
		resp["actual_vs_estimate"] = cost.CompareActualVsEstimate(mtd, est)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// wasteJSON 即時執行一次 waste 掃描（基於最近載入的目錄）。
func (a *readAPI) wasteJSON(w http.ResponseWriter, r *http.Request) {
	if a.d.lastCatalog == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"candidates": []any{}, "note": "目錄尚未載入"})
		return
	}
	sc := &waste.Scanner{Src: a.d.src}
	cands, err := sc.Scan(r.Context(), a.d.lastCatalog, time.Now().UTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"candidates": cands})
}

func recentTail(spends []billing.DailySpend, n int) []billing.DailySpend {
	if len(spends) <= n {
		return spends
	}
	return spends[len(spends)-n:]
}

func lastConfirmed(spends []billing.DailySpend) string {
	if len(spends) == 0 {
		return ""
	}
	return spends[len(spends)-1].Date.Format("2006-01-02")
}

// storeSensorStateJSON 為對外的 JSON 形狀（與 store 內部結構解耦）。
type storeSensorStateJSON struct {
	SensorID  string  `json:"sensor_id"`
	State     string  `json:"state"`
	LastValue float64 `json:"last_value"`
	UpdatedAt string  `json:"updated_at"`
}
