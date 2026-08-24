package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"slo-sentinel/internal/promdur"

	"gopkg.in/yaml.v3"
)

// Loader 負責從 dir 載入感測目錄與熱載入監看。
type Loader struct {
	Dir    string
	Logger *slog.Logger

	promtoolPath string // 快取 promtool 位置；"" 表示未安裝
	promtoolDone bool
}

// rulesFile 對應 Prometheus rules 檔的標準格式。
type rulesFile struct {
	Groups []struct {
		Name  string     `yaml:"name"`
		Rules []ruleYAML `yaml:"rules"`
	} `yaml:"groups"`
}

type ruleYAML struct {
	Record      string            `yaml:"record"`
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

// Load 掃描 dir 下所有 .yaml/.yml：
//   - promtool 可用時逐檔驗證，失敗整檔隔離
//   - YAML 解析失敗同樣整檔隔離
//   - 成功載入的規則依 Classify 分類；KindNone 的規則保留但不會被引擎路由
//
// 回傳目錄、隔離清單。dir 不存在時回傳空目錄與 error（呼叫端決定是否致命）。
func (l *Loader) Load(dir string) (*Catalog, []Quarantine, error) {
	if l.Logger == nil {
		l.Logger = slog.Default()
	}
	l.checkPromtool()

	cat := &Catalog{LoadedAt: time.Now().UTC()}
	var quarantined []Quarantine

	info, err := os.Stat(dir)
	if err != nil {
		return cat, nil, fmt.Errorf("stat rules dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return cat, nil, fmt.Errorf("%s 不是目錄", dir)
	}

	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 無法存取的子路徑：略過不中斷
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		if reason := l.validateFile(path); reason != "" {
			quarantined = append(quarantined, Quarantine{Path: path, Reason: reason})
			return nil
		}
		groups, err := parseFile(path)
		if err != nil {
			quarantined = append(quarantined, Quarantine{Path: path, Reason: err.Error()})
			return nil
		}
		cat.Groups = append(cat.Groups, groups...)
		return nil
	})
	if err != nil {
		return cat, quarantined, err
	}

	cat.Version = l.upstreamVersion(dir)
	l.Logger.Info("catalog_loaded",
		"groups", len(cat.Groups),
		"quarantined", len(quarantined),
		"version", cat.Version,
	)
	for _, q := range quarantined {
		l.Logger.Warn("rule_file_quarantined", "path", q.Path, "reason", q.Reason)
	}
	return cat, quarantined, nil
}

// parseFile 解析單一 rules 檔為 Group 清單（含分類）。
func parseFile(path string) ([]*Group, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rf rulesFile
	if err := yaml.Unmarshal(b, &rf); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	var groups []*Group
	for _, g := range rf.Groups {
		group := &Group{Name: g.Name}
		for _, r := range g.Rules {
			rule := &Rule{
				Record:      r.Record,
				Alert:       r.Alert,
				Expr:        r.Expr,
				ForSet:      r.For != "",
				Labels:      r.Labels,
				Annotations: r.Annotations,
				SourceFile:  path,
			}
			rule.For = ParsePromDuration(r.For)
			if rule.Record == "" && rule.Alert == "" {
				continue // 無 record 也無 alert 的條目無意義，略過
			}
			rule.Kind = Classify(rule)
			group.Rules = append(group.Rules, rule)
		}
		groups = append(groups, group)
	}
	return groups, nil
}

// Classify 依 sentinel 慣例判定規則種類（algs/sensor-catalog.md §C.3）：
//  1. labels["sentinel_kind"] 明確指定者優先
//  2. labels 含 sloth 前綴 → budget（Sloth 生成的 SLO 規則）
//  3. recording rule 名稱以 sentinel_ 開頭，或 annotations 含 sentinel_sensor → capacity
//     （正規化系列 sentinel_capacity_* 由容量引擎產出/消費）
//  4. 其餘 → KindNone（非 sentinel 管理規則：載入供 amcoord 比對，但不路由到引擎）
func Classify(r *Rule) Kind {
	if v := strings.TrimSpace(r.Labels["sentinel_kind"]); v != "" {
		switch Kind(v) {
		case KindBudget, KindCapacity, KindWaste:
			return Kind(v)
		}
		return KindNone
	}
	for k := range r.Labels {
		if strings.HasPrefix(k, "sloth") {
			return KindBudget
		}
	}
	if strings.HasPrefix(r.Record, "sentinel_") {
		return KindCapacity
	}
	if _, ok := r.Annotations["sentinel_sensor"]; ok {
		return KindCapacity
	}
	return KindNone
}

// validateFile 以 promtool 驗證（可用時）。回傳空字串表示通過或 promtool 不可用；
// 回傳非空字串表示隔離原因。
func (l *Loader) validateFile(path string) string {
	if l.promtoolPath == "" {
		return "" // 未安裝 promtool：跳過驗證（已在 checkPromtool 記錄警告）
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.promtoolPath, "check", "rules", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("promtool: %v: %s", err, truncateStr(string(out), 300))
	}
	return ""
}

func (l *Loader) checkPromtool() {
	if l.promtoolDone {
		return
	}
	l.promtoolDone = true
	path, err := exec.LookPath("promtool")
	if err != nil {
		l.Logger.Warn("promtool_not_found_rules_unvalidated",
			"hint", "安裝 prometheus 後將 promtool 放入 PATH 以啟用規則驗證")
		return
	}
	l.promtoolPath = path
}

// upstreamVersion 讀取 community/UPSTREAM_COMMIT（T005 上游同步腳本產出）。
func (l *Loader) upstreamVersion(rulesDir string) string {
	b, err := os.ReadFile(filepath.Join(rulesDir, "community", "UPSTREAM_COMMIT"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ParsePromDuration 解析 Prometheus 風格時長（委派至 internal/promdur）。
func ParsePromDuration(s string) time.Duration {
	return promdur.Parse(s)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
