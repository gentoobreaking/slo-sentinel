package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
