// Package cost 實作營運成本推估（F12/F13）。
//
// 唯一實作依據：algs/cost-forecast.md §D.2 公式、§D.3 觸發條件。
// 本套件為純函式；帳務資料由 internal/billing 提供。
package cost

import (
	"fmt"
	"time"

	"slo-sentinel/internal/billing"
)

// Rates 為雙視野日速率（§D.2）。
type Rates struct {
	Recent float64 // 近 7 天日均——反映近期變化（擴容/新服務）
	Mtd    float64 // 全月日均——被月初平滑
}

// EstimateRates 由 MTD 累積與近 7 天日花費計算雙視野速率。
func EstimateRates(mtdTotal float64, daysElapsed int, last7Days []billing.DailySpend) Rates {
	r := Rates{}
	if daysElapsed > 0 {
		r.Mtd = mtdTotal / float64(daysElapsed)
	}
	if len(last7Days) > 0 {
		sum := 0.0
		for _, s := range last7Days {
			sum += s.CostUSD
		}
		r.Recent = sum / float64(len(last7Days))
	}
	return r
}

// EomProjection 為月底推估結果。
type EomProjection struct {
	Aggressive   float64 // 用 r_recent：反映爆量情境
	Conservative float64 // 用 r_mtd：常態趨勢
	Budget       float64
}

// ProjectEOM 推估月底花費：projected_EOM = S + r × 剩餘天數。
func ProjectEOM(mtdTotal float64, now time.Time, rates Rates) EomProjection {
	dTotal := float64(daysInMonth(now))
	dElapsed := float64(now.Day())
	remaining := dTotal - dElapsed
	if remaining < 0 {
		remaining = 0
	}
	return EomProjection{
		Aggressive:   mtdTotal + rates.Recent*remaining,
		Conservative: mtdTotal + rates.Mtd*remaining,
	}
}

func daysInMonth(t time.Time) int {
	next := t.AddDate(0, 1, 0)
	firstOfNext := time.Date(next.Year(), next.Month(), 1, 0, 0, 0, 0, t.Location())
	return int(firstOfNext.Sub(time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())).Hours() / 24)
}

// BudgetETA 計算預算燒穿時間：ETA = (B − S) / r，r ≤ ε 時回傳 nil（無風險）。
// 回傳秒數。§D.2 預算 ETA 公式。
func BudgetETA(mtdTotal, budget, dailyRate float64) *float64 {
	const eps = 1e-6
	if dailyRate <= eps {
		return nil
	}
	remaining := budget - mtdTotal
	if remaining <= 0 { // 已超支
		v := 0.0
		return &v
	}
	seconds := remaining / dailyRate * 86400
	return &seconds
}

// DetectSpike 爆衝偵測（§D.3）：單日花費 > 日均預算 × 2 倍即為異常——
// 獨立於預算總額（配置錯誤訊號），即使預算充裕也值得知道。
func DetectSpike(todayCost, dailyBudgetAvg float64) bool {
	return dailyBudgetAvg > 0 && todayCost > dailyBudgetAvg*2
}

// YearProjection 年推估：已完成月取實際值，未完成月取推估值。
type MonthPoint struct {
	Month  time.Month
	Value  float64
	Actual bool // true=已發生（實際值）；false=推估
}

// ProjectYear 合併各月點位。now 由呼叫端注入（可測試性）。
func ProjectYear(now time.Time, year int, monthlyActual map[time.Month]float64, currentMonth time.Month, currentMTD, recentRate float64) ([]MonthPoint, float64) {
	var out []MonthPoint
	total := 0.0
	for m := time.January; m <= time.December; m++ {
		if m > currentMonth {
			break
		}
		if v, ok := monthlyActual[m]; ok && m < currentMonth {
			out = append(out, MonthPoint{Month: m, Value: v, Actual: true})
			total += v
		} else if m == currentMonth {
			p := ProjectEOM(currentMTD, now, Rates{Recent: recentRate, Mtd: recentRate})
			v := p.Conservative
			out = append(out, MonthPoint{Month: m, Value: v})
			total += v
		}
	}
	return out, total
}

// FormatReport 產生人話報表（Telegram/UI 共用）。confirmedDate 為帳務資料截止日。
func FormatReport(eom EomProjection, eta *float64, confirmedDate time.Time) string {
	s := fmt.Sprintf("💰 月底推估：$%.2f（爆量情境 $%.2f）", eom.Conservative, eom.Aggressive)
	if eta != nil {
		s += fmt.Sprintf("\n照目前速度，預算 %s 後燒穿", humanDays(*eta))
	}
	s += fmt.Sprintf("\n（帳務資料截至 %s——雲端確認有延遲）", confirmedDate.Format("2006-01-02"))
	return s
}

func humanDays(seconds float64) string {
	days := seconds / 86400
	if days < 1 {
		return fmt.Sprintf("%.1f 小時內", days*24)
	}
	return fmt.Sprintf("%.1f 天", days)
}
