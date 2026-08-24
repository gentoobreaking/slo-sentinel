package capacity

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"slo-sentinel/internal/budget"
	"slo-sentinel/internal/promdur"
)

// PromDuration 支援 Prometheus 風格單位（如 14d、6h）的 YAML 時長型別。
type PromDuration struct {
	time.Duration
}

// UnmarshalYAML 實現 yaml.v3 自訂解碼。
func (d *PromDuration) UnmarshalYAML(node *yaml.Node) error {
	d.Duration = promdur.Parse(node.Value)
	if d.Duration <= 0 && node.Value != "" && node.Value != "0" {
		return fmt.Errorf("無法解析時長 %q", node.Value)
	}
	return nil
}

// Def 為單一容量感測定義。
type Def struct {
	ID     string `yaml:"id"`
	Desc   string `yaml:"desc"`
	Metric struct {
		Value   string `yaml:"value"`   // 消耗量 m(t)
		Ceiling string `yaml:"ceiling"` // 天花板 C(t)（可為動態查詢）
	} `yaml:"metric"`
	Horizons   []PromDuration     `yaml:"horizons"`   // 空 → budget.DefaultHorizons
	Thresholds *ThresholdsOverlay `yaml:"thresholds"` // nil → 全預設
}

// HorizonDurations 轉出 budget 引擎用的 []time.Duration。
func (d *Def) HorizonDurations() []time.Duration {
	if len(d.Horizons) == 0 {
		return budget.DefaultHorizons
	}
	out := make([]time.Duration, 0, len(d.Horizons))
	for _, h := range d.Horizons {
		out = append(out, h.Duration)
	}
	return out
}

// ThresholdsOverlay 允許部分覆寫門檻。
type ThresholdsOverlay struct {
	WarnEta   *string  `yaml:"warn_eta"` // 如 "72h"
	CritEta   *string  `yaml:"crit_eta"` // 如 "6h"
	SoftRatio *float64 `yaml:"soft_ratio"`
	CritRatio *float64 `yaml:"crit_ratio"`
}

// Thresholds 轉為 budget.Thresholds（未覆寫處用預設值）。
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

type defsFile struct {
	Sensors []Def `yaml:"sensors"`
}

// LoadDefs 自目錄（或單一檔案）載入所有容量感測定義。
func LoadDefs(dirOrFile string) ([]Def, error) {
	info, err := os.Stat(dirOrFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var paths []string
	if info.IsDir() {
		entries, err := os.ReadDir(dirOrFile)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			ext := filepath.Ext(e.Name())
			if ext == ".yaml" || ext == ".yml" {
				paths = append(paths, filepath.Join(dirOrFile, e.Name()))
			}
		}
	} else {
		paths = append(paths, dirOrFile)
	}

	var out []Def
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		var df defsFile
		if err := yaml.Unmarshal(b, &df); err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		for i, d := range df.Sensors {
			if d.ID == "" {
				return nil, fmt.Errorf("%s: sensors[%d] 缺少 id", p, i)
			}
			if d.Metric.Value == "" || d.Metric.Ceiling == "" {
				return nil, fmt.Errorf("%s: %s 缺少 metric.value / metric.ceiling", p, d.ID)
			}
			out = append(out, d)
		}
	}
	return out, nil
}
