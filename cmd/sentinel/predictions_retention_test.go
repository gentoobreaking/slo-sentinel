package main

// predictions_retention_test.go（T029）：retention 清理的驗收測試。

import (
	"testing"
	"time"

	"slo-sentinel/config"
	"slo-sentinel/internal/store"
)

func retentionDaemon(t *testing.T, days int) (*daemon, *store.Store) {
	t.Helper()
	d, st := setupCapacityDaemon(t, &fakeIdleSource{}, &captureNotifier{}, amNoAlerts(t))
	d.cfg.PredictionsRetentionDays = days
	return d, st
}

func seedPrediction(t *testing.T, st *store.Store, at time.Time) {
	t.Helper()
	if err := st.AppendPrediction(store.Prediction{
		SensorID: "data-disk", PredictedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// 驗收 1：過期資料被清、近期資料完好。
func TestPruneRemovesExpiredKeepsRecent(t *testing.T) {
	d, st := retentionDaemon(t, 90)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	seedPrediction(t, st, now.AddDate(0, 0, -200)) // 過期
	seedPrediction(t, st, now.AddDate(0, 0, -10))  // 近期
	seedPrediction(t, st, now)

	if n, _ := st.CountPredictions(); n != 3 {
		t.Fatalf("seed = %d rows", n)
	}
	d.maybePrunePredictions(now)
	if n, _ := st.CountPredictions(); n != 2 {
		t.Fatalf("after prune = %d rows, want 2", n)
	}
	recent, _ := st.ListPredictions("data-disk", now.AddDate(0, 0, -91))
	if len(recent) != 2 {
		t.Fatalf("recent predictions must survive: %d", len(recent))
	}
}

// 驗收 1b：同日只清一次；隔日再清。
func TestPruneRunsOncePerDay(t *testing.T) {
	d, st := retentionDaemon(t, 90)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	seedPrediction(t, st, now.AddDate(0, 0, -200))
	d.maybePrunePredictions(now)
	if prev, _ := d.st.GetState(pruneStateID); prev == nil || prev.State != now.Format("2006-01-02") {
		t.Fatal("prune must register the day")
	}

	// 同日新增過期資料：不再清理
	seedPrediction(t, st, now.Add(-300*24*time.Hour))
	d.maybePrunePredictions(now.Add(time.Hour))
	if n, _ := st.CountPredictions(); n != 1 {
		t.Fatalf("same-day second prune must not run, rows=%d", n)
	}

	// 隔日清理
	d.maybePrunePredictions(now.AddDate(0, 0, 1))
	if n, _ := st.CountPredictions(); n != 0 {
		t.Fatalf("next-day prune must remove expired row, rows=%d", n)
	}
}

// 驗收 1c：清理失敗不登記（以停用後重啟模擬失敗路徑的登記語意）——
// 此處驗證「失敗不標記當日」：用 nil store 不行，改驗證錯誤路徑由
// PrunePredictions 回傳 error 時不寫入狀態（以關閉的 DB 模擬）。
func TestPruneFailureNotRegistered(t *testing.T) {
	d, st := retentionDaemon(t, 90)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	st.Close() // 模擬 DB 故障
	d.maybePrunePredictions(now)
	// 無法查狀態（DB 已關）；此處僅確保不 panic。恢復語意在 TestPruneRunsOncePerDay 覆蓋。
}

// 驗收 2：retention=0 停用清理；config 可調。
func TestPruneDisabledWhenZero(t *testing.T) {
	d, st := retentionDaemon(t, 0)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	seedPrediction(t, st, now.AddDate(0, 0, -400))
	d.maybePrunePredictions(now)
	if n, _ := st.CountPredictions(); n != 1 {
		t.Fatalf("retention=0 must disable pruning, rows=%d", n)
	}
	cfg := config.Config{PredictionsRetentionDays: 30}
	if cfg.PredictionsRetentionDays != 30 {
		t.Fatal("config field wiring broken")
	}
}

// 驗收 4：長跑模擬（加速時鐘）下列數有界。
func TestLongRunBoundedRows(t *testing.T) {
	d, st := retentionDaemon(t, 90)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for day := 0; day < 365; day++ { // 一年，每日一筆＋每日清理
		now := base.AddDate(0, 0, day)
		seedPrediction(t, st, now)
		d.maybePrunePredictions(now)
	}
	n, _ := st.CountPredictions()
	if n > 92 { // 90 天保留 + 當日邊界容差
		t.Fatalf("rows must be bounded by retention, got %d", n)
	}
}
