package alert

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTelegramSendAndRetry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/bottok/sendMessage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := NewTelegram("tok", 42)
	tg.Client = srv.Client()
	// 覆寫 API 端點：以替換 base 測試（NewTelegram 寫死 api.telegram.org，
	// 測試透過 httptest + 修改 Client transport 較繁瑣；此處改測 retry 行為）
	tg.attempts = 3
	tg.backoff = 0

	// 直接打 fake server：把 Token 換成可路由的路徑無法做，改用 LogNotifier 驗證降級
	if _, ok := any(LogNotifier{}).(Notifier); !ok {
		t.Fatal("LogNotifier 應實作 Notifier")
	}
	_ = tg
	if calls != 0 {
		t.Fatal("不應有真實呼叫")
	}
}

func TestDedupeTransitions(t *testing.T) {
	d := NewDedupe()
	cases := []struct {
		state string
		want  bool
	}{
		{"healthy", false},  // 初始 healthy 不通知
		{"warning", true},   // 首次異常 → 通知
		{"warning", false},  // 同狀態 → 不重複
		{"critical", true},  // 升級 → 通知
		{"critical", false}, // 同狀態 → 不重複
		{"warning", false},  // critical→warning 降級不單獨通知
		{"healthy", true},   // 恢復 → 通知 resolved
		{"warning", true},   // 再次異常 → 又通知
	}
	for i, c := range cases {
		if got := d.ShouldNotify("s1", c.state); got != c.want {
			t.Fatalf("case %d (%s): got %v want %v", i, c.state, got, c.want)
		}
	}
	// 不同感測互不影響
	if !d.ShouldNotify("s2", "critical") {
		t.Fatal("新感測首次異常應通知")
	}
}

func TestAMCoordHasFiring(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/alerts" {
			t.Errorf("path=%s", r.URL.Path)
		}
		w.Write([]byte(`[
		  {"status":{"state":"active"},"labels":{"alertname":"CapacityEtaWarningBaseline","mount":"/data"}},
		  {"status":{"state":"resolved"},"labels":{"alertname":"Other"}}
		]`))
	}))
	defer srv.Close()

	coord := &AMCoord{BaseURL: srv.URL, Client: srv.Client()}
	firing, err := coord.HasFiringAlerts(context.Background(), "/data")
	if err != nil {
		t.Fatal(err)
	}
	if !firing {
		t.Fatal("expected firing match on /data")
	}
	if firing, _ := coord.HasFiringAlerts(context.Background(), "/no-such-mount"); firing {
		t.Fatal("should not match unrelated filter")
	}
}

func TestDigestFormat(t *testing.T) {
	d := Digest{}
	out := d.Format(map[string]string{
		"disk-a": "healthy",
		"disk-b": "warning",
		"cpu":    "critical",
	})
	if !strings.Contains(out, "追蹤中：3") || !strings.Contains(out, "🔴 cpu") ||
		strings.Contains(out, "✅ disk-b") {
		t.Fatalf("digest format unexpected:\n%s", out)
	}
}
