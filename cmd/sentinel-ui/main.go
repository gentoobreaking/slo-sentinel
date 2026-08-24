// sentinel-ui（T016）：唯讀 Web 服務。
//
// 安全邊界（spec.md §2.5）：
//   - 純唯讀——只有 GET 路由，無任何寫入端點
//   - 資料源僅 sentinel 的唯讀 API（不直連 SQLite）
//   - 預設綁 127.0.0.1；對外一律經反向代理認證
//
// 頁面（T016 五張）：/（總表）、/slo/{name}（詳情+預測vs實際）、
// /accuracy（命中統計）、/cost（成本燃盡+推估）、/waste（候選清單）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type uiConfig struct {
	SentinelAPI string // sentinel 的唯讀 API 位址
	ListenAddr  string
}

func loadUIConfig(path string) uiConfig {
	cfg := uiConfig{SentinelAPI: "http://127.0.0.1:9099", ListenAddr: "127.0.0.1:9098"}
	if path == "" {
		return cfg
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err == nil {
		if v, ok := raw["sentinel_api"]; ok && v != "" {
			cfg.SentinelAPI = v
		}
		if v, ok := raw["listen_addr"]; ok && v != "" {
			cfg.ListenAddr = v
		}
	}
	return cfg
}

func main() {
	configPath := flag.String("config", "", "UI 設定檔路徑（JSON）")
	flag.Parse()

	cfg := loadUIConfig(*configPath)

	// 安全警示：非本機綁定需自負代理層責任
	if cfg.ListenAddr != "" && !isLoopbackAddr(cfg.ListenAddr) {
		fmt.Fprintf(os.Stderr,
			"⚠️  監聽位址 %s 非本機迴環：UI 必須置於反向代理認證之後，否則事故資料將暴露\n",
			cfg.ListenAddr)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleIndex(cfg))
	mux.HandleFunc("GET /slo/", handleSloDetail(cfg))
	mux.HandleFunc("GET /accuracy", handleAccuracy(cfg))
	mux.HandleFunc("GET /cost", handleCost(cfg))
	mux.HandleFunc("GET /waste", handleWaste(cfg))
	mux.HandleFunc("GET /api/status.json", proxyTo(cfg.SentinelAPI+"/api/status.json"))

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	logger.Info("sentinel_ui_started", "listen", cfg.ListenAddr, "sentinel_api", cfg.SentinelAPI)
	if err := server.ListenAndServe(); err != nil {
		logger.Error("ui_stopped", "error", err.Error())
		os.Exit(1)
	}
}

func isLoopbackAddr(addr string) bool {
	host := addr
	for i := 0; i < len(addr); i++ {
		if addr[i] == ':' {
			host = addr[:i]
			break
		}
	}
	return host == "127.0.0.1" || host == "localhost" || host == "[::1]"
}

func handleIndex(cfg uiConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed（唯讀 UI）", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			States map[string]struct {
				SensorID  string  `json:"sensor_id"`
				State     string  `json:"state"`
				LastValue float64 `json:"last_value"`
				UpdatedAt string  `json:"updated_at"`
			} `json:"states"`
		}
		if err := fetchJSON(r.Context(), cfg.SentinelAPI+"/api/status.json", &payload); err != nil {
			http.Error(w, "sentinel API 無法連線："+err.Error(), http.StatusBadGateway)
			return
		}
		var rows strings.Builder
		for id, st := range payload.States {
			icon := stateIcon(st.State)
			fmt.Fprintf(&rows,
				"<tr><td><a href='/slo/%s'>%s</a></td><td>%s %s</td><td>%.4g</td><td>%s</td></tr>\n",
				id, id, icon, st.State, st.LastValue, fmtGMT8(st.UpdatedAt))
		}
		page(w, "感測總表", "<table border='1' cellpadding='4'><tr><th>感測</th><th>狀態</th><th>最後值</th><th>更新時間</th></tr>\n"+rows.String()+"</table>")
	}
}

func handleSloDetail(cfg uiConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed（唯讀 UI）", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/slo/")
		if id == "" || strings.Contains(id, "/") {
			http.Error(w, "invalid sensor id", http.StatusBadRequest)
			return
		}
		var detail struct {
			SensorID    string                  `json:"sensor_id"`
			State       *struct{ State string } `json:"state"`
			Predictions []struct {
				PredictedAt     string   `json:"predicted_at"`
				EtaAggressive   *float64 `json:"eta_aggressive"`
				EtaConservative *float64 `json:"eta_conservative"`
				ActualValue     float64  `json:"actual_value"`
				CatalogVersion  string   `json:"catalog_version"`
				Utilization     *float64 `json:"utilization"`
			} `json:"predictions"`
		}
		if err := fetchJSON(r.Context(), cfg.SentinelAPI+"/api/slo/"+id, &detail); err != nil {
			http.Error(w, "查詢失敗："+err.Error(), http.StatusBadGateway)
			return
		}
		stateStr := "unknown"
		if detail.State != nil {
			stateStr = detail.State.State
		}
		var rows strings.Builder
		catVer := ""
		for _, p := range detail.Predictions {
			util := "—"
			if p.Utilization != nil {
				util = fmt.Sprintf("%.1f%%", *p.Utilization*100)
			}
			fmt.Fprintf(&rows, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
				fmtGMT8(p.PredictedAt), humanDur(p.EtaAggressive), humanDur(p.EtaConservative),
				thousandSep(p.ActualValue), util)
			if p.CatalogVersion != "" {
				catVer = p.CatalogVersion
			}
		}
		note := ""
		if catVer != "" {
			note = `<p style="color:#888;font-size:small">目錄版本：` + catVer + `</p>`
		}
		body := fmt.Sprintf("<h2>%s</h2><p>狀態：%s</p>"+
			"<table border='1' cellpadding='4'><tr><th>預測時間</th><th>激進預估（1 小時速率）</th>"+
			"<th>穩健預估（最長窗速率）</th><th>當下用量</th><th>當下使用率</th></tr>\n%s</table>%s",
			id, stateStr, rows.String(), note)
		page(w, "感測詳情："+id, body)
	}
}

var gmt8 = time.FixedZone("GMT+8", 8*3600)

// humanDur 將預測秒數轉為人話（T031）。
// 契約對齊引擎：nil = 斜率 ≤ ε（無成長，不存在觸頂）；負值 = 已穿越天花板。
func humanDur(sec *float64) string {
	if sec == nil {
		return "無成長跡象"
	}
	s := *sec
	switch {
	case s < 0:
		return "已越過天花板"
	case s < 120*60:
		return fmt.Sprintf("約 %.0f 分鐘後觸頂", math.Max(s, 0)/60)
	case s < 48*3600:
		return fmt.Sprintf("約 %.1f 小時後觸頂", s/3600)
	default:
		return fmt.Sprintf("約 %.1f 天後觸頂", s/86400)
	}
}

// thousandSep 千分位格式化（保留小數部分；感測單位未知，不虛構單位）。
func thousandSep(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, frac, _ := strings.Cut(s, ".")
	var b []byte
	for i := 0; i < len(intPart); i++ {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, intPart[i])
	}
	out := string(b)
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

// fmtGMT8 將 RFC3339 UTC 時間戳轉為 GMT+8 人話格式顯示。
// 解析失敗時回傳原字串（容錯不擋頁面）。固定偏移而非載入 tzdata——
// alpine 基礎映像不保證有時區資料庫。
func fmtGMT8(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.In(gmt8).Format("2006-01-02 15:04:05")
}

func handleAccuracy(cfg uiConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed（唯讀 UI）", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			Since   string `json:"since"`
			Sensors []struct {
				SensorID    string   `json:"sensor_id"`
				Predictions int      `json:"predictions"`
				LastEtaAgg  *float64 `json:"last_eta_aggressive_sec"`
			} `json:"sensors"`
		}
		if err := fetchJSON(r.Context(), cfg.SentinelAPI+"/api/accuracy", &payload); err != nil {
			http.Error(w, "查詢失敗："+err.Error(), http.StatusBadGateway)
			return
		}
		var rows strings.Builder
		for _, sn := range payload.Sensors {
			fmt.Fprintf(&rows, "<tr><td>%s</td><td>%d</td><td>%v s</td></tr>\n",
				sn.SensorID, sn.Predictions, ptrVal(sn.LastEtaAgg))
		}
		page(w, "預測命中統計（近 7 天）",
			fmt.Sprintf("<p>資料起始：%s</p><table border='1' cellpadding='4'>"+
				"<tr><th>感測</th><th>預測筆數</th><th>最近激進 ETA(s)</th></tr>\n%s</table>",
				fmtGMT8(payload.Since), rows.String()))
	}
}

func handleCost(cfg uiConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed（唯讀 UI）", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			Enabled          bool    `json:"enabled"`
			Hint             string  `json:"hint"`
			ConfirmedThrough string  `json:"confirmed_through"`
			MtdUSD           float64 `json:"mtd_usd"`
			EomProjection    struct {
				Aggressive   float64 `json:"aggressive"`
				Conservative float64 `json:"conservative"`
			} `json:"eom_projection"`
			Estimate *struct {
				Total    float64 `json:"total"`
				Currency string  `json:"currency"`
				Stale    bool    `json:"stale"`
			} `json:"estimate"`
			ActualVsEstimate *struct {
				ActualMTD float64  `json:"actual_mtd"`
				Estimate  float64  `json:"estimate"`
				Delta     float64  `json:"delta"`
				DeltaPct  *float64 `json:"delta_pct"`
			} `json:"actual_vs_estimate"`
		}
		if err := fetchJSON(r.Context(), cfg.SentinelAPI+"/api/cost", &payload); err != nil {
			http.Error(w, "查詢失敗："+err.Error(), http.StatusBadGateway)
			return
		}
		if !payload.Enabled {
			page(w, "營運成本", "<p>⚠️ 帳務來源未啟用。"+payload.Hint+"</p>")
			return
		}
		body := fmt.Sprintf(
			"<p>本月累積：$%.2f（帳務確認至 %s）</p>"+
				"<p>月底推估：<b>$%.2f</b>（爆量情境 $%.2f）</p>",
			payload.MtdUSD, payload.ConfirmedThrough,
			payload.EomProjection.Conservative, payload.EomProjection.Aggressive)
		// estimate 對照（§D.0：actual 為校準工具，兩者並存）
		if payload.ActualVsEstimate != nil && payload.Estimate != nil && payload.Estimate.Total > 0 {
			staleNote := ""
			if payload.Estimate.Stale {
				staleNote = "（部分單價為過期快取）"
			}
			body += fmt.Sprintf(
				"<p>推估（用量×單價）：$%.2f%s｜actual−estimate 差額 $%.2f（%.1f%%）</p>",
				payload.Estimate.Total, staleNote,
				payload.ActualVsEstimate.Delta, *payload.ActualVsEstimate.DeltaPct)
		}
		page(w, "營運成本", body)
	}
}

func handleWaste(cfg uiConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed（唯讀 UI）", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			Candidates []struct {
				SensorID  string            `json:"sensor_id"`
				AlertName string            `json:"alert_name"`
				Reason    string            `json:"reason"`
				IdleDays  float64           `json:"idle_days"`
				Renotify  time.Duration     `json:"renotify"`
				Labels    map[string]string `json:"labels"`
			} `json:"candidates"`
			Note string `json:"note"`
		}
		if err := fetchJSON(r.Context(), cfg.SentinelAPI+"/api/waste", &payload); err != nil {
			http.Error(w, "查詢失敗："+err.Error(), http.StatusBadGateway)
			return
		}
		if payload.Note != "" {
			page(w, "瘦身影／殭屍資源", "<p>"+payload.Note+"</p>")
			return
		}
		var rows strings.Builder
		for _, c := range payload.Candidates {
			fmt.Fprintf(&rows, "<tr><td>%s</td><td>%s</td><td>%.0f 天</td><td>%s</td></tr>\n",
				c.SensorID, c.AlertName, c.IdleDays, c.Reason)
		}
		page(w, "瘦身影／殭屍資源候選",
			fmt.Sprintf("<p>共 %d 個候選</p><table border='1' cellpadding='4'>"+
				"<tr><th>感測</th><th>規則</th><th>閒置天數</th><th>說明</th></tr>\n%s</table>",
				len(payload.Candidates), rows.String()))
	}
}

// ---- 共用小工具 ----

func fetchJSON(ctx context.Context, url string, out any) error {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func page(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w,
		`<!DOCTYPE html><html><head><meta charset='utf-8'><title>%s — sentinel</title></head>
<body>
<h1>🔍 slo-sentinel</h1>
<nav><a href='/'>總表</a> | <a href='/accuracy'>命中率</a> | <a href='/cost'>成本</a> | <a href='/waste'>瘦身影</a></nav>
<hr/><h2>%s</h2>
%s
</body></html>`, title, title, body)
}

func stateIcon(state string) string {
	switch state {
	case "warning":
		return "⚠️"
	case "critical":
		return "🔴"
	}
	return "✅"
}

// proxyTo 轉發 sentinel 的 JSON 端點（保留給程式化查詢）。
func proxyTo(url string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client := &http.Client{Timeout: 5 * time.Second}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
	}
}

func ptrVal(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}
