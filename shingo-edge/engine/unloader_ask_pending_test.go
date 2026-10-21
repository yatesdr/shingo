package engine

import (
	"slices"
	"testing"
	"time"

	"shingo/protocol"
)

// unloader_ask_pending_test.go — the release's U1 is asked of Core without
// waiting: the release returns at once, the unloader's window says the ask is
// out until Core answers, and the answer heals it.

// releaseWithU1 is a two-robot produce release carrying capture_lineside (the
// sim's auto-operator at door 1) beside an armed unloader of the released
// part, with Core holding its loader.retrieve answers while hold is set. It
// returns the harness and how long the release call took.
func releaseWithU1(t *testing.T, hold bool) (*relHarness, time.Duration) {
	t.Helper()
	return timedRelease(t, true, hold)
}

// timedRelease is releaseWithU1, or the same release with no unloader armed
// (no U1 asked) when arm is false: the baseline.
func timedRelease(t *testing.T, arm, hold bool) (*relHarness, time.Duration) {
	t.Helper()
	h := newRelHarness(t)
	S := protocol.StatusStaged
	pairAt(pairSpec{mode: protocol.SwapModeTwoRobot}, "evac", S, "supply", S)(h)
	if arm {
		charSPArmUnloader(h)
		h.loaderAsks.hold.Store(hold)
	}
	h.mark()
	start := time.Now()
	err := pairClick(dispEmpty)(h)
	took := time.Since(start)
	if got := h.outcome(err); got != "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0" {
		t.Fatalf("the release: %q", got)
	}
	return h, took
}

// TestReleaseU1_AnUnansweredAskShowsPendingAndTheAnswerHealsIt: with Core not
// answering, the release still returns (it does not wait for the U1's
// answer), no U1 exists yet, and the unloader's window carries the pending
// note. When Core answers, the note goes and the U1 is there.
func TestReleaseU1_AnUnansweredAskShowsPendingAndTheAnswerHealsIt(t *testing.T) {
	t.Parallel()
	h, took := releaseWithU1(t, true)
	if took >= AskWait/2 {
		t.Errorf("the release took %v with Core not answering; it must not wait on the U1's ask (AskWait %v)", took, AskWait)
	}
	const pending = "A full of " + fxPart + " is asked of Core; no answer yet"
	if got := h.eng.UnloaderAskNote(charSPUnloaderNode); got != pending {
		t.Errorf("window note while Core has not answered = %q, want %q", got, pending)
	}
	if n := inFlightFulls(t, h.db, []string{charSPUnloaderNode}); n != 0 {
		t.Errorf("U1s before Core answers = %d, want 0", n)
	}

	h.loaderAsks.hold.Store(false)
	awaitAsks(t, h.eng)
	if got := h.eng.UnloaderAskNote(charSPUnloaderNode); got != "" {
		t.Errorf("window note after Core answered = %q, want none", got)
	}
	if n := inFlightFulls(t, h.db, []string{charSPUnloaderNode}); n != 1 {
		t.Errorf("U1s after Core answers = %d, want 1", n)
	}
}

// TestReleaseU1_TheAskAddsNoWaitToTheRelease measures the release call with
// Core not answering the U1's ask, with Core answering it, and with no U1 at
// all (the baseline): the ask is an outbox row, so neither waits on Core.
// Five of each, the median logged for the cost table. What the U1 ADDS to the
// baseline is pinned under a quarter of AskWait: a release that waited on the
// ask would add the whole of it. The bound is on the difference because the
// release's own time is not this test's: under -race on a four-core box the
// baseline alone is about 700 ms.
func TestReleaseU1_TheAskAddsNoWaitToTheRelease(t *testing.T) {
	t.Parallel()
	median := func(arm, hold bool) time.Duration {
		ds := make([]time.Duration, 5)
		for i := range ds {
			_, ds[i] = timedRelease(t, arm, hold)
		}
		slices.Sort(ds)
		return ds[len(ds)/2]
	}
	held, answered, none := median(true, true), median(true, false), median(false, false)
	t.Logf("release call, median of 5: no U1 %v; U1 asked, Core not answering %v; U1 asked, Core answering %v (AskWait %v)",
		none, held, answered, AskWait)
	for name, d := range map[string]time.Duration{"not answering": held, "answering": answered} {
		if added := d - none; added >= AskWait/4 {
			t.Errorf("release with Core %s took %v, %v over the baseline %v, want under %v more: the U1's ask must not be waited for",
				name, d, added, none, AskWait/4)
		}
	}
}

// TestUnloaderAskNote_OneAskManyParts: the note covers the ask, whatever it
// names: one part is named, several are counted (a twenty-part ask would not
// fit a card note), and parts out in two asks at one window count once each.
func TestUnloaderAskNote_OneAskManyParts(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	eng.setUnloaderAsk(unloaderAskKey{window: "W1", parts: "PART-A"}, true)
	if got := eng.UnloaderAskNote("W1"); got != "A full of PART-A is asked of Core; no answer yet" {
		t.Errorf("one part: %q", got)
	}
	eng.setUnloaderAsk(unloaderAskKey{window: "W1", parts: "PART-A,PART-B,PART-C"}, true)
	if got := eng.UnloaderAskNote("W1"); got != "Fulls of 3 parts are asked of Core; no answer yet" {
		t.Errorf("several parts: %q", got)
	}
	if got := eng.UnloaderAskNote("W2"); got != "" {
		t.Errorf("another window: %q, want none", got)
	}
}
