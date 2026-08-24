package main

// waste_wiring_test.go（T024）：daemon waste 定期掃描接線的驗收測試——
// 新候選直推＋同資源去重、掃描結果持久化、dismiss/resolve 跨重啟、
// 到期復活、週期環境變數覆寫。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"log/slog"

	"slo-sentinel/config"
	"slo-sentinel/internal/catalog"
	"slo-sentinel/internal/query"
	"slo-sentinel/internal/store"
	"slo-sentinel/internal/waste"
)

const wasteRuleYAML = `groups:
- name: waste.test
  rules:
  - alert: WasteTestIdle
    expr: max_over_time(test_metric[14d]) <= 10
    for: 14d
    labels:
      sentinel_kind: waste
      notify_every: 7d
    annotations:
      sentinel_sensor: test.idle
      summary: "測試閒置資源"
`

// fakeIdleSource 對任意查詢回傳全為真（1）的逐日序列。
type fakeIdleSource struct{}

func (f *fakeIdleSource) InstantQuery(_ context.Context, _ string, at time.Time) ([]query.Result, error) {
	return []query.Result{{Samples: []query.Sample{{Time: at, Value: 1}}}}, nil
}

func (f *fakeIdleSource) RangeQuery(_ context.Context, _ string, start, end time.Time, step time.Duration) ([]query.Result, error) {
	var samples []query.Sample
	for t := start.Add(step); !t.After(end); t = t.Add(step) {
		samples = append(samples, query.Sample{Time: t, Value: 1})
	}
	return []query.Result{{Samples: samples}}, nil
}

// setupWasteDaemon 組出含 waste 規則目錄的 daemon（store/notifier 可注入）。
func setupWasteDaemon(t *testing.T) (*daemon, *captureNotifier, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	rulesDir := filepath.Join(dir, "rules")
	if err := os.MkdirAll(rulesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rulesDir, "waste.yaml"), []byte(wasteRuleYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	notifier := &captureNotifier{}
	d := &daemon{
		cfg: config.Config{
			PollIntervalSec:      60,
			RulesDir:             rulesDir,
			WasteScanIntervalSec: 6 * 3600,
			LogFormat:            "text",
		},
		log:      log,
		src:      &fakeIdleSource{},
		st:       st,
		notifier: notifier,
		metrics:  newMetricsRegistry(),
		tracker:  waste.NewLiveTracker(),
	}
	l := &catalog.Loader{Dir: rulesDir}
	cat, _, err := l.Load(rulesDir)
	if err != nil {
		t.Fatal(err)
	}
	d.lastCatalog = cat
	return d, notifier, st
}

func TestRunWasteScanNotifiesNewCandidatesAndDedupes(t *testing.T) {
	d, notifier, st := setupWasteDaemon(t)
	ctx := context.Background()

	// 第一輪：新候選 → 直推
	d.runWasteScan(ctx)
	if len(notifier.msgs) != 1 {
		t.Fatalf("first scan should notify once, got %d", len(notifier.msgs))
	}
	entries, err := st.AllWasteEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].State != string(waste.LifecycleNotified) {
		t.Fatalf("persisted entry = %+v", entries)
	}

	// 第二輪：同資源去重，不再推播
	d.runWasteScan(ctx)
	if len(notifier.msgs) != 1 {
		t.Fatalf("second scan must dedupe (same resource), got %d msgs", len(notifier.msgs))
	}
}

func TestWasteDismissSurvivesRestartAndRevivesAfterDeadline(t *testing.T) {
	ctx := context.Background()
	d, notifier, st := setupWasteDaemon(t)

	d.runWasteScan(ctx)
	rows, _ := st.AllWasteEntries()
	resourceID := rows[0].ResourceID

	// dismiss（7 天後復活）並寫回 store
	until := time.Now().Add(7 * 24 * time.Hour)
	tr := loadTracker(st)
	if err := tr.Dismiss(resourceID, "保留觀察", until); err != nil {
		t.Fatal(err)
	}
	syncTracker(st, tr)

	// 「重啟」：全新 tracker 自 store 還原
	d2, notifier2, st2rows := d, notifier, st // 同一 store 模擬重啟後狀態
	d2.tracker = waste.NewLiveTracker()
	restored, err := st2rows.AllWasteEntries()
	if err != nil {
		t.Fatal(err)
	}
	d2.tracker.Restore(entriesFromStore(restored))

	// 期限前：已 dismiss 的候選不再通知
	d2.runWasteScan(ctx)
	if len(notifier2.msgs) != 1 {
		t.Fatalf("dismissed candidate must stay silent, got %d msgs", len(notifier2.msgs))
	}

	// 把期限改成過去 → 掃描應「到期自動復活」再次提醒
	e := entriesFromStore(restored)[0]
	e.DismissUntil = time.Now().Add(-time.Hour)
	if err := st.SetWasteEntry(entryToStore(e)); err != nil {
		t.Fatal(err)
	}
	d2.tracker = waste.NewLiveTracker()
	rows3, _ := st.AllWasteEntries()
	d2.tracker.Restore(entriesFromStore(rows3))
	d2.runWasteScan(ctx)
	if len(notifier2.msgs) != 2 {
		t.Fatalf("expired dismissal should revive and notify, got %d msgs", len(notifier2.msgs))
	}
}

func TestWasteResolveAccumulatesSavingQueryable(t *testing.T) {
	ctx := context.Background()
	d, _, st := setupWasteDaemon(t)
	d.runWasteScan(ctx)

	rows, _ := st.AllWasteEntries()
	tr := loadTracker(st)
	if err := tr.Resolve(rows[0].ResourceID); err != nil {
		t.Fatal(err)
	}
	syncTracker(st, tr)

	got, err := st.ResolvedWasteSaving()
	if err != nil {
		t.Fatal(err)
	}
	if got < 0 {
		t.Fatalf("saving should be queryable and non-negative, got %v", got)
	}
	entries, _ := st.AllWasteEntries()
	if entries[0].State != string(waste.LifecycleResolved) {
		t.Fatalf("state after resolve = %s", entries[0].State)
	}
}

func TestApplyWasteEnvOverride(t *testing.T) {
	base := config.Config{WasteScanIntervalSec: 21600}

	t.Setenv("WASTE_SCAN_INTERVAL_SEC", "3600")
	if got := applyWasteEnvOverride(base).WasteScanIntervalSec; got != 3600 {
		t.Fatalf("env override = %d, want 3600", got)
	}
	t.Setenv("WASTE_SCAN_INTERVAL_SEC", "off")
	if got := applyWasteEnvOverride(base).WasteScanIntervalSec; got != 0 {
		t.Fatalf("off should disable, got %d", got)
	}
	t.Setenv("WASTE_SCAN_INTERVAL_SEC", "")
	if got := applyWasteEnvOverride(base).WasteScanIntervalSec; got != 21600 {
		t.Fatalf("unset env keeps default, got %d", got)
	}
}

func TestRunWasteScanDisabledDoesNothing(t *testing.T) {
	d, notifier, _ := setupWasteDaemon(t)
	d.cfg.WasteScanIntervalSec = 0 // off
	d.runWasteScan(context.Background())
	if len(notifier.msgs) != 0 {
		t.Fatalf("disabled scan must not notify")
	}
}
