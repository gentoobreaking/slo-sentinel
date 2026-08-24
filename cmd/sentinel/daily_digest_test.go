package main

// daily_digest_test.go（T025）：每日摘要定時觸發的驗收測試——
// 到點發送、同日去重（重啟不重發）、失敗補發、off 開關。

import (
	"context"
	"strings"
	"testing"
	"time"

	"slo-sentinel/internal/alert"
	"slo-sentinel/internal/query"
	"slo-sentinel/internal/store"
)

// digestDaemon 組出含 data-disk 感測與摘要設定的 daemon（感測告警與摘要
// 分別用獨立 capture，避免計數互相污染）。
func digestDaemon(t *testing.T) (*daemon, *store.Store, *captureNotifier, *captureNotifier, *switchableSource) {
	t.Helper()
	sw := &switchableSource{inner: &fakeRisingSource{base: 150, perMin: 2.0, now: time.Now().UTC().Add(-time.Hour)}}
	alertCap := &captureNotifier{}
	d, st := setupCapacityDaemon(t, sw, alertCap, amNoAlerts(t))
	d.digestTime = "09:00"
	return d, st, alertCap, &captureNotifier{}, sw
}

// switchableSource 可在測試中途替換內部資料源（感測器建構時捕獲 src）。
type switchableSource struct{ inner query.Source }

func (s *switchableSource) InstantQuery(ctx context.Context, q string, at time.Time) ([]query.Result, error) {
	return s.inner.InstantQuery(ctx, q, at)
}

func (s *switchableSource) RangeQuery(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]query.Result, error) {
	return s.inner.RangeQuery(ctx, q, start, end, step)
}

// 驗收 1：到達設定時刻自動發送；同日（含重啟模擬）不重發。
func TestDailyDigestSendsOncePerDay(t *testing.T) {
	d, _, _, digestCap, _ := digestDaemon(t)
	ctx := context.Background()

	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.Local)
	if err := d.runOnePoll(ctx); err != nil { // 讓感測有狀態
		t.Fatal(err)
	}
	d.notifier = digestCap // 摘要用獨立通道計數
	d.maybeDailyDigest(ctx, now)
	if len(digestCap.msgs) != 1 {
		t.Fatalf("digest should send once at/after configured time, got %d", len(digestCap.msgs))
	}

	// 同日晚一點再檢查（模擬重啟後同日）：不得重發
	d.maybeDailyDigest(ctx, now.Add(3*time.Hour))
	if len(digestCap.msgs) != 1 {
		t.Fatalf("same day must not resend, got %d", len(digestCap.msgs))
	}

	// 隔日到點：再發一次
	d.maybeDailyDigest(ctx, now.Add(24*time.Hour))
	if len(digestCap.msgs) != 2 {
		t.Fatalf("next day should send again, got %d", len(digestCap.msgs))
	}
}

// 驗收 1b：時刻未到不發。
func TestDailyDigestWaitsForConfiguredTime(t *testing.T) {
	d, _, _, digestCap, _ := digestDaemon(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 8, 0, 0, 0, time.Local) // 09:00 之前
	if err := d.runOnePoll(ctx); err != nil {
		t.Fatal(err)
	}
	d.notifier = digestCap
	d.maybeDailyDigest(ctx, now)
	if len(digestCap.msgs) != 0 {
		t.Fatalf("must not send before configured time")
	}
}

// 驗收 2：發送失敗不登記，恢復後補發。
func TestDailyDigestFailureNotRegistered(t *testing.T) {
	d, st, _, _, _ := digestDaemon(t)
	d.notifier = &deadNotifier{}
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.Local)
	if err := d.runOnePoll(ctx); err != nil {
		t.Fatal(err)
	}

	d.maybeDailyDigest(ctx, now) // 失敗
	if prev, _ := st.GetState(digestStateID); prev != nil && prev.State == now.Format("2006-01-02") {
		t.Fatal("failed send must not register the day")
	}

	// 通道恢復 → 補發成功並登記
	good := &captureNotifier{}
	d.notifier = good
	d.maybeDailyDigest(ctx, now.Add(5*time.Minute))
	if len(good.msgs) != 1 {
		t.Fatalf("recovered channel must deliver digest, got %d", len(good.msgs))
	}
	prev, _ := st.GetState(digestStateID)
	if prev == nil || prev.State != now.Format("2006-01-02") {
		t.Fatal("successful send must register the day")
	}
}

// 驗收 3：digestTime 清空（DAILY_DIGEST=off）時完全停用。
func TestDailyDigestDisabled(t *testing.T) {
	d, _, _, digestCap, _ := digestDaemon(t)
	d.digestTime = digestDisabled
	ctx := context.Background()
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.Local)
	if err := d.runOnePoll(ctx); err != nil {
		t.Fatal(err)
	}
	d.notifier = digestCap
	d.maybeDailyDigest(ctx, now)
	if len(digestCap.msgs) != 0 {
		t.Fatalf("disabled digest must not send")
	}
}

// 狀態變化清單：與上次快照比對，隔日摘要列出轉移。
func TestDailyDigestListsChanges(t *testing.T) {
	d, _, _, digestCap, sw := digestDaemon(t)
	ctx := context.Background()

	day1 := time.Date(2026, 8, 26, 10, 0, 0, 0, time.Local)
	if err := d.runOnePoll(ctx); err != nil { // critical
		t.Fatal(err)
	}
	d.notifier = digestCap
	d.maybeDailyDigest(ctx, day1)

	// 隔日感測轉 healthy（解除遲滯需連續兩輪低於門檻）→ 摘要應列出變化
	sw.inner = &twoMetricSource{used: 10, total: 1000}
	d.notifier = &captureNotifier{} // 感測降級/恢復告警走別的通道
	if err := d.runOnePoll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.runOnePoll(ctx); err != nil {
		t.Fatal(err)
	}
	d.notifier = digestCap
	digestCap.msgs = nil
	d.maybeDailyDigest(ctx, day1.Add(24*time.Hour))
	if len(digestCap.msgs) != 1 {
		t.Fatalf("next-day digest missing, got %d", len(digestCap.msgs))
	}
	msg := digestCap.msgs[0]
	if !strings.Contains(msg, "critical") || !strings.Contains(msg, "healthy") {
		t.Fatalf("change list should mention critical→healthy transition:\n%s", msg)
	}
}

// twoMetricSource 以不同值回應 used/total 兩類查詢，可製造狀態變化。
type twoMetricSource struct{ used, total float64 }

func (f *twoMetricSource) valueFor(q string) float64 {
	if strings.Contains(q, "used") {
		return f.used
	}
	return f.total
}

func (f *twoMetricSource) InstantQuery(_ context.Context, q string, at time.Time) ([]query.Result, error) {
	return []query.Result{{Samples: []query.Sample{{Time: at, Value: f.valueFor(q)}}}}, nil
}

func (f *twoMetricSource) RangeQuery(_ context.Context, q string, start, end time.Time, step time.Duration) ([]query.Result, error) {
	var samples []query.Sample
	for t := start; !t.After(end); t = t.Add(step) {
		samples = append(samples, query.Sample{Time: t, Value: f.valueFor(q)})
	}
	return []query.Result{{Samples: samples}}, nil
}

var _ alert.Notifier = (*captureNotifier)(nil)
