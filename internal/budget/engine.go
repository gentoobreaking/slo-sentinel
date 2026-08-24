// Package budget 實作 SLO 預算與容量的多視野 ETA 前驅預警引擎。
//
// 唯一實作依據：algs/capacity-eta.md §A.1–§A.7。
// 本套件為純函式計算核心——I/O（Prometheus 查詢、通知）由呼叫端注入。
package budget

import (
	"fmt"
	"math"
	"sort"
	"time"

	"slo-sentinel/internal/query"
)

// State 為感測狀態機狀態。
type State string

const (
	StateHealthy  State = "healthy"
	StateWarning  State = "warning"
	StateCritical State = "critical"
)

// 預設觸發門檻（§A.4 表格；YAML 可覆寫）。
const (
	DefaultWarnEta   = 72 * time.Hour
	DefaultCritEta   = 6 * time.Hour
	DefaultSoftRatio = 0.80
	DefaultCritRatio = 0.95
	DefaultExitPolls = 2     // 解除遲滯：連續 N 個輪詢週期低於門檻才降級
	DefaultEpsilon   = 1e-12 // 斜率絕對值 ≤ ε 視為無成長
)

// 預設迴歸視野窗（§F2：激進=1h 速率；穩健=3d 速率；6h 為中間視野一併提供）。
var DefaultHorizons = []time.Duration{time.Hour, 6 * time.Hour, 3 * 24 * time.Hour}

// Thresholds 為可覆寫的觸發門檻組。
type Thresholds struct {
	WarnEta   time.Duration
	CritEta   time.Duration
	SoftRatio float64
	CritRatio float64
}

func DefaultThresholds() Thresholds {
	return Thresholds{WarnEta: DefaultWarnEta, CritEta: DefaultCritEta,
		SoftRatio: DefaultSoftRatio, CritRatio: DefaultCritRatio}
}

// validate 確保門檻組合合理（soft < crit、warn > crit_eta）。
func (t Thresholds) validate() error {
	if !(t.SoftRatio < t.CritRatio) {
		return fmt.Errorf("soft_ratio(%v) 必須小於 crit_ratio(%v)", t.SoftRatio, t.CritRatio)
	}
	if !(t.WarnEta > t.CritEta) {
		return fmt.Errorf("warn_eta(%v) 必須大於 crit_eta(%v)", t.WarnEta, t.CritEta)
	}
	return nil
}

// ---- 數值工具 ----

// TheilSenSlope 以 Theil–Sen 法估計斜率（單位/秒）：所有兩兩配對斜率的中位數。
// 樣本超過 maxPairs 點數時先均勻降採樣（控制 O(n²) 成本）。
// 樣本 <2 或全為同時刻時回傳 (0, false)。
func TheilSenSlope(samples []query.Sample, maxPoints int) (float64, bool) {
	pts := decimate(samples, maxPoints)
	if len(pts) < 2 {
		return 0, false
	}
	var slopes []float64
	for i := 0; i < len(pts); i++ {
		for j := i + 1; j < len(pts); j++ {
			dt := pts[j].Time.Sub(pts[i].Time).Seconds()
			if dt <= 0 {
				continue
			}
			slopes = append(slopes, (pts[j].Value-pts[i].Value)/dt)
		}
	}
	if len(slopes) == 0 {
		return 0, false
	}
	sort.Float64s(slopes)
	n := len(slopes)
	if n%2 == 1 {
		return slopes[n/2], true
	}
	return (slopes[n/2-1] + slopes[n/2]) / 2, true
}

// OLSSlope 以最小平方法估計斜率——僅供 /accuracy 對比實驗（feature flag），
// 正式路徑一律用 TheilSenSlope（脈衝穩健性見 algs/capacity-eta.md §A.2）。
func OLSSlope(samples []query.Sample) (float64, bool) {
	if len(samples) < 2 {
		return 0, false
	}
	var st, sy, sty, stt float64
	for _, s := range samples {
		t := s.Time.Sub(samples[0].Time).Seconds()
		st += t
		sy += s.Value
		sty += t * s.Value
		stt += t * t
	}
	den := float64(len(samples))*stt - st*st
	if den == 0 {
		return 0, false
	}
	return (float64(len(samples))*sty - st*sy) / den, true
}

// decimate 均勻降採樣至至多 max 點。
func decimate(samples []query.Sample, max int) []query.Sample {
	if len(samples) <= max {
		return samples
	}
	step := float64(len(samples)-1) / float64(max-1)
	out := make([]query.Sample, 0, max)
	for i := 0; i < max; i++ {
		out = append(out, samples[int(math.Round(float64(i)*step))])
	}
	return out
}

// SplitByGaps 將序列切成連續段：任何相鄰間隙 > maxGap 即切斷。
// （§A.5：跨越缺口的配對不納入斜率計算。）
func SplitByGaps(samples []query.Sample, maxGap time.Duration) [][]query.Sample {
	var runs [][]query.Sample
	cur := make([]query.Sample, 0, len(samples))
	for _, s := range samples {
		if len(cur) > 0 && s.Time.Sub(cur[len(cur)-1].Time) > maxGap {
			runs = append(runs, cur)
			cur = make([]query.Sample, 0, len(samples))
		}
		cur = append(cur, s)
	}
	if len(cur) > 0 {
		runs = append(runs, cur)
	}
	return runs
}

// MinSamples 回傳視野窗在給定取樣間隔下的最少樣本數（§A.5：間隔的 83%）。
func MinSamples(window, interval time.Duration) int {
	if interval <= 0 {
		return 0
	}
	expected := int(math.Round(float64(window) / float64(interval)))
	return expected * 83 / 100
}

// CeilingJumped 判斷天花板是否跳變超過 1%（§A.5：擴容後舊斜率全部失效）。
// prevCeiling ≤ 0（首次觀測）視為未跳變。
func CeilingJumped(prevCeiling, newCeiling float64) bool {
	if prevCeiling <= 0 || newCeiling <= 0 {
		return false
	}
	return math.Abs(newCeiling-prevCeiling)/prevCeiling > 0.01
}
