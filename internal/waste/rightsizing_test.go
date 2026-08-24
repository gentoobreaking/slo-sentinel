package waste

import "testing"

func TestLowUtil(t *testing.T) {
	if !LowUtil(0.10, 0.15) {
		t.Fatal("P95=10% < 15% should be low util")
	}
	if LowUtil(0.20, 0.15) {
		t.Fatal("P95=20% is not low util")
	}
}

func TestSuggestedSaving(t *testing.T) {
	if got := SuggestedSaving(100, 40); got != 730*60 {
		t.Fatalf("saving = %v", got)
	}
	if got := SuggestedSaving(30, 50); got != 0 { // 建議價更高時不為負
		t.Fatalf("negative saving must clamp to 0, got %v", got)
	}
}

func TestRoundUpTo(t *testing.T) {
	if RoundUpTo(3.2, 1) != 4 {
		t.Fatalf("roundup = %v", RoundUpTo(3.2, 1))
	}
	if RoundUpTo(5, 2) != 6 {
		t.Fatalf("roundup = %v", RoundUpTo(5, 2))
	}
}
