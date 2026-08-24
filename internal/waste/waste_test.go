package waste

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"slo-sentinel/internal/catalog"
	"slo-sentinel/internal/query"
)

const wasteRules = `groups:
- name: waste.cloud.elb
  rules:
  - alert: WasteElbZeroTraffic
    expr: max_over_time(aws_elb_request_count_sum[14d]) <= 10
    for: 14d
    labels:
      sentinel_kind: waste
      scope: cloud
      notify_every: 7d
    annotations:
      sentinel_sensor: cloud.elb.zero-traffic
      summary: "ELB 已 14 天零流量"
`

// fakeWasteSource 回傳 expr 的求值序列：vals 為逐日結果（1=成立，0=不成立）。
type fakeWaste struct{ vals []float64 }

func (f *fakeWaste) InstantQuery(_ context.Context, _ string, _ time.Time) ([]query.Result, error) {
	return nil, nil
}

func (f *fakeWaste) RangeQuery(_ context.Context, _ string, start, end time.Time, _ time.Duration) ([]query.Result, error) {
	var samples []query.Sample
	for i, v := range f.vals {
		t := start.Add(time.Duration(i+1) * 24 * time.Hour)
		if t.After(end) {
			break
		}
		samples = append(samples, query.Sample{Time: t, Value: v})
	}
	return []query.Result{{Samples: samples}}, nil
}

func loadCatalog(t *testing.T, dir string) *catalog.Catalog {
	t.Helper()
	if err := catalogWrite(dir, wasteRules); err != nil {
		t.Fatal(err)
	}
	l := &catalog.Loader{Dir: dir}
	cat, _, err := l.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func catalogWrite(dir, body string) error {
	return os.WriteFile(filepath.Join(dir, "waste.yaml"), []byte(body), 0o600)
}

func TestScanDetectsSustainedIdle(t *testing.T) {
	dir := t.TempDir()
	cat := loadCatalog(t, dir)

	// 全部 14 天皆成立 → 候選
	sc := &Scanner{Src: &fakeWaste{vals: fourteenOnes()}}
	cands, err := sc.Scan(context.Background(), cat, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %d", len(cands))
	}
	c := cands[0]
	if c.SensorID != "cloud.elb.zero-traffic" {
		t.Fatalf("sensor id = %s", c.SensorID)
	}
	if c.Window != 14*24*time.Hour || c.Renotify != 7*24*time.Hour {
		t.Fatalf("window/renotify = %v/%v", c.Window, c.Renotify)
	}
	if c.Reason == "" {
		t.Fatal("summary 應帶入 Reason")
	}
}

func TestScanSkipsWhenConditionBreaks(t *testing.T) {
	dir := t.TempDir()
	cat := loadCatalog(t, dir)

	// 第 10 天條件中斷（有流量經過）→ 不構成候選
	vals := fourteenOnes()
	vals[9] = 0
	sc := &Scanner{Src: &fakeWaste{vals: vals}}
	cands, err := sc.Scan(context.Background(), cat, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("expected no candidates, got %d", len(cands))
	}
}

func fourteenOnes() []float64 {
	v := make([]float64, 14)
	for i := range v {
		v[i] = 1
	}
	return v
}
