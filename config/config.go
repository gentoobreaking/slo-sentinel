// Package config 載入與驗證 sentinel 全域設定。
// 預設值 → YAML 覆寫；缺欄位不視為錯誤（沿用預設），格式錯誤才回傳 error。
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config 為 sentinel daemon 與 UI 的全域設定。
type Config struct {
	PollIntervalSec int    `yaml:"poll_interval_sec"` // 主迴圈輪詢間隔（秒）
	PrometheusURL   string `yaml:"prometheus_url"`
	AlertManagerURL string `yaml:"alertmanager_url"`
	TelegramToken   string `yaml:"telegram_token"`
	RulesDir        string `yaml:"rules_dir"`
	SloDefsDir      string `yaml:"slo_defs_dir"`
	CapacityDefsDir string `yaml:"capacity_defs_dir"`
	DBPath          string `yaml:"db_path"`
	LogFormat       string `yaml:"log_format"` // json | text
}

func defaults() Config {
	return Config{
		PollIntervalSec: 60,
		PrometheusURL:   "http://localhost:9090",
		AlertManagerURL: "http://localhost:9093",
		RulesDir:        "rules.d",
		SloDefsDir:      "slo_defs",
		CapacityDefsDir: "capacity_defs",
		DBPath:          "sentinel.db",
		LogFormat:       "json",
	}
}

// Load 自 path 讀取 YAML 設定並套用到預設值上。
// path 為空或檔案不存在時回傳純預設值（方便首次啟動）；
// 檔案存在但內容非合法 YAML 時回傳 error。
func Load(path string) (Config, error) {
	cfg := defaults()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	var over Config
	if err := yaml.Unmarshal(b, &over); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	apply(&cfg, over)
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func apply(base *Config, over Config) {
	if over.PollIntervalSec > 0 {
		base.PollIntervalSec = over.PollIntervalSec
	}
	setStr(&base.PrometheusURL, over.PrometheusURL)
	setStr(&base.AlertManagerURL, over.AlertManagerURL)
	setStr(&base.TelegramToken, over.TelegramToken)
	setStr(&base.RulesDir, over.RulesDir)
	setStr(&base.SloDefsDir, over.SloDefsDir)
	setStr(&base.CapacityDefsDir, over.CapacityDefsDir)
	setStr(&base.DBPath, over.DBPath)
	setStr(&base.LogFormat, over.LogFormat)
}

func setStr(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func (c Config) validate() error {
	if c.PollIntervalSec <= 0 {
		return fmt.Errorf("poll_interval_sec 必須 > 0，得到 %d", c.PollIntervalSec)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("log_format 必須是 json 或 text，得到 %q", c.LogFormat)
	}
	return nil
}
