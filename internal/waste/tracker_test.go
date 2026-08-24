package waste

import (
	"testing"
	"time"
)

func TestTrackerLifecycle(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	now := base
	tr := &Tracker{nowFn: func() time.Time { return now }, entries: map[string]*Entry{}}

	c := Candidate{SensorID: "w", AlertName: "elb-1", IdleDays: 14,
		WastedCost: 5.0, Renotify: 7 * 24 * time.Hour}

	e, notify, msg := tr.Observe(c)
	if !notify || e.State != LifecycleNotified {
		t.Fatalf("first observe: notify=%v state=%s", notify, e.State)
	}
	_ = msg

	// 7 天後 → 重提
	now = base.Add(7 * 24 * time.Hour)
	e, notify, _ = tr.Observe(c)
	if !notify || e.State != LifecycleRenoted {
		t.Fatalf("renote expected, got %v/%s", notify, e.State)
	}

	// 暫不處理 30 天
	dismissUntil := now.Add(30 * 24 * time.Hour)
	if err := tr.Dismiss("elb-1", "災難備援保留", dismissUntil); err != nil {
		t.Fatal(err)
	}
	now = base.Add(8 * 24 * time.Hour)
	e, notify, _ = tr.Observe(c)
	if notify || e.State != LifecycleDismissed {
		t.Fatalf("dismissed should be silent until deadline")
	}
}

func TestTrackerDismissExpiryRevives(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	now := base
	tr := &Tracker{nowFn: func() time.Time { return now }, entries: map[string]*Entry{}}
	c := Candidate{SensorID: "w", AlertName: "elb-2"}

	tr.Observe(c)
	if err := tr.Dismiss("elb-2", "", base.Add(10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 期限前：靜默
	now = base.Add(5 * 24 * time.Hour)
	if _, notify, _ := tr.Observe(c); notify {
		t.Fatal("dismissed must be silent before expiry")
	}
	// 期限後：復活提醒
	now = base.Add(11 * 24 * time.Hour)
	e, notify, _ := tr.Observe(c)
	if !notify || e.State != LifecycleRenoted {
		t.Fatalf("revival expected, got %v/%s", notify, e.State)
	}
}

func TestTrackerResolveSumsSaving(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	tr := &Tracker{nowFn: func() time.Time { return base }, entries: map[string]*Entry{}}

	c1 := Candidate{SensorID: "w", AlertName: "elb-a"}
	tr.Observe(c1)
	if err := tr.Resolve("elb-a"); err != nil {
		t.Fatal(err)
	}
	if tr.ResolvedSaving() != 0 {
		t.Fatalf("no waste recorded yet, got %v", tr.ResolvedSaving())
	}
	// 未登記的資源 Resolve 要報錯
	if err := tr.Resolve("ghost"); err == nil {
		t.Fatal("expected error for unknown resource")
	}
}
