package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"shingocore/store/bins"
	"shingocore/store/orders"
)

func ptr64(v int64) *int64 { return &v }

// TestRefuseArrival covers the predicate's four answers in one table. It is a
// pure function on two structs, so it needs no database — which is the point of
// having extracted it: the rule is now testable apart from the four writers that
// used to each carry their own copy.
func TestRefuseArrival(t *testing.T) {
	cases := []struct {
		name       string
		order      *orders.Order
		bin        *bins.Bin
		wantRefuse bool
	}{
		{
			name:       "claimed by this order — proceed",
			order:      &orders.Order{ID: 7},
			bin:        &bins.Bin{ID: 3, ClaimedBy: ptr64(7)},
			wantRefuse: false,
		},
		{
			name:       "claimed by nobody — refuse",
			order:      &orders.Order{ID: 7},
			bin:        &bins.Bin{ID: 3, ClaimedBy: nil},
			wantRefuse: true,
		},
		{
			name:       "claimed by another order — refuse",
			order:      &orders.Order{ID: 7},
			bin:        &bins.Bin{ID: 3, ClaimedBy: ptr64(9)},
			wantRefuse: true,
		},
		{
			// The exemption, and it must survive: one compound plan claims a bin
			// for its LAST leg only, so an interim child legitimately places a bin
			// its sibling holds. Refusing here would break every swap.
			name:       "compound child — exempt even when claimed elsewhere",
			order:      &orders.Order{ID: 7, ParentOrderID: ptr64(1)},
			bin:        &bins.Bin{ID: 3, ClaimedBy: ptr64(9)},
			wantRefuse: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := refuseArrival(tc.order, tc.bin, 42, arrivalSiteDelivery)
			if (got != nil) != tc.wantRefuse {
				t.Fatalf("refuseArrival refused=%v, want %v", got != nil, tc.wantRefuse)
			}
			if got != nil && got.Site != arrivalSiteDelivery {
				t.Errorf("Site = %q, want %q — a refusal must name the writer that made it", got.Site, arrivalSiteDelivery)
			}
		})
	}
}

// TestArrivalRefusalTally_CountsPerSite pins the number the deferred fail-vs-park
// ruling waits on. A refusal that is not counted is a refusal nobody can weigh,
// and weighing it is the whole reason the disposition was deferred rather than
// guessed.
func TestArrivalRefusalTally_CountsPerSite(t *testing.T) {
	resetArrivalRefusalTally()
	t.Cleanup(resetArrivalRefusalTally)

	order := &orders.Order{ID: 7}
	unowned := &bins.Bin{ID: 3, ClaimedBy: nil}

	noteArrivalRefusal(refuseArrival(order, unowned, 99, arrivalSiteDelivery))
	noteArrivalRefusal(refuseArrival(order, unowned, 99, arrivalSiteDelivery))
	noteArrivalRefusal(refuseArrival(order, unowned, 99, arrivalSiteCompletionNet))

	tally := ArrivalRefusalTally()
	if tally[arrivalSiteDelivery] != 2 {
		t.Errorf("%s = %d, want 2", arrivalSiteDelivery, tally[arrivalSiteDelivery])
	}
	if tally[arrivalSiteCompletionNet] != 1 {
		t.Errorf("%s = %d, want 1", arrivalSiteCompletionNet, tally[arrivalSiteCompletionNet])
	}
	if tally[arrivalSiteMultiBinDelivery] != 0 {
		t.Errorf("%s = %d, want 0 — a site that never refused must not appear",
			arrivalSiteMultiBinDelivery, tally[arrivalSiteMultiBinDelivery])
	}
}

// TestArrivalGuard_OneSpelling is the drift test standing law 3 requires of a
// load-bearing predicate.
//
// The guard lived as FOUR inline copies — delivery and completion, each single-
// and multi-bin — and they had already drifted in the way that mattered: two
// logged through logFn and two through dbg, a nil-able debug hook, so with debug
// off half the refusals were invisible. This fails if a fifth copy is written
// inline instead of calling refuseArrival.
func TestArrivalGuard_OneSpelling(t *testing.T) {
	// The shape of the old inline test, in either of its two orderings.
	inline := regexp.MustCompile(`ClaimedBy\s*(==|!=)\s*nil\s*\|\||\*\w+\.ClaimedBy\s*!=\s*\w+\.ID`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// The predicate itself is allowed to spell it — once.
		if name == "arrival_guard.go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rErr := os.ReadFile(filepath.Clean(name))
		if rErr != nil {
			t.Fatalf("read %s: %v", name, rErr)
		}
		if loc := inline.FindIndex(src); loc != nil {
			line := 1 + strings.Count(string(src[:loc[0]]), "\n")
			t.Errorf("%s:%d spells the arrival claim guard inline — call refuseArrival instead. "+
				"Four copies of this rule drifted into two log levels and half of them were "+
				"invisible with debug off (PLAN §R.5).", name, line)
		}
	}
}

// TestBinAlreadyAt covers the arms the two settle sites share, including the one
// neither site's own tests reach: an UNPLACED bin.
//
// A bin in flight has node_id NULL, and the delivery-time settle is called with
// destination node ids that come from a lookup — so "nil node_id" must never
// compare equal to anything, least of all to a zero-valued destination. Getting
// that backwards would skip exactly the bins the settle exists to place.
func TestBinAlreadyAt(t *testing.T) {
	at := func(node int64) *bins.Bin { n := node; return &bins.Bin{ID: 3, NodeID: &n} }

	cases := []struct {
		name string
		bin  *bins.Bin
		dest int64
		want bool
	}{
		{"at the destination", at(42), 42, true},
		{"somewhere else", at(41), 42, false},
		{"in flight — node_id NULL", &bins.Bin{ID: 3}, 42, false},
		{"in flight against a zero destination", &bins.Bin{ID: 3}, 0, false},
		{"nil bin", nil, 42, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := binAlreadyAt(tc.bin, tc.dest); got != tc.want {
				t.Errorf("binAlreadyAt = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReapplyRefused_ArrivedCheckComesFirst pins the ordering that made this
// instrument readable, and it took two corrections to get right.
//
// First attempt: one predicate for all four sites. Every ordinary completion
// counted as a refusal (121 in a nine-minute soak) because a released claim at
// completion time means ApplyArrival already succeeded.
//
// Second attempt: silence the nil arm. That left `other` counting a second
// benign shape — bin delivered, claim released, the NEXT order claims it, and a
// repeat completion firing sees the new owner.
//
// The actual question is whether the bin LANDED. Asked first, everything past it
// is a bin that did not arrive, and only there is a foreign or absent claim a
// finding. The ordering lives inside reapplyRefused so no call site has to
// remember it.
func TestReapplyRefused_ArrivedCheckComesFirst(t *testing.T) {
	const dest = int64(42)
	other := int64(77)
	order := &orders.Order{ID: 7, Status: "delivered"} // live: the firing the net exists for
	at := func(node int64, claim *int64) *bins.Bin {
		n := node
		return &bins.Bin{ID: 3, NodeID: &n, ClaimedBy: claim}
	}

	cases := []struct {
		name        string
		bin         *bins.Bin
		wantSkip    bool
		wantRefusal bool
	}{
		// LANDED — benign whoever holds the claim by now. All three were counted
		// as refusals by one or other of the broken versions.
		{"landed, claim released", at(dest, nil), true, false},
		{"landed, re-claimed by the next order", at(dest, &other), true, false},
		{"landed, still ours", at(dest, &order.ID), true, false},

		// DID NOT LAND — now the claim is the question.
		{"not landed, still ours — re-apply", at(99, &order.ID), false, false},
		{"not landed, owned by another order", at(99, &other), true, true},
		// The bin-17 shape: lost its claim AND never arrived.
		{"not landed, owned by nobody", at(99, nil), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skip, refusal := reapplyRefused(order, tc.bin, dest, arrivalSiteCompletionNet)
			if skip != tc.wantSkip {
				t.Errorf("skip = %v, want %v", skip, tc.wantSkip)
			}
			if (refusal != nil) != tc.wantRefusal {
				t.Fatalf("refusal = %v, want refusal %v", refusal, tc.wantRefusal)
			}
			if refusal != nil {
				// The diagnosable half: the bin-20 survivor could not be explained
				// after the fact because order_bins is deleted at terminal.
				if refusal.DestNodeID != dest {
					t.Errorf("DestNodeID = %d, want %d — a refusal must record where the bin was owed",
						refusal.DestNodeID, dest)
				}
				if refusal.AtNodeID == 0 {
					t.Error("AtNodeID = 0 — a refusal must record where the bin actually is")
				}
			}
		})
	}

	// A compound child is exempt at this site too — its sibling holds the claim.
	child := &orders.Order{ID: 7, Status: "delivered", ParentOrderID: ptr64(1)}
	if skip, r := reapplyRefused(child, at(99, &other), dest, arrivalSiteCompletionNet); skip || r != nil {
		t.Errorf("compound child: skip=%v refusal=%v, want false/nil", skip, r)
	}
}

// TestReapplyRefused_TerminalOrderHasNoSafetyNetWork is the FOURTH and last cut
// to this predicate, and the shape it closes is the one the arrived check
// structurally cannot catch.
//
// The completion handler fires twice: on (X → delivered) and again on
// (delivered → confirmed). The first firing is the one the net exists for, and
// `delivered` is NOT terminal, so it still runs and still recovers.
//
// The later firings are a finished order looking at a bin that has, correctly,
// moved on to a successor that claimed it. "Not at my destination, owned by
// someone else" is the EXPECTED state of a completed order — and the arrived
// check cannot filter it, because a completed order's bin has genuinely left.
//
// Specimen, from the full-window soak (PLAN §R.12): order 7 confirmed with
// destination LSD_023, against bin 62 already held by live order 67 at LSD_022.
func TestReapplyRefused_TerminalOrderHasNoSafetyNetWork(t *testing.T) {
	const dest = int64(30) // LSD_023
	successor := int64(67)
	elsewhere := int64(29) // LSD_022 — where the successor already took it
	specimenBin := func() *bins.Bin {
		n := elsewhere
		return &bins.Bin{ID: 62, NodeID: &n, ClaimedBy: &successor}
	}

	// THE SPECIMEN. Terminal order, bin moved on to a live successor.
	done := &orders.Order{ID: 7, Status: "confirmed"}
	skip, refusal := reapplyRefused(done, specimenBin(), dest, arrivalSiteCompletionNet)
	if !skip {
		t.Error("skip = false for a terminal order — its safety net has nothing to recover")
	}
	if refusal != nil {
		t.Errorf("terminal order produced a refusal (%s) — a finished order whose bin moved on to a "+
			"successor is the expected state, not a defect, and counting it re-inflates the instrument",
			refusal.Reason())
	}

	// THE SAME SHAPE, STILL LIVE, must still be a refusal — the cut must not
	// swallow the real conflict it sits next to.
	live := &orders.Order{ID: 7, Status: "delivered"}
	skip, refusal = reapplyRefused(live, specimenBin(), dest, arrivalSiteCompletionNet)
	if !skip || refusal == nil {
		t.Fatalf("live order in the same shape: skip=%v refusal=%v — want a real refusal; "+
			"`delivered` is not terminal and that firing is what the net is for", skip, refusal)
	}
	if refusal.ClaimedBy == nil || *refusal.ClaimedBy != successor {
		t.Errorf("refusal names claimant %v, want order %d", refusal.ClaimedBy, successor)
	}

	// And a live order whose arrival was genuinely missed still gets re-applied.
	ours := int64(7)
	n := elsewhere
	if skip, r := reapplyRefused(live, &bins.Bin{ID: 62, NodeID: &n, ClaimedBy: &ours}, dest,
		arrivalSiteCompletionNet); skip || r != nil {
		t.Errorf("still-ours, not-landed: skip=%v refusal=%v, want false/nil — the net must re-apply", skip, r)
	}
}

// TestReapplyRefused_UnsetStatusDoesNotDisableTheGuard pins the fail-closed
// direction of the terminal cut.
//
// protocol.IsTerminal answers "has no outgoing transitions", and the empty
// string has none — so a zero-value Status reads as TERMINAL and would switch
// the guard off entirely. Skipping is the permissive answer here, and an order
// whose state cannot be read is exactly when the teleport check should still run.
// Caught by an existing test whose fixture never set a status.
func TestReapplyRefused_UnsetStatusDoesNotDisableTheGuard(t *testing.T) {
	other := int64(9)
	n := int64(29)
	unset := &orders.Order{ID: 7} // Status == ""

	skip, refusal := reapplyRefused(unset, &bins.Bin{ID: 3, NodeID: &n, ClaimedBy: &other}, 30,
		arrivalSiteCompletionNet)
	if !skip || refusal == nil {
		t.Fatalf("unset status: skip=%v refusal=%v — an unreadable order must NOT be treated as "+
			"terminal, or IsTerminal(\"\")==true silently disables the guard", skip, refusal)
	}
}
