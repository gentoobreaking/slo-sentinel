package main

// predictions_retention.go（T029）：predictions 表保留期限清理。
//
// 每次輪詢對每顆感測 AppendPrediction，表無清理會無限成長——
// daemon 每日（隨主迴圈，同日去重）刪除超過 retention 的舊紀錄並 PRAGMA optimize。
// /accuracy 只查近 7～30 天（api.go accuracyJSON/sloDetail），遠小於預設 90 天，
// 清理不影響其正確性。

import (
	"time"

	"slo-sentinel/internal/store"
)

const pruneStateID = "__predictions_prune__" // 最後清理日（YYYY-MM-DD）

// maybePrunePredictions 每輪檢查；每日最多執行一次，retention=0 停用。
func (d *daemon) maybePrunePredictions(now time.Time) {
	days := d.cfg.PredictionsRetentionDays
	if days <= 0 {
		return // 停用
	}
	dayKey := now.Format("2006-01-02")
	if prev, _ := d.st.GetState(pruneStateID); prev != nil && prev.State == dayKey {
		return // 今日已清
	}
	n, err := d.st.PrunePredictions(now.AddDate(0, 0, -days))
	if err != nil {
		// 失敗不登記：下一輪重試
		d.log.Error("predictions_prune_failed", "error", err.Error())
		return
	}
	_ = d.st.SetState(store.SensorState{SensorID: pruneStateID, State: dayKey})
	if n > 0 {
		d.log.Info("predictions_pruned", "deleted", n, "retention_days", days)
	}
}
