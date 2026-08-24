package waste

// Package waste 實作瘦身與閒置偵測（F14）。
//
// 架構：感測條目由 catalog 的 waste 類規則提供（Prometheus rules 格式，
// 詳見 algs/waste-detection.md §8.8）；本套件對每條規則的 expr 做持續成立掃描——
// 規則運算式在 Prometheus 端求值（如 max_over_time[14d] <= 10），
// sentinel 只負責查詢結果序列並判定「連續成立天數」。

import (
	"context"
	"fmt"
	"time"

	"slo-sentinel/internal/catalog"
	"slo-sentinel/internal/query"
)

// Candidate 為一個瘦身影/殭屍資源候選。
type Candidate struct {
	SensorID   string        // algs §8.8：sentinel_sensor 或 alertname
	AlertName  string
	Reason     string        // annotations.summary
	Window     time.Duration // 判定視窗（如 14d）
	IdleDays   float64       // 連續成立天數
	WastedCost float64       // 累積浪費估算（USD；無單價資料時為 0）
	Renotify   time.Duration // 重提週期（labels notify_every）
	Labels     map[string]string
}

// Scanner 對目錄中的 waste 類規則做每日批次掃描。
type Scanner struct {
	Src    query.Source
	Logger interface {
		Warn(msg string, args ...any)
		Info(msg string, args ...any)
	}
}

// Scan 對每條 waste 警告規則查詢其 expr 的歷史序列：
// 若整個「for」視窗內所有樣本皆為真（≥0.5），即判定候選成立。
func (sc *Scanner) Scan(ctx context.Context, cat *catalog.Catalog, now time.Time) ([]Candidate, error) {
	var out []Candidate
	for _, r := range cat.RulesOfKind(catalog.KindWaste) {
		if r.Alert == "" {
			continue // 只掃 alert 類；record 由引擎維護
		}
		window := r.For
		if window <= 0 {
			window = 14 * 24 * time.Hour // 預設視窗（algs §8.8 慣例）
		}
		res, err := sc.Src.RangeQuery(ctx, r.Expr, now.Add(-window), now, 24*time.Hour)
		if err != nil {
			return nil, fmt.Errorf("waste %s: %w", r.ID(), err)
		}
		if len(res) == 0 || len(res[0].Samples) == 0 {
			continue
		}
		samples := res[0].Samples
		allTrue := true
		for _, smp := range samples {
			if smp.Value < 0.5 {
				allTrue = false
				break
			}
		}
		if !allTrue {
			continue
		}
		out = append(out, Candidate{
			SensorID:  r.ID(),
			AlertName: r.Alert,
			Reason:    r.Annotations["summary"],
			Window:    window,
			IdleDays:  window.Hours() / 24,
			Renotify:  r.NotifyEvery(),
			Labels:    r.Labels,
		})
	}
	return out, nil
}
