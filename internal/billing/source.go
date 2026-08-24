// Package billing 接雲端帳務 API，將日花費轉為標準感測資料（F11）。
//
// 誠實前提（algs/cost-forecast.md §D.1）：帳務資料有 ~24h 延遲，
// 所有回傳的 Date 為「雲端確認日」，非即時。
package billing

import (
	"context"
	"time"
)

// DailySpend 為單日單一維度的花費。
type DailySpend struct {
	Date    time.Time // 雲端確認日（UTC，當日起始）
	CostUSD float64   // 未攤銷成本（unblended）
	Service string    // 服務名稱（如 Amazon EC2）；"" 表示全部加總
}

// Filter 描述查詢範圍。
type Filter struct {
	Account  string            // 帳號（可空 = 全部）
	Tags     map[string]string // 資源標籤（如 team=platform）
	Services []string          // 指定服務；空 = 全部
}

// BillingSource 為帳務來源的抽象介面（adapter 模式，同 query.Source 慣例）。
type BillingSource interface {
	// DailySpend 回傳 [start, end] 的每日花費（含端點日）。
	DailySpend(ctx context.Context, f Filter, start, end time.Time) ([]DailySpend, error)
	// Name 回傳來源名稱（aws-ce / alicloud-bss）。
	Name() string
}
