package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoopbackDetection(t *testing.T) {
	if !isLoopbackAddr("127.0.0.1:9098") || !isLoopbackAddr("localhost:9098") {
		t.Fatal("loopback addresses not detected")
	}
	if isLoopbackAddr("0.0.0.0:9098") || isLoopbackAddr("192.168.1.5:9098") {
		t.Fatal("non-loopback misdetected")
	}
}

func TestIndexRendersStatesFromAPI(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"states":{"disk":{"sensor_id":"disk","state":"warning","last_value":150,"updated_at":"2026-08-24T00:00:00Z"}}}`))
	}))
	defer api.Close()

	cfg := uiConfig{SentinelAPI: api.URL, ListenAddr: "127.0.0.1:9098"}
	h := handleIndex(cfg)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/", nil))

	body := rec.Body.String()
	for _, want := range []string{"disk", "⚠️", "150", "感測總表"} {
		if !contains(body, want) {
			t.Fatalf("missing %q in output", want)
		}
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
