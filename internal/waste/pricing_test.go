package waste

// pricing_test.go（T027）：waste 候選接 pricing 的每月可省金額。

import (
	"context"
	"errors"
	"testing"
	"time"

	"slo-sentinel/internal/catalog"
	"slo-sentinel/internal/pricing"
)

// fakePricer 離線假報價器。
type fakePricer struct {
	q   pricing.Quote
	err error
}

func (f *fakePricer) Quote(_ context.Context, _ pricing.Family, _ pricing.Attrs) (pricing.Quote, error) {
	return f.q, f.err
}

const pricedRules = `groups:
- name: waste.priced
  rules:
  - alert: WastePricedVolume
    expr: max_over_time(vol_used[14d]) <= 10
    for: 14d
    labels:
      sentinel_kind: waste
      sentinel_sensor: cloud.vol.idle
      sentinel_price_family: ebs
      sentinel_price_attrs: '{"region":"ap-east-1","quantity":"100"}'
    annotations:
      summary: "閒置磁碟"
`

func loadPricedCatalog(t *testing.T, body string) *catalog.Catalog {
	t.Helper()
	dir := t.TempDir()
	if err := catalogWrite(dir, body); err != nil {
		t.Fatal(err)
	}
	l := &catalog.Loader{Dir: dir}
	cat, _, err := l.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestScanAttachesMonthlySaving(t *testing.T) {
	cat := loadPricedCatalog(t, pricedRules)
	sc := &Scanner{
		Src:    &fakeWaste{vals: fourteenOnes()},
		Pricer: &fakePricer{q: pricing.Quote{UnitPrice: 0.1, Currency: "USD", Unit: "GB-Mo"}},
	}
	cands, err := sc.Scan(context.Background(), cat, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c := cands[0]
	if c.SavingMonthly != 10.0 { // 0.1 USD/GB-Mo × 100 GB
		t.Fatalf("saving = %v, want 10", c.SavingMonthly)
	}
	if c.Currency != "USD" || c.WastedCost <= 0 {
		t.Fatalf("currency/wasted cost = %s/%v", c.Currency, c.WastedCost)
	}
	if c.PriceError != "" || c.PriceStale {
		t.Fatalf("unexpected price state: %q %v", c.PriceError, c.PriceStale)
	}
}

func TestScanHourlyQuoteConvertsToMonthly(t *testing.T) {
	cat := loadPricedCatalog(t, pricedRules)
	sc := &Scanner{
		Src:    &fakeWaste{vals: fourteenOnes()},
		Pricer: &fakePricer{q: pricing.Quote{UnitPrice: 0.01, Currency: "USD", Unit: "Hrs"}},
	}
	cands, _ := sc.Scan(context.Background(), cat, time.Now())
	want := 0.01 * 24 * 30 * 100 // Hrs×24×30×quantity(100)
	if diff := cands[0].SavingMonthly - want; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("saving = %v, want %v", cands[0].SavingMonthly, want)
	}
}

func TestScanPriceFailureKeepsCandidate(t *testing.T) {
	cat := loadPricedCatalog(t, pricedRules)
	sc := &Scanner{
		Src:    &fakeWaste{vals: fourteenOnes()},
		Pricer: &fakePricer{err: errors.New("network unreachable")},
	}
	cands, err := sc.Scan(context.Background(), cat, time.Now())
	if err != nil {
		t.Fatalf("查價失敗不應阻擋候選成立：%v", err)
	}
	c := cands[0]
	if c.SavingMonthly != 0 || c.Currency != "" {
		t.Fatalf("failed quote must leave amount empty, got %+v", c)
	}
	if c.PriceError == "" {
		t.Fatal("PriceError must carry the failure reason")
	}
}

func TestScanWithoutPricerOrLabelsLeavesAmountEmpty(t *testing.T) {
	// 無 price labels：金額欄留空且無錯誤標注（顯示層呈現「—」）
	cat := loadPricedCatalog(t, wasteRules)
	sc := &Scanner{Src: &fakeWaste{vals: fourteenOnes()}, Pricer: &fakePricer{}}
	cands, err := sc.Scan(context.Background(), cat, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if cands[0].SavingMonthly != 0 || cands[0].PriceError != "" {
		t.Fatalf("unpriced candidate should be blank/quiet, got %+v", cands[0])
	}

	// 有 labels 但 Pricer=nil（未啟用 estimate）：同樣留空
	cat2 := loadPricedCatalog(t, pricedRules)
	sc2 := &Scanner{Src: &fakeWaste{vals: fourteenOnes()}}
	cands2, err := sc2.Scan(context.Background(), cat2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if cands2[0].SavingMonthly != 0 || cands2[0].PriceError != "" {
		t.Fatalf("nil pricer should leave amount empty, got %+v", cands2[0])
	}
}

func TestScanStaleQuoteIsFlagged(t *testing.T) {
	cat := loadPricedCatalog(t, pricedRules)
	sc := &Scanner{
		Src:    &fakeWaste{vals: fourteenOnes()},
		Pricer: &fakePricer{q: pricing.Quote{UnitPrice: 0.1, Currency: "USD", Unit: "GB-Mo", Stale: true}},
	}
	cands, _ := sc.Scan(context.Background(), cat, time.Now())
	if !cands[0].PriceStale || cands[0].Currency != "USD" {
		t.Fatalf("stale flag lost: %+v", cands[0])
	}
}
