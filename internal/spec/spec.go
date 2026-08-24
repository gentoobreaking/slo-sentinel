// Package spec 解析使用者 SLO 定義檔（F1：YAML 宣告式定義，相容 OpenSLO 子集）。
package spec

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SLO 為單一服務等級目標定義。
type SLO struct {
	ID          string  `yaml:"id"`
	Service     string  `yaml:"service"`
	Description string  `yaml:"description"`
	SLIQuery    string  `yaml:"sli_query"`   // 錯誤率（或不良事件比）PromQL，值域 [0,1]
	Objective   float64 `yaml:"objective"`   // 目標百分比，如 99.9
	WindowDays  int     `yaml:"window_days"` // 計算視窗天數，預設 28
	BudgetUSD   float64 `yaml:"budget_usd"`  // （選配）月度預算天花板——供 cost 家族使用
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
