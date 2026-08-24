package waste

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"slo-sentinel/internal/catalog"
)

const k8sRules = `groups:
- name: waste.k8s.over-requested
  rules:
  - alert: WasteK8sOverRequested
    expr: sentinel_k8s_request_ratio_p95 < 0.30
    for: 14d
    labels:
      sentinel_kind: waste
      scope: k8s
      notify_every: 14d
      sentinel_exclude_namespaces: "kube-system,openshift-*"
    annotations:
      sentinel_sensor: k8s.over-requested
`

func TestK8sProviderRulesContainFourFamilies(t *testing.T) {
	k := &K8sProvider{}
	rules := k.Rules()
	for _, want := range []string{"over-requested", "pvc-unattached", "kube-system", "14d"} {
		if !contains(rules, want) {
			t.Fatalf("rules 缺少 %q", want)
		}
	}
}

func TestScanK8sWithScopeFilter(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "k8s.yaml"), []byte(k8sRules), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &catalog.Loader{Dir: dir}
	cat, _, err := l.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	vals := fourteenOnes() // 條件持續成立
	k := &K8sProvider{Src: &fakeWaste{vals: vals}}
	cands, err := k.ScanK8s(context.Background(), cat, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].SensorID != "k8s.over-requested" {
		t.Fatalf("cands = %+v", cands)
	}
	// 排除命名空間清單有被解析進 Labels
	excl := cands[0].Labels["sentinel_exclude_namespaces"]
	if !contains(excl, "openshift-") {
		t.Fatalf("exclude namespaces lost: %q", excl)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
