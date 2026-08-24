package catalog

// watch.go（T028）：rules.d 熱載入——底層改用共用的 internal/watch.Dir，
// 與 slo_defs／capacity_defs 行為一致。

import (
	"context"

	"slo-sentinel/internal/watch"
)

// Watch 監看 dir（含第一層子目錄）的變更，檔案事件經 debounce 後重新載入並呼叫 onChange。
// 回傳的 stop 函式用於結束監看。onChange 內不可長時間阻塞。
func (l *Loader) Watch(ctx context.Context, dir string, onChange func(*Catalog)) (stop func(), err error) {
	return watch.Dir(ctx, dir, func() {
		cat, quarantined, err := l.Load(dir)
		if err != nil {
			l.log().Warn("catalog_reload_failed", "error", err.Error())
			return
		}
		for _, q := range quarantined {
			l.log().Warn("rule_file_quarantined_on_reload", "path", q.Path, "reason", q.Reason)
		}
		onChange(cat)
	})
}
