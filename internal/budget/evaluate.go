package budget

import (
	"fmt"
	"time"

	"slo-sentinel/internal/query"
)

// Forecast 為一次完整預測的輸出。
type Forecast struct {
	ID              string
	Now             time.Time
	Value           float64  // m(t₀)
	Ceiling         float64  // C(t₀)
	Headroom        float64  // H = Ceiling − Value
	Utilization     float64  // U = Value / Ceiling
	EtaAggressive   *float64 // 秒；nil = 該視野無風險（β≤ε）或無法預測
	EtaConservative *float64
	PerWindow       map[string]*WindowEval
	State           State
	ExitStreak      int  // 解除遲滯計數（連續低於門檻的輪詢次數）
	CeilingJumped   bool // 本輪偵測到天花板跳變 >1%
}

// WindowEval 為單一視野窗的評估結果。
type WindowEval struct {
	Window  time.Duration
	Slope   *float64 // 單位/秒；nil = 樣本不足
	Samples int
	ETA     *float64 // 秒
	Valid   bool
	Reason  string // 無效原因（樣本不足/全為缺口…）
}

// Definition 描述一個容量/SLO 預算感測的查詢與門檻。
type Definition struct {
	ID           string
	ValueQuery   string          // m(t) 的 PromQL
	CeilingQuery string          // C(t) 的 PromQL（動態天花板）
	Horizons     []time.Duration // 迴歸視野窗；空則用 DefaultHorizons
}

// Input 為 Evaluate 的全部輸入。
type Input struct {
	Def            Definition
	Now            time.Time
	Value          float64
	Ceiling        float64
	PrevCeiling    float64                          // 上次天花板；≤0 表示首次觀測
	Samples        map[time.Duration][]query.Sample // 各視野窗原始樣本（含至 Now）
	Interval       time.Duration                    // 取樣間隔
	PrevState      State                            // 上次狀態（解除遲滯用）
	PrevExitStreak int                              // 連續低於門檻次數
	Th             Thresholds                       // 門檻（零值 → DefaultThresholds）
	UseOLS         bool                             // 實驗旗標：改用 OLS 斜率
}

// Evaluate 執行 §A.3–A.6 的完整預測流程。
func Evaluate(in Input) (Forecast, error) {
	th := in.Th
	if th == (Thresholds{}) {
		th = DefaultThresholds()
	} else if err := th.validate(); err != nil {
		return Forecast{}, fmt.Errorf("thresholds: %w", err)
	}
	if in.Def.ID == "" {
		return Forecast{}, fmt.Errorf("definition id 不可為空")
	}
	if in.Ceiling <= 0 {
		return Forecast{}, fmt.Errorf("ceiling 必須 > 0，得到 %v", in.Ceiling)
	}
	horizons := in.Def.Horizons
	if len(horizons) == 0 {
		horizons = DefaultHorizons
	}
	jumped := CeilingJumped(in.PrevCeiling, in.Ceiling)

	f := Forecast{
		ID:            in.Def.ID,
		Now:           in.Now,
		Value:         in.Value,
		Ceiling:       in.Ceiling,
		Headroom:      in.Ceiling - in.Value,
		Utilization:   in.Value / in.Ceiling,
		PerWindow:     map[string]*WindowEval{},
		CeilingJumped: jumped,
	}

	for _, w := range horizons {
		ev := evalWindow(w, in, jumped)
		key := w.String()
		f.PerWindow[key] = ev

		if w == time.Hour {
			f.EtaAggressive = ev.ETA // 激進視野 = 最短窗速率
		}
	}
	// 穩健視野 = 最長窗速率（§A.7：3d 案例）
	var longest time.Duration
	for _, w := range horizons {
		if w > longest {
			longest = w
			if f.PerWindow[w.String()] != nil && f.PerWindow[w.String()].ETA != nil {
				v := *f.PerWindow[w.String()].ETA
				f.EtaConservative = &v
			} else {
				f.EtaConservative = nil
			}
		}
	}

	f.State, f.ExitStreak = decide(f, in)
	return f, nil
}

// evalWindow 對單一視野窗做有效性校驗＋斜率＋ETA。
func evalWindow(w time.Duration, in Input, ceilingJumped bool) *WindowEval {
	ev := &WindowEval{Window: w}
	samples := in.Samples[w]

	minN := MinSamples(w, in.Interval)
	if len(samples) < minN {
		ev.Reason = fmt.Sprintf("樣本 %d < 最少需求 %d", len(samples), minN)
		return ev
	}
	if ceilingJumped {
		ev.Reason = "天花板跳變，重新累積中"
		return ev
	}

	runs := SplitByGaps(samples, 5*time.Minute)
	totalPairs := 0
	var slopes []float64
	for _, run := range runs {
		var slope float64
		var ok bool
		if in.UseOLS {
			slope, ok = OLSSlope(run)
		} else {
			slope, ok = TheilSenSlope(run, 100)
		}
		if ok {
			slopes = append(slopes, slope)
		}
		totalPairs += len(run)
	}
	if len(slopes) == 0 {
		ev.Reason = "無有效斜率"
		return ev
	}
	// 多段時取中位數斜率（與 Theil–Sen 同哲學：抗單段異常）
	sortSlice(slopes)
	beta := slopes[len(slopes)/2]
	ev.Samples = totalPairs
	ev.Slope = &beta

	if beta <= DefaultEpsilon { // 未成長或下降：該視野無風險
		ev.Valid = true
		return ev
	}
	eta := f_headroom(in) / beta
	ev.ETA = &eta
	ev.Valid = true
	return ev
}

func f_headroom(in Input) float64 {
	return in.Ceiling - in.Value
}

func sortSlice(s []float64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// decide 依 §A.4 判定狀態並套用解除遲滯（連續 2 輪詢低於門檻才降回 healthy）。
// 向上升級（healthy→warning→critical）立即生效。
func decide(f Forecast, in Input) (State, int) {
	th := in.Th
	if th == (Thresholds{}) {
		th = DefaultThresholds()
	}
	streak := in.PrevExitStreak

	wouldWarn := false
	if f.EtaAggressive != nil &&
		time.Duration(*f.EtaAggressive*float64(time.Second)) < th.WarnEta &&
		f.Utilization >= th.SoftRatio {
		wouldWarn = true
	}
	if f.EtaConservative != nil &&
		time.Duration(*f.EtaConservative*float64(time.Second)) < th.WarnEta {
		wouldWarn = true
	}

	wouldCrit := false
	if f.EtaAggressive != nil &&
		time.Duration(*f.EtaAggressive*float64(time.Second)) < th.CritEta {
		wouldCrit = true
	}
	if f.Utilization >= th.CritRatio {
		wouldCrit = true
	}

	switch {
	case wouldCrit:
		return StateCritical, 0
	case wouldWarn:
		return StateWarning, 0
	default:
		// 門檻外：套用解除遲滯——連續 ExitStreak+1 ≥ DefaultExitPolls 才降回 healthy
		streak++
		if streak >= DefaultExitPolls || in.PrevState == StateHealthy {
			return StateHealthy, streak
		}
		if in.PrevState == "" {
			return StateHealthy, streak
		}
		return in.PrevState, streak // 保持現狀等待第二輪確認
	}
}
