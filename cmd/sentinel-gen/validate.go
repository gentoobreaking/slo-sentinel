package main

// validate.go：三層審查的實作——靜態 schema、live Prometheus、LLM 第二意見。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
	"path/filepath"
	"strings"

	"slo-sentinel/internal/capacity"
	"slo-sentinel/internal/catalog"
	"slo-sentinel/internal/spec"
)

type issue struct {
	Level string // ERROR / WARN
	Where string
	Msg   string
}

func (i issue) String() string { return fmt.Sprintf("[%s] %s： %s", i.Level, i.Where, i.Msg) }

// detectKind 依內容嗅探家族（review/verify 未指定 -kind 時用）。
func detectKind(content string) string {
	switch {
	case strings.Contains(content, "\nsensors:") || strings.HasPrefix(strings.TrimSpace(content), "sensors:"):
		return "capacity"
	case strings.Contains(content, "\nslos:") || strings.HasPrefix(strings.TrimSpace(content), "slos:") ||
		strings.Contains(content, "\n- id:") && strings.Contains(content, "sli_query"):
		return "slo"
	default:
		return "waste"
	}
}

// staticValidate 用「daemon 同款解析器」做 schema 驗證——能載入才能套用。
func staticValidate(path, kind string) ([]issue, error) {
	var iss []issue
	switch kind {
	case "capacity":
		defs, err := capacity.LoadDefs(path)
		if err != nil {
			iss = append(iss, issue{"ERROR", path, err.Error()})
			return iss, nil
		}
		if len(defs) == 0 {
			iss = append(iss, issue{"ERROR", path, "未定義任何 sensors"})
		}
		for _, d := range defs {
			th := d.Thresholds.Resolve()
			if verr := th.Validate(); verr != nil {
				iss = append(iss, issue{"ERROR", d.ID, verr.Error()})
			}
		}
	case "slo":
		slos, err := spec.Load(path)
		if err != nil {
			iss = append(iss, issue{"ERROR", path, err.Error()})
			return iss, nil
		}
		if len(slos) == 0 {
			iss = append(iss, issue{"ERROR", path, "未定義任何 slos"})
		}
	case "waste":
		tmp, err := os.MkdirTemp("", "sentinel-gen-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		dst := tmp + "/" + filepath.Base(path)
		if err := os.WriteFile(dst, raw, 0o600); err != nil {
			return nil, err
		}
		l := &catalog.Loader{Dir: tmp}
		cat, quarantined, lerr := l.Load(tmp)
		if lerr != nil {
			iss = append(iss, issue{"ERROR", path, lerr.Error()})
			return iss, nil
		}
		for _, q := range quarantined {
			iss = append(iss, issue{"ERROR", q.Path, q.Reason})
		}
		wastes := cat.RulesOfKind(catalog.KindWaste)
		if len(wastes) == 0 {
			iss = append(iss, issue{"WARN", path, "沒有規則被分類為 waste（缺 sentinel_kind label？）"})
		}
		for _, r := range wastes {
			if attrs := r.Labels["sentinel_price_attrs"]; attrs != "" {
				var m map[string]any
				if json.Unmarshal([]byte(attrs), &m) != nil {
					iss = append(iss, issue{"ERROR", r.ID(), "sentinel_price_attrs 非合法 JSON"})
				}
			}
		}
	default:
		return nil, fmt.Errorf("未知家族 %q", kind)
	}
	return iss, nil
}

// liveExpr 為一條 expr 的 live 檢查結論。
type liveExpr struct {
	Desc       string
	Expr       string
	ResultType string // vector / scalar / …
	Series     int
	Err        string
}

// liveCheckExpr 打真實 Prometheus 的 instant query。
// 重點攔截 scalar 形狀回應——那是「裸 scalar 函式」陷阱的實錘。
func liveCheckExpr(ctx context.Context, promURL, expr string) (liveExpr, error) {
	out := liveExpr{Desc: expr, Expr: expr}
	u, err := url.Parse(promURL)
	if err != nil {
		return out, err
	}
	q := u.Query()
	q.Set("query", expr)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return out, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("prometheus 回 %d：%s", resp.StatusCode, truncate(string(raw), 120))
	}
	var body struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return out, fmt.Errorf("回應非合法 JSON: %w", err)
	}
	if body.Status != "success" {
		return out, fmt.Errorf("prometheus status=%s error=%s", body.Status, body.Error)
	}
	out.ResultType = body.Data.ResultType
	out.Series = len(body.Data.Result)
	return out, nil
}

// collectExprs 收集某家族檔案中所有需要 live 驗證的 expr。
func collectExprs(kind, path string) ([]struct{ Desc, Expr string }, []issue, error) {
	var out []struct{ Desc, Expr string }
	var iss []issue
	switch kind {
	case "capacity":
		defs, err := capacity.LoadDefs(path)
		if err != nil {
			return nil, nil, err
		}
		for _, d := range defs {
			out = append(out,
				struct{ Desc, Expr string }{d.ID + " value", d.Metric.Value},
				struct{ Desc, Expr string }{d.ID + " ceiling", d.Metric.Ceiling})
		}
	case "slo":
		slos, err := spec.Load(path)
		if err != nil {
			return nil, nil, err
		}
		for _, s := range slos {
			out = append(out, struct{ Desc, Expr string }{s.ID + " sli_query", s.SLIQuery})
		}
	case "waste":
		tr, err := os.MkdirTemp("", "sentinel-gen-*")
		if err != nil {
			return nil, nil, err
		}
		defer os.RemoveAll(tr)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		dst := tr + "/" + filepath.Base(path)
		if err := os.WriteFile(dst, raw, 0o600); err != nil {
			return nil, nil, err
		}
		cat, _, err := (&catalog.Loader{Dir: tr}).Load(tr)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range cat.RulesOfKind(catalog.KindWaste) {
			out = append(out, struct{ Desc, Expr string }{r.ID(), r.Expr})
		}
	}
	return out, iss, nil
}
