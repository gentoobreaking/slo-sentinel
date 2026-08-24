package main

// weekly_test.go（T011）：每週成本摘要的排程接線——同 ISO 週只發一封、
// 成長服務比對 capacity 擴容軌跡（algs/cost-forecast.md §D.5）。

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"slo-sentinel/internal/billing"
)

// fakeBilling 固定回傳兩週日花費：EC2 成長、RDS 大幅成長。
type fakeBilling struct{}

func (fakeBilling) Name() string { return "fake" }
func (fakeBilling) DailySpend(_ context.Context, _ billing.Filter, start, end time.Time) ([]billing.DailySpend, error) {
	var out []billing.DailySpend
	for t := start; !t.After(end); t = t.AddDate(0, 0, 1) {
		rds := 10.0
		if t.Day() >= 20 { // 8/20 起「擴容」→ 本週 vs 上週成長
			rds = 40
		}
		out = append(out,
			billing.DailySpend{Date: t.UTC(), CostUSD: 20, Service: "Amazon EC2"},
			billing.DailySpend{Date: t.UTC(), CostUSD: rds, Service: "Amazon RDS"},
		)
	}
	return out, nil
}

func TestMaybeWeeklyCostSendsOncePerWeek(t *testing.T) {
	dir := t.TempDir()
	st, err := openStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cap := &captureNotifier{}
	log := slog.Default()
	d := &daemon{st: st, notifier: cap, billingSrc: fakeBilling{}, log: log}
	d.capDefs = nil // 無容量感測 → 原因比對走「未對應」分支

	t1 := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC) // 週二 W35
	d.maybeWeeklyCost(context.Background(), t1)
	if len(cap.msgs) != 1 {
		t.Fatalf("首次應發 1 封，得 %d", len(cap.msgs))
	}
	msg := cap.msgs[0]
	for _, want := range []string{"每週成本摘要", "Amazon RDS", "月底推估"} {
		if !strings.Contains(msg, want) {
			t.Errorf("摘要缺 %q:\n%s", want, msg)
		}
	}

	// 同週再呼叫：不重複發
	d.maybeWeeklyCost(context.Background(), t1.Add(24*time.Hour))
	if len(cap.msgs) != 1 {
		t.Fatalf("同一 ISO 週不應重複發，得 %d 封", len(cap.msgs))
	}

	// 下一週：再發一封
	d.maybeWeeklyCost(context.Background(), t1.AddDate(0, 0, 7))
	if len(cap.msgs) != 2 {
		t.Fatalf("跨週應再發，得 %d 封", len(cap.msgs))
	}

	// 發送失敗不登記 → 下輪重試
	fail := &failingNotifier{}
	d2 := &daemon{st: st, notifier: fail, billingSrc: fakeBilling{}, log: log}
	t3 := t1.AddDate(0, 0, 14) // W37
	d2.maybeWeeklyCost(context.Background(), t3)
	d2.maybeWeeklyCost(context.Background(), t3.Add(time.Hour)) // 仍未登記，會再次嘗試
	if fail.attempts < 2 {
		t.Fatalf("發送失敗後應重試（attempts=%d）", fail.attempts)
	}
}

// failingNotifier 永永遠失敗，驗證「未成功不登記」。
type failingNotifier struct{ attempts int }

func (f *failingNotifier) Send(_ context.Context, _ string) error {
	f.attempts++
	return context.DeadlineExceeded
}
