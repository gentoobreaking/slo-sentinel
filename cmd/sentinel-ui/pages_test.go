package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeSentinel() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"states":{"disk":{"sensor_id":"disk","state":"critical","last_value":270,"updated_at":"2026-08-24T00:00:00Z"}}}`))
	})
	mux.HandleFunc("/api/slo/disk", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"sensor_id":"disk","state":{"state":"critical"},"predictions":[
		  {"predicted_at":"2026-08-24T00:00:00Z","eta_aggressive":null,"eta_conservative":-3600,"actual_value":1788000000},
		  {"predicted_at":"2026-08-24T01:00:00Z","eta_aggressive":10500,"eta_conservative":420000,"actual_value":1234567,"catalog_version":"v1","ceiling":900,"utilization":0.135}]}`))
	})
	mux.HandleFunc("/api/accuracy", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"since":"2026-08-17T00:00:00Z","sensors":[{"sensor_id":"disk","predictions":5,"last_eta_aggressive_sec":6900}]}`))
	})
	mux.HandleFunc("/api/cost", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"enabled":true,"confirmed_through":"2026-08-23","mtd_usd":150,
		  "eom_projection":{"aggressive":400,"conservative":320}}`))
	})
	mux.HandleFunc("/api/waste", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"candidates":[{"sensor_id":"cloud.elb.zero-traffic","alert_name":"WasteElbZeroTraffic",
		  "reason":"ELB 已 14 天零流量","idle_days":14}]}`))
	})
	return httptest.NewServer(mux)
}

func uiFor(t *testing.T, api string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	cfg := uiConfig{SentinelAPI: api, ListenAddr: "127.0.0.1:9098"}
	return nil, cfg.SentinelAPI
}

func TestPagesRender(t *testing.T) {
	api := fakeSentinel()
	defer api.Close()
	cfg := uiConfig{SentinelAPI: api.URL, ListenAddr: "127.0.0.1:9098"}

	cases := []struct {
		path string
		want []string
	}{
		{"/", []string{"感測總表", "disk", "critical"}},
		{"/slo/disk", []string{"感測詳情", "激進預估", "目錄版本：v1"}},
		{"/accuracy", []string{"命中統計", "2026-08-17"}},
		{"/cost", []string{"月底推估", "$320", "2026-08-23"}},
		{"/waste", []string{"候選", "cloud.elb.zero-traffic", "14 天"}},
	}
	for _, c := range cases {
		h := routeHandler(cfg, c.path)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", c.path, nil))
		body := rec.Body.String()
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", c.path, rec.Code)
		}
		for _, w := range c.want {
			if !containsStr(body, w) {
				t.Fatalf("%s: missing %q in output", c.path, w)
			}
		}
	}
}

func TestOnlyGetRoutesAllowed(t *testing.T) {
	api := fakeSentinel()
	defer api.Close()
	cfg := uiConfig{SentinelAPI: api.URL, ListenAddr: "127.0.0.1:9098"}

	post := httptest.NewRequest("POST", "/waste", nil)
	rec := httptest.NewRecorder()
	routeHandler(cfg, "/waste")(rec, post)
	// POST 到唯讀頁面：Go mux 對未註冊方法回 405
	if rec.Code != 405 && rec.Code != 404 {
		t.Fatalf("POST /waste should not be handled, got %d", rec.Code)
	}
}

func routeHandler(cfg uiConfig, path string) http.HandlerFunc {
	switch {
	case path == "/" || path == "":
		return handleIndex(cfg)
	case len(path) > 5 && path[:5] == "/slo/":
		return handleSloDetail(cfg)
	case path == "/accuracy":
		return handleAccuracy(cfg)
	case path == "/cost":
		return handleCost(cfg)
	case path == "/waste":
		return handleWaste(cfg)
	}
	return handleIndex(cfg)
}

func containsStr(s, sub string) bool { return indexOf2(s, sub) >= 0 }
func indexOf2(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestFmtGMT8(t *testing.T) {
	got := fmtGMT8("2026-08-24T05:41:33.710666886Z")
	want := "2026-08-24 13:41:33"
	if got != want {
		t.Fatalf("fmtGMT8 = %q, want %q", got, want)
	}
	// 非 RFC3339 輸入原樣返回（容錯）
	if got := fmtGMT8("not-a-time"); got != "not-a-time" {
		t.Fatalf("fallback broken: %q", got)
	}
}

// ---- T031：欄位人話化 ----

func TestHumanDurBoundaries(t *testing.T) {
	cases := []struct {
		sec  *float64
		want string
	}{
		{nil, "無成長跡象"},
		{ptr(-5), "已越過天花板"},
		{ptr(0), "約 0 分鐘後觸頂"},
		{ptr(90 * 60), "約 90 分鐘後觸頂"},
		{ptr(3 * 3600), "約 3.0 小時後觸頂"},
		{ptr(72 * 3600), "約 3.0 天後觸頂"},
		{ptr(100 * 86400), "約 100.0 天後觸頂"},
	}
	for _, c := range cases {
		if got := humanDur(c.sec); got != c.want {
			t.Fatalf("humanDur(%v) = %q, want %q", c.sec, got, c.want)
		}
	}
}

func TestThousandSep(t *testing.T) {
	cases := map[float64]string{
		0:         "0",
		1234567:   "1,234,567",
		-9876543:  "-9,876,543",
		12.5:      "12.5",
		1.788e+09: "1,788,000,000",
	}
	for v, want := range cases {
		if got := thousandSep(v); got != want {
			t.Fatalf("thousandSep(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestSloDetailHumanizedRows(t *testing.T) {
	api := fakeSentinel()
	defer api.Close()
	cfg := uiConfig{SentinelAPI: api.URL, ListenAddr: "127.0.0.1:9098"}
	rec := httptest.NewRecorder()
	routeHandler(cfg, "/slo/disk")(rec, httptest.NewRequest("GET", "/slo/disk", nil))
	body := rec.Body.String()

	// null ETA → 無成長跡象；負 ETA → 已越過天花板；正常 → 人話時長＋千分位
	for _, want := range []string{"無成長跡象", "已越過天花板", "約 2.9 小時後觸頂", "1,788,000,000", "約 2.9 小時後觸頂"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q\n%s", want, body)
		}
	}
	// 主表不再有舊表頭與秒數裸值
	if strings.Contains(body, "ETA 激進") || strings.Contains(body, "420000 s") {
		t.Fatalf("old headers/raw seconds must not appear:\n%s", body)
	}
}

func ptr(f float64) *float64 { return &f }

// T032：當下使用率欄——有值顯示百分比、NULL 顯示 —
func TestSloDetailUtilizationColumn(t *testing.T) {
	api := fakeSentinel()
	defer api.Close()
	cfg := uiConfig{SentinelAPI: api.URL, ListenAddr: "127.0.0.1:9098"}
	rec := httptest.NewRecorder()
	routeHandler(cfg, "/slo/disk")(rec, httptest.NewRequest("GET", "/slo/disk", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "當下使用率") || !strings.Contains(body, "13.5%") {
		t.Fatalf("utilization column missing:\n%s", body)
	}
	if !strings.Contains(body, "<td>—</td>") {
		t.Fatalf("legacy NULL row must render as em dash:\n%s", body)
	}
}
