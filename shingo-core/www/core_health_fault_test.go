package www

import (
	"strings"
	"testing"
)

// The verdict rule for faults: only a fault past HALF THE GRACE WINDOW colours
// it.
//
// 706 of 730 faults over 30 days recovered on their own with a median of 20
// seconds. A strip that goes amber for those is amber most of the day, and a
// strip that is amber most of the day is not read. The notice threshold (60s by
// default) is the right line for the GAUGE and the wrong one for the VERDICT:
// at a minute it still fires on ordinary slow replans several times a shift.
// Half the grace window is the point of no return — past it Core is more likely
// to write the order off than not, which is a degraded core and is rare.

func TestDeriveReasons_ReplanDoesNotDegradeTheCore(t *testing.T) {
	t.Parallel()
	h := CoreHealth{Cores: 8, FaultedNow: 3, FaultedNotice: 0, FaultedHalfGrace: 0,
		FaultNoticeAfterSeconds: 60, FaultHalfGraceSeconds: 1350}
	if got := deriveReasons(h, nil); len(got) != 0 {
		t.Errorf("three replanning orders must not degrade the core, got %v", got)
	}
}

// BELOW the line: notice faults alone are the gauge's business, not the
// verdict's. This is the case that used to go degraded and no longer does.
func TestDeriveReasons_NoticeFaultsAloneDoNotDegradeTheCore(t *testing.T) {
	t.Parallel()
	h := CoreHealth{Cores: 8, FaultedNow: 3, FaultedNotice: 2, FaultedHalfGrace: 0,
		FaultNoticeAfterSeconds: 60, FaultHalfGraceSeconds: 1350}
	if got := deriveReasons(h, nil); len(got) != 0 {
		t.Errorf("two orders past the 60s NOTICE threshold must not degrade the core — "+
			"that is a slow replan, not a stall; got %v", got)
	}
}

// ABOVE the line.
func TestDeriveReasons_HalfGraceFaultsNameThemselves(t *testing.T) {
	t.Parallel()
	h := CoreHealth{Cores: 8, FaultedNow: 3, FaultedNotice: 2, FaultedHalfGrace: 2,
		FaultNoticeAfterSeconds: 60, FaultHalfGraceSeconds: 1350}
	got := deriveReasons(h, nil)
	if len(got) != 1 {
		t.Fatalf("want exactly one reason, got %v", got)
	}
	if got[0] != "2 orders faulted more than 22m — halfway to giving up" {
		t.Errorf("reason = %q", got[0])
	}
	// The rule the plural helper exists for.
	one := deriveReasons(CoreHealth{Cores: 8, FaultedHalfGrace: 1, FaultHalfGraceSeconds: 1350}, nil)
	if len(one) != 1 || one[0] != "1 order faulted more than 22m — halfway to giving up" {
		t.Errorf("singular reason = %v", one)
	}
	// And it never says "fail" about an order that is still live.
	for _, r := range append(got, one...) {
		if strings.Contains(strings.ToLower(r), "fail") {
			t.Errorf("a faulted order is live; the verdict must not say %q", r)
		}
	}
}
