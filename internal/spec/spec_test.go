package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"slo-sentinel/internal/budget"

	"gopkg.in/yaml.v3"
)

const validYAML = `slos:
  - id: api-availability
    service: api
    description: API 可用性
    sli_query: 'sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m]))'
    objective: 99.9
    window_days: 28
`

func TestLoadValid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "api.yaml"), []byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	slos, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(slos) != 1 {
		t.Fatalf("slos = %d", len(slos))
	}
	s := slos[0]
	if s.ID != "api-availability" || s.Objective != 99.9 || s.WindowDays != 28 {
		t.Fatalf("unexpected slo: %+v", s)
	}
}

func TestLoadMissingDirReturnsNil(t *testing.T) {
	slos, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil || slos != nil {
		t.Fatalf("expected nil,nil got %v,%v", slos, err)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		errSub string
	}{
		{"缺 id", "slos:\n  - sli_query: up\n    objective: 99.9\n", "缺少 id"},
		{"缺查詢", "slos:\n  - id: x\n    objective: 99.9\n", "sli_query"},
		{"objective 超界", "slos:\n  - id: x\n    sli_query: up\n    objective: 100\n", "objective"},
		{"window 負數", "slos:\n  - id: x\n    sli_query: up\n    objective: 99.9\n    window_days: -5\n", "window_days"},
		{"最小合法定義", "slos:\n  - id: x\n    sli_query: up\n    objective: 99\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "x.yaml")
			if err := os.WriteFile(p, []byte(c.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if c.errSub == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.errSub) {
				t.Fatalf("err = %v, want contains %q", err, c.errSub)
			}
		})
	}
}

// ---- T023：slo_defs thresholds 覆寫 ----

func writeSLO(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "slo.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadThresholdsOverride(t *testing.T) {
	dir := writeSLO(t, `slos:
  - id: api-availability
    sli_query: 'rate(err[5m])'
    objective: 99.9
    thresholds:
      warn_eta: 48h
      crit_eta: 4h
      soft_ratio: 0.70
      crit_ratio: 0.90
`)
	slos, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	th := slos[0].Thresholds.Resolve()
	want := budget.Thresholds{WarnEta: 48 * time.Hour, CritEta: 4 * time.Hour,
		SoftRatio: 0.70, CritRatio: 0.90}
	if th != want {
		t.Fatalf("th = %+v, want %+v", th, want)
	}
}

func TestLoadThresholdsAbsentUsesDefaults(t *testing.T) {
	// 缺省 thresholds 不影響既有檔案（回歸）：解析成功且 Resolve = 全預設
	slos, err := Load(writeSLO(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if slos[0].Thresholds != nil {
		t.Fatalf("thresholds should be nil when absent")
	}
	if got := slos[0].Thresholds.Resolve(); got != budget.DefaultThresholds() {
		t.Fatalf("resolve = %+v, want defaults", got)
	}
}

func TestLoadThresholdsPartialOverride(t *testing.T) {
	// 只寫部分欄位，其餘維持預設
	dir := writeSLO(t, `slos:
  - id: api-availability
    sli_query: 'rate(err[5m])'
    objective: 99.9
    thresholds:
      crit_eta: 2h
`)
	slos, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	th := slos[0].Thresholds.Resolve()
	def := budget.DefaultThresholds()
	if th.CritEta != 2*time.Hour || th.WarnEta != def.WarnEta ||
		th.SoftRatio != def.SoftRatio || th.CritRatio != def.CritRatio {
		t.Fatalf("partial override = %+v", th)
	}
}

func TestLoadInvalidThresholdCombinations(t *testing.T) {
	cases := map[string]string{
		"soft ≥ crit":         "soft_ratio: 0.95\n        crit_ratio: 0.90",
		"warn_eta ≤ crit_eta": "warn_eta: 4h\n        crit_eta: 6h",
	}
	for name, body := range cases {
		yaml := "slos:\n  - id: api-availability\n    sli_query: 'rate(err[5m])'\n    objective: 99.9\n    thresholds:\n        " + body + "\n"
		if _, err := Load(writeSLO(t, yaml)); err == nil {
			t.Fatalf("%s must be rejected at load time", name)
		} else if !strings.Contains(err.Error(), "thresholds") {
			t.Fatalf("%s error should mention thresholds, got %v", name, err)
		}
	}
}

// T034：範本庫檔（.example 副檔名）不可被載入。
func TestLoadIgnoresExampleTemplates(t *testing.T) {
	dir := writeSLO(t, validYAML)
	for _, name := range []string{"TEMPLATE.http-service.yaml.example", "TEMPLATE.k8s-cloud.yaml.example"} {
		if err := os.WriteFile(filepath.Join(dir, name),
			[]byte("slos:\n  - id: should-not-load\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	slos, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range slos {
		if s.ID == "should-not-load" {
			t.Fatal(".example file must not be loaded")
		}
	}
	if len(slos) != 1 || slos[0].ID != "api-availability" {
		t.Fatalf("expected only validYAML slo, got %v", ids(slos))
	}
}

func ids(slos []SLO) []string {
	var out []string
	for _, s := range slos {
		out = append(out, s.ID)
	}
	return out
}

// T035：範本庫去註解後必須是合法 YAML（語法防呆——範本會被使用者複製啟用）。
func TestTemplateFilesAreValidYAML(t *testing.T) {
	for _, f := range []string{"TEMPLATE.http-service.yaml.example", "TEMPLATE.k8s-cloud.yaml.example"} {
		raw, err := os.ReadFile("../../slo_defs/" + f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var code []string
		for _, line := range strings.Split(string(raw), "\n") {
			t := strings.TrimSpace(line)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			code = append(code, line)
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(strings.Join(code, "\n")), &m); err != nil {
			t.Fatalf("%s 去註解後非合法 YAML：%v", f, err)
		}
	}
}
