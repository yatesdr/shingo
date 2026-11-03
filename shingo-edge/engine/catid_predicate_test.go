// catid_predicate_test.go — the auto-cutover's decision on a stable CATID, the
// identity rule (0 and negatives name no part), the identity tag's index, the
// boot prime and the post-cutover verify's one read.
package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/plc"
)

// stableTicks drives three reads across the 60 s settle window — the least
// that lets the monitor decide on a value.
func stableTicks(mon *catidMonitor, processID int64, st *catidState, v string) {
	t0 := time.Unix(0, 0)
	for i := 0; i < 3; i++ {
		tickAutoCutover(mon, processID, st, v, true, t0.Add(time.Duration(i)*30*time.Second))
	}
}

// TestPredicate_SharedCATIDNeverFires: a CATID both styles run (one assembly,
// different consume parts) is no declaration of a change. Characterization:
// decideAutoArm returns early whenever the value is in the active set, so this
// is green before the predicate is written down; it guards the fixed form
// against the round-1 form (out≠∅ ∧ v∉out) ∨ v∈in, which would fire here.
func TestPredicate_SharedCATIDNeverFires(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoCutover(t, "auto", "1001", "1001", true)
	stableTicks(mon, processID, st, "1001")
	if len(*calls) != 0 {
		t.Fatalf("a CATID both styles run cut over %d time(s), want 0", len(*calls))
	}
}

// TestPredicate_EveryTracedCaseUnchanged walks the cases the cutover design
// traced (one cell, an outgoing part 1001, an incoming part 1002): the decision
// is the same before and after the predicate was made one function.
func TestPredicate_EveryTracedCaseUnchanged(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		mode           string
		active, target string
		changeover     bool
		live           string
		wantFires      bool
	}{
		{"run-out on the outgoing part", "auto", "1001", "1002", true, "1001", false},
		{"the incoming part, both pinned", "auto", "1001", "1002", true, "1002", true},
		{"a part only the outgoing pin rules out", "auto", "1001", "", true, "1003", true},
		{"the incoming part, outgoing unpinned", "auto", "", "1002", true, "1002", true},
		{"zero is no program", "auto", "1001", "1002", true, "0", false},
		{"no changeover open", "auto", "1001", "1002", false, "1002", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			mon, st, processID, _, calls := setupAutoCutover(t, c.mode, c.active, c.target, c.changeover)
			stableTicks(mon, processID, st, c.live)
			if got := len(*calls) == 1; got != c.wantFires {
				t.Errorf("fired = %v (%d calls), want %v", got, len(*calls), c.wantFires)
			}
		})
	}
}

// TestPredicate_BothSetsEmptyIsInert: with no CATIDs on either style a value
// says nothing about the changeover; today any stable value cut over.
func TestPredicate_BothSetsEmptyIsInert(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoCutover(t, "auto", "", "", true)
	stableTicks(mon, processID, st, "1003")
	if len(*calls) != 0 {
		t.Fatalf("a value with both sets empty cut over %d time(s), want 0", len(*calls))
	}
}

// TestIsIdentity_RejectsZeroAndNegatives: an unused CATID slot reads -1. It
// names no part, so it neither raises the wrong-part alert, nor arms a
// cutover, nor reaches the request guard as a live part.
func TestIsIdentity_RejectsZeroAndNegatives(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoCutover(t, "auto", "1001", "1002", true)

	var alarms int
	mon.eng.Events.SubscribeTypes(func(evt Event) {
		if _, ok := evt.Payload.(CATIDMismatchEvent); ok {
			alarms++
		}
	}, EventCATIDMismatch)

	for _, v := range []string{"-1", "0", "-20"} {
		st.lastConfirmed, st.seenValue = v, true
		mon.onConfirmedCATID(processID, st, true)
		if _, ok := mon.liveCATID(processID); ok {
			t.Errorf("liveCATID reported %q as a live part", v)
		}
	}
	if alarms != 0 {
		t.Errorf("non-identities raised %d wrong-part alert(s), want 0", alarms)
	}
	stableTicks(mon, processID, st, "-1")
	if len(*calls) != 0 {
		t.Errorf("-1 cut over %d time(s), want 0", len(*calls))
	}
}

// TestDeriveIdentityTag_FollowsTheCounterIndex: a cell counting on its second
// output reads its part identity from the second identity slot.
func TestDeriveIdentityTag_FollowsTheCounterIndex(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"MES_Cell.Prod_Counter_02", "MES_Cell.CATID_02"},
		{"MES_Cell.Prod_Counter_01", "MES_Cell.CATID_01"},
		{"MES_Cell.Prod_Counter_11", "MES_Cell.CATID_11"},
		{"MES_Cell.Count", "MES_Cell.CATID_01"}, // no index: the first slot, as before
	}
	for _, c := range cases {
		if got := deriveIdentityTag(c.in); got != c.want {
			t.Errorf("deriveIdentityTag(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// failingPublishClient is a WarLink whose publish toggle always fails.
type failingPublishClient struct{ curtainStubClient }

func (*failingPublishClient) SetTagPublishing(context.Context, string, string, bool) error {
	return errors.New("warlink unreachable")
}

// TestPrime_KeepsStateWhenEnableFails: a WarLink that refuses the publish
// toggle at boot must not switch the monitor off for the process — the tag
// may already be published, and a read failure is handled on every tick.
func TestPrime_KeepsStateWhenEnableFails(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.plcMgr = plc.NewManager(nil, config.WarLinkConfig{}, 0, 0, nil, &failingPublishClient{})
	pid, err := db.CreateProcess("CELL", "", "plc", "MES_Cell.Prod_Counter_01", true)
	testutil.MustNoErr(t, err, "create process")

	cm := &catidMonitor{eng: eng, states: map[int64]*catidState{}}
	cm.prime()
	st, ok := cm.states[pid]
	if !ok {
		t.Fatal("a failed publish toggle dropped the process from the monitor")
	}
	if st.tagName != "MES_Cell.CATID_01" || st.plcName != "plc" {
		t.Errorf("state = %+v, want plc/MES_Cell.CATID_01", st)
	}
}

// TestVerify_SetComputedOnceAtOpen: the post-cutover watch reads the style's
// CATID set when it opens; the 500 ms checks under the monitor lock read
// nothing.
func TestVerify_SetComputedOnceAtOpen(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	processID, _, styleA, _ := seedProduceNode(t, db, "two_robot")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleA, "1001"), "pin")
	eng := testEngine(t, db)
	coID := seedChangeoverTo(t, eng, db, processID, styleA)
	seedCatidMonitor(eng, processID, "PROC", "1002") // disagrees: the watch stays open
	eng.openPostCutoverVerify(processID, coID)
	cm := eng.catidMon
	if _, open := cm.verify[processID]; !open {
		t.Fatal("watch not opened")
	}

	counter.Reset()
	now := time.Now()
	for i := 0; i < 5; i++ {
		cm.checkPostCutoverVerify(processID, cm.states[processID], now)
	}
	if n := counter.Count(); n != 0 {
		t.Errorf("five checks inside the window ran %d statements, want 0", n)
	}
}

// TestDecisionLine_LogsInEveryMode: an engineer soaking a fresh pin with the
// mode off or on prompt sees what auto would have done.
func TestDecisionLine_LogsInEveryMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"auto", "prompt", "off"} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			mon, st, processID, _, _ := setupAutoCutover(t, mode, "1001", "1002", true)
			var mu sync.Mutex
			var lines []string
			mon.eng.logFn = func(format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				lines = append(lines, fmt.Sprintf(format, args...))
			}
			stableTicks(mon, processID, st, "1002")
			mu.Lock()
			defer mu.Unlock()
			for _, l := range lines {
				if strings.Contains(l, "predicate held on CATID 1002") && strings.Contains(l, "mode="+mode) {
					return
				}
			}
			t.Errorf("no decision line for mode %s; logged %q", mode, lines)
		})
	}
}
