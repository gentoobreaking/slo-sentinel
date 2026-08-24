package main

// daemon.go（T009）：主輪詢迴圈——串接 spec/catalog/budget/capacity/alert/store。
// 實作對照 algs/capacity-eta.md §A.6 虛擬碼；通知採「直推中心」定案
// （algs/sensor-catalog.md §C.1）。

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	internalSpec "slo-sentinel/internal/spec"

	"slo-sentinel/config"
	"slo-sentinel/internal/alert"
	"slo-sentinel/internal/budget"
	"slo-sentinel/internal/capacity"
	"slo-sentinel/internal/promdur"
	"slo-sentinel/internal/query"
	"slo-sentinel/internal/store"
)

type specSLO struct {
	ID         string
	Service    string
	SLIQuery   string
	Objective  float64
	WindowDays int
}

var internalSpecLoad = internalSpec.Load

// telegramChatFromEnv 讀取 TELEGRAM_CHAT_ID。

// sensorRunner 抽象一個可輪詢的感測（SLO 預算或容量），供迴圈統一處理。
type sensorRunner struct {
	id     string
	poll   func(ctx context.Context) (budget.Forecast, error)
	kind   string // capacity | budget
	filter string // amcoord 比對用（感測 id）
}

// daemon 持有跨輪詢的共享元件。
type daemon struct {
	cfg      config.Config
	log      *slog.Logger
	src      query.Source
	st       *store.Store
	notifier alert.Notifier
	dedupe   *alert.Dedupe
	amcoord  *alert.AMCoord
	capDefs  []capacity.Def
	metrics  *metricsRegistry

	sensors []sensorRunner
}

func newDaemon(cfg config.Config, log *slog.Logger, src query.Source, st *store.Store) *daemon {
	var notifier alert.Notifier
	if cfg.TelegramToken != "" {
		notifier = alert.NewTelegram(cfg.TelegramToken, telegramChatFromEnv())
	} else {
		log.Warn("telegram_token 未設定：通知降級為 log-only")
		notifier = alert.LogNotifier{}
	}
	return &daemon{
		cfg:      cfg,
		log:      log,
		src:      src,
		st:       st,
		notifier: notifier,
		dedupe:   alert.NewDedupe(),
		amcoord:  &alert.AMCoord{BaseURL: cfg.AlertManagerURL},
		metrics:  newMetricsRegistry(),
	}
}

func (d *daemon) setupSensors(ctx context.Context) error {
	// 容量感測（F8/F9）
	defs, err := capacity.LoadDefs(d.cfg.CapacityDefsDir)
	if err != nil {
		return fmt.Errorf("載入容量定義: %w", err)
	}
	for _, def := range defs {
		def := def
		sensor, err := capacity.New(def, d.src, d.log)
		if err != nil {
			return err
		}
		d.sensors = append(d.sensors, sensorRunner{
			id:     def.ID,
			kind:   "capacity",
			filter: def.ID,
			poll: func(c context.Context) (budget.Forecast, error) {
				return sensor.Poll(c)
			},
		})
	}

	// SLO 預算感測（F1/F2）：以 SLIQuery 為消耗量、預算比為天花板重用同一引擎
	slos, err := specLoadAll(d.cfg.SloDefsDir)
	if err != nil {
		return fmt.Errorf("載入 SLO 定義: %w", err)
	}
	for _, slo := range slos {
		slo := slo
		budgetRatio := (100 - slo.Objective) / 100
		d.sensors = append(d.sensors, sensorRunner{
			id:     slo.ID,
			kind:   "budget",
			filter: slo.Service,
			poll: func(c context.Context) (budget.Forecast, error) {
				now := time.Now().UTC()
				window := promdur.Parse(fmt.Sprintf("%dd", slo.WindowDays))
				res, err := d.src.RangeQuery(c, slo.SLIQuery, now.Add(-window), now, time.Minute)
				if err != nil {
					return budget.Forecast{}, err
				}
				var series []query.Sample
				if len(res) > 0 {
					series = res[0].Samples
				}
				samples := map[time.Duration][]query.Sample{}
				for _, w := range budget.DefaultHorizons {
					samples[w] = series
				}
				inst, err := d.src.InstantQuery(c, slo.SLIQuery, now)
				if err != nil {
					return budget.Forecast{}, err
				}
				value := 0.0
				if len(inst) > 0 && len(inst[0].Samples) > 0 {
					value = inst[0].Samples[len(inst[0].Samples)-1].Value
				}
				return budget.Evaluate(budget.Input{
					Def: budget.Definition{
						ID: slo.ID,
					},
					Now:      now,
					Value:    value,
					Ceiling:  budgetRatio, // 錯誤預算比 = (100−objective)/100
					Samples:  samples,
					Interval: time.Minute,
					Th:       budget.DefaultThresholds(),
				})
			},
		})
	}
	d.log.Info("sensors_configured", "count", len(d.sensors))
	return nil
}

// runOnePoll 執行一輪全部感測（§A.6 虛擬碼的主體）。單一感測 panic 不拖垮整輪。
func (d *daemon) runOnePoll(ctx context.Context) error {
	for _, sr := range d.sensors {
		func() {
			defer func() {
				if r := recover(); r != nil {
					d.log.Error("sensor_poll_panic", "sensor", sr.id, "panic", fmt.Sprint(r))
				}
			}()
			f, err := sr.poll(ctx)
			if err != nil {
				d.log.Error("sensor_poll_failed", "sensor", sr.id, "error", err.Error())
				return
			}
			// 記錄預測（/accuracy 自評資料源）
			if err := d.st.AppendPrediction(store.Prediction{
				SensorID:    f.ID,
				PredictedAt: f.Now,
				EtaAggressive:   f.EtaAggressive,
				EtaConservative: f.EtaConservative,
				ActualValue: f.Value,
			}); err != nil {
				d.log.Error("append_prediction_failed", "sensor", f.ID, "error", err.Error())
			}
			// AM 協調靜默（F2b）：靜態告警已 firing → 不直推，只更新狀態
			firing := false
			if sr.filter != "" {
				firing, err = d.amcoord.HasFiringAlerts(ctx, sr.filter)
				if err != nil {
					d.log.Warn("amcoord_failed", "error", err.Error())
				}
			}
			if !firing && d.dedupe.ShouldNotify(f.ID, string(f.State)) {
				msg := formatForecastCard(f)
				if err := d.notifier.Send(ctx, msg); err != nil {
					d.log.Error("notify_failed", "error", err.Error())
				}
			}
			prev, _ := d.st.GetState(f.ID)
			var lastNotify time.Time
			if prev != nil {
				lastNotify = prev.LastNotifyAt
			}
			if err := d.st.SetState(store.SensorState{
				SensorID:     f.ID,
				State:        string(f.State),
				LastValue:    f.Value,
				LastNotifyAt: lastNotify,
			}); err != nil {
				d.log.Error("set_state_failed", "error", err.Error())
			}
			// 指標暴露（G2）：eta 與使用率（僅觀測）
			if f.EtaAggressive != nil {
				d.metrics.Set(fmt.Sprintf("sentinel_eta_seconds{sensor=%q,horizon=%q}", f.ID, "aggressive"), *f.EtaAggressive)
			}
			if f.EtaConservative != nil {
				d.metrics.Set(fmt.Sprintf("sentinel_eta_seconds{sensor=%q,horizon=%q}", f.ID, "conservative"), *f.EtaConservative)
			}
			d.metrics.Set(fmt.Sprintf("sentinel_capacity_used_ratio{sensor=%q}", f.ID), f.Utilization)
		}()
	}
	return ctx.Err()
}

// Run 主迴圈：setup → 啟動 API/metrics → 每隔 interval 執行 runOnePoll → 收尾。
func (d *daemon) Run(ctx context.Context) error {
	if err := d.setupSensors(ctx); err != nil {
		return err
	}
	api := &readAPI{d: d}
	apiErr := make(chan error, 1)
	go func() { apiErr <- api.serve(d.cfg.ListenAddr) }()
	metricsErr := make(chan error, 1)
	go func() { metricsErr <- serveMetrics(d.cfg.MetricsAddr, d.metrics) }()

	ticker := time.NewTicker(time.Duration(d.cfg.PollIntervalSec) * time.Second)
	defer ticker.Stop()

	// 立即執行第一輪，之後每間格一次
	if err := d.runOnePoll(ctx); err != nil {
		d.log.Info("scheduler_stopped", "reason", err.Error())
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			d.log.Info("scheduler_finished")
			return nil
		case e := <-apiErr:
			return fmt.Errorf("read API 中止: %w", e)
		case e := <-metricsErr:
			return fmt.Errorf("metrics 中止: %w", e)
		case <-ticker.C:
			if err := d.runOnePoll(ctx); err != nil {
				return nil
			}
		}
	}
}

// formatForecastCard 產生人話卡（雙視野並陳，§A.3/A.7 格式）。
func formatForecastCard(f budget.Forecast) string {
	var b strings.Builder
	icon := map[budget.State]string{
		budget.StateHealthy: "✅", budget.StateWarning: "⚠️", budget.StateCritical: "🔴",
	}[f.State]
	fmt.Fprintf(&b, "%s %s\n使用率 %.1f%%｜餘量 %.4g\n", icon, f.ID, f.Utilization*100, f.Headroom)
	if f.EtaAggressive != nil {
		fmt.Fprintf(&b, "若持續爆量：約 %s 後觸頂\n", humanDur(*f.EtaAggressive))
	} else {
		b.WriteString("激進視野：無觸頂風險\n")
	}
	if f.EtaConservative != nil {
		fmt.Fprintf(&b, "回到常態：尚餘 %s\n", humanDur(*f.EtaConservative))
	}
	return b.String()
}

func humanDur(seconds float64) string {
	switch {
	case seconds < 3600:
		return fmt.Sprintf("%.0f 分鐘", seconds/60)
	case seconds < 48*3600:
		return fmt.Sprintf("%.1f 小時", seconds/3600)
	default:
		return fmt.Sprintf("%.1f 天", seconds/86400)
	}
}

func telegramChatFromEnv() int64 {
	v := os.Getenv("TELEGRAM_CHAT_ID")
	var id int64
	fmt.Sscanf(v, "%d", &id)
	return id
}

// specLoadAll 包裝 internal/spec，維持 cmd 層簡潔。
func specLoadAll(dir string) ([]specSLO, error) {
	slos, err := internalSpecLoad(dir)
	if err != nil {
		return nil, err
	}
	out := make([]specSLO, 0, len(slos))
	for _, x := range slos {
		out = append(out, specSLO{ID: x.ID, Service: x.Service,
			SLIQuery: x.SLIQuery, Objective: x.Objective, WindowDays: x.WindowDays})
	}
	return out, nil
}

// sortedSensorIDs 供狀態表穩定輸出。
func sortedSensorIDs(states map[string]store.SensorState) []string {
	out := make([]string, 0, len(states))
	for id := range states {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ---- 組裝 helpers（可測試：回傳介面）----

func newPrometheusSource(promURL string) query.Source { return query.New(promURL) }

func openStore(path string) (*store.Store, error) { return store.Open(path) }

// contextWithSignal 掛 SIGTERM/SIGINT → ctx 取消（graceful shutdown，T009 驗收項）。
func contextWithSignal(ctx context.Context) (context.Context, func()) {
	ctx2, cancel := context.WithCancel(ctx)
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM, syscall.SIGINT)
		select {
		case <-c:
			cancel()
		case <-ctx2.Done():
		}
	}()
	return ctx2, cancel
}

// eachState 迭代所有已設定感測的最新狀態。
func (d *daemon) eachState(fn func(store.SensorState)) {
	for _, sr := range d.sensors {
		st, err := d.st.GetState(sr.id)
		if err == nil && st != nil {
			fn(*st)
		}
	}
}
