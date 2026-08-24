// tracker.go（T015）：候選清單生命週期管理。
//
// 實作依據：algs/waste-detection.md §E.2 狀態機（candidate→notified→renote→
// dismissed/resolved）＋「已處理」統計（價值自我證明）。
package waste

import (
	"fmt"
	"sync"
	"time"
)

// Lifecycle 為候選清單條目的生命週期狀態。
type Lifecycle string

const (
	LifecycleNotified  Lifecycle = "notified"  // 首次提醒
	LifecycleRenoted   Lifecycle = "renoted"   // 週期重提
	LifecycleDismissed Lifecycle = "dismissed" // 暫不處理（可設復活期限）
	LifecycleResolved  Lifecycle = "resolved"  // 已處理（入結案統計）
)

// Entry 為一個候選的追蹤紀錄。
type Entry struct {
	SensorID    string
	ResourceID  string
	Reason      string
	State       Lifecycle
	FirstSeen   time.Time
	LastNotified time.Time
	Renotify    time.Duration // 重提週期（0 = 只提一次）
	WasteUSDPerDay float64
	TotalWasteUSD  float64
	DismissReason  string
	DismissUntil   time.Time // dismissed 的復活時刻；零值 = 不復活
}

// Tracker 管理候選清單（執行緒安全；正式部署可接 store 持久化）。
type Tracker struct {
	mu     sync.Mutex
	nowFn  func() time.Time
	entries map[string]*Entry // key: SensorID + "/" + ResourceID
	resolvedSaving float64
}

func NewTracker(now time.Time) *Tracker {
	return &Tracker{nowFn: func() time.Time { return now }, entries: map[string]*Entry{}}
}

// Observe 登記/更新一次掃描結果：
// 已存在的候選 → 更新累積浪費並依 renotify 週期決定是否重提；
// 新候選 → 建立 Entry 並標記為首次提醒。
func (t *Tracker) Observe(c Candidate) (entry *Entry, shouldNotify bool, msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.nowFn()
	key := c.SensorID + "/" + resourceKey(c)
	e, ok := t.entries[key]
	if !ok {
		e = &Entry{
			SensorID: c.SensorID, ResourceID: resourceKey(c), Reason: c.Reason,
			State: LifecycleNotified, FirstSeen: now, LastNotified: now,
			Renotify: c.Renotify,
		}
		t.entries[key] = e
		return e, true, fmt.Sprintf("🪦 %s\n原因：%s\n閒置 %.0f 天", c.SensorID, c.Reason, c.IdleDays)
	}
	// 累積浪費：距上次通知的天數 × 日浪費（先算 elapsed 再更新時間戳）
	elapsedSinceNotify := now.Sub(e.LastNotified)
	if elapsedSinceNotify > 0 && c.WastedCost > 0 {
		e.TotalWasteUSD += c.WastedCost * elapsedSinceNotify.Hours() / 24
	}

	if e.State == LifecycleDismissed {
		e.LastNotified = now
		if !e.DismissUntil.IsZero() && now.After(e.DismissUntil) {
			e.State = LifecycleRenoted
			e.DismissUntil = time.Time{}
			return e, true, fmt.Sprintf("🔁 %s：暫緩期限已過，再次提醒", key)
		}
		return e, false, "" // 明確暫不處理且未到期：不打擾
	}

	if e.Renotify > 0 && elapsedSinceNotify >= e.Renotify {
		e.State = LifecycleRenoted
		e.LastNotified = now
		return e, true, fmt.Sprintf("🔁 %s：仍未處理，累積浪費 $%.2f", key, e.TotalWasteUSD)
	}
	return e, false, ""
}

func resourceKey(c Candidate) string {
	if v, ok := c.Labels["resource"]; ok && v != "" {
		return v
	}
	return c.AlertName
}

// Dismiss 標記暫不處理。until 零值 = 永久擱置；非零 = 到期自動重新提醒。
func (t *Tracker) Dismiss(resourceID, reason string, until time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.entries {
		if e.ResourceID == resourceID {
			e.State = LifecycleDismissed
			e.DismissReason = reason
			e.DismissUntil = until
			return nil
		}
	}
	return fmt.Errorf("找不到資源 %s 的候選紀錄", resourceID)
}

// Resolve 標記已處理並累計節省金額。
func (t *Tracker) Resolve(resourceID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.entries {
		if e.ResourceID == resourceID && e.State != LifecycleResolved {
			e.State = LifecycleResolved
			t.resolvedSaving += e.TotalWasteUSD
			return nil
		}
	}
	return fmt.Errorf("找不到資源 %s", resourceID)
}

// ResolvedSaving 回傳已結案候選的節省總額（月報價值自證，§E.2 最末條）。
func (t *Tracker) ResolvedSaving() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resolvedSaving
}
