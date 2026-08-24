package capacity

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"slo-sentinel/internal/budget"
	"slo-sentinel/internal/query"
)

// Sensor 維護單一容量感測的跨輪詢狀態（天花板/狀態/解除遲滯計數）。
type Sensor struct {
	def  Def
	src  query.Source
	log  *slog.Logger
	step time.Duration // RangeQuery 取樣間隔

	prevCeiling float64
	prevState   budget.State
	streak      int
	started     bool
}

// New 建立感測器。
func New(def Def, src query.Source, logger *slog.Logger) (*Sensor, error) {
	if def.Metric.Value == "" || def.Metric.Ceiling == "" {
		return nil, fmt.Errorf("capacity %s: metric.value/ceiling 不可為空", def.ID)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Sensor{def: def, src: src, log: logger, step: time.Minute,
		prevState: budget.StateHealthy}, nil
}

// Poll 執行一輪：查詢現值/天花板/各視野窗序列 → budget.Evaluate → 更新狀態。
func (s *Sensor) Poll(ctx context.Context) (budget.Forecast, error) {
	now := time.Now().UTC()

	value, err := s.instant(ctx, s.def.Metric.Value, now)
	if err != nil {
		return budget.Forecast{}, fmt.Errorf("value query: %w", err)
	}
	ceiling, err := s.instant(ctx, s.def.Metric.Ceiling, now)
	if err != nil {
		return budget.Forecast{}, fmt.Errorf("ceiling query: %w", err)
	}

	horizons := s.def.HorizonDurations()
	samples := map[time.Duration][]query.Sample{}
	for _, w := range horizons {
		res, err := s.src.RangeQuery(ctx, s.def.Metric.Value, now.Add(-w), now, s.step)
		if err != nil {
			return budget.Forecast{}, fmt.Errorf("range %s: %w", w, err)
		}
		if len(res) > 0 {
			samples[w] = res[0].Samples
		}
	}

	f, err := budget.Evaluate(budget.Input{
		Def: budget.Definition{
			ID:         s.def.ID,
			Horizons:   horizons,
		},
		Now:            now,
		Value:          value,
		Ceiling:        ceiling,
		PrevCeiling:    s.prevCeiling,
		Samples:        samples,
		Interval:       s.step,
		PrevState:      s.prevState,
		PrevExitStreak: s.streak,
		Th:             s.def.Thresholds.Resolve(),
	})
	if err != nil {
		return f, err
	}

	s.started = true
	s.prevCeiling = ceiling
	s.prevState = f.State
	s.streak = f.ExitStreak
	s.log.Info("capacity_polled",
		"sensor", s.def.ID, "state", string(f.State),
		"utilization", f.Utilization,
	)
	return f, nil
}

// instant 回傳查詢的第一個結果值（無結果回錯誤）。
func (s *Sensor) instant(ctx context.Context, q string, at time.Time) (float64, error) {
	res, err := s.src.InstantQuery(ctx, q, at)
	if err != nil {
		return 0, err
	}
	for _, r := range res {
		if len(r.Samples) > 0 {
			return r.Samples[len(r.Samples)-1].Value, nil
		}
	}
	return 0, fmt.Errorf("查詢 %q 無資料點", q)
}
