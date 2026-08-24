// Package spec 解析使用者 SLO 定義檔（F1：YAML 宣告式定義，相容 OpenSLO 子集）。
package spec

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
	"slo-sentinel/internal/budget"
	"slo-sentinel/internal/promdur"
)

// SLO 為單一服務等級目標定義。
type SLO struct {
	ID          string             `yaml:"id"`
	Service     string             `yaml:"service"`
	Description string             `yaml:"description"`
	SLIQuery    string             `yaml:"sli_query"`   // 錯誤率（或不良事件比）PromQL，值域 [0,1]
	Objective   float64            `yaml:"objective"`   // 目標百分比，如 99.9
	WindowDays  int                `yaml:"window_days"` // 計算視窗天數，預設 28
	BudgetUSD   float64            `yaml:"budget_usd"`  // （選配）月度預算天花板——供 cost 家族使用
	Thresholds  *ThresholdsOverlay `yaml:"thresholds"`  // （選配）觸發門檻覆寫（T023）；nil → 全預設
}

// ThresholdsOverlay 允許部分覆寫 SLO 感測門檻（比照 capacity 家族；T023）。
type ThresholdsOverlay struct {
	WarnEta   *string  `yaml:"warn_eta"` // 如 "48h"
	CritEta   *string  `yaml:"crit_eta"` // 如 "4h"
	SoftRatio *float64 `yaml:"soft_ratio"`
	CritRatio *float64 `yaml:"crit_ratio"`
}

// Resolve 轉為 budget.Thresholds（未覆寫處用預設值）。
func (o *ThresholdsOverlay) Resolve() budget.Thresholds {
	th := budget.DefaultThresholds()
	if o == nil {
		return th
	}
	if o.WarnEta != nil {
		if d := promdur.Parse(*o.WarnEta); d > 0 {
			th.WarnEta = d
		}
	}
	if o.CritEta != nil {
		if d := promdur.Parse(*o.CritEta); d > 0 {
			th.CritEta = d
		}
	}
	if o.SoftRatio != nil {
		th.SoftRatio = *o.SoftRatio
	}
	if o.CritRatio != nil {
		th.CritRatio = *o.CritRatio
	}
	return th
}

func (s SLO) Validate() error {
	if s.ID == "" {
		return fmt.Errorf("缺少 id")
	}
	if s.SLIQuery == "" {
		return fmt.Errorf("%s: 缺少 sli_query", s.ID)
	}
	if s.Objective <= 0 || s.Objective >= 100 {
		return fmt.Errorf("%s: objective 必須介於 (0,100)，得到 %v", s.ID, s.Objective)
	}
	if s.WindowDays < 0 {
		return fmt.Errorf("%s: window_days 不可為負，得到 %d", s.ID, s.WindowDays)
	}
	// thresholds 非法組合啟動即報錯（T023：soft ≥ crit、warn_eta ≤ crit_eta）
	if s.Thresholds != nil {
		if err := s.Thresholds.Resolve().Validate(); err != nil {
			return fmt.Errorf("%s: thresholds: %w", s.ID, err)
		}
	}
	return nil
}

type defsFile struct {
	SLOs []SLO `yaml:"slos"`
}

// Load 自目錄（或單一檔案）載入所有 SLO 定義並逐條驗證。
// 目錄不存在回傳空清單不視為錯誤；個別檔案格式錯誤指名回傳。
func Load(path string) ([]SLO, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			ext := filepath.Ext(e.Name())
			if ext == ".yaml" || ext == ".yml" {
				paths = append(paths, filepath.Join(path, e.Name()))
			}
		}
	} else {
		paths = append(paths, path)
	}

	var out []SLO
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		var df defsFile
		if err := yaml.Unmarshal(b, &df); err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		for i, slo := range df.SLOs {
			if err := slo.Validate(); err != nil {
				return nil, fmt.Errorf("%s: sensors[%d]: %w", p, i, err)
			}
			if slo.WindowDays == 0 {
				slo.WindowDays = 28
			}
			out = append(out, slo)
		}
	}
	return out, nil
}
