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
	"strconv"
	"strings"
	"syscall"
	"time"

	internalSpec "slo-sentinel/internal/spec"

	"slo-sentinel/config"
	"slo-sentinel/internal/alert"
	"slo-sentinel/internal/billing"
	"slo-sentinel/internal/budget"
	"slo-sentinel/internal/capacity"
	"slo-sentinel/internal/catalog"
	"slo-sentinel/internal/cost"
	"slo-sentinel/internal/pricing"
	"slo-sentinel/internal/promdur"
	"slo-sentinel/internal/query"
	"slo-sentinel/internal/store"
	"slo-sentinel/internal/waste"
	"slo-sentinel/internal/watch"
)

type specSLO struct {
	ID         string
	Service    string
	SLIQuery   string
	Objective  float64
	WindowDays int
	Th         budget.Thresholds // 觸發門檻（slo_defs thresholds 覆寫後；T023）
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

	sensors     []sensorRunner
	lastCatalog *catalog.Catalog
	tracker     *waste.Tracker         // waste 候選生命週期（T024）；Run 時建立並自 store 還原
	notifyRetry map[string]*retryState // 感測通知連續失敗追蹤（T026 退避）
	digestTime  string                 // 每日摘要發送時刻 HH:MM（本地時區）；空 = 停用（T025）

	publisher       *AMPublisher          // ai-oncall 分診閘門；nil = 未啟用（T020）
	sensorMeta      map[string]sensorMeta // 感測 → 分診標籤（T020）
	publishedFiring map[string]bool       // 已轉交 firing 的感測（供 resolved 轉交判定）

	billingSrc billing.BillingSource // 由環境變數組態；nil = 未啟用
	pricer     *pricing.Catalog      // estimate 模式單價目錄；nil = 未啟用
	costMap    []cost.UsageTemplate  // 感測 → 價目家族映射；空 = 未啟用
}

// 通知失敗退避參數（T026）：連續失敗 N 輪後降級為每 M 輪重試一次。
const (
	notifyBackoffAfter = 3
	notifyRetryEvery   = 5
)

type retryState struct {
	fails     int // 連續失敗次數
	sinceFail int // 降級後經過的輪數（達 notifyRetryEvery 歸零並重試）
}

func newDaemon(cfg config.Config, log *slog.Logger, src query.Source, st *store.Store) *daemon {
	var bill billing.BillingSource
	switch {
	case os.Getenv("AWS_ACCESS_KEY_ID") != "" && os.Getenv("AWS_SECRET_ACCESS_KEY") != "":
		bill = &billing.AWSCostExplorer{
			AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
			SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
			Region:    envOr("AWS_REGION", "us-east-1"),
		}
		log.Info("billing_source_enabled", "source", "aws-ce")
	case os.Getenv("ALICLOUD_ACCESS_KEY_ID") != "" && os.Getenv("ALICLOUD_ACCESS_KEY_SECRET") != "":
		bill = &billing.AlicloudBSS{
			AccessKeyID:     os.Getenv("ALICLOUD_ACCESS_KEY_ID"),
			AccessKeySecret: os.Getenv("ALICLOUD_ACCESS_KEY_SECRET"),
		}
		log.Info("billing_source_enabled", "source", "alicloud-bss")
	}

	// estimate 模式（algs/cost-forecast.md §D.0 主路徑）：
	// SENTINEL_COST_MAP 指向「感測 → 價目家族」映射檔即啟用；無需任何 billing IAM。
	var pricer *pricing.Catalog
	var costMap []cost.UsageTemplate
	if mapPath := os.Getenv("SENTINEL_COST_MAP"); mapPath != "" {
		cm, err := cost.LoadCostMap(mapPath)
		if err != nil {
			log.Error("cost_map_load_failed", "error", err.Error())
		} else {
			ali := &pricing.AlicloudSKU{
				AccessKeyID:     os.Getenv("ALICLOUD_ACCESS_KEY_ID"),
				AccessKeySecret: os.Getenv("ALICLOUD_ACCESS_KEY_SECRET"),
			}
			pricer = &pricing.Catalog{CacheDir: envOr("PRICING_CACHE_DIR", ".pricing-cache"), Ali: ali}
			costMap = cm
			log.Info("estimate_mode_enabled", "mappings", len(cm))
		}
	}

	var publisher *AMPublisher
	var notifier alert.Notifier
	if cfg.TelegramToken != "" {
		notifier = alert.NewTelegram(cfg.TelegramToken, telegramChatFromEnv())
	} else {
		log.Warn("telegram_token 未設定：通知降級為 log-only")
		notifier = alert.LogNotifier{}
	}
	// 分診閘門（T020）：ONCALL_GATE_URL 設定即啟用容量預警轉交 ai-oncall
	if u := os.Getenv("ONCALL_GATE_URL"); u != "" {
		publisher = &AMPublisher{URL: u, Token: os.Getenv("ONCALL_GATE_TOKEN")}
	}
	cfg = applyWasteEnvOverride(cfg)
	// 每日摘要時刻（T025）：DAILY_DIGEST=off 停用；HH:MM 覆寫；未設定用 config 預設
	switch v := os.Getenv("DAILY_DIGEST"); {
	case v == "off":
		cfg.DailyDigestTime = ""
	case v != "":
		if _, _, ok := parseDigestTime(v); ok {
			cfg.DailyDigestTime = v
		}
	}
	return &daemon{
		cfg:             cfg,
		log:             log,
		src:             src,
		st:              st,
		notifier:        notifier,
		dedupe:          alert.NewDedupe(),
		amcoord:         &alert.AMCoord{BaseURL: cfg.AlertManagerURL},
		metrics:         newMetricsRegistry(),
		notifyRetry:     map[string]*retryState{},
		digestTime:      cfg.DailyDigestTime,
		publisher:       publisher,
		sensorMeta:      map[string]sensorMeta{},
		publishedFiring: map[string]bool{},
		billingSrc:      bill,
		pricer:          pricer,
		costMap:         costMap,
	}
}

// applyWasteEnvOverride 以環境變數覆寫 waste 掃描週期（T024）：
// WASTE_SCAN_INTERVAL_SEC=off|0 完全停用；數字 = 週期秒數。
func applyWasteEnvOverride(cfg config.Config) config.Config {
	v := os.Getenv("WASTE_SCAN_INTERVAL_SEC")
	switch {
	case v == "off" || v == "0":
		cfg.WasteScanIntervalSec = 0
	case v != "":
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.WasteScanIntervalSec = n
		}
	}
	return cfg
}

// allowNotify 退避閘門（T026）：連續失敗 ≥notifyBackoffAfter 輪後，
// 每 notifyRetryEvery 輪才放行一次重試。
func (d *daemon) allowNotify(sensorID string) bool {
	if d.notifyRetry == nil {
		d.notifyRetry = map[string]*retryState{}
	}
	rt, ok := d.notifyRetry[sensorID]
	if !ok || rt.fails < notifyBackoffAfter {
		return true
	}
	rt.sinceFail++
	if rt.sinceFail >= notifyRetryEvery {
		rt.sinceFail = 0
		return true // 到了重試輪
	}
	return false
}

// markNotifyResult 登記發送結果：成功清除退避計數；失敗累計次數。
func (d *daemon) markNotifyResult(sensorID string, err error) {
	if err == nil {
		delete(d.notifyRetry, sensorID)
		return
	}
	rt := d.notifyRetry[sensorID]
	if rt == nil {
		rt = &retryState{}
		d.notifyRetry[sensorID] = rt
	}
	rt.fails++
}

func (d *daemon) setupSensors(ctx context.Context) error {
	// 可重入：熱載入時重建。先建在區域變數，全部成功才覆寫 d.sensors——
	// 新檔解析失敗時保留舊感測（T028：與 rules.d 失敗處理一致）。
	sensors := make([]sensorRunner, 0, len(d.sensors))
	if d.sensorMeta == nil {
		d.sensorMeta = map[string]sensorMeta{}
	}
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
		d.sensorMeta[def.ID] = sensorMeta{Scope: def.Scope, Service: def.Service, Cluster: def.Cluster}
		sensors = append(sensors, sensorRunner{
			id:     def.ID,
			kind:   "capacity",
			filter: def.ID,
			poll: func(c context.Context) (budget.Forecast, error) {
				return sensor.Poll(c)
			},
		})
	}
	d.capDefs = defs // 每週摘要的擴容軌跡比對用（§D.5）

	// SLO 預算感測（F1/F2）：以 SLIQuery 為消耗量、預算比為天花板重用同一引擎
	slos, err := specLoadAll(d.cfg.SloDefsDir)
	if err != nil {
		return fmt.Errorf("載入 SLO 定義: %w", err)
	}
	for _, slo := range slos {
		slo := slo
		budgetRatio := (100 - slo.Objective) / 100
		sensors = append(sensors, sensorRunner{
			id:     slo.ID,
			kind:   "budget",
			filter: slo.Service,
			poll: func(c context.Context) (budget.Forecast, error) {
				now := time.Now().UTC()
				window := promdur.Parse(fmt.Sprintf("%dd", slo.WindowDays))
				// 步長自適應：Prometheus range query 上限 11,000 點/序列——
				// 28d 視窗 × 1m 步長 = 40,320 點會被拒（400 bad_data）。
				// 長視窗自動放寬步長，短視窗維持 1m。
				step := window / 10000
				step = step / time.Second * time.Second // 截斷到整秒——Prometheus 不接受 4m1.92s 這類帶小數的 step
				if step < time.Minute {
					step = time.Minute
				}
				res, err := d.src.RangeQuery(c, slo.SLIQuery, now.Add(-window), now, step)
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
					Interval: step,
					Th:       slo.Th,
				})
			},
		})
	}
	d.sensors = sensors // 全部成功才切換
	d.capDefs = defs    // 每週摘要的擴容軌跡比對用（§D.5）
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
				SensorID:        f.ID,
				PredictedAt:     f.Now,
				EtaAggressive:   f.EtaAggressive,
				EtaConservative: f.EtaConservative,
				ActualValue:     f.Value,
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
			// 通知發送失敗保護（T026）：先 Peek 判定、Send 成功才 Commit——
			// 失敗不推進去重狀態，下一輪自動重試同一轉移；
			// 連續失敗 ≥N 輪後每 M 輪才重試一次（避免打掛掉的 API）。
			if !firing && d.dedupe.Peek(f.ID, string(f.State)) && d.allowNotify(f.ID) {
				// 分診轉交（T020）：容量預警先轉 ai-oncall，成功則本地精簡卡；
				// handled=true 時去重已由 triageHandled 路徑 Commit。
				if d.triageHandled(ctx, sr, f) {
					d.dedupe.Commit(f.ID, string(f.State))
				} else {
					msg := formatForecastCard(f)
					if serr := d.notifier.Send(ctx, msg); serr != nil {
						d.markNotifyResult(f.ID, serr)
						rt := d.notifyRetry[f.ID]
						d.log.Error("notify_failed_will_retry",
							"sensor", f.ID, "fails", rt.fails,
							"backoff", rt.fails >= notifyBackoffAfter, "error", serr.Error())
						d.metrics.Set(fmt.Sprintf("sentinel_notify_failures_total{sensor=%q}", f.ID), float64(rt.fails))
					} else {
						d.markNotifyResult(f.ID, nil)
						d.dedupe.Commit(f.ID, string(f.State))
					}
				}
			}
			prev, _ := d.st.GetState(f.ID)
			var lastNotify time.Time
			if prev != nil {
				lastNotify = prev.LastNotifyAt
			}
			if err := d.st.SetState(store.SensorState{
				SensorID:        f.ID,
				State:           string(f.State),
				LastValue:       f.Value,
				LastUtilization: f.Utilization,
				LastNotifyAt:    lastNotify,
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

// Run 主迴圈：setup → 啟動 API/metrics/rules.d 熱載入 → 每隔 interval 執行 runOnePoll → 收尾。
func (d *daemon) Run(ctx context.Context) error {
	if err := d.setupSensors(ctx); err != nil {
		return err
	}

	// rules.d 熱載入：變更後下一輪詢以新目錄生效（重建感測器）；目錄快照供 /api/waste 使用
	catalogLoader := &catalog.Loader{Dir: d.cfg.RulesDir}
	if cat0, _, loadErr := catalogLoader.Load(d.cfg.RulesDir); loadErr != nil {
		d.log.Warn("rules_dir_load_failed", "error", loadErr.Error())
	} else {
		d.lastCatalog = cat0
	}
	if stopWatch, werr := catalogLoader.Watch(ctx, d.cfg.RulesDir,
		func(cat *catalog.Catalog) {
			d.log.Info("rules_hot_reloaded")
			d.lastCatalog = cat
			if err := d.setupSensors(ctx); err != nil {
				d.log.Error("sensors_rebuild_failed", "error", err.Error())
			}
		}); werr != nil {
		d.log.Warn("rules_watch_failed", "error", werr.Error())
	} else {
		defer stopWatch()
	}

	// slo_defs／capacity_defs 熱載入（T028）：與 rules.d 行為一致——
	// 變更後重建感測器（下一輪以新定義生效）；解析失敗保留舊感測。
	// 副作用：重建會重置引擎內部狀態（如解除遲滯計數、前次天花板）。
	for _, dir := range []string{d.cfg.CapacityDefsDir, d.cfg.SloDefsDir} {
		wdir := dir
		stopDefWatch, werr := watch.Dir(ctx, wdir, func() {
			d.log.Info("defs_hot_reloading", "dir", wdir)
			if err := d.setupSensors(ctx); err != nil {
				d.log.Error("sensors_rebuild_failed", "dir", wdir, "error", err.Error())
			}
		})
		if werr != nil {
			d.log.Warn("defs_watch_failed", "dir", wdir, "error", werr.Error())
			continue
		}
		defer stopDefWatch()
	}
	api := &readAPI{d: d}
	apiErr := make(chan error, 1)
	go func() { apiErr <- api.serve(d.cfg.ListenAddr) }()
	metricsErr := make(chan error, 1)
	go func() { metricsErr <- serveMetrics(d.cfg.MetricsAddr, d.metrics) }()

	// waste 候選生命週期（T024）：自 store 還原（重啟不丟 dismiss/resolve），
	// 以獨立 ticker 定期掃描；週期可用 config／WASTE_SCAN_INTERVAL_SEC 覆寫，off 完全停用
	d.tracker = waste.NewLiveTracker()
	if entries, err := d.st.AllWasteEntries(); err == nil {
		d.tracker.Restore(entriesFromStore(entries))
	} else {
		d.log.Warn("waste_entries_load_failed", "error", err.Error())
	}
	var wasteTicker *time.Ticker
	if d.cfg.WasteScanIntervalSec > 0 {
		wasteTicker = time.NewTicker(time.Duration(d.cfg.WasteScanIntervalSec) * time.Second)
		defer wasteTicker.Stop()
	} else {
		d.log.Info("waste_scan_disabled")
	}

	ticker := time.NewTicker(time.Duration(d.cfg.PollIntervalSec) * time.Second)
	defer ticker.Stop()

	// 立即執行第一輪，之後每間格一次
	if err := d.runOnePoll(ctx); err != nil {
		d.log.Info("scheduler_stopped", "reason", err.Error())
		return nil
	}
	d.runWasteScan(ctx) // 啟動即掃一次 waste，之後每 N 小時一輪
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
			d.maybeWeeklyCost(ctx, time.Now().UTC())  // 每週成本摘要（§D.5，同 ISO 週去重）
			d.maybeDailyDigest(ctx, time.Now())       // 每日狀態彙總摘要（T025，同日去重）
			d.maybePrunePredictions(time.Now().UTC()) // predictions retention 清理（T029，每日一次）
		case <-wasteTicker.C: // ticker 為 nil 時永不觸發（已停用）
			d.runWasteScan(ctx)
		}
	}
}

// runWasteScan 執行一輪 waste 掃描（T024）：結果餵 Tracker.Observe——新候選、
// 重提、dismiss 到期復活才直推通知（同資源去重）；逐項 best-effort，
// 單一規則 expr 失敗不拖垮整輪。掃描後把全部條目寫回 SQLite。
func (d *daemon) runWasteScan(ctx context.Context) {
	if d.cfg.WasteScanIntervalSec <= 0 || d.lastCatalog == nil || d.tracker == nil {
		return // 已停用或目錄尚未載入
	}
	sc := &waste.Scanner{Src: d.src, Logger: d.log, Pricer: d.pricer} // pricer nil 時金額欄留空（T027）
	cands, err := sc.Scan(ctx, d.lastCatalog, time.Now().UTC())
	if err != nil {
		d.log.Error("waste_scan_failed", "error", err.Error())
		return
	}
	notified := 0
	for _, c := range cands {
		_, shouldNotify, msg := d.tracker.Observe(c)
		if !shouldNotify {
			continue
		}
		notified++
		if err := d.notifier.Send(ctx, msg); err != nil {
			d.log.Error("waste_notify_failed", "error", err.Error())
		}
	}
	d.persistWasteEntries()
	d.log.Info("waste_scan_done", "candidates", len(cands), "notified", notified)
}

// persistWasteEntries 把 Tracker 全部條目寫回 SQLite（重啟還原 dismiss/resolve 用）。
func (d *daemon) persistWasteEntries() {
	for _, e := range d.tracker.Entries() {
		if err := d.st.SetWasteEntry(entryToStore(e)); err != nil {
			d.log.Error("waste_entry_persist_failed", "resource", e.ResourceID, "error", err.Error())
		}
	}
}

// estimateLines 依映射範本＋感測最新值組出用量列（§D.0：用量取自 capacity/waste 感測）。
func (d *daemon) estimateLines() []cost.UsageLine {
	var lines []cost.UsageLine
	for _, tpl := range d.costMap {
		q := 0.0
		if st, err := d.st.GetState(tpl.Sensor); err == nil && st != nil {
			q = st.LastValue
		}
		lines = append(lines, cost.UsageLine{UsageTemplate: tpl, Quantity: q})
	}
	return lines
}

// maybeWeeklyCost 每輪檢查是否該發每週成本摘要（§D.5）：
// 同一 ISO 週只發一封；需 actual 帳務來源（成長服務需要分服務日花費）。
func (d *daemon) maybeWeeklyCost(ctx context.Context, now time.Time) {
	if d.billingSrc == nil || os.Getenv("WEEKLY_COST_SUMMARY") == "off" {
		return
	}
	const stateID = "__weekly_cost_summary__"
	weekKey := cost.ISOWeekKey(now)
	if prev, _ := d.st.GetState(stateID); prev != nil && string(prev.State) == weekKey {
		return // 本週已發
	}

	thisStart := now.AddDate(0, 0, -7).Truncate(24 * time.Hour)
	prevEnd := thisStart.Add(-time.Second)
	prevStart := now.AddDate(0, 0, -14).Truncate(24 * time.Hour)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	thisWeek, err := d.billingSrc.DailySpend(ctx, billing.Filter{}, thisStart, now)
	if err != nil {
		d.log.Warn("weekly_cost_fetch_failed", "error", err.Error())
		return
	}
	prevWeek, _ := d.billingSrc.DailySpend(ctx, billing.Filter{}, prevStart, prevEnd)
	spends, _ := d.billingSrc.DailySpend(ctx, billing.Filter{}, monthStart, now)

	var mtd float64
	for _, sp := range spends {
		mtd += sp.CostUSD
	}
	rates := cost.EstimateRates(mtd, now.Day(), recentTail(spends, 7))
	eom := cost.ProjectEOM(mtd, now, rates)

	rows := cost.WeeklyTopGrowth(cost.WeeklyGrowthInput{
		ThisWeek: thisWeek, PrevWeek: prevWeek, CapTrend: d.capacityTrend(ctx, now),
	}, 5)
	confirmed := time.Now()
	if len(spends) > 0 {
		confirmed = spends[len(spends)-1].Date
	}
	msg := cost.FormatWeeklySummary(rows, eom, confirmed)
	if err := d.notifier.Send(ctx, msg); err != nil {
		d.log.Error("weekly_summary_send_failed", "error", err.Error())
		return // 未成功不登記，下一輪重試
	}
	lastNotify := now
	_ = d.st.SetState(store.SensorState{SensorID: stateID, State: weekKey, LastNotifyAt: lastNotify})
	d.log.Info("weekly_summary_sent", "week", weekKey)
}

// capacityTrend 近 7 天各容量感測數值變化量（>0 = 擴容中），供成長原因比對。逐項 best-effort。
func (d *daemon) capacityTrend(ctx context.Context, now time.Time) map[string]float64 {
	out := map[string]float64{}
	start := now.AddDate(0, 0, -7)
	for _, def := range d.capDefs {
		res, err := d.src.RangeQuery(ctx, def.Metric.Value, start, now, time.Hour)
		if err != nil || len(res) == 0 || len(res[0].Samples) < 2 {
			continue
		}
		samples := res[0].Samples
		out[def.ID] = samples[len(samples)-1].Value - samples[0].Value
	}
	return out
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
		th := budget.DefaultThresholds()
		if x.Thresholds != nil {
			th = x.Thresholds.Resolve() // 未覆寫欄位用預設；非法組合已在 Load 擋下
		}
		out = append(out, specSLO{ID: x.ID, Service: x.Service,
			SLIQuery: x.SLIQuery, Objective: x.Objective, WindowDays: x.WindowDays, Th: th})
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
