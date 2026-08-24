package cost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"slo-sentinel/internal/billing"
	"slo-sentinel/internal/pricing"
)

// fakePricer 固定回價，驗證 Σ(用量 × 單價) 與 stale/錯誤處理。
type fakePricer struct {
	quotes map[pricing.Family]pricing.Quote
	err    error
}

func (f fakePricer) Quote(_ context.Context, fam pricing.Family, _ pricing.Attrs) (pricing.Quote, error) {
	if f.err != nil {
		return pricing.Quote{}, f.err
	}
	return f.quotes[fam], nil
}

func TestEstimateSpendSumsUsageTimesPrice(t *testing.T) {
	pr := fakePricer{quotes: map[pricing.Family]pricing.Quote{
		pricing.FamEC2: {UnitPrice: 0.096, Currency: "USD", Unit: "Hrs"},
		pricing.FamEBS: {UnitPrice: 0.08, Currency: "USD", Unit: "GB-Mo"},
	}}
	lines := []UsageLine{
		{UsageTemplate: UsageTemplate{Sensor: "web-replicas", Service: "web", Family: pricing.FamEC2}, Quantity: 10},
		{UsageTemplate: UsageTemplate{Sensor: "disk-data-db", Service: "db-volume", Family: pricing.FamEBS}, Quantity: 500},
	}
	res := EstimateSpend(context.Background(), lines, pr)
	want := 10*0.096 + 500*0.08 // 41.6
	if res.Total != want || res.Currency != "USD" || len(res.Lines) != 2 {
		t.Fatalf("總額應 %.4f，得 %+v", want, res)
	}
	if res.Lines[1].Cost != 40 || res.Lines[1].Unit != "GB-Mo" {
		t.Fatalf("單列結果錯: %+v", res.Lines[1])
	}
}

func TestEstimateSpendPerLineErrorAndStale(t *testing.T) {
	pr := fakePricer{quotes: map[pricing.Family]pricing.Quote{
		pricing.FamEC2: {UnitPrice: 0.096, Currency: "USD", Stale: true},
	}}
	lines := []UsageLine{
		{UsageTemplate: UsageTemplate{Sensor: "a", Family: pricing.FamEC2}, Quantity: 1},
		{UsageTemplate: UsageTemplate{Sensor: "b", Family: "lambda"}, Quantity: 1}, // fake 無此家族 → 零值 quote，非錯誤
	}
	res := EstimateSpend(context.Background(), lines, pr)
	if !res.Stale {
		t.Fatal("任一列 stale 應標注整份報告 stale")
	}

	errPr := fakePricer{err: errors.New("offline")}
	res2 := EstimateSpend(context.Background(), lines, errPr)
	if res2.Total != 0 || res2.Lines[0].Err == "" {
		t.Fatalf("查價失敗應記入 Err 不拖垮報表: %+v", res2)
	}
}

func TestCompareActualVsEstimate(t *testing.T) {
	cmp := CompareActualVsEstimate(50, &EstimateResult{Total: 41.6})
	if cmp.Delta == 0 || cmp.Delta < 8.3 || cmp.Delta > 8.5 || cmp.DeltaPct == nil {
		t.Fatalf("對照結果錯: %+v", cmp)
	}
	zero := CompareActualVsEstimate(50, nil)
	if zero.Estimate != 0 || zero.DeltaPct != nil {
		t.Fatalf("nil estimate 應安全: %+v", zero)
	}
}

func TestLoadCostMapMissingFile(t *testing.T) {
	out, err := LoadCostMap("/nonexistent/cost_map.yaml")
	if err != nil || out != nil {
		t.Fatalf("檔案不存在應回 nil,nil: %v %v", out, err)
	}
}

func TestLoadCostMapYAML(t *testing.T) {
	yamlDoc := `
- sensor: web-replicas
  service: web
  family: ec2
  attrs: {region: us-east-1, instance_type: m5.large}
- sensor: disk-data-db
  service: db-volume
  family: ebs
  attrs: {region: us-east-1, volume_type: gp3}
`
	path := t.TempDir() + "/cost_map.yaml"
	if err := os.WriteFile(path, []byte(yamlDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := LoadCostMap(path)
	if err != nil || len(out) != 2 {
		t.Fatalf("載入失敗: %v %v", out, err)
	}
	if out[0].Sensor != "web-replicas" || out[0].Family != "ec2" ||
		out[0].Attrs["instance_type"] != "m5.large" {
		t.Fatalf("範本內容錯: %+v", out[0])
	}

	// 缺 family → 明確錯誤
	badPath := t.TempDir() + "/bad.yaml"
	os.WriteFile(badPath, []byte("- sensor: x\n"), 0o644)
	if _, err := LoadCostMap(badPath); err == nil || !strings.Contains(err.Error(), "family") {
		t.Fatalf("缺 family 應報錯: %v", err)
	}
}

func TestWeeklyTopGrowth(t *testing.T) {
	day := func(offset int, svc string, cost float64) billing.DailySpend {
		return billing.DailySpend{Date: time.Date(2026, 8, 18+offset, 0, 0, 0, 0, time.UTC), CostUSD: cost, Service: svc}
	}
	var thisWeek, prevWeek []billing.DailySpend
	for i := 0; i < 7; i++ {
		thisWeek = append(thisWeek,
			day(i, "Amazon EC2", 20), // 成長 $10/天
			day(i, "Amazon RDS", 40), // 成長 $30/天（最大）
			day(i, "Amazon S3", 5),   // 持平 → 不列
		)
		prevWeek = append(prevWeek,
			day(i, "Amazon EC2", 10),
			day(i, "Amazon RDS", 10),
			day(i, "Amazon S3", 5),
		)
	}
	rows := WeeklyTopGrowth(WeeklyGrowthInput{
		ThisWeek: thisWeek, PrevWeek: prevWeek,
		CapTrend: map[string]float64{"rds-connections-growth": 12.5},
	}, 5)
	if len(rows) != 2 {
		t.Fatalf("應只列成長服務，得 %+v", rows)
	}
	if rows[0].Service != "Amazon RDS" || rows[0].Growth != 30 {
		t.Fatalf("top1 應為 RDS +30： %+v", rows[0])
	}
	if rows[1].Service != "Amazon EC2" || rows[1].Growth != 10 {
		t.Fatalf("top2 應為 EC2 +10： %+v", rows[1])
	}
	if !strings.Contains(rows[0].LikelyCause, "擴容") || !strings.Contains(rows[0].LikelyCause, "rds-connections") {
		t.Fatalf("RDS 成長應比對到擴容感測: %q", rows[0].LikelyCause)
	}
	if !strings.Contains(rows[1].LikelyCause, "人工確認") {
		t.Fatalf("無對應感測應標注需人工確認: %q", rows[1].LikelyCause)
	}
}

func TestWeeklyTopGrowthTopN(t *testing.T) {
	day := func(i int, svc string, c float64) billing.DailySpend {
		return billing.DailySpend{Date: time.Date(2026, 8, 18+i, 0, 0, 0, 0, time.UTC), CostUSD: c, Service: svc}
	}
	var tw, pw []billing.DailySpend
	for i := 0; i < 7; i++ {
		for s := 0; s < 8; s++ { // 8 個服務都成長
			svc := fmt.Sprintf("svc-%d", s)
			tw = append(tw, day(i, svc, float64(s)*3+2)) // 成長量隨 s 遞增
			pw = append(pw, day(i, svc, float64(s)))
		}
	}
	rows := WeeklyTopGrowth(WeeklyGrowthInput{ThisWeek: tw, PrevWeek: pw}, 5)
	if len(rows) != 5 {
		t.Fatalf("topN=5 應截斷，得 %d", len(rows))
	}
	if rows[0].Growth <= rows[4].Growth {
		t.Fatal("應依成長量遞減排序")
	}
}

func TestISOWeekKeyAndFormatSummary(t *testing.T) {
	ts := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC) // 週二
	if k := ISOWeekKey(ts); k != "2026-W35" {
		t.Fatalf("ISOWeekKey=%q", k)
	}
	rows := []GrowthRow{{Service: "Amazon EC2", PreviousAvg: 10, RecentAvg: 20, Growth: 10,
		LikelyCause: "疑似對應容量擴容：web-replicas（+3/7d）"}}
	msg := FormatWeeklySummary(rows, EomProjection{Conservative: 320, Aggressive: 400},
		time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC))
	for _, want := range []string{"每週成本摘要", "Amazon EC2", "$10.00 → $20.00", "擴容", "$320.00", "2026-08-23"} {
		if !strings.Contains(msg, want) {
			t.Errorf("摘要缺 %q:\n%s", want, msg)
		}
	}
	empty := FormatWeeklySummary(nil, EomProjection{}, time.Now())
	if !strings.Contains(empty, "無顯著成長") {
		t.Errorf("空列應有說明: %s", empty)
	}
}
