package waste

// rightsizing.go（T013 前置）：供給過剩判定（algs/waste-detection.md §E.1-B）。

import "math"

// LowUtil 判定供給過剩：P95 使用率比 < threshold。
// 用 P95 而非均值——尖峰時刻撐得住就不該縮（§E.1-B）。
func LowUtil(p95Ratio, threshold float64) bool {
	return p95Ratio < threshold
}

// SuggestedSaving 估算月省金額：月省 = (現價 − 建議價) × 730h（§E.1-B 最末）。
func SuggestedSaving(priceCurrent, priceSuggested float64) float64 {
	saving := (priceCurrent - priceSuggested) * 730
	if saving < 0 {
		return 0
	}
	return saving
}

// RoundUpTo 決定建議檔位：將使用量向上取整至 step 的倍數（供降規檔位對照）。
func RoundUpTo(usage, step float64) float64 {
	if step <= 0 {
		return usage
	}
	return math.Ceil(usage/step) * step
}
