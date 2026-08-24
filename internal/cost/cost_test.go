package cost

import (
	"math"
	"strings"
	"testing"
	"time"

	"slo-sentinel/internal/billing"
)

func TestEstimateRates(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	last7 := make([]billing.DailySpend, 7)
	for i := range last7 {
		last7[i] = billing.DailySpend{Date: now.AddDate(0, 0, -i), CostUSD: 10}
	}
	r := EstimateRates(150, 14, last7)
	if want := 150 / float64(14); r.Mtd != want {
		t.Fatalf("mtd rate = %v, want %v", r.Mtd, want)
	}
	if r.Recent != 10 {
		t.Fatalf("recent rate = %v", r.Recent)
	}
}

func TestProjectEOM(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC) // 31 天月，剩 16 天
	p := ProjectEOM(150, now, Rates{Recent: 20, Mtd: 10})
	if p.Aggressive != 150+20*16 {
		t.Fatalf("aggressive = %v", p.Aggressive)
	}
	if p.Conservative != 150+10*16 {
		t.Fatalf("conservative = %v", p.Conservative)
	}
}

func TestBudgetETA(t *testing.T) {
	// 剩 $500、日速 $100 → 5 天
	eta := BudgetETA(150, 650, 100)
	if eta == nil || math.Abs(*eta-5*86400) > 1e-6 {
		t.Fatalf("eta = %v", *eta)
	}
	// 已超支 → 0
	if e := BudgetETA(700, 650, 100); e == nil || *e != 0 {
		t.Fatalf("overspend should give 0, got %v", e)
	}
	// 無花費速率 → nil（無風險）
	if e := BudgetETA(100, 650, 0); e != nil {
		t.Fatal("zero rate must be nil")
	}
}

func TestDetectSpike(t *testing.T) {
	if !DetectSpike(210, 100) { // >2x
		t.Fatal("210 vs 100 avg should spike")
	}
	if DetectSpike(150, 100) {
		t.Fatal("1.5x is not a spike")
	}
	if DetectSpike(500, 0) {
		t.Fatal("zero baseline cannot spike")
	}
}

func TestYearProjectionAndReport(t *testing.T) {
	aug := time.August
	actual := map[time.Month]float64{
		time.January: 100, time.February: 110,
	}
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	points, total := ProjectYear(now, 2026, actual, aug, 150, 10)
	// ProjectYear 只回放至 currentMonth（含），且實際值缺漏的月份不產點位
	if len(points) != 3 {
		t.Fatalf("points = %d (%+v)", len(points), points)
	}
	if !points[0].Actual {
		t.Fatal("Jan should be actual")
	}
	if points[2].Actual {
		t.Fatal("Aug 應為推估（actual=false）")
	}
	wantTotal := 100.0 + 110.0 + float64(150+10*16) // Jan+Feb 實際 + Aug 推估（剩 16 天）
	if total != wantTotal {
		t.Fatalf("total = %v, want %v", total, wantTotal)
	}
	rep := FormatReport(EomProjection{Aggressive: 400, Conservative: 320}, ptr(72*3600),
		time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC))
	if !strings.Contains(rep, "2026-08-23") {
		t.Fatal("report must include confirmed_date")
	}
}

func ptr(v float64) *float64 { return &v }
