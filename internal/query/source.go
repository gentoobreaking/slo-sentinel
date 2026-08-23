package query

import (
	"context"
	"time"
)

// Sample 為單一時間點的指標值。
type Sample struct {
	Time  time.Time
	Value float64
}

// Result 為一次查詢回傳的序列（含標籤）。
type Result struct {
	Labels  map[string]string
	Samples []Sample
}

// Source 是指標來源的抽象介面：下游（budget/capacity/waste）只依賴此介面，
// 便於以 FakeSource 注入測試資料。
type Source interface {
	// InstantQuery 回傳 query 在 at 時刻附近的即時值。
	InstantQuery(ctx context.Context, query string, at time.Time) ([]Result, error)

	// RangeQuery 回傳 [start, end] 內、間隔 step 的時間序列。
	RangeQuery(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Result, error)
}
