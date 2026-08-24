package main

// triage.go（T020）：容量預警轉交 ai-oncall 分診閘門。
//
// 流程：容量感測狀態轉移（warning/critical）→ Peek 判定應通知 →
// 先轉交 gate（AM webhook 格式）→ 成功則本地只發精簡卡（「已轉交分診」），
// 避免同一事件兩份長文；轉交失敗則退回完整本地推播——
// critical 通知不丟失優先於分診閉環（對齊 T026 的一等事故原則）。
// resolved 轉移僅在先前已轉交 firing 時才轉交（否則 gate 無 incident 可關）。

import (
	"context"
	"fmt"
	"os"
	"time"

	"slo-sentinel/internal/budget"
)

// sensorMeta 為感測的分診標籤（setupSensors 時自 def 收集）。
type sensorMeta struct {
	Scope   string
	Service string
	Cluster string // 空 → 發布時以 SENTINEL_CLUSTER_NAME 補
}

// triageHandled 執行容量預警的分診轉交。
// 回傳 true 表示本轉移已由分診路徑處理完畢（含本地精簡推播與去重登記），
// 呼叫端可跳過原本的 Telegram 完整卡流程；false = 回退既有流程。
func (d *daemon) triageHandled(ctx context.Context, sr sensorRunner, f budget.Forecast) bool {
	if d.publisher == nil || sr.kind != "capacity" {
		return false
	}
	state := string(f.State)
	status, severity := "firing", state
	if state == "healthy" {
		if !d.publishedFiring[f.ID] {
			return false // 從未轉交過 firing，無 incident 可關——走本地流程即可
		}
		status, severity = "resolved", "info"
	}

	meta := d.sensorMeta[f.ID]
	labels := map[string]string{
		"alertname": "CapacityEtaWarning",
		"sensor_id": f.ID,
		"severity":  severity,
	}
	if meta.Scope != "" {
		labels["scope"] = meta.Scope
	}
	if svc := meta.Service; svc != "" {
		labels["service"] = svc
	} else if sr.filter != "" && sr.filter != f.ID {
		labels["service"] = sr.filter
	}
	if cluster := envOr("SENTINEL_CLUSTER_NAME", meta.Cluster); cluster != "" {
		// per-def cluster 覆寫全域環境變數
		if meta.Cluster != "" {
			labels["cluster"] = meta.Cluster
		} else {
			labels["cluster"] = cluster
		}
	}
	if f.EtaAggressive != nil {
		labels["eta_aggressive"] = fmt.Sprintf("%.0f", *f.EtaAggressive)
	}
	if f.EtaConservative != nil {
		labels["eta_conservative"] = fmt.Sprintf("%.0f", *f.EtaConservative)
	}

	annotations := map[string]string{"summary": formatForecastCard(f)}
	if rb := os.Getenv("SENTINEL_RUNBOOK_URL"); rb != "" {
		annotations["runbook_url"] = rb
	}

	alert := AMAlert{
		Status:      status,
		Labels:      labels,
		Annotations: annotations,
		StartsAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := d.publisher.Publish(ctx, []AMAlert{alert}); err != nil {
		d.log.Error("triage_publish_failed_falling_back_local",
			"sensor", f.ID, "state", state, "error", err.Error())
		return false // 回退完整本地推播：critical 不丟失優先
	}

	if status == "resolved" {
		delete(d.publishedFiring, f.ID)
	} else {
		d.publishedFiring[f.ID] = true
	}

	// 本地精簡卡：同一事件不再重複推播長文（T020 功能設計 3）
	short := fmt.Sprintf("📨 %s %s — 已轉交 ai-oncall 分診", f.ID, state)
	if rb := annotations["runbook_url"]; rb != "" {
		short += "\n" + rb
	}
	if err := d.notifier.Send(ctx, short); err != nil {
		d.log.Error("triage_local_note_failed", "error", err.Error())
	}
	return true
}
