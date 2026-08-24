package cost

// weekly.go——每週成本摘要（algs/cost-forecast.md §D.5）：
// Telegram 每週一封成本摘要，含 top 5 成長服務與原因猜測——
// 成長來源比對 capacity 感測的擴容軌跡。

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"slo-sentinel/internal/billing"
)

// GrowthRow 為單一服務的週成長列。
type GrowthRow struct {
	Service     string  `json:"service"`
	RecentAvg   float64 `json:"recent_avg"`   // 近 7 天日均
	PreviousAvg float64 `json:"previous_avg"` // 前 7 天日均
	Growth      float64 `json:"growth"`       // RecentAvg − PreviousAvg
	LikelyCause string  `json:"likely_cause"` // 原因猜測（容量擴容比對結果）
}

// WeeklyGrowthInput 為摘要計算輸入。
type WeeklyGrowthInput struct {
	ThisWeek []billing.DailySpend // 近 7 天日花費（Service 標注服務；"" 表示未分服務）
	PrevWeek []billing.DailySpend // 再前一個 7 天
	CapTrend map[string]float64   // capacity 感測 id → 近 7 天數值變化量（>0 = 擴容中）
}

// WeeklyTopGrowth 計算 top N（預設 5）成長服務，並比對 capacity 擴容軌跡產生原因猜測。
func WeeklyTopGrowth(in WeeklyGrowthInput, topN int) []GrowthRow {
	if topN <= 0 {
		topN = 5
	}
	recent := dailyByService(in.ThisWeek)
	prev := dailyByService(in.PrevWeek)

	rows := make([]GrowthRow, 0, len(recent))
	for svc, r := range recent {
		p := prev[svc]
		g := r - p
		if g <= 1e-9 {
			continue // 只關心成長
		}
		rows = append(rows, GrowthRow{
			Service: svc, RecentAvg: r, PreviousAvg: p, Growth: g,
			LikelyCause: guessCause(svc, in.CapTrend),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Growth > rows[j].Growth })
	if len(rows) > topN {
		rows = rows[:topN]
	}
	return rows
}

func dailyByService(spends []billing.DailySpend) map[string]float64 {
	sum, days := map[string]float64{}, map[string]int{}
	for _, s := range spends {
		sum[s.Service] += s.CostUSD
		days[s.Service]++
	}
	out := map[string]float64{}
	for svc, total := range sum {
		if d := days[svc]; d > 0 {
			out[svc] = total / float64(d)
		}
	}
	return out
}

// guessCause 比對 capacity 擴容軌跡：感測 id 與服務名互相包含、或趨勢同時上升
// 即視為「可能相關」。純啟發式，輸出措辭保持不確定語氣（§D.5「原因猜測」）。
func guessCause(service string, capTrend map[string]float64) string {
	var related []string
	svcLower, words := strings.ToLower(service), strings.Fields(strings.ToLower(service))
	for id, delta := range capTrend {
		if delta <= 0 {
			continue // 只看擴容方向
		}
		idLower := strings.ToLower(id)
		if idLower == "" || svcLower == "" {
			continue
		}
		hit := strings.Contains(svcLower, idLower)
		for _, w := range words {
			if len(w) >= 3 && strings.Contains(idLower, w) { // 詞級比對："Amazon RDS" 的 "rds" 命中 "rds-connections"
				hit = true
				break
			}
		}
		if hit {
			related = append(related, fmt.Sprintf("%s（+%.4g/7d）", id, delta))
		}
	}
	if len(related) > 0 {
		sort.Strings(related)
		return "疑似對應容量擴容：" + strings.Join(related, "、")
	}
	return "成長來源未對應到任何擴容中的容量感測（需人工確認：新服務上線？流量異常？）"
}

// ISOWeekKey 回傳 ISO 週鍵（如 2026-W35），供每週排程去重。
func ISOWeekKey(t time.Time) string {
	year, week := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

// FormatWeeklySummary 產生人話摘要卡（Telegram/UI 共用）。
// confirmedDate 標注帳務資料截止時間（§D.1 鐵律：「今日」其實是昨日）。
func FormatWeeklySummary(rows []GrowthRow, eom EomProjection, confirmedDate time.Time) string {
	var b strings.Builder
	b.WriteString("📊 每週成本摘要\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "• %s：日均 $%.2f → $%.2f（+$%.2f/天）\n  %s\n",
			r.Service, r.PreviousAvg, r.RecentAvg, r.Growth, r.LikelyCause)
	}
	if len(rows) == 0 {
		b.WriteString("近兩週無顯著成長服務\n")
	}
	fmt.Fprintf(&b, "💰 月底推估：$%.2f（爆量情境 $%.2f）\n", eom.Conservative, eom.Aggressive)
	fmt.Fprintf(&b, "（帳務資料截至 %s——雲端確認有延遲）", confirmedDate.Format("2006-01-02"))
	return b.String()
}
