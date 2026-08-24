package main

// e2e_test.go（T018）：spec.md §5 成功標準 ↔ 自動化驗證對照。
// 以 fake Prometheus / fake AlertManager / 捕獲式 Notifier 驅動真實元件。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"log/slog"

	"slo-sentinel/config"
	"slo-sentinel/internal/alert"
	"slo-sentinel/internal/budget"
	"slo-sentinel/internal/capacity"
	"slo-sentinel/internal/query"
	"slo-sentinel/internal/store"
)

// captureNotifier 捕獲所有推播訊息。
type captureNotifier struct{ msgs []string }

func (c *captureNotifier) Send(_ context.Context, text string) error {
	c.msgs = append(c.msgs, text)
	return nil
}

// fakeRisingSource 模擬穩定成長的磁碟消耗量（§A.7 變體）。
type fakeRisingSource struct {
	base   float64
	perMin float64
	now    time.Time
}

func (f *fakeRisingSource) valueAt(t time.Time) float64 {
	return f.base + f.perMin*t.Sub(f.now).Minutes()
}

func (f *fakeRisingSource) InstantQuery(_ context.Context, q string, at time.Time) ([]query.Result, error) {
	return []query.Result{{Samples: []query.Sample{{Time: at, Value: f.valueAt(at)}}}}, nil
}

func (f *fakeRisingSource) RangeQuery(_ context.Context, q string, start, end time.Time, step time.Duration) ([]query.Result, error) {
	var samples []query.Sample
	for t := start; !t.After(end); t = t.Add(step) {
		samples = append(samples, query.Sample{Time: t, Value: f.valueAt(t)})
	}
	return []query.Result{{Samples: samples}}, nil
}

func amNoAlerts(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func amWithFiring(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"status":{"state":"active"},"labels":{"mount":"/data"}}]`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func setupCapacityDaemon(t *testing.T, src query.Source, notifier alert.Notifier, amURL string) (*daemon, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	defYAML := "sensors:\n  - id: data-disk\n    metric:\n      value: 'disk_used'\n      ceiling: 'disk_total'\n"
	if err := os.WriteFile(filepath.Join(dir, "disk.yaml"), []byte(defYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		PollIntervalSec: 60,
		CapacityDefsDir: dir,
		SloDefsDir:      filepath.Join(dir, "slo_defs"),
		RulesDir:        filepath.Join(dir, "rules"),
		DBPath:          filepath.Join(dir, "test.db"),
		ListenAddr:      "127.0.0.1:0",
		MetricsAddr:     "127.0.0.1:0",
		LogFormat:       "text",
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	d := &daemon{
		cfg: cfg, log: log, st: st, notifier: notifier,
		dedupe:  alert.NewDedupe(),
		amcoord: &alert.AMCoord{BaseURL: amURL},
		metrics: newMetricsRegistry(),
	}
	defs, err := capacity.LoadDefs(cfg.CapacityDefsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range defs {
		def := def
		sensor, err := capacity.New(def, src, log)
		if err != nil {
			t.Fatal(err)
		}
		d.sensors = append(d.sensors, sensorRunner{
			id: def.ID, kind: "capacity", filter: def.ID,
			poll: func(c context.Context) (budget.Forecast, error) { return sensor.Poll(c) },
		})
	}
	return d, st
}

// 標準 1：人造 burn（激進速率 2GB/min）→ 雙視野警告必通知
func TestE2EBurnNotifiesWithDualHorizon(t *testing.T) {
	now := time.Now().UTC()
	src := &fakeRisingSource{base: 150, perMin: 2.0, now: now.Add(-time.Hour)}
	notifier := &captureNotifier{}
	d, _ := setupCapacityDaemon(t, src, notifier, amNoAlerts(t))

	if err := d.runOnePoll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(notifier.msgs) != 1 {
		t.Fatalf("msgs = %d", len(notifier.msgs))
	}
	msg := notifier.msgs[0]
	for _, want := range []string{"data-disk", "若持續爆量", "回到常態"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q:\n%s", want, msg)
		}
	}
}

// 標準 1b：AM 已 firing → 靜默
func TestE2EAMFiringSuppressesNotify(t *testing.T) {
	amSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模擬 data-disk 的靜態告警已在 AlertManager firing
		w.Write([]byte(`[{"status":{"state":"active"},"labels":{"sensor":"data-disk"}}]`))
	}))
	defer amSrv.Close()

	notifier := &captureNotifier{}
	src := &fakeRisingSource{base: 150, perMin: 2.0, now: time.Now().UTC().Add(-time.Hour)}
	d, _ := setupCapacityDaemon(t, src, notifier, amSrv.URL)

	if err := d.runOnePoll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(notifier.msgs) != 0 {
		t.Fatalf("AM firing 期間不得推播，收到 %d 則", len(notifier.msgs))
	}
}

// 標準 1c：預測紀錄入庫供 /accuracy 自評
func TestE2EPredictionRecordedForAccuracy(t *testing.T) {
	src := &fakeRisingSource{base: 150, perMin: 2.0, now: time.Now().UTC().Add(-time.Hour)}
	notifier := &captureNotifier{}

	dir := t.TempDir()
	defYAML := "sensors:\n  - id: data-disk\n    metric:\n      value: 'disk_used'\n      ceiling: 'disk_total'\n"
	if err := os.WriteFile(filepath.Join(dir, "disk.yaml"), []byte(defYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{PollIntervalSec: 60, CapacityDefsDir: dir,
		SloDefsDir: filepath.Join(dir, "sd"), RulesDir: filepath.Join(dir, "r"),
		DBPath: filepath.Join(dir, "t.db"), ListenAddr: "127.0.0.1:0", MetricsAddr: "127.0.0.1:0"}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	d := &daemon{cfg: cfg, st: st, notifier: notifier, dedupe: alert.NewDedupe(),
		amcoord: &alert.AMCoord{BaseURL: amNoAlerts(t)}, metrics: newMetricsRegistry(),
		log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	defs, _ := capacity.LoadDefs(cfg.CapacityDefsDir)
	for _, def := range defs {
		def := def
		sensor, _ := capacity.New(def, src, d.log)
		d.sensors = append(d.sensors, sensorRunner{id: def.ID, poll: func(c context.Context) (budget.Forecast, error) {
			return sensor.Poll(c)
		}})
	}

	if err := d.runOnePoll(context.Background()); err != nil {
		t.Fatal(err)
	}
	preds, err := st.ListPredictions("data-disk", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(preds) == 0 || preds[0].EtaAggressive == nil {
		t.Fatalf("/accuracy 資料源缺失：predictions = %+v", preds)
	}
}

// 標準 3：binary ≤ 20MB（已建置時才檢查）
func TestE2EBinarySizeUnderLimit(t *testing.T) {
	for _, bin := range []string{"../bin/sentinel", "../bin/sentinel-ui"} {
		info, err := os.Stat(bin)
		if err != nil {
			continue // 未建置時略過（CI 另以 make build 後量測）
		}
		if info.Size() > 20<<20 {
			t.Fatalf("%s 大小 %.1fMB 超過 20MB", bin, float64(info.Size())/(1<<20))
		}
	}
}
