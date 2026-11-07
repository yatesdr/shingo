package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
)

// unloader_ask_pending_test.go — the release's U1 is asked of Core without
// waiting: the release returns at once, the unloader's window says the ask is
// out until Core answers, and the answer heals it.

// armU1 is a two-robot produce release carrying capture_lineside (the sim's
// auto-operator at door 1) beside an armed unloader of the released part, with
// Core holding its loader.retrieve answers while hold is set: marked, not yet
// clicked.
func armU1(t *testing.T, hold bool) *relHarness {
	t.Helper()
	h := newRelHarness(t)
	S := protocol.StatusStaged
	pairAt(pairSpec{mode: protocol.SwapModeTwoRobot}, "evac", S, "supply", S)(h)
	charSPArmUnloader(h)
	h.loaderAsks.hold.Store(hold)
	h.mark()
	return h
}

// wantReleased fails the test unless the click released both legs, with one
// ingest and no capture refused.
func wantReleased(h *relHarness, err error) {
	h.t.Helper()
	if got := h.outcome(err); got != "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0" {
		h.t.Fatalf("the release: %q", got)
	}
}

// releaseWithU1 is armU1, clicked.
func releaseWithU1(t *testing.T, hold bool) *relHarness {
	t.Helper()
	h := armU1(t, hold)
	wantReleased(h, pairClick(dispEmpty)(h))
	return h
}

// TestReleaseU1_AnUnansweredAskShowsPendingAndTheAnswerHealsIt: with Core not
// answering, the release still returns, no U1 exists yet, and the unloader's
// window carries the pending note. When Core answers, the note goes and the
// U1 is there. That the release does not wait is the next test's.
func TestReleaseU1_AnUnansweredAskShowsPendingAndTheAnswerHealsIt(t *testing.T) {
	t.Parallel()
	h := releaseWithU1(t, true)
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

// releaseHangBound is how long the next test gives the release before reading
// it as blocked. It is not the assertion: with the ask's clock stopped, a
// release that waits on the ask at all never returns, so this only says how
// long a red test takes to say so. A passing release returns in milliseconds.
const releaseHangBound = 30 * time.Second

// TestReleaseU1_TheAskAddsNoWaitToTheRelease: with Core never answering the
// U1's ask, the release returns and the ask is still out. The ask's wait runs
// on a clock that does not move, on which only a zero wait runs out: a release
// that waited on the ask for any time, all of AskWait or a sliver of it, would
// hold until Core answered, and Core never does. The U1 is asked with no wait
// (askUnloaderFulls).
func TestReleaseU1_TheAskAddsNoWaitToTheRelease(t *testing.T) {
	t.Parallel()
	h := armU1(t, true)
	stopped := clock.NewManual(time.Unix(0, 0))
	setAskClock(h.eng, stopped)
	done := make(chan error, 1)
	go func() { done <- pairClick(dispEmpty)(h) }()
	select {
	case err := <-done:
		wantReleased(h, err)
	case <-time.After(releaseHangBound):
		stopped.Advance(24 * time.Hour) // free a release blocked on the ask's wait
		select {
		case <-done:
		case <-time.After(releaseHangBound):
		}
		t.Fatalf("the release did not return within %v with Core not answering its U1: it waits on the ask", releaseHangBound)
	}
	// The witness: the U1 was asked, and it is still unanswered.
	if _, ask := askSent(t, h.eng); ask.Kind != protocol.AskLoaderRetrieve {
		t.Errorf("the last ask out is %s, want the U1's %s", ask.Kind, protocol.AskLoaderRetrieve)
	}
	if n := h.loaderAsks.asks.Load(); n != 0 {
		t.Errorf("Core answered %d loader.retrieve asks, want 0: it holds them", n)
	}
	if got := h.eng.UnloaderAskNote(charSPUnloaderNode); got == "" {
		t.Error("the window shows no pending ask after the release: the U1's ask is out and unanswered")
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
