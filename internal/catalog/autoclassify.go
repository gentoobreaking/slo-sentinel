package catalog

// autoclassify.go——社群規則自動分類（sync-community.sh 的後處理）。
//
// 目標：使用者只挑服務，不需要理解 sentinel_kind 分類學。
// 對載入路徑中的規則檔做內容啟發式判定，自動補上 sentinel_kind 標籤：
//
//	budget → expr/labels 引用 sloth_* （Sloth 生成的 SLO 規則）
//	waste  → 長期狀態特徵：
//	           - annotations 出現 idle/unused/orphan/zombie/stale… 字眼
//	           - expr 含 *_over_time[...Nd]（天級回看窗）
//	           - for ≥ 7 天
//	（其餘不加標籤＝KindNone：保持慣性、不執行——預設安全）
//
// 已有 sentinel_kind 標籤的規則一律不動（人工覆寫優先）。
// 以 yaml.Node 改寫，保留原檔註解與格式。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	wasteAnnoRe   = regexp.MustCompile(`(?i)\b(idle|unused|orphan|zombie|stale|unattached|no[_ ](?:requests|connections|traffic)|zero[_ ](?:traffic|requests))\b`)
	longWindowRe  = regexp.MustCompile(`(?i)_over_time`)
	dayWindowRe   = regexp.MustCompile(`\b\d+d\b`)
	wasteMetricRe = regexp.MustCompile(`(?i)\b(idle|orphan|stale|unattached)\b`)
	quotedStrRe   = regexp.MustCompile(`"[^"]*"`) // label matcher 內的 "idle" 是技術參數，非浪費語意
	slothRefRe    = regexp.MustCompile(`(?i)sloth[:_]`)
)

// SuggestKind 依內容建議種類；第二個回傳值為人話理由（空字串＝不建議）。
func SuggestKind(r *Rule) (Kind, string) {
	// budget：引用 Sloth 生成的指標即屬預算家族
	if slothRefRe.MatchString(r.Expr) {
		return KindBudget, "expr 引用 sloth_* 指標"
	}
	for k := range r.Labels {
		if strings.HasPrefix(k, "sloth_") {
			return KindBudget, "labels 含 sloth_* 前綴"
		}
	}

	// waste：長期狀態特徵（任一命中）
	if wasteAnnoRe.MatchString(r.Annotations["summary"] + " " + r.Annotations["description"]) {
		return KindWaste, "annotations 描述閒置/孤兒資源"
	}
	if longWindowRe.MatchString(r.Expr) && dayWindowRe.MatchString(r.Expr) {
		return KindWaste, "expr 使用天級 _over_time 回看窗"
	}
	// 剝除引號字串再比對：避免把 mode="idle"（CPU 閒置時間時間參數）誤判為浪費
	exprBare := quotedStrRe.ReplaceAllString(r.Expr, "")
	if wasteMetricRe.MatchString(exprBare) {
		return KindWaste, "expr 涉及 idle/orphan/stale 語意"
	}
	if r.ForSet && r.For >= 7*24*time.Hour {
		return KindWaste, "for ≥ 7 天（長期觀察型告警）"
	}
	return "", ""
}

// AutoClassifyFile 對單一 rules 檔跑自動分類並寫回。
// 回傳新加上標籤的規則數。已有 sentinel_kind 的規則不動。
func AutoClassifyFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return 0, fmt.Errorf("yaml 解析 %s: %w", path, err)
	}

	changed := 0
	root := doc.Content[0] // 頂層 mapping
	if root == nil || root.Kind != yaml.MappingNode {
		return 0, fmt.Errorf("%s: 頂層不是物件", path)
	}
	if groupsNode := mapValue(root, "groups"); groupsNode != nil && groupsNode.Kind == yaml.SequenceNode {
		for _, groupItem := range groupsNode.Content {
			if groupItem.Kind != yaml.MappingNode {
				continue
			}
			rulesNode := mapValue(groupItem, "rules")
			if rulesNode == nil || rulesNode.Kind != yaml.SequenceNode {
				continue
			}
			for _, ruleItem := range rulesNode.Content {
				if ruleItem.Kind != yaml.MappingNode {
					continue
				}
				n, err := classifyRuleNode(ruleItem)
				if err != nil {
					return changed, fmt.Errorf("%s: %w", path, err)
				}
				changed += n
			}
		}
	}
	if changed == 0 {
		return 0, nil
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return changed, err
	}
	return changed, os.WriteFile(path, out, 0o644)
}

// classifyRuleNode 對單一 rule 節點判定並補標籤。回傳是否變更。
func classifyRuleNode(rule *yaml.Node) (int, error) {
	expr := scalarOf(rule, "expr")
	forStr := scalarOf(rule, "for")
	r := &Rule{
		Expr:        expr,
		For:         ParsePromDuration(forStr),
		ForSet:      forStr != "",
		Annotations: map[string]string{},
	}
	if ann := mapValue(rule, "annotations"); ann != nil && ann.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(ann.Content); i += 2 {
			r.Annotations[ann.Content[i].Value] = ann.Content[i+1].Value
		}
	}
	labelsNode := mapValue(rule, "labels")
	if labelsNode != nil && labelsNode.Kind == yaml.MappingNode {
		r.Labels = map[string]string{}
		for i := 0; i+1 < len(labelsNode.Content); i += 2 {
			r.Labels[labelsNode.Content[i].Value] = labelsNode.Content[i+1].Value
		}
	}

	// 已明確指定 → 人工覆寫優先，跳過
	if _, ok := r.Labels["sentinel_kind"]; ok {
		return 0, nil
	}

	kind, _ := SuggestKind(r)
	if kind == "" || kind == KindNone {
		return 0, nil
	}

	// 補上 labels.sentinel_kind（無 labels 節點則建立）
	if labelsNode == nil {
		labelsNode = &yaml.Node{Kind: yaml.MappingNode}
		rule.Content = append(rule.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "labels"},
			labelsNode)
	}
	labelsNode.Content = append(labelsNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "sentinel_kind"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: string(kind)})
	return 1, nil
}

// ---- yaml.Node 小工具 ----

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalarOf(m *yaml.Node, key string) string {
	n := mapValue(m, key)
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// AutoClassifyDir 對目錄下所有 .yml/.yaml 遞迴執行自動分類。
func AutoClassifyDir(dir string) (files int, rules int, err error) {
	return files, rules, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		files++
		n, ferr := AutoClassifyFile(path)
		if ferr != nil {
			return ferr
		}
		rules += n
		return nil
	})
}
