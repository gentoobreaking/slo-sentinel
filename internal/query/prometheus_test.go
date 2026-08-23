package query

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const instantJSON = `{"status":"success","data":{"resultType":"vector","result":[
  {"metric":{"__name__":"up","job":"api"},"value":[1756000000,"1"]}]}}`

const rangeJSON = `{"status":"success","data":{"resultType":"matrix","result":[
  {"metric":{"sensor":"disk"},"values":[[1756000000,"100"],[1756000060,"150"],[1756000120,"200"]]}]}}`

func TestInstantQueryParsesResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("query"); got != "up" {
			t.Errorf("query = %s", got)
		}
		w.Write([]byte(instantJSON))
	}))
	defer srv.Close()

	p := New(srv.URL)
	res, err := p.InstantQuery(context.Background(), "up", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Samples[0].Value != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res[0].Labels["job"] != "api" {
		t.Fatalf("labels lost: %+v", res[0].Labels)
	}
}

func TestRangeQueryParsesSamples(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(rangeJSON))
	}))
	defer srv.Close()

	p := New(srv.URL)
	start := time.Unix(1756000000, 0)
	end := start.Add(2 * time.Minute)
	res, err := p.RangeQuery(context.Background(), "disk", start, end, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || len(res[0].Samples) != 3 {
		t.Fatalf("unexpected samples: %+v", res)
	}
	if res[0].Samples[2].Value != 200 {
		t.Fatalf("last value = %v", res[0].Samples[2].Value)
	}
}

func TestRetryOn5xxThenSuccess(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(instantJSON))
	}))
	defer srv.Close()

	p := New(srv.URL)
	p.sleepFn = func(time.Duration) {} // 測試不等待
	res, err := p.InstantQuery(context.Background(), "up", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
	if len(res) == 0 {
		t.Fatal("expected result after retry")
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	p := New(srv.URL)
	p.sleepFn = func(time.Duration) {}
	if _, err := p.InstantQuery(context.Background(), "up", time.Now()); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("4xx must not retry; got %d calls", calls)
	}
}

func TestExhaustedRetriesReturnsError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := New(srv.URL)
	p.sleepFn = func(time.Duration) {}
	if _, err := p.InstantQuery(context.Background(), "up", time.Now()); err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if calls != 3 {
		t.Fatalf("expected exactly 3 attempts, got %d", calls)
	}
}

func TestFakeSourceInterpolation(t *testing.T) {
	f := NewFake()
	f.Ranges["disk"] = Result{Labels: map[string]string{"sensor": "disk"}, Samples: []Sample{
		{Value: 100}, {Value: 200},
	}}
	res, err := f.RangeQuery(context.Background(), "disk",
		time.Unix(0, 0), time.Unix(0, 0).Add(3*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got := res[0].Samples
	if len(got) != 4 { // 0,1m,2m,3m
		t.Fatalf("samples = %d", len(got))
	}
	if got[0].Value != 100 || got[3].Value != 200 || got[2].Value != 166.66666666666666 {
		t.Fatalf("interpolation wrong: %+v", got)
	}
}
