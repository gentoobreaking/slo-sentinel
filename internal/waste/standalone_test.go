package waste

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"slo-sentinel/internal/catalog"
)

func TestStandaloneRulesContainThreeFamilies(t *testing.T) {
	p := &StandaloneProvider{}
	rules := p.Rules()
	for _, want := range []string{"WasteServerZombie", "WasteGhostService", "WasteDiskOrphan", "30d"} {
		if !contains(rules, want) {
			t.Fatalf("rules 缺少 %q", want)
		}
	}
}

func TestScanStandalone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "onprem.yaml"), []byte((&StandaloneProvider{}).Rules()), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &catalog.Loader{Dir: dir}
	cat, _, err := l.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	vals := fourteenOnes()
	p := &StandaloneProvider{Src: &fakeWaste{vals: vals}}
	cands, err := p.ScanStandalone(context.Background(), cat, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// 三個家族的 expr 都成立 → 各產生一個候選
	if len(cands) != 3 {
		t.Fatalf("cands = %d (%+v)", len(cands), cands)
	}
}
