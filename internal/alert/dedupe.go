package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Dedupe 實作通知去重狀態機（F3/F4）：
// 同狀態不重複；狀態轉移才推播；resolved 才發「已恢復」。
type Dedupe struct {
	lastState map[string]string // sensor → 上次已通知的狀態
}

func NewDedupe() *Dedupe { return &Dedupe{lastState: map[string]string{}} }

// ShouldNotify 回傳是否應推播此狀態轉移。
//
// 規則：
//   - 首次出現非 healthy 狀態 → 通知
//   - warning→critical 升級 → 通知
//   - 任一狀態 → healthy（恢復）→ 通知 resolved
//   - 同狀態重複 / healthy 持續 → 不通知（digest 負責週期彙總）
func (d *Dedupe) ShouldNotify(sensorID, newState string) bool {
	if d.lastState == nil {
		d.lastState = map[string]string{}
	}
	prev := d.lastState[sensorID]
	if prev == "" {
		prev = "healthy"
	}
	if prev == newState {
		return false
	}
	if newState != "healthy" && prev != "healthy" && rank(newState) <= rank(prev) {
		return false // critical→warning 降級不單獨通知，等恢復一起說
	}
	d.lastState[sensorID] = newState
	return true
}

func rank(state string) int {
	switch state {
	case "critical":
		return 2
	case "warning":
		return 1
	}
	return 0 // healthy
}

// MarkHealthy 直接登記為 healthy（供測試與恢復路徑使用）。
func (d *Dedupe) MarkHealthy(sensorID string) {
	if d.lastState == nil {
		d.lastState = map[string]string{}
	}
	d.lastState[sensorID] = "healthy"
}

// AMCoord 查詢 AlertManager 活躍告警，實作 F2b 協調靜默。
type AMCoord struct {
	BaseURL string
	Client  *http.Client
}

// HasFiringAlerts 回傳 AlertManager 是否已有匹配 filter 的 firing 告警。
// filter 為 alertname 或 label 子字串比對（對 API 回應做包含式檢查）。
func (a *AMCoord) HasFiringAlerts(ctx context.Context, filter string) (bool, error) {
	u := strings.TrimRight(a.BaseURL, "/") + "/api/v2/alerts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	client := a.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var alerts []struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
		Labels map[string]string `json:"labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&alerts); err != nil {
		return false, err
	}
	for _, al := range alerts {
		if al.Status.State != "active" && al.Status.State != "suppressed" {
			continue
		}
		for _, v := range al.Labels {
			if filter != "" && strings.Contains(v, filter) {
				return true, nil
			}
		}
	}
	return false, nil
}

// Digest 將多筆感測狀態彙總為一封摘要訊息（F12/F4 的每日摘要）。
type Digest struct{}

// Format 產生每日摘要文字。entries: sensor → 狀態描述。
func (Digest) Format(entries map[string]string) string {
	var ids []string
	for k := range entries {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("📋 sentinel 每日摘要\n")
	b.WriteString(fmt.Sprintf("追蹤中：%d 個感測\n", len(ids)))
	for _, id := range ids {
		state := entries[id]
		icon := "✅"
		switch state {
		case "warning":
			icon = "⚠️"
		case "critical":
			icon = "🔴"
		}
		fmt.Fprintf(&b, "%s %s — %s\n", icon, id, state)
	}
	return b.String()
}
