package cost

// estimate.go——成本推估的 estimate 模式（algs/cost-forecast.md §D.0 主路徑）。
//
// 核心約束：維護人員通常沒有 billing IAM，主路徑不能依賴帳務 API：
//
//	推估花費 = Σ(用量指標 × 單價)
//
// 用量取自 capacity/waste 感測值（副本數、磁碟 GB、連線數…），
// 單價由 internal/pricing 公開價目表查得。actual 模式（billing IAM）
// 保留為校準工具：兩者並存，差異可在 UI 對照。

import (
	"context"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"slo-sentinel/internal/pricing"
)

// Pricer 為單價查詢抽象（pricing.Catalog 滿足；測試可替換 fake）。
type Pricer interface {
	Quote(ctx context.Context, f pricing.Family, attrs pricing.Attrs) (pricing.Quote, error)
}

var _ Pricer = (*pricing.Catalog)(nil)

// UsageTemplate 為「感測 → 價目家族」映射範本（來自 cost_map.yaml）。
type UsageTemplate struct {
	Sensor  string         `yaml:"sensor"`  // capacity/waste 感測 id；最新數值即用量
	Service string         `yaml:"service"` // 報表顯示名稱
	Family  pricing.Family `yaml:"family"`  // ec2 | ebs | rds | 阿里雲 module code
	Attrs   pricing.Attrs  `yaml:"attrs"`   // region / instance_type / volume_type / cloud…
}

// LoadCostMap 載入用量映射檔；檔案不存在回傳 nil、nil（estimate 模式停用）。
func LoadCostMap(path string) ([]UsageTemplate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("cost: 讀取 cost_map: %w", err)
	}
	var out []UsageTemplate
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("cost: 解析 cost_map: %w", err)
	}
	for i, t := range out {
		if t.Sensor == "" || t.Family == "" {
			return nil, fmt.Errorf("cost: cost_map 第 %d 項缺少 sensor 或 family", i+1)
		}
	}
	return out, nil
}

// UsageLine 為一列待估花費：範本 × 最新感測值。
type UsageLine struct {
	UsageTemplate
	Quantity float64 // 用量數值（感測 LastValue）
}

// EstimatedLine 為單列估算結果。
type EstimatedLine struct {
	Service   string  `json:"service"`
	Sensor    string  `json:"sensor"`
	Quantity  float64 `json:"quantity"`
	UnitPrice float64 `json:"unit_price"`
	Currency  string  `json:"currency"`
	Unit      string  `json:"unit"`
	Cost      float64 `json:"cost"` // Quantity × UnitPrice
	Stale     bool    `json:"stale"`
	Err       string  `json:"err,omitempty"` // 查價失敗不拖垮整份報表
}

// EstimateResult 為整份估算報告。
type EstimateResult struct {
	Lines    []EstimatedLine `json:"lines"`
	Total    float64         `json:"total"`
	Currency string          `json:"currency"`
	Stale    bool            `json:"stale"` // 任一列使用過期快取
}

// EstimateSpend 執行 Σ(用量 × 單價)。單列查價失敗記入 Err、不計入總額；
// 幣別如實標注（§D.4：v1 以單一幣別為前提，混合幣別時 Currency 標 "mixed"）。
func EstimateSpend(ctx context.Context, lines []UsageLine, pricer Pricer) *EstimateResult {
	res := &EstimateResult{Currency: ""}
	for _, ln := range lines {
		row := EstimatedLine{
			Service:  ln.Service,
			Sensor:   ln.Sensor,
			Quantity: ln.Quantity,
		}
		if ln.Service == "" {
			row.Service = ln.Sensor
		}
		q, err := pricer.Quote(ctx, ln.Family, ln.Attrs)
		switch {
		case err != nil:
			row.Err = err.Error()
		default:
			row.UnitPrice = q.UnitPrice
			row.Currency = q.Currency
			row.Unit = q.Unit
			row.Cost = ln.Quantity * q.UnitPrice
			row.Stale = q.Stale
			res.Total += row.Cost
			res.Stale = res.Stale || q.Stale
			if res.Currency == "" {
				res.Currency = q.Currency
			} else if res.Currency != q.Currency && res.Currency != "mixed" {
				res.Currency = "mixed" // §D.4 不在此換算，如實標注
			}
		}
		res.Lines = append(res.Lines, row)
	}
	return res
}

// ActualVsEstimate 對照 actual 模式 MTD 與 estimate 總額（校準用），
// 回傳差額（actual − estimate）與相對百分比。
type ActualVsEstimate struct {
	ActualMTD float64  `json:"actual_mtd"`
	Estimate  float64  `json:"estimate"`
	Delta     float64  `json:"delta"`     // actual − estimate
	DeltaPct  *float64 `json:"delta_pct"` // Delta / Estimate × 100；estimate=0 時 nil
}

func CompareActualVsEstimate(actualMTD float64, est *EstimateResult) ActualVsEstimate {
	out := ActualVsEstimate{ActualMTD: actualMTD}
	if est != nil {
		out.Estimate = est.Total
	}
	out.Delta = out.ActualMTD - out.Estimate
	if out.Estimate > 1e-9 {
		p := out.Delta / out.Estimate * 100
		out.DeltaPct = &p
	}
	return out
}
