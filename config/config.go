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
	ListenAddr      string `yaml:"listen_addr"`  // 唯讀 JSON API；預設僅綁本機
	MetricsAddr     string `yaml:"metrics_addr"` // Prometheus /metrics；預設僅綁本機
	LogFormat       string `yaml:"log_format"`   // json | text
	// waste 掃描週期（秒，T024）；0 = 完全停用。可用環境變數 WASTE_SCAN_INTERVAL_SEC 覆寫（off/0 停用）
	WasteScanIntervalSec int `yaml:"waste_scan_interval_sec"`
	// 每日摘要發送時刻（T025），本地時區 HH:MM；空字串 = 停用。
	// 環境變數 DAILY_DIGEST=off 停用；DAILY_DIGEST=HH:MM 覆寫時刻
	DailyDigestTime string `yaml:"daily_digest_time"`
	// predictions 保留天數（T029）；0 = 停用清理。預設 90 天
	PredictionsRetentionDays int `yaml:"predictions_retention_days"`
}

func defaults() Config {
	return Config{
		PollIntervalSec:      60,
		PrometheusURL:        "http://localhost:9090",
		AlertManagerURL:      "http://localhost:9093",
		RulesDir:             "rules.d",
		SloDefsDir:           "slo_defs",
		CapacityDefsDir:      "capacity_defs",
		DBPath:               "sentinel.db",
		ListenAddr:           "127.0.0.1:9099",
		MetricsAddr:          "127.0.0.1:9102",
		LogFormat:            "json",
		WasteScanIntervalSec: 6 * 3600, // 預設每 6 小時掃一次 waste（建議 6h～1d）
		DailyDigestTime:      "09:00",  // 每日摘要預設每日 09:00 本地時區

		PredictionsRetentionDays: 90, // predictions 預設保留 90 天（T029）
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
	overrideWasteInterval(&cfg, raw)
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
	setStr(&base.ListenAddr, over.ListenAddr)
	setStr(&base.MetricsAddr, over.MetricsAddr)
	setStr(&base.LogFormat, over.LogFormat)
	setStr(&base.DailyDigestTime, over.DailyDigestTime)
	if over.PredictionsRetentionDays > 0 {
		base.PredictionsRetentionDays = over.PredictionsRetentionDays
	}
	if over.WasteScanIntervalSec > 0 {
		base.WasteScanIntervalSec = over.WasteScanIntervalSec
	}
}

func setStr(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// overrideWasteInterval 處理 waste_scan_interval_sec 的「明確設 0 = 停用」：
// YAML 未寫此欄時維持預設值；寫了（含 0）就以檔案值為準。
func overrideWasteInterval(cfg *Config, raw map[string]any) {
	v, ok := raw["waste_scan_interval_sec"]
	if !ok {
		return
	}
	if n, ok := toInt(v); ok && n >= 0 {
		cfg.WasteScanIntervalSec = n
	}
	// predictions_retention_days 同理：明確寫 0 = 停用清理
	if v, ok := raw["predictions_retention_days"]; ok {
		if n, ok := toInt(v); ok && n >= 0 {
			cfg.PredictionsRetentionDays = n
		}
	}
}

func toInt(v any) (int, bool) {
	n, ok := v.(int)
	return n, ok
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
	if c.WasteScanIntervalSec < 0 {
		return fmt.Errorf("waste_scan_interval_sec 不可為負，得到 %d", c.WasteScanIntervalSec)
	}
	if c.PredictionsRetentionDays < 0 {
		return fmt.Errorf("predictions_retention_days 不可為負，得到 %d", c.PredictionsRetentionDays)
	}
	return nil
}
