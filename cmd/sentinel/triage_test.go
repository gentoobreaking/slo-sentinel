package main

// triage_test.go（T020）：容量預警轉交 ai-oncall 分診閘門的驗收測試。
// payload 相容性以 AM webhook schema 鏡像結構驗證（amtool 需實際安裝，
// 離線環境以結構斷言替代並記錄於任務書）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"log/slog"

	"slo-sentinel/config"
	"slo-sentinel/internal/alert"
	"slo-sentinel/internal/store"
)

// amWebhookMirror 是 AM webhook payload 的鏡像結構，供相容性斷言。
type amWebhookMirror struct {
	Version string `json:"version"`
	Alerts  []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    string            `json:"startsAt"`
	} `json:"alerts"`
}

const triageDefYAML = "sensors:\n" +
	"  - id: data-disk\n" +
	"    service: storage-api\n" +
	"    scope: k8s\n" +
	"    metric:\n" +
	"      value: 'disk_used'\n" +
	"      ceiling: 'disk_total'\n"

// triageDaemon 組出含 publisher 的容量 daemon；回傳 gate 捕獲通道與可換源包裝。
func triageDaemon(t *testing.T, gate *httptest.Server) (*daemon, *captureNotifier, *switchableSource) {
	t.Helper()
	dir := t.TempDir()
	capDir := filepath.Join(dir, "capacity_defs")
	if err := os.MkdirAll(capDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capDir, "disk.yaml"), []byte(triageDefYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	sw := &switchableSource{inner: &fakeRisingSource{base: 150, perMin: 2.0, now: time.Now().UTC().Add(-time.Hour)}}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	localCap := &captureNotifier{}
	d := &daemon{
		cfg: config.Config{
			PollIntervalSec: 60,
			CapacityDefsDir: capDir,
			SloDefsDir:      filepath.Join(dir, "sd"),
			RulesDir:        filepath.Join(dir, "r"),
			LogFormat:       "text",
		},
		log:             log,
		src:             sw,
		st:              st,
		notifier:        localCap,
		dedupe:          alert.NewDedupe(),
		amcoord:         &alert.AMCoord{BaseURL: amNoAlerts(t)},
		metrics:         newMetricsRegistry(),
		sensorMeta:      map[string]sensorMeta{},
		publishedFiring: map[string]bool{},
	}
	if gate != nil {
		d.publisher = &AMPublisher{URL: gate.URL, Token: "secret"}
	}
	if err := d.setupSensors(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d, localCap, sw
}

// 驗收 2＋功能設計 1：payload 通過 AM webhook schema 鏡像斷言、Bearer 認證。
func TestTriagePublishesAMFormatAlert(t *testing.T) {
	var gotBody []byte
	var gotAuth string
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer gate.Close()

	d, _, _ := triageDaemon(t, gate)
	if err := d.runOnePoll(context.Background()); err != nil {
		t.Fatal(err)
	}

	if gotAuth != "Bearer secret" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	var p amWebhookMirror
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatalf("payload must be valid AM webhook JSON: %v\n%s", err, gotBody)
	}
	if len(p.Alerts) != 1 || p.Version == "" {
		t.Fatalf("unexpected payload: %s", gotBody)
	}
	a := p.Alerts[0]
	for k, want := range map[string]string{
		"alertname": "CapacityEtaWarning",
		"sensor_id": "data-disk",
		"severity":  "critical",
		"service":   "storage-api",
		"scope":     "k8s",
	} {
		if a.Labels[k] != want {
			t.Fatalf("label %s = %q, want %q", k, a.Labels[k], want)
		}
	}
	if a.Status != "firing" || a.StartsAt == "" {
		t.Fatalf("status/startsAt missing: %+v", a)
	}
	if a.Annotations["summary"] == "" {
		t.Fatal("summary annotation required")
	}
}

// 功能設計 3：進入分診管線的事件，本地只發精簡卡不重複長文。
func TestTriagePublishedEventGetsCondensedLocalCard(t *testing.T) {
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer gate.Close()

	d, local, _ := triageDaemon(t, gate)
	if err := d.runOnePoll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(local.msgs) != 1 {
		t.Fatalf("exactly one condensed local card expected, got %d", len(local.msgs))
	}
	msg := local.msgs[0]
	if !strings.Contains(msg, "已轉交") {
		t.Fatalf("condensed card should note triage handoff:\n%s", msg)
	}
	if strings.Contains(msg, "若持續爆量") {
		t.Fatalf("full card must not be duplicated locally:\n%s", msg)
	}
}

// 轉交失敗回退：gate 掛掉 → 完整本地卡照發（critical 不丟失優先）。
func TestTriagePublishFailureFallsBackToLocalFullCard(t *testing.T) {
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	gate.Close() // 立即關閉模擬故障

	d, local, _ := triageDaemon(t, gate)
	if err := d.runOnePoll(context.Background()); err != nil {
		t.Fatal(err)
	}
	full := false
	for _, m := range local.msgs {
		if strings.Contains(m, "若持續爆量") {
			full = true
		}
	}
	if !full {
		t.Fatalf("publish failure must fall back to full local card, msgs=%v", local.msgs)
	}
}

// resolved 轉移：先前已轉交 firing → 發 resolved status 給 gate。
func TestResolvedPublishedAfterFiringHandoff(t *testing.T) {
	var statuses []string
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p amWebhookMirror
		json.Unmarshal(b, &p)
		for _, a := range p.Alerts {
			statuses = append(statuses, a.Status)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer gate.Close()

	d, _, sw := triageDaemon(t, gate)
	ctx := context.Background()
	if err := d.runOnePoll(ctx); err != nil { // firing critical
		t.Fatal(err)
	}
	// 解除遲滯需連續兩輪低於門檻 → healthy 後發 resolved
	sw.inner = &twoMetricSource{used: 10, total: 1000}
	if err := d.runOnePoll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.runOnePoll(ctx); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range statuses {
		if s == "resolved" {
			found = true
		}
	}
	if !found {
		t.Fatalf("resolved event expected after recovery, statuses=%v", statuses)
	}
}
