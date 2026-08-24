package main

// gen_test.go（T036）：sentinel-gen 各子命令的離線單元測試。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLLM 依呼叫次序回傳預設回應的 OpenAI 相容假端點。
type fakeLLM struct {
	responses []string
	calls     int
}

func (f *fakeLLM) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		i := f.calls
		f.calls++
		if i >= len(f.responses) {
			i = len(f.responses) - 1
		}
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": f.responses[i]}},
			},
			"usage": map[string]any{"total_tokens": 10},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func TestExtractYAML(t *testing.T) {
	fenced := "說明文字\n```yaml\nsensors:\n  - id: a\n```\n結尾"
	if got := extractYAML(fenced); got != "sensors:\n  - id: a" {
		t.Fatalf("extract = %q", got)
	}
	if got := extractYAML("sensors:\n- id: b"); got != "sensors:\n- id: b" {
		t.Fatalf("no-fence passthrough broken: %q", got)
	}
}

// 驗收：三家族靜態驗證各自攔截壞檔。
func TestStaticValidationCatchesBadFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o600)
		return p
	}

	// capacity：thresholds 非法組合
	cap := write("cap.yaml", `sensors:
  - id: x
    metric: {value: 'a', ceiling: 'b'}
    thresholds: {soft_ratio: 0.95, crit_ratio: 0.90}
`)
	iss, err := staticValidate(cap, "capacity")
	if err != nil || countErrors(iss) == 0 {
		t.Fatalf("illegal thresholds must be caught: err=%v iss=%v", err, iss)
	}

	// slo：objective 超界
	slo := write("slo.yaml", `slos:
  - id: s
    sli_query: 'rate(x[5m])'
    objective: 150
`)
	if iss, err = staticValidate(slo, "slo"); err != nil || countErrors(iss) == 0 {
		t.Fatalf("objective out of range must be caught")
	}

	// slo：缺 sli_query
	slo2 := write("slo2.yaml", "slos:\n  - id: s\n    objective: 99\n")
	if iss, _ = staticValidate(slo2, "slo"); countErrors(iss) == 0 {
		t.Fatal("missing sli_query must be caught")
	}

	// waste：price_attrs 非合法 JSON
	waste := write("waste.yaml", `groups:
- name: g
  rules:
  - alert: W
    expr: up == 0
    labels: {sentinel_kind: waste, sentinel_sensor: w1, sentinel_price_attrs: '{bad json'}
    annotations: {summary: "x"}
`)
	if _, err := os.Stat(waste); err != nil {
		t.Fatal(err)
	}
	iss, _ = staticValidate(waste, "waste")
	found := false
	for _, i := range iss {
		if strings.Contains(i.Msg, "JSON") {
			found = true
		}
	}
	if !found {
		t.Fatalf("invalid price attrs JSON must be flagged, iss=%v", iss)
	}
}

// 驗收：live 層攔截 scalar 回應、放行 vector。
func TestLiveCheckDetectsScalarTrap(t *testing.T) {
	scalarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[1787589716,"42"]}}`))
	}))
	defer scalarSrv.Close()

	vectorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up"},"value":[1787589716,"1"]}]}}`))
	}))
	defer vectorSrv.Close()

	ctx := context.Background()
	if res, err := liveCheckExpr(ctx, scalarSrv.URL, "time()"); err != nil || res.ResultType != "scalar" {
		t.Fatalf("scalar shape must be detected: res=%+v err=%v", res, err)
	}
	if _, err := liveCheckExpr(ctx, vectorSrv.URL, "up"); err != nil {
		t.Fatalf("vector shape should pass: %v", err)
	}
}

// 驗收：verify 對 scalar 形狀判失敗（exit 語意由 cmdVerify 錯誤返回承擔）。
func TestVerifyRejectsScalarResult(t *testing.T) {
	dir := t.TempDir()
	def := "sensors:\n  - id: s\n    metric:\n      value: 'vector(time())'\n      ceiling: 'vector(9999999999)'\n"
	p := filepath.Join(dir, "d.yaml")
	os.WriteFile(p, []byte(def), 0o600)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[1,"2"]}}`))
	}))
	defer srv.Close()

	err := cmdVerify([]string{"-file", p, "-prom", srv.URL})
	if err == nil {
		t.Fatal("scalar-shaped responses must fail verify")
	}
}

// 驗收：fix 迴圈——第一輪壞檔、LLM 第二輪給好檔 → 最終 PASS。
func TestFixLoopConverges(t *testing.T) {
	bad := "slos:\n  - id: s\n    sli_query: 'rate(x[5m])'\n    objective: 150\n"
	good := "slos:\n  - id: s\n    service: api\n    sli_query: 'rate(err[5m])'\n    objective: 99.9\n"

	llm := &fakeLLM{responses: []string{
		"```yaml\n" + good + "```",
	}}
	srv := httptest.NewServer(llm.handler())
	defer srv.Close()

	t.Setenv("GEN_LLM_URL", srv.URL)
	t.Setenv("GEN_LLM_MODEL", "mock")

	p := filepath.Join(t.TempDir(), "s.yaml")
	os.WriteFile(p, []byte(bad), 0o600)

	if err := cmdFix([]string{"-file", p}); err != nil {
		t.Fatalf("fix loop failed: %v", err)
	}
	final, _ := os.ReadFile(p)
	if !contains(final, "service: api") {
		t.Fatalf("final file should be the LLM-fixed version:\n%s", final)
	}
	if _, err := os.Stat(p + ".bak"); err != nil {
		t.Fatal(".bak backup expected")
	}
}

// 驗收：GEN_LLM_URL 未設定時 generate 明確報錯。
func TestGenerateRequiresLLMEnv(t *testing.T) {
	t.Setenv("GEN_LLM_URL", "")
	t.Setenv("GEN_LLM_MODEL", "")
	if err := cmdGenerate([]string{"-kind", "capacity", "-desc", "x"}); err == nil {
		t.Fatal("generate without LLM env must fail with clear message")
	}
}

// 驗收：generate 經 fake LLM 產出檔案並抽取圍欄內容。
func TestGenerateWritesExtractedFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "gen.yaml")
	llm := &fakeLLM{responses: []string{
		"好的，以下是設定：\n```yaml\n# 註解\nsensors:\n  - id: gen-a\n    metric:\n      value: 'up'\n      ceiling: 'vector(1)'\n```\n自我檢查：OK",
	}}
	srv := httptest.NewServer(llm.handler())
	defer srv.Close()

	t.Setenv("GEN_LLM_URL", srv.URL)
	t.Setenv("GEN_LLM_MODEL", "mock")

	if err := cmdGenerate([]string{"-kind", "capacity", "-desc", "demo", "-out", out}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !contains(b, "gen-a") {
		t.Fatalf("extracted content wrong:\n%s", b)
	}
}

func contains(b []byte, sub string) bool {
	return strings.Contains(string(b), sub)
}
