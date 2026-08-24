package main

// defs_hot_reload_test.go（T028）：defs 熱載入的失敗安全——
// 新檔解析失敗時 setupSensors 保留舊感測，不中斷運作。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"slo-sentinel/config"
)

func TestSetupSensorsKeepsOldOnBadDefs(t *testing.T) {
	dir := t.TempDir()
	capDir := filepath.Join(dir, "capacity_defs")
	sloDir := filepath.Join(dir, "slo_defs")
	for _, d := range []string{capDir, sloDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	goodDef := "sensors:\n  - id: data-disk\n    metric:\n      value: 'disk_used'\n      ceiling: 'disk_total'\n"
	if err := os.WriteFile(filepath.Join(capDir, "disk.yaml"), []byte(goodDef), 0o600); err != nil {
		t.Fatal(err)
	}

	d, _ := setupCapacityDaemon(t, &fakeIdleSource{}, &captureNotifier{}, amNoAlerts(t))
	d.cfg = config.Config{
		PollIntervalSec:      60,
		CapacityDefsDir:      capDir,
		SloDefsDir:           sloDir,
		RulesDir:             filepath.Join(dir, "rules"),
		LogFormat:            "text",
		WasteScanIntervalSec: 6 * 3600,
	}
	if err := d.setupSensors(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(d.sensors)
	if before == 0 {
		t.Fatal("expected initial sensors")
	}

	// 寫入解析失敗的新檔 → 重建應報錯但保留舊感測
	badYAML := "sensors:\n  - id: broken\n   metric: [oops\n"
	if err := os.WriteFile(filepath.Join(capDir, "bad.yaml"), []byte(badYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.setupSensors(context.Background()); err == nil {
		t.Fatal("bad defs must surface an error")
	}
	if len(d.sensors) != before {
		t.Fatalf("old sensors must be kept on reload failure: before=%d after=%d", before, len(d.sensors))
	}

	// 修好後（移除壞檔）重建成功且數量一致
	if err := os.Remove(filepath.Join(capDir, "bad.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := d.setupSensors(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.sensors) != before {
		t.Fatalf("healthy rebuild mismatch: %d vs %d", len(d.sensors), before)
	}
}
