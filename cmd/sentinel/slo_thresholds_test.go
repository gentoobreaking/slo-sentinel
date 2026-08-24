package main

// slo_thresholds_test.go（T023）：daemon 組裝處把覆寫後門檻傳給引擎。

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"slo-sentinel/internal/budget"
)

func TestSpecLoadAllResolvesThresholds(t *testing.T) {
	dir := t.TempDir()
	body := `slos:
  - id: slo-a
    service: api
    sli_query: 'rate(err[5m])'
    objective: 99.9
    thresholds:
      warn_eta: 48h
      soft_ratio: 0.70
`
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	slos, err := specLoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	def := budget.DefaultThresholds()
	th := slos[0].Th
	if th.WarnEta != 48*time.Hour || th.SoftRatio != 0.70 {
		t.Fatalf("override not applied: %+v", th)
	}
	// 未覆寫欄位維持預設
	if th.CritEta != def.CritEta || th.CritRatio != def.CritRatio {
		t.Fatalf("unset fields must stay default: %+v", th)
	}

	// 未設定 thresholds → 行為與現狀一致（全預設）
	dir2 := t.TempDir()
	body2 := `slos:
  - id: slo-b
    service: api
    sli_query: 'rate(err[5m])'
    objective: 99.9
`
	if err := os.WriteFile(filepath.Join(dir2, "b.yaml"), []byte(body2), 0o600); err != nil {
		t.Fatal(err)
	}
	slos2, err := specLoadAll(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if slos2[0].Th != budget.DefaultThresholds() {
		t.Fatalf("regression: without thresholds must use defaults, got %+v", slos2[0].Th)
	}
}
