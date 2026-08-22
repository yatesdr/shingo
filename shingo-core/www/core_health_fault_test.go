package www

import (
	"strings"
	"testing"
)

// The verdict rule for faults: only a fault past the threshold colours it.
//
// 706 of 730 faults over 30 days recovered on their own with a median of 20
// seconds. A strip that goes amber for those is amber most of the day, and a
// strip that is amber most of the day is not read.

func TestDeriveReasons_ReplanDoesNotDegradeTheCore(t *testing.T) {
	t.Parallel()
	h := CoreHealth{Cores: 8, FaultedNow: 3, FaultedNotice: 0, FaultNoticeAfterSeconds: 60}
	if got := deriveReasons(h, nil); len(got) != 0 {
		t.Errorf("three replanning orders must not degrade the core, got %v", got)
	}
}

func TestDeriveReasons_NoticeFaultsNameThemselves(t *testing.T) {
	t.Parallel()
	h := CoreHealth{Cores: 8, FaultedNow: 3, FaultedNotice: 2, FaultNoticeAfterSeconds: 60}
	got := deriveReasons(h, nil)
	if len(got) != 1 {
		t.Fatalf("want exactly one reason, got %v", got)
	}
	if got[0] != "2 orders faulted over 60s" {
		t.Errorf("reason = %q", got[0])
	}
	// The rule the plural helper exists for.
	one := deriveReasons(CoreHealth{Cores: 8, FaultedNotice: 1, FaultNoticeAfterSeconds: 60}, nil)
	if len(one) != 1 || one[0] != "1 order faulted over 60s" {
		t.Errorf("singular reason = %v", one)
	}
	// And it never says "fail" about an order that is still live.
	for _, r := range append(got, one...) {
		if strings.Contains(strings.ToLower(r), "fail") {
			t.Errorf("a faulted order is live; the verdict must not say %q", r)
		}
	}
}
