package query

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Retryable 判斷錯誤是否值得重試：5xx 與傳輸/逾時錯誤可重試；4xx 屬非暫時性。
type Retryable interface {
	Retryable() bool
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string   { return e.msg }
func (e *httpError) Retryable() bool { return e.code >= 500 }

// Prometheus 實作以 HTTP API 存取 Prometheus（T003）。
type Prometheus struct {
	BaseURL string
	Client  *http.Client
	Timeout time.Duration

	attempts int // 重試次數，預設 3
	backoff  time.Duration
	nowFn    func() time.Time
	sleepFn  func(time.Duration)
}

func New(baseURL string) *Prometheus {
	return &Prometheus{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		Client:   &http.Client{},
		Timeout:  30 * time.Second,
		attempts: 3,
		backoff:  500 * time.Millisecond,
		nowFn:    time.Now,
		sleepFn:  time.Sleep,
	}
}

// get 執行帶重試的 GET：5xx/逾時指數退避重試至多 attempts 次；4xx 直接回錯。
func (p *Prometheus) get(ctx context.Context, endpoint string, params url.Values, out any) error {
	var lastErr error
	for i := 0; i < p.attempts; i++ {
		if err := p.once(ctx, endpoint, params, out); err != nil {
			lastErr = err
			var r Retryable
			if !asRetryable(err, &r) || !r.Retryable() {
				return err
			}
			if i < p.attempts-1 {
				p.sleepFn(p.backoff << i)
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("query %s 經 %d 次重試仍失敗: %w", endpoint, p.attempts, lastErr)
}

func asRetryable(err error, target *Retryable) bool {
	r, ok := err.(Retryable)
	if ok {
		*target = r
	}
	return ok
}

func (p *Prometheus) once(ctx context.Context, endpoint string, params url.Values, out any) error {
	u := p.BaseURL + endpoint + "?" + params.Encode()
	cctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		// 傳輸錯誤（含逾時）視為可重試
		return &transportError{err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return &transportError{err}
	}
	if resp.StatusCode != http.StatusOK {
		return &httpError{code: resp.StatusCode,
			msg: fmt.Sprintf("prometheus %s 回應 %d: %s", endpoint, resp.StatusCode, truncate(body, 200))}
	}

	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string   `json:"metric"`
				Value  []json.RawMessage   `json:"value,omitempty"`  // instant
				Values [][]json.RawMessage `json:"values,omitempty"` // range
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if envelope.Status != "success" {
		return fmt.Errorf("prometheus status=%s", envelope.Status)
	}
	for _, r := range envelope.Data.Result {
		if len(r.Value) == 2 {
			ts, v, err := parseValue(r.Value[0], r.Value[1])
			if err != nil {
				return err
			}
			res := out.(*[]Result)
			*res = append(*res, Result{Labels: r.Metric, Samples: []Sample{{Time: ts, Value: v}}})
		}
		if len(r.Values) > 0 {
			samples := make([]Sample, 0, len(r.Values))
			for _, pair := range r.Values {
				if len(pair) != 2 {
					continue
				}
				ts, v, err := parseValue(pair[0], pair[1])
				if err != nil {
					return err
				}
				samples = append(samples, Sample{Time: ts, Value: v})
			}
			res := out.(*[]Result)
			*res = append(*res, Result{Labels: r.Metric, Samples: samples})
		}
	}
	return nil
}

type transportError struct{ err error }

func (e *transportError) Error() string   { return e.err.Error() }
func (e *transportError) Retryable() bool { return true }
func (e *transportError) Unwrap() error   { return e.err }

func parseValue(tsRaw, valRaw json.RawMessage) (time.Time, float64, error) {
	tsf, err := strconv.ParseFloat(string(tsRaw), 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("parse timestamp: %w", err)
	}
	// Prometheus 回傳的值為 JSON 字串（如 "1.5"），需先解字串再轉數值
	var vs string
	if err := json.Unmarshal(valRaw, &vs); err != nil {
		vs = string(valRaw) // 容錯：若已是裸數字
	}
	v, err := strconv.ParseFloat(vs, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("parse value %s: %w", vs, err)
	}
	return time.Unix(int64(tsf), 0).UTC(), v, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// InstantQuery 實作 Source 介面。
func (p *Prometheus) InstantQuery(ctx context.Context, query string, at time.Time) ([]Result, error) {
	params := url.Values{"query": {query}, "time": {strconv.FormatFloat(float64(at.UnixMilli())/1e3, 'f', -1, 64)}}
	var res []Result
	if err := p.get(ctx, "/api/v1/query", params, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// RangeQuery 實作 Source 介面。
func (p *Prometheus) RangeQuery(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Result, error) {
	params := url.Values{
		"query": {query},
		"start": {strconv.FormatFloat(float64(start.UnixMilli())/1e3, 'f', -1, 64)},
		"end":   {strconv.FormatFloat(float64(end.UnixMilli())/1e3, 'f', -1, 64)},
		"step":  {step.String()},
	}
	var res []Result
	if err := p.get(ctx, "/api/v1/query_range", params, &res); err != nil {
		return nil, err
	}
	return res, nil
}
