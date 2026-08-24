package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsWhenPathEmpty(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollIntervalSec != 60 || cfg.PrometheusURL != "http://localhost:9090" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.LogFormat != "json" || cfg.DBPath != "sentinel.db" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadDefaultsWhenFileMissing(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollIntervalSec != 60 {
		t.Fatalf("expected default poll interval, got %d", cfg.PollIntervalSec)
	}
}

func TestLoadOverridesOnlyPresentKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sentinel.yaml")
	body := "poll_interval_sec: 30\nprometheus_url: http://p:9090\ntelegram_token: tok\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollIntervalSec != 30 {
		t.Fatalf("poll interval override failed: %d", cfg.PollIntervalSec)
	}
	if cfg.PrometheusURL != "http://p:9090" || cfg.TelegramToken != "tok" {
		t.Fatalf("override failed: %+v", cfg)
	}
	// 未覆寫的鍵維持預設
	if cfg.AlertManagerURL != "http://localhost:9093" || cfg.DBPath != "sentinel.db" {
		t.Fatalf("defaults must survive partial override: %+v", cfg)
	}
}

func TestLoadWasteScanInterval(t *testing.T) {
	dir := t.TempDir()

	// 未寫欄位 → 維持預設（6h）
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WasteScanIntervalSec != 6*3600 {
		t.Fatalf("default waste interval = %d", cfg.WasteScanIntervalSec)
	}

	// 覆寫為自訂週期
	p := filepath.Join(dir, "a.yaml")
	if err := os.WriteFile(p, []byte("waste_scan_interval_sec: 86400\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(p); err != nil {
		t.Fatal(err)
	}
	if cfg.WasteScanIntervalSec != 86400 {
		t.Fatalf("override waste interval = %d", cfg.WasteScanIntervalSec)
	}

	// 明確設 0 = 完全停用
	p2 := filepath.Join(dir, "off.yaml")
	if err := os.WriteFile(p2, []byte("waste_scan_interval_sec: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(p2); err != nil {
		t.Fatal(err)
	}
	if cfg.WasteScanIntervalSec != 0 {
		t.Fatalf("explicit 0 must disable, got %d", cfg.WasteScanIntervalSec)
	}
}

func TestLoadInvalidYAMLIsError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(p, []byte("poll_interval_sec: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	c := defaults()
	c.PollIntervalSec = -1
	if err := c.validate(); err == nil {
		t.Fatal("expected error for negative poll interval")
	}
	c = defaults()
	c.LogFormat = "xml"
	if err := c.validate(); err == nil {
		t.Fatal("expected error for bad log format")
	}
}
