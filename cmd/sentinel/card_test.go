package main

import (
	"strings"
	"testing"

	"slo-sentinel/internal/budget"
)

func TestFormatForecastCardGolden(t *testing.T) {
	etaA, etaC := 10440.0, 700000.0
	f := budget.Forecast{
		ID: "data-disk", State: budget.StateCritical,
		Value: 270, Ceiling: 500, Headroom: 230, Utilization: 0.54,
		EtaAggressive: &etaA, EtaConservative: &etaC,
	}
	msg := formatForecastCard(f)
	for _, want := range []string{"🔴 data-disk", "若持續爆量", "回到常態", "54"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("card missing %q:\n%s", want, msg)
		}
	}
}
