package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch 監看 dir（含第一層子目錄）的變更，檔案事件經 debounce 後重新載入並呼叫 onChange。
// 回傳的 stop 函式用於結束監看。onChange 內不可長時間阻塞。
func (l *Loader) Watch(ctx context.Context, dir string, onChange func(*Catalog)) (stop func(), err error) {
	if l.Logger == nil {
		l.Logger = slog.Default()
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fsnotify: %w", err)
	}
	if werr := watcher.Add(dir); werr != nil {
		watcher.Close()
		return nil, fmt.Errorf("watch %s: %w", dir, werr)
	}
	// 第一層子目錄也加入監看（community/ local/ sloth-generated/ …）
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			if werr := watcher.Add(filepath.Join(dir, e.Name())); werr != nil {
				l.Logger.Warn("watch_subdir_failed", "dir", e.Name(), "error", werr.Error())
			}
		}
	}

	var (
		mu       sync.Mutex
		timer    *time.Timer
		stopped  bool
		stopOnce sync.Once
	)
	debounce := 500 * time.Millisecond
	reload := func() {
		cat, quarantined, err := l.Load(dir)
		if err != nil {
			l.Logger.Warn("catalog_reload_failed", "error", err.Error())
			return
		}
		for _, q := range quarantined {
			l.Logger.Warn("rule_file_quarantined_on_reload", "path", q.Path, "reason", q.Reason)
		}
		onChange(cat)
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				stopOnce.Do(func() { mu.Lock(); stopped = true; mu.Unlock() })
				watcher.Close()
				return
			case ev := <-watcher.Events:
				if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
					continue
				}
				if filepath.Ext(ev.Name) != ".yaml" && filepath.Ext(ev.Name) != ".yml" {
					continue
				}
				// debounce：500ms 內的連續事件只觸發一次重載
				mu.Lock()
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(debounce, func() {
					mu.Lock()
					if stopped {
						mu.Unlock()
						return
					}
					mu.Unlock()
					reload()
				})
				mu.Unlock()
			case werr := <-watcher.Errors:
				l.Logger.Warn("catalog_watch_error", "error", werr.Error())
			}
		}
	}()

	stop = func() {
		stopOnce.Do(func() { mu.Lock(); stopped = true; mu.Unlock() })
		watcher.Close()
	}
	return stop, nil
}
