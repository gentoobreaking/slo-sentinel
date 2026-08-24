package capacity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"slo-sentinel/internal/budget"
	"slo-sentinel/internal/query"
)

const defYAML = `sensors:
  - id: data-disk
    desc: "資料磁碟"
    metric:
      value:   'node_filesystem_used_bytes{mount="/data"}'
      ceiling: 'node_filesystem_size_bytes{mount="/data"}'
    horizons: [1h, 3d]
    thresholds:
      warn_eta: 48h
      crit_eta: 4h
      soft_ratio: 0.70
`

func writeDef(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "disk.yaml"), []byte(defYAML), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDefs(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir)
	defs, err := LoadDefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].ID != "data-disk" {
		t.Fatalf("defs = %+v", defs)
	}
	if got := defs[0].HorizonDurations(); len(got) != 2 || got[0] != time.Hour || got[1] != 24*3*time.Hour {
		t.Fatalf("horizons wrong: %+v", got)
	}
	th := defs[0].Thresholds.Resolve()
	if th.WarnEta != 48*time.Hour || th.CritEta != 4*time.Hour || th.SoftRatio != 0.70 {
		t.Fatalf("thresholds resolve wrong: %+v", th)
	}
	if th.CritRatio != budget.DefaultCritRatio {
		t.Fatalf("crit_ratio should stay default: %v", th.CritRatio)
	}
}

func TestLoadDefsMissingDirReturnsNil(t *testing.T) {
	defs, err := LoadDefs(filepath.Join(t.TempDir(), "nope"))
	if err != nil || defs != nil {
		t.Fatalf("expected nil,nil got %v,%v", defs, err)
	}
}

func TestLoadDefsValidationErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("sensors:\n  - desc: 缺 id\n    metric:\n      value: up\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDefs(bad); err == nil {
		t.Fatal("expected error for missing id")
	}
}

func TestPollComputesForecastAndUpdatesState(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir)
	defs, err := LoadDefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	def := defs[0]

	fake := query.NewFake()
	now := time.Now().UTC()

	valueQ := def.Metric.Value
	ceilQ := def.Metric.Ceiling

	// 消耗量：一小時內 150GB → 268GB（每分鐘約 +2GB）
	fake.Ranges[valueQ] = query.Result{
		Labels: map[string]string{"mount": "/data"},
		Samples: []query.Sample{
			{Time: now.Add(-time.Hour), Value: 150},
			{Time: now.Add(-30 * time.Minute), Value: 209},
			{Time: now, Value: 268},
		},
	}
	fake.Instant[valueQ] = []query.Result{
		{Samples: []query.Sample{{Time: now, Value: 268}}}}
	fake.Instant[ceilQ] = []query.Result{
		{Samples: []query.Sample{{Time: now, Value: 500}}}}

	sensor, err := New(def, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := sensor.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.Value != 268 || f.Ceiling != 500 {
		t.Fatalf("value/ceiling wrong: %+v", f)
	}
	if abs(f.Utilization-0.536) > 0.01 {
		t.Fatalf("utilization = %v", f.Utilization)
	}

	// 第二輪：狀態/天花板延續不 panic
	if _, err := sensor.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
