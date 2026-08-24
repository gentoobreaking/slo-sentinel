package waste

// failover_test.go（T024）：單一規則 expr 查詢失敗不拖垮整輪掃描。

import (
	"context"
	"errors"
	"testing"
	"time"

	"slo-sentinel/internal/catalog"
	"slo-sentinel/internal/query"
)

// flakySource 對含 "bad" 的 expr 回傳錯誤，其餘走 fakeWaste 邏輯。
type flakySource struct{ fakeWaste }

func (f *flakySource) RangeQuery(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]query.Result, error) {
	if containsBad(expr) {
		return nil, errors.New("prometheus: parse error")
	}
	return f.fakeWaste.RangeQuery(ctx, expr, start, end, step)
}

func containsBad(expr string) bool {
	for i := 0; i+3 <= len(expr); i++ {
		if expr[i:i+3] == "bad" {
			return true
		}
	}
	return false
}

func TestScanContinuesWhenSingleRuleFails(t *testing.T) {
	dir := t.TempDir()
	multi := `groups:
- name: waste.multi
  rules:
  - alert: WasteBrokenRule
    expr: bad_expr_here
    for: 14d
    labels:
      sentinel_kind: waste
    annotations:
      sentinel_sensor: cloud.broken
      summary: "壞掉的規則"
  - alert: WasteGoodRule
    expr: max_over_time(good_metric[14d]) <= 10
    for: 14d
    labels:
      sentinel_kind: waste
    annotations:
      sentinel_sensor: cloud.good
      summary: "正常的閒置資源"
`
	if err := catalogWrite(dir, multi); err != nil {
		t.Fatal(err)
	}
	l := &catalog.Loader{Dir: dir}
	cat, _, err := l.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	sc := &Scanner{Src: &flakySource{fakeWaste{vals: fourteenOnes()}}}
	cands, err := sc.Scan(context.Background(), cat, time.Now())
	if err != nil {
		t.Fatalf("單一規則失敗不應拖垮整輪掃描：%v", err)
	}
	if len(cands) != 1 || cands[0].SensorID != "cloud.good" {
		t.Fatalf("healthy rule should still produce candidate, got %+v", cands)
	}
}
