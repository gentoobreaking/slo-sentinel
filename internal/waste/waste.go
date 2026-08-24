package waste

// Package waste 實作瘦身與閒置偵測（F14）。
//
// 架構：感測條目由 catalog 的 waste 類規則提供（Prometheus rules 格式，
// 詳見 algs/waste-detection.md §8.8）；本套件對每條規則的 expr 做持續成立掃描——
// 規則運算式在 Prometheus 端求值（如 max_over_time[14d] <= 10），
// sentinel 只負責查詢結果序列並判定「連續成立天數」。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"slo-sentinel/internal/catalog"
	"slo-sentinel/internal/pricing"
	"slo-sentinel/internal/query"
)

// Pricer 抽象單價查詢（internal/pricing.Catalog 滿足；測試用 fake 離線跑）。
type Pricer interface {
	Quote(ctx context.Context, f pricing.Family, attrs pricing.Attrs) (pricing.Quote, error)
}

// Candidate 為一個瘦身影/殭屍資源候選。
type Candidate struct {
	SensorID   string // algs §8.8：sentinel_sensor 或 alertname
	AlertName  string
	Reason     string        // annotations.summary
	Window     time.Duration // 判定視窗（如 14d）
	IdleDays   float64       // 連續成立天數
	WastedCost float64       // 日浪費估算；有報價時 = 月省/30，無為 0
	Renotify   time.Duration // 重提週期（labels notify_every）
	Labels     map[string]string

	// 每月可省金額（T027）：查 pricing 而來；查不到時 SavingMonthly=0 且
	// PriceError 帶原因——顯示層以「—」呈現，不誤導。
	SavingMonthly float64
	Currency      string // 原幣別（§D.4 如實標注，不換算）
	PriceUnit     string // Hrs / GB-Mo…
	PriceStale    bool   // 過期快取
	PriceError    string // 查價失敗原因（空 = 成功或未設定對照）
}

// Scanner 對目錄中的 waste 類規則做每日批次掃描。
type Scanner struct {
	Src    query.Source
	Logger interface {
		Warn(msg string, args ...any)
		Info(msg string, args ...any)
	}
	Pricer Pricer // nil = 不查價（金額欄留空）
}

// Scan 對每條 waste 警告規則查詢其 expr 的歷史序列：
// 若整個「for」視窗內所有樣本皆為真（≥0.5），即判定候選成立。
func (sc *Scanner) Scan(ctx context.Context, cat *catalog.Catalog, now time.Time) ([]Candidate, error) {
	var out []Candidate
	for _, r := range cat.RulesOfKind(catalog.KindWaste) {
		if r.Alert == "" {
			continue // 只掃 alert 類；record 由引擎維護
		}
		window := r.For
		if window <= 0 {
			window = 14 * 24 * time.Hour // 預設視窗（algs §8.8 慣例）
		}
		res, err := sc.Src.RangeQuery(ctx, r.Expr, now.Add(-window), now, 24*time.Hour)
		if err != nil {
			// best-effort（T024）：單一規則 expr 失敗不拖垮整輪掃描
			if sc.Logger != nil {
				sc.Logger.Warn("waste_rule_scan_failed", "rule", r.ID(), "error", err.Error())
			}
			continue
		}
		if len(res) == 0 || len(res[0].Samples) == 0 {
			continue
		}
		samples := res[0].Samples
		allTrue := true
		for _, smp := range samples {
			if smp.Value < 0.5 {
				allTrue = false
				break
			}
		}
		if !allTrue {
			continue
		}
		c := Candidate{
			SensorID:  r.ID(),
			AlertName: r.Alert,
			Reason:    r.Annotations["summary"],
			Window:    window,
			IdleDays:  window.Hours() / 24,
			Renotify:  r.NotifyEvery(),
			Labels:    r.Labels,
		}
		sc.applyPricing(ctx, &c, r)
		out = append(out, c)
	}
	return out, nil
}

// applyPricing 依規則 labels 的價目對照查每月可省金額（T027）：
//   - sentinel_price_family：ec2 / ebs / rds / 阿里雲 module code
//   - sentinel_price_attrs：JSON 編碼 {region, instance_type, quantity…}
//
// 未設定對照 → 金額欄留空（不誤導）；查價失敗 → PriceError 帶原因，
// 不阻擋候選成立。
func (sc *Scanner) applyPricing(ctx context.Context, c *Candidate, r *catalog.Rule) {
	famStr := r.Labels["sentinel_price_family"]
	if famStr == "" || sc.Pricer == nil {
		return
	}
	attrs := pricing.Attrs{}
	if raw := r.Labels["sentinel_price_attrs"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
			c.PriceError = "sentinel_price_attrs JSON 解析失敗"
			return
		}
	}
	q, err := sc.Pricer.Quote(ctx, pricing.Family(famStr), attrs) // 走 Catalog.Quote：TTL 快取＋stale fallback
	if err != nil {
		c.PriceError = err.Error()
		return
	}
	qty := 1.0
	if v := attrs["quantity"]; v != "" {
		if f, perr := parseFloat(v); perr == nil && f > 0 {
			qty = f
		}
	}
	c.SavingMonthly = monthlyFromQuote(q, qty)
	c.Currency = q.Currency
	c.PriceUnit = q.Unit
	c.PriceStale = q.Stale
	c.WastedCost = c.SavingMonthly / 30 // 日率：供 Tracker 累積與 resolve 統計
}

// monthlyFromQuote 依計價單位換算月省金額：Hrs×24×30；GB-Mo 及其他月計單位直接乘。
func monthlyFromQuote(q pricing.Quote, qty float64) float64 {
	switch q.Unit {
	case "Hrs":
		return q.UnitPrice * 24 * 30 * qty
	default:
		return q.UnitPrice * qty
	}
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%g", &f)
	return f, err
}
