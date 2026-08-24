// Package alert 實作 Telegram 通知層與 AlertManager 協調靜默（F2b/F3/F4）。
//
// 通知架構為「直推中心」定案：sentinel 直推人話卡是唯一通知路徑；
// /metrics 僅供觀測，不作為告警輸入（algs/sensor-catalog.md §C.1）。
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Notifier 是通知出口的抽象介面。
type Notifier interface {
	Send(ctx context.Context, text string) error
}

// Telegram 以 Bot API sendMessage 推播到指定 chat_id。
type Telegram struct {
	Token   string
	ChatID  int64
	Client  *http.Client
	attempts int
	backoff  time.Duration
	nowFn    func() time.Time
	sleepFn  func(time.Duration)
}

func NewTelegram(token string, chatID int64) *Telegram {
	return &Telegram{
		Token:   token,
		ChatID:  chatID,
		Client:  &http.Client{},
		attempts: 3,
		backoff:  time.Second,
		nowFn:    time.Now,
		sleepFn:  time.Sleep,
	}
}

// Send 推播文字；失敗指數退避重試至多 3 次，最終失敗回傳錯誤（呼叫端記 log 不阻塞主迴圈）。
func (t *Telegram) Send(ctx context.Context, text string) error {
	payload := map[string]any{
		"chat_id": t.ChatID,
		"text":    text,
		"parse_mode": "Markdown",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var lastErr error
	for i := 0; i < t.attempts; i++ {
		err = t.post(ctx, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryable(err) {
			return err
		}
		if i < t.attempts-1 {
			t.sleepFn(t.backoff << i)
		}
	}
	return fmt.Errorf("telegram 經 %d 次重試仍失敗: %w", t.attempts, lastErr)
}

type retryableErr struct{ err error }

func (e *retryableErr) Error() string { return e.err.Error() }
func isRetryable(err error) bool {
	re, ok := err.(*retryableErr)
	return ok && re.err != nil
}

func (t *Telegram) post(ctx context.Context, body []byte) error {
	u := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.Token)
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.Client.Do(req)
	if err != nil {
		return &retryableErr{err} // 網路錯誤可重試
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		io.Copy(io.Discard, resp.Body)
		return &retryableErr{fmt.Errorf("telegram 回應 %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("telegram 回應 %d: %s", resp.StatusCode, b) // 4xx 不重試
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// LogNotifier 降級用：token 未設定時以日誌取代推播。
type LogNotifier struct{}

func (LogNotifier) Send(_ context.Context, text string) error {
	fmt.Println("[notify]", text)
	return nil
}
