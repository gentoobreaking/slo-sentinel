// Package catalog 載入與管理 Prometheus rules 格式的感測目錄。
//
// T005：解析 rules.d/ 下所有 .yaml/.yml、promtool 驗證（可用時）、
// 失敗整檔隔離不拖垮 daemon、依 sentinel 慣例分類感測種類（budget/capacity/waste）。
// 格式不自造——一切以標準 Prometheus rules 為基底（algs/sensor-catalog.md §C.1）。
package catalog

import (
	"fmt"
	"strings"
	"time"
)

// Kind 為感測種類，決定路由到哪個引擎。
type Kind string

const (
	KindBudget   Kind = "budget"   // Sloth 生成的 SLO 規則（labels 含 sloth_*）
	KindCapacity Kind = "capacity" // 容量感測（labels 含 sentinel_kind: capacity 或預設）
	KindWaste    Kind = "waste"    // 瘦身與閒置（labels 含 sentinel_kind: waste）
	KindNone     Kind = ""         // 非 sentinel 管理的規則：載入但不路由
)

// Rule 為單一條目（record 或 alert 二擇一）。
type Rule struct {
	Record      string            // recording rule 名稱（可空）
	Alert       string            // alert 名稱（可空）
	Expr        string
	For         time.Duration     // alert 的 for 持續時間（0 = 未設定）
	ForSet      bool              // 是否明確設定 for
	Labels      map[string]string
	Annotations map[string]string
	Kind        Kind              // 分類結果（見 Classify）
	SourceFile  string            // 來源檔案路徑
}

// ID 回傳感測識別：優先 sentinel_sensor（label 或 annotation），其次 record/alert 名稱。
func (r *Rule) ID() string {
	if v, ok := r.Labels["sentinel_sensor"]; ok && v != "" {
		return v
	}
	if v, ok := r.Annotations["sentinel_sensor"]; ok && v != "" {
		return v
	}
	if r.Record != "" {
		return r.Record
	}
	return r.Alert
}

// NotifyEvery 回傳重提週期（label notify_every），未設定回傳 0。
func (r *Rule) NotifyEvery() time.Duration {
	return ParsePromDuration(r.Labels["notify_every"])
}

// ExcludeNamespaces 解析 sentinel_exclude_namespaces（逗號分隔；支援 openshift-* 萬用字尾）。
func (r *Rule) ExcludeNamespaces() []string {
	v := r.Labels["sentinel_exclude_namespaces"]
	if v == "" {
		return nil
	}
	var out []string
	for _, part := range splitComma(v) {
		out = append(out, part)
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Group 為一個 Prometheus rule group。
type Group struct {
	Name  string
	Rules []*Rule
}

// Catalog 為成功載入的完整目錄。
type Catalog struct {
	Groups     []*Group
	Version    string // rules.d/community/UPSTREAM_COMMIT 內容（若存在）
	LoadedAt   time.Time
}

// RulesOfKind 回傳指定種類的所有規則。
func (c *Catalog) RulesOfKind(k Kind) []*Rule {
	var out []*Rule
	for _, g := range c.Groups {
		for _, r := range g.Rules {
			if r.Kind == k {
				out = append(out, r)
			}
		}
	}
	return out
}

// Quarantine 描述被隔離的檔案與原因。
type Quarantine struct {
	Path   string
	Reason string
}

func (q Quarantine) String() string {
	return fmt.Sprintf("%s: %s", q.Path, q.Reason)
}

// ---- 小工具（避免引入 strings 以外的依賴到型別層）----

func fmtSplit(s, sep string) []string {
	var out []string
	start := 0
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			out = append(out, s[start:i])
			i += len(sep) - 1
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
