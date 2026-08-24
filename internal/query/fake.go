package query

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// FakeSource 為測試用 Source 實作：注入固定序列，並可模擬錯誤。
type FakeSource struct {
	mu sync.Mutex

	Instant map[string][]Result // query → results
	Ranges  map[string]Result   // query → 單一序列（時間範圍由呼叫端決定）
	Fails   map[string]int      // query → 剩餘失敗次數（回 Retryable 錯誤）
	Calls   map[string]int      // query → 被呼叫次數
}

func NewFake() *FakeSource {
	return &FakeSource{
		Instant: map[string][]Result{},
		Ranges:  map[string]Result{},
		Fails:   map[string]int{},
		Calls:   map[string]int{},
	}
}

func (f *FakeSource) fail(q string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Fails[q] > 0 {
		f.Fails[q]--
		return &httpError{code: 503, msg: fmt.Sprintf("fake 503 for %s", q)}
	}
	return nil
}

func (f *FakeSource) InstantQuery(_ context.Context, q string, _ time.Time) ([]Result, error) {
	f.mu.Lock()
	f.Calls[q]++
	res := f.Instant[q]
	f.mu.Unlock()
	if err := f.fail(q); err != nil {
		return nil, err
	}
	return res, nil
}

func (f *FakeSource) RangeQuery(_ context.Context, q string, start, end time.Time, step time.Duration) ([]Result, error) {
	f.mu.Lock()
	f.Calls[q]++
	r := f.Ranges[q]
	f.mu.Unlock()
	if err := f.fail(q); err != nil {
		return nil, err
	}
	// 依 step 切樣本：以 Ranges 注入的起訖值線性內插，方便測試外插邏輯
	var samples []Sample
	if len(r.Samples) == 2 {
		y0, y1 := r.Samples[0].Value, r.Samples[1].Value
		for t := start; !t.After(end); t = t.Add(step) {
			frac := float64(t.Sub(start)) / float64(end.Sub(start))
			samples = append(samples, Sample{Time: t, Value: y0 + (y1-y0)*frac})
		}
	} else {
		samples = r.Samples
	}
	return []Result{{Labels: r.Labels, Samples: samples}}, nil
}
