package main

// am_publish.go（T020）：容量預警以標準 AlertManager alert 格式轉交
// ai-oncall gate 分診。payload 即 AM webhook 格式（version + alerts 陣列），
// 與 gate/internal/ingest/alertmanager.go 的 Normalize 契約對齊：
//   - labels: alertname / scope / sensor_id / severity / service / cluster /
//     eta_aggressive / eta_conservative
//   - annotations: summary（雙視野人話卡）/ runbook_url
//   - status: firing / resolved；startsAt RFC3339

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// AMPublisher 將 alert 以 AM webhook 格式 POST 到分診閘門。
type AMPublisher struct {
	URL   string // 如 http://gate:8080/alerts
	Token string // Bearer token（與 gate SHARED_SECRET 一致）；空 = 不帶認證
}

// AMAlert 為單筆 alert（AM webhook 格式的 alerts 元素）。
type AMAlert struct {
	Status       string            `json:"status,omitempty"` // firing / resolved
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	StartsAt     string            `json:"startsAt,omitempty"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
}

// Publish 送出一批 alert。非 2xx 視為失敗（呼叫端走 fallback/重試語意）。
func (p *AMPublisher) Publish(ctx context.Context, alerts []AMAlert) error {
	if p.URL == "" || len(alerts) == 0 {
		return fmt.Errorf("publisher 未設定或無 alert")
	}
	body, err := json.Marshal(struct {
		Version string    `json:"version"`
		Alerts  []AMAlert `json:"alerts"`
	}{Version: "4", Alerts: alerts})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gate 回 %d", resp.StatusCode)
	}
	return nil
}
