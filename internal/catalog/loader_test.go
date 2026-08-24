package catalog

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const validRules = `groups:
- name: capacity.test
  rules:
  - record: sentinel_capacity_used
    expr: up{job="api"}
  - alert: CapacityEtaWarningBaseline
    expr: predict_linear(sentinel_capacity_used[6h], 72*3600) >= 1
    for: 10m
    labels:
      severity: warning
      team: platform
    annotations:
      summary: "預計觸頂"

- name: budget.sloth-generated
  rules:
  - record: slo:sli_error:ratio_rate5m
    expr: sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m]))
    labels:
      sloth_service: api
      sloth_slo: availability

- name: waste.cloud.elb
  rules:
  - alert: WasteElbZeroTraffic
    expr: max_over_time(aws_elb_request_count_sum[14d]) <= 10
    for: 14d
    labels:
      sentinel_kind: waste
      scope: cloud
      notify_every: 7d
    annotations:
      sentinel_sensor: cloud.elb.zero-traffic
      summary: "零流量 ELB"

- name: unrelated.alerts
  rules:
  - alert: SomethingElse
    expr: up == 0
`

const badYAML = "groups: [unclosed"

func writeRules(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"rules.yaml":  validRules,
		"broken.yaml": badYAML,
		"readme.txt":  "非 yaml 檔應被略過",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadClassifiesAndQuarantines(t *testing.T) {
	dir := t.TempDir()
	writeRules(t, dir)

	l := &Loader{Dir: dir}
	cat, quarantined, err := l.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 隔離：壞 yaml 被隔離，txt 被略過（不出現也不隔離）
	if len(quarantined) != 1 || filepath.Base(quarantined[0].Path) != "broken.yaml" {
		t.Fatalf("quarantine = %+v", quarantined)
	}

	budgets := len(cat.RulesOfKind(KindBudget))
	capacities := len(cat.RulesOfKind(KindCapacity))
	wastes := len(cat.RulesOfKind(KindWaste))
	none := 0

	// KindNone 的規則存在但不路由到引擎：
	// CapacityEtaWarningBaseline（AM 路由的告警）與 SomethingElse（無關警報）
	total := 0
	for _, g := range cat.Groups {
		total += len(g.Rules)
	}
	none = total - budgets - capacities - wastes

	if budgets != 1 || wastes != 1 || capacities != 1 || none != 2 {
		t.Fatalf("classification wrong: budget=%d cap=%d waste=%d none=%d",
			budgets, capacities, wastes, none)
	}

	// waste 條目的擴充欄位解析
	w := cat.RulesOfKind(KindWaste)[0]
	if w.NotifyEvery() != 7*24*time.Hour {
		t.Fatalf("notify_every = %v", w.NotifyEvery())
	}
	if w.Annotations["sentinel_sensor"] != "cloud.elb.zero-traffic" {
		t.Fatalf("sensor id lost")
	}
	if w.ID() != "cloud.elb.zero-traffic" {
		t.Fatalf("ID = %s", w.ID())
	}
}

func TestLoadMissingDirIsError(t *testing.T) {
	l := &Loader{Dir: "/nonexistent"}
	if _, _, err := l.Load("/nonexistent"); err == nil {
		t.Fatal("expected error for missing dir")
	}
}

func TestWatchReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	writeRules(t, dir)

	l := &Loader{Dir: dir}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reloaded := make(chan *Catalog, 4)
	stop, err := l.Watch(ctx, dir, func(c *Catalog) { reloaded <- c })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// 寫入新檔案 → 觸發熱載入
	newFile := filepath.Join(dir, "extra.yaml")
	body := "groups:\n- name: extra\n  rules:\n  - record: sentinel_extra\n    expr: up\n"
	if err := os.WriteFile(newFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case cat := <-reloaded:
		found := false
		for _, g := range cat.Groups {
			if g.Name == "extra" {
				found = true
			}
		}
		if !found {
			t.Fatal("reloaded catalog lacks new group")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hot reload did not fire within 3s")
	}

	stop() // 二次呼叫不得 panic
	stop()
}
