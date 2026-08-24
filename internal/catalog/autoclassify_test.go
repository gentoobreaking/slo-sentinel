package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureMixed = `groups:
- name: community-example
  rules:
  # 1. 閒置型：annotations 有 unused + 天級 over_time → 應標 waste
  - alert: RedisIdleInstance
    expr: avg_over_time(redis_connected_clients[14d]) < 1
    for: 30d
    annotations:
      summary: "Redis instance appears unused (idle)"

  # 2. 一般閾值告警 → 不加標籤（KindNone，AM 材料）
  - alert: RedisDown
    expr: redis_up == 0
    for: 5m
    annotations:
      summary: "Redis is down"

  # 3. 已有人工標籤 → 不覆寫
  - alert: ManualWaste
    expr: foo_bar == 1
    labels:
      sentinel_kind: capacity
    annotations:
      summary: "manually classified"

  # 4. Sloth 指標引用 → 應標 budget
  - alert: SlothBudgetBurn
    expr: sloth:sli_error:ratio_rate5m > 0.001
    annotations:
      summary: "error budget burning"
`

func TestAutoClassifyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mixed.yml")
	if err := os.WriteFile(path, []byte(fixtureMixed), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := AutoClassifyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 { // waste + budget 各一；KindNone 與人工標籤不動
		t.Fatalf("應更新 2 條，得 %d", n)
	}

	out, _ := os.ReadFile(path)
	s := string(out)
	for _, want := range []string{"sentinel_kind: waste", "sentinel_kind: budget"} {
		if !strings.Contains(s, want) {
			t.Errorf("輸出缺 %q:\n%s", want, s)
		}
	}
	// KindNone 規則（RedisDown）不應被加上標籤——檢查它附近沒有 sentinel_kind
	if strings.Count(s, "sentinel_kind:") != 3 {
		t.Fatalf("sentinel_kind 出現次數應為 3（waste+capacity+budget）:\n%s", s)
	}
	// 人工標籤保留原值
	if !strings.Contains(s, "sentinel_kind: capacity") {
		t.Error("人工標籤 capacity 應保留")
	}
}

func TestSuggestKindHeuristics(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		want Kind
	}{
		{"sloth 引用", Rule{Expr: "sloth_sli_error:ratio > x"}, KindBudget},
		{"sloth label", Rule{Labels: map[string]string{"sloth_window": "28d"}}, KindBudget},
		{"閒置註解", Rule{Annotations: map[string]string{"summary": "volume unused for weeks"}}, KindWaste},
		{"天級回看窗", Rule{Expr: "avg_over_time(metric[30d]) < 1"}, KindWaste},
		{"長 for", Rule{ForSet: true, For: 400 * 3600 * 1e9}, KindWaste}, // 400h ≈ 16.7d
		{"一般閾值", Rule{Expr: "up == 0"}, ""},
		{"引號內 idle 不算", Rule{Expr: `rate(node_cpu_seconds_total{mode="idle"}[5m]) > .8`}, ""},
	}
	for _, tc := range cases {
		got, _ := SuggestKind(&tc.rule)
		if got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// 端到端：分類後的檔案要能被載入器正常解析（格式沒被改壞）。
func TestAutoClassifyOutputStillLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.yml")
	os.WriteFile(path, []byte(fixtureMixed), 0o644)

	if _, err := AutoClassifyFile(path); err != nil {
		t.Fatal(err)
	}
	groups, err := parseFile(path)
	if err != nil {
		t.Fatalf("改寫後無法解析: %v", err)
	}
	total := 0
	kinds := map[Kind]int{}
	for _, g := range groups {
		for _, r := range g.Rules {
			total++
			kinds[r.Kind]++
		}
	}
	if total != 4 {
		t.Fatalf("應有 4 條規則，得 %d", total)
	}
	if kinds[KindWaste] != 1 || kinds[KindBudget] != 1 || kinds[KindCapacity] != 1 || kinds[KindNone] != 1 {
		t.Fatalf("分類結果錯: %v", kinds)
	}
}

// 目錄模式。
func TestAutoClassifyDir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.yml"), []byte(fixtureMixed), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.yml"), []byte(fixtureMixed), 0o644)
	os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("not yaml"), 0o644)

	files, rules, err := AutoClassifyDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if files != 2 || rules != 4 {
		t.Fatalf("files=%d rules=%d，應為 2/4", files, rules)
	}
}
