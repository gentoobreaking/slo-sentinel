package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path) // 第二次開啟不得重跑遷移報錯
	if err != nil {
		t.Fatalf("reopen after migrate: %v", err)
	}
	s2.Close()
}

func TestSetGetStateRoundtrip(t *testing.T) {
	s := openTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	notify := now.Add(-time.Hour)

	st := SensorState{SensorID: "disk", State: "warning", LastValue: 150,
		LastNotifyAt: notify, UpdatedAt: now}
	if err := s.SetState(st); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetState("disk")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.State != "warning" || got.LastValue != 150 {
		t.Fatalf("unexpected state: %+v", got)
	}
	if !got.LastNotifyAt.Equal(notify) {
		t.Fatalf("last_notify_at = %v, want %v", got.LastNotifyAt, notify)
	}
}

func TestGetStateMissingReturnsNilNil(t *testing.T) {
	s := openTest(t)
	got, err := s.GetState("ghost")
	if err != nil || got != nil {
		t.Fatalf("expected (nil,nil), got (%v,%v)", got, err)
	}
}

func TestUpsertOverwritesState(t *testing.T) {
	s := openTest(t)
	if err := s.SetState(SensorState{SensorID: "s", State: "healthy"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetState(SensorState{SensorID: "s", State: "critical", LastValue: 9}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetState("s")
	if got.State != "critical" || got.LastValue != 9 {
		t.Fatalf("upsert failed: %+v", got)
	}
}

func TestPredictionsRoundtripAndNullETAs(t *testing.T) {
	s := openTest(t)
	etaA, etaC := 10440.0, 7000.0
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.AppendPrediction(Prediction{SensorID: "disk",
		PredictedAt: now, EtaAggressive: &etaA, EtaConservative: &etaC,
		ActualValue: 150, CatalogVersion: "2026.08.1"}); err != nil {
		t.Fatal(err)
	}
	// ETA 均為 nil 的預測也要能存（無風險視野）
	if err := s.AppendPrediction(Prediction{SensorID: "disk",
		PredictedAt: now.Add(time.Minute), ActualValue: 151}); err != nil {
		t.Fatal(err)
	}
	preds, err := s.ListPredictions("disk", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(preds) != 2 {
		t.Fatalf("predictions = %d", len(preds))
	}
	if preds[0].EtaAggressive == nil || *preds[0].EtaAggressive != etaA {
		t.Fatalf("eta_aggressive lost: %+v", preds[0])
	}
	if preds[1].EtaAggressive != nil {
		t.Fatalf("nil eta must stay nil: %+v", preds[1])
	}
	if preds[0].CatalogVersion != "2026.08.1" {
		t.Fatalf("catalog_version lost: %q", preds[0].CatalogVersion)
	}
}

func TestListPredictionsSinceFilter(t *testing.T) {
	s := openTest(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for i, at := range []time.Time{base, base.Add(30 * time.Minute), base.Add(2 * time.Hour)} {
		if err := s.AppendPrediction(Prediction{SensorID: "cpu", PredictedAt: at, ActualValue: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	preds, err := s.ListPredictions("cpu", base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(preds) != 1 || preds[0].ActualValue != 2 {
		t.Fatalf("since filter failed: %+v", preds)
	}
}

func TestConcurrentWritesAreSafe(t *testing.T) {
	s := openTest(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if err := s.SetState(SensorState{
					SensorID: fmt.Sprintf("sensor-%d", i%3),
					State:    "healthy", LastValue: float64(j),
				}); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
