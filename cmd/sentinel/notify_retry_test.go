package main

// notify_retry_test.go（T026）：通知發送失敗保護——先發送成功才登記去重狀態。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"slo-sentinel/internal/alert"
)

// flakyNotifier 前 failTimes 次回傳錯誤，之後成功並記錄訊息。
type flakyNotifier struct {
	failTimes int
	attempts  int
	sent      []string
}

func (f *flakyNotifier) Send(_ context.Context, text string) error {
	f.attempts++
	if f.attempts <= f.failTimes {
		return errors.New("telegram: connection reset")
	}
	f.sent = append(f.sent, text)
	return nil
}

// deadNotifier 永遠失敗。
type deadNotifier struct{ attempts int }

func (d *deadNotifier) Send(_ context.Context, _ string) error {
	d.attempts++
	return errors.New("telegram: down")
}

// 驗收 1：fake notifier 先失敗後成功——第二次輪詢補送同一狀態轉移。
func TestNotifyRetriesSameTransitionAfterFailure(t *testing.T) {
	src := &fakeRisingSource{base: 150, perMin: 2.0, now: time.Now().UTC().Add(-time.Hour)}
	notifier := &flakyNotifier{failTimes: 1}
	d, _ := setupCapacityDaemon(t, src, notifier, amNoAlerts(t))

	if err := d.runOnePoll(context.Background()); err != nil { // 發送失敗
		t.Fatal(err)
	}
	if len(notifier.sent) != 0 {
		t.Fatalf("failed send must not record message")
	}
	if err := d.runOnePoll(context.Background()); err != nil { // 同狀態重試成功
		t.Fatal(err)
	}
	if len(notifier.sent) != 1 {
		t.Fatalf("second poll must deliver the pending transition, got %d", len(notifier.sent))
	}
	if !strings.Contains(notifier.sent[0], "data-disk") {
		t.Fatalf("unexpected message: %s", notifier.sent[0])
	}
}

// 驗收 2：Send 永遠失敗——dedupe 狀態不被推進，恢復後未來轉移不被吞掉。
func TestNotifyPermanentFailureDoesNotAdvanceDedupe(t *testing.T) {
	src := &fakeRisingSource{base: 150, perMin: 2.0, now: time.Now().UTC().Add(-time.Hour)}
	dead := &deadNotifier{}
	d, _ := setupCapacityDaemon(t, src, dead, amNoAlerts(t))
	ctx := context.Background()

	for i := 0; i < 6; i++ { // 連續失敗多輪（含退避輪）
		if err := d.runOnePoll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// 去重狀態未被推進：同一轉移仍處於「待通知」
	st, err := d.st.GetState("data-disk")
	if err != nil || st == nil {
		t.Fatalf("sensor state missing: %v", err)
	}
	if !d.dedupe.Peek("data-disk", st.State) {
		t.Fatal("dedupe state must remain unadvanced while send keeps failing")
	}

	// 通道恢復：換上可用的 notifier；退避期內每輪重試直到放行補送
	good := &captureNotifier{}
	d.notifier = good
	for i := 0; i < notifyRetryEvery+1 && len(good.msgs) == 0; i++ {
		if err := d.runOnePoll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(good.msgs) == 0 {
		t.Fatal("recovered channel must deliver the pending transition")
	}
}

// 驗收 4：連續失敗退避——≥3 次失敗後每 5 輪才放行一次重試。
func TestNotifyBackoffGating(t *testing.T) {
	d := &daemon{}
	id := "data-disk"

	// 前 3 次失敗：每輪都允許嘗試
	for i := 1; i <= 3; i++ {
		if !d.allowNotify(id) {
			t.Fatalf("attempt %d should be allowed before backoff", i)
		}
		d.markNotifyResult(id, errors.New("down"))
	}
	// 降級期：第 1–4 輪擋下、第 5 輪放行並歸零
	for i := 1; i <= 4; i++ {
		if d.allowNotify(id) {
			t.Fatalf("backoff round %d should be skipped", i)
		}
	}
	if !d.allowNotify(id) {
		t.Fatal("retry round must be allowed after notifyRetryEvery polls")
	}
	d.markNotifyResult(id, errors.New("down")) // 重試仍失敗，退避重新計時
	for i := 1; i <= 4; i++ {
		if d.allowNotify(id) {
			t.Fatalf("post-retry backoff round %d should be skipped", i)
		}
	}
	// 成功後清除退避
	d.markNotifyResult(id, nil)
	if rt := d.notifyRetry[id]; rt != nil {
		t.Fatalf("success must clear retry state, got %+v", rt)
	}
	if !d.allowNotify(id) {
		t.Fatal("after recovery every attempt is allowed again")
	}
}

// 邊界：critical→warning 降級與 resolved（→healthy）路徑同樣受保護——
// 失敗的通知不推進狀態，後續轉移不被吞掉。
func TestDowngradeAndResolvedPathsProtected(t *testing.T) {
	d := alert.NewDedupe()

	// critical 首次告警：Send 失敗（不 Commit）→ 狀態維持未登記
	if !d.Peek("s", "critical") {
		t.Fatal("first critical must notify")
	}
	// 數值回穩但未達恢復：critical→warning 降級——因前次失敗未登記，
	// warning 轉移仍應通知（不會因「降級不單獨通知」被吞掉）
	if !d.Peek("s", "warning") {
		t.Fatal("failed send must not swallow later downgrade transition")
	}
	d.Commit("s", "warning") // 補送成功

	// resolved：任一狀態→healthy 必通知
	if !d.Peek("s", "healthy") {
		t.Fatal("resolved transition must notify")
	}
	d.Commit("s", "healthy")
	if d.Peek("s", "healthy") {
		t.Fatal("steady healthy must not notify")
	}
}
