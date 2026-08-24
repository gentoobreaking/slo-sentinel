package main

// budget_status_test.go（T019 Phase 1）：/api/budget-status/{slo_id} 契約測試
// ＋ cd-budget-handler.sh 端到端煙霧測試（httptest fake server → 真 bash 腳本）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"slo-sentinel/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedBudgetState 寫入感測狀態，供端點查詢。
func seedBudgetState(t *testing.T, st *store.Store, id, state string, utilization float64) {
	t.Helper()
	if err := st.SetState(store.SensorState{
		SensorID: id, State: state, LastValue: utilization,
		LastNotifyAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func budgetTestServer(t *testing.T, d *daemon) *httptest.Server {
	t.Helper()
	api := &readAPI{d: d}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/budget-status/", api.budgetStatusJSON)
	return httptest.NewServer(mux)
}

// ---- 端點契約 ----

func TestBudgetStatusEndpointContract(t *testing.T) {
	st := openTestStore(t)
	seedBudgetState(t, st, "api-availability", "warning", 0.575) // 消耗 57.5%
	srv := budgetTestServer(t, &daemon{st: st})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/budget-status/api-availability")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body struct {
		Mode            string   `json:"mode"`
		State           string   `json:"state"`
		RemainingBudget float64  `json:"remaining_budget"`
		EtaHours        *float64 `json:"eta_hours"`
		ConfirmedDate   string   `json:"confirmed_date"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Mode != "notify" || body.State != "warning" {
		t.Fatalf("mode/state 錯: %+v", body)
	}
	if body.RemainingBudget < 42.49 || body.RemainingBudget > 42.51 {
		t.Fatalf("remaining_budget=%v，應為 42.5", body.RemainingBudget)
	}
	if body.ConfirmedDate == "" {
		t.Fatal("confirmed_date 必填（§D.1 鐵律：標注資料截止）")
	}
	if body.EtaHours != nil {
		t.Fatalf("無預測紀錄時 eta_hours 應為 null，得 %v", *body.EtaHours)
	}
}

func TestBudgetStatusEndpointNotFound(t *testing.T) {
	st := openTestStore(t)
	srv := budgetTestServer(t, &daemon{st: st})
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/api/budget-status/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知感測應 404，得 %d", resp.StatusCode)
	}
}

// ---- 腳本煙霧測試（真 bash + fake server）----

func runBudgetHandler(t *testing.T, serverURL, sloID string, extraEnv ...string) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash 不存在，跳過腳本煙霧測試")
	}
	script := filepath.Join("..", "..", "scripts", "cd-budget-handler.sh")
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "SENTINEL_URL="+serverURL, "SLO_ID="+sloID)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	rc := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		rc = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("執行腳本失敗: %v\n%s", err, out)
	}
	return string(out), rc
}

func TestBudgetHandlerSmokeThreeStates(t *testing.T) {
	st := openTestStore(t)
	seedBudgetState(t, st, "slo-healthy", "healthy", 0.1)
	seedBudgetState(t, st, "slo-warning", "warning", 0.85)
	seedBudgetState(t, st, "slo-critical", "critical", 0.97)
	d := &daemon{st: st}

	cases := []struct {
		id     string
		expect string
	}{
		{"slo-healthy", "✅"},
		{"slo-warning", "⚠️"},
		{"slo-critical", "🔴"},
	}
	for _, tc := range cases {
		srv := budgetTestServer(t, d)
		out, rc := runBudgetHandler(t, srv.URL, tc.id)
		srv.Close()
		if rc != 0 {
			t.Errorf("%s: notify 模式應 exit 0，得 %d\n%s", tc.id, rc, out)
		}
		if !strings.Contains(out, tc.expect) || !strings.Contains(out, tc.id) {
			t.Errorf("%s: 輸出缺 %q:\n%s", tc.id, tc.expect, out)
		}
	}
}

// fail-open：連不上 sentinel → 警告＋exit 0。
func TestBudgetHandlerSmokeFailOpen(t *testing.T) {
	out, rc := runBudgetHandler(t, "http://127.0.0.1:1", "any-slo")
	if rc != 0 {
		t.Fatalf("fail-open 應 exit 0，得 %d\n%s", rc, out)
	}
	if !strings.Contains(out, "fail-open") {
		t.Errorf("應標注 fail-open:\n%s", out)
	}
}

// enforce 預留：BUDGET_ENFORCE=1 且 critical → exit 1；warning 仍 exit 0。
func TestBudgetHandlerSmokeEnforceHook(t *testing.T) {
	st := openTestStore(t)
	seedBudgetState(t, st, "slo-critical", "critical", 0.97)
	seedBudgetState(t, st, "slo-warning", "warning", 0.85)
	d := &daemon{st: st}

	srv := budgetTestServer(t, d)
	defer srv.Close()

	_, rc := runBudgetHandler(t, srv.URL, "slo-critical", "BUDGET_ENFORCE=1")
	if rc != 1 {
		t.Fatalf("enforce+critical 應 exit 1，得 %d", rc)
	}
	_, rc = runBudgetHandler(t, srv.URL, "slo-warning", "BUDGET_ENFORCE=1")
	if rc != 0 {
		t.Fatalf("enforce+warning 仍應 exit 0，得 %d", rc)
	}
}

// jq 備援：模擬無 jq 環境（FORCE_NO_JQ 由腳本內部支援的測試掛點）。
func TestBudgetHandlerSmokeNoJqFallback(t *testing.T) {
	st := openTestStore(t)
	seedBudgetState(t, st, "slo-warning", "warning", 0.85)
	srv := budgetTestServer(t, &daemon{st: st})
	defer srv.Close()

	out, rc := runBudgetHandler(t, srv.URL, "slo-warning", "FORCE_NO_JQ=1")
	if rc != 0 {
		t.Fatalf("exit=%d\n%s", rc, out)
	}
	if !strings.Contains(out, "⚠️") || !strings.Contains(out, "15") { // 餘量 15%
		t.Errorf("備援解析結果錯:\n%s", out)
	}
}
