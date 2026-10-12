package www

import "testing"

// A frozen loop is the first reason on the Core Health strip: Core answers the
// page, so every gauge can read healthy while nothing scans.
func TestCoreHealthVerdict_AFrozenLoopIsTheFirstReason(t *testing.T) {
	c := CoreHealth{Cores: 8, Load1: 9, Frozen: []string{"the scan pass has not turned over for 45s"}}
	got := deriveReasons(c, []string{"fleet down"})
	if len(got) < 3 || got[0] != "the scan pass has not turned over for 45s" {
		t.Fatalf("reasons = %v; want the frozen loop first, then the rest", got)
	}
}
