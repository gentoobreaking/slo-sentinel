// sentinel-ui（T016）：唯讀 Web 服務。
//
// 安全邊界（spec.md §2.5）：
//   - 純唯讀——只有 GET 路由，無任何寫入端點
//   - 資料源僅 sentinel 的 /api/status.json（不直連 SQLite）
//   - 預設綁 127.0.0.1；對外一律經反向代理認證
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
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
	mux.HandleFunc("GET /", handleIndex(cfg))
	mux.HandleFunc("GET /api/status.json", proxyTo(cfg.SentinelAPI + "/api/status.json"))

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
		client := &http.Client{}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", cfg.SentinelAPI+"/api/status.json", nil)
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "sentinel API 無法連線", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		var payload struct {
			States map[string]struct {
				SensorID  string  `json:"sensor_id"`
				State     string  `json:"state"`
				LastValue float64 `json:"last_value"`
				UpdatedAt string  `json:"updated_at"`
			} `json:"states"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			http.Error(w, "回應解析失敗", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!DOCTYPE html><html><head><meta charset='utf-8'><title>sentinel</title></head><body>")
		fmt.Fprint(w, "<h1>🔍 slo-sentinel 感測總表</h1><table border='1' cellpadding='4'>")
		fmt.Fprint(w, "<tr><th>感測</th><th>狀態</th><th>最後值</th><th>更新時間</th></tr>")
		icon := map[string]string{"healthy": "✅", "warning": "⚠️", "critical": "🔴"}
		for id, st := range payload.States {
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s %s</td><td>%.4g</td><td>%s</td></tr>",
				id, icon[st.State], st.State, st.LastValue, st.UpdatedAt)
		}
		fmt.Fprint(w, "</table></body></html>")
	}
}

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
