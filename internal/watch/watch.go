// Package watch 提供定義目錄的共通檔案監看（T028）。
//
// rules.d／slo_defs／capacity_defs 三個目錄共用同一個 watcher 實作：
// fsnotify + 500ms 防抖；單一目錄單一 goroutine，重複變更不疊加。
// macOS Docker Desktop（virtiofs）的 fsnotify 事件可能不可達——
// 屆時由呼叫端降級為定期 mtime 檢查或手動重啟。
package watch

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const debounce = 500 * time.Millisecond

// Dir 監看 dir（含第一層子目錄）下的 .yaml/.yml 變更，防抖後呼叫 onChange。
// 回傳 stop 函式；ctx 取消亦會停止。onChange 內不可長時間阻塞。
func Dir(ctx context.Context, dir string, onChange func()) (stop func(), err error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if werr := watcher.Add(dir); werr != nil {
		watcher.Close()
		return nil, werr
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			if werr := watcher.Add(filepath.Join(dir, e.Name())); werr != nil {
				continue // 子目錄監看失敗不致命
			}
		}
	}

	var (
		mu       sync.Mutex
		timer    *time.Timer
		stopped  bool
		stopOnce sync.Once
	)
	fire := func() {
		mu.Lock()
		if stopped {
			mu.Unlock()
			return
		}
		mu.Unlock()
		onChange()
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
				if ext := filepath.Ext(ev.Name); ext != ".yaml" && ext != ".yml" {
					continue
				}
				// 防抖：500ms 內連續事件只觸發一次 onChange（timer 覆寫，不疊加 goroutine）
				mu.Lock()
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(debounce, fire)
				mu.Unlock()
			case werr := <-watcher.Errors:
				mu.Lock()
				wasStopped := stopped
				mu.Unlock()
				if wasStopped || werr == nil {
					continue // teardown 競態的殘餘事件
				}
				_ = werr
			}
		}
	}()

	stop = func() {
		stopOnce.Do(func() { mu.Lock(); stopped = true; mu.Unlock() })
		watcher.Close()
	}
	return stop, nil
}
