//go:build docker

package dispatch

import (
	"fmt"
	"shingocore/store/reservations"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// dig_blind_pins_docker_test.go — MG3-0, pin 2. EMPTY SELECTION IS DIG-BLIND,
// and this file says so as current behaviour, in both of the two shapes that
// behaviour takes.
//
// ── WHY TWO CASES AND NOT ONE ───────────────────────────────────────────────
//
// Round 1 split three-to-one on what a parked order does on its next tick, and
// the fourth review found the variable that settles it: THE KIND OF DIG HOLD.
//
//   - EXCAVATION (compound-backed): the dig claims its own buried target inside
//     the compound transaction, and every empty finder excludes claimed bins.
//     The parked order's next tick therefore skips the dig's target and diverts
//     on its own — the park SELF-HEALS in one tick.
//   - §R.101 SOURCE LOCK: a mouth row held by a demand. No compound, no bin
//     claims, nothing hidden. The parked order re-picks the SAME buried empty
//     every tick and re-parks, indefinitely, while a diggable free lane sits
//     unconsidered.
//
// The second is the defect MG3-1b buys. Anyone evaluating that fix against the
// first case will conclude it was pointless — which is exactly why both are
// pinned here, next to each other, before the fix exists.
//
// ── WHAT THESE PIN, PRECISELY ───────────────────────────────────────────────
//
// WHICH CARRIER THE FINDER SELECTS. That is the single thing MG3-1b changes.
// The downstream consequences — tier 6 raising OutcomeReshuffle, the planner
// refusing a held lane at IsLocked, the order parking under CauseLaneLocked —
// already have their own tests and are not restated here.
//
// MG3-1b EDITS THIS FILE. The source-lock case's assertion inverts, and its
// message stops describing a defect. That is the intended lifecycle: a
// deliberate reversal reads as one only if what it reverses was written down.

// twoLaneAllBuried builds one group with two depth-2 lanes, each holding a
// blocker at the mouth and ONE empty carrier of `code` behind it.
//
// EVERY COMPATIBLE EMPTY IS BURIED, which is the precondition for reaching a
// dig at all: AccessibleEmptyOrder ranks reachable candidates first, so a
// buried empty only wins when there is no accessible one. That is the shape
// tier 6 exists for.
//
// The two buried empties are at EQUAL depth, so the id tiebreak decides — and
// the fixture gives the held lane the LOWER id, so the finder prefers it. A
// fixture where the free lane won would pass every assertion below while
// proving nothing about dig-blindness.
func twoLaneAllBuried(t *testing.T, db *store.DB, prefix, code string) (heldLane, freeLane, heldEmpty, freeEmpty int64) {
	t.Helper()

	grpID, err := nodes.CreateGroup(db.DB, prefix+"-GRP")
	testutil.MustNoErr(t, err, "CreateGroup")

	var btID int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, code).Scan(&btID), "bin type")
	blockerBT, err := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, err, "DEFAULT bin type")

	build := func(name string) (laneID, emptyID int64) {
		laneType, terr := db.GetNodeTypeByCode(protocol.NodeClassLANE)
		testutil.MustNoErr(t, terr, "LANE type")
		lane := &nodes.Node{Name: name, IsSynthetic: true, Enabled: true,
			NodeTypeID: &laneType.ID, ParentID: &grpID}
		testutil.MustNoErr(t, db.CreateNode(lane), "create "+name)

		slots := make([]int64, 2)
		for i := 0; i < 2; i++ {
			d := i
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, i), Enabled: true,
				ParentID: &lane.ID, Depth: &d}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			slots[i] = s.ID
		}
		// The mouth blocker carries a payload, so it is not itself an empty
		// candidate — it is only in the way.
		_, berr := db.Exec(
			`INSERT INTO bins (bin_type_id, label, node_id, status, payload_code)
			 VALUES ($1,$2,$3,'available','PANEL-A')`,
			blockerBT.ID, name+"-BLOCKER", slots[0])
		testutil.MustNoErr(t, berr, "mouth blocker")

		testutil.MustNoErr(t, db.QueryRow(
			`INSERT INTO bins (bin_type_id, label, node_id, status)
			 VALUES ($1,$2,$3,'available') RETURNING id`,
			btID, name+"-EMPTY", slots[1]).Scan(&emptyID), "buried empty")
		return lane.ID, emptyID
	}

	// HELD LANE FIRST, so its buried empty takes the lower bin id and wins the
	// tiebreak against an equally-buried candidate in the free lane.
	heldLane, heldEmpty = build(prefix + "-LANE-HELD")
	freeLane, freeEmpty = build(prefix + "-LANE-FREE")
	if heldEmpty >= freeEmpty {
		t.Fatalf("fixture: held-lane empty id %d does not precede the free-lane one %d, so "+
			"the finder would divert for ordering reasons rather than dig-blindness",
			heldEmpty, freeEmpty)
	}
	return heldLane, freeLane, heldEmpty, freeEmpty
}

// selectedEmpty runs the real cascade for an empty pull scoped to the group and
// reports which carrier it chose — through OutcomeReshuffle's BuriedError,
// which is where a buried pick surfaces.
func selectedEmpty(t *testing.T, d *Dispatcher, groupName, destName string) (binID int64, laneID int64) {
	t.Helper()
	got := d.finder.FindSourceForNeed(SourceNeed{
		SourceNode: groupName, DeliveryNode: destName, Intent: IntentEmpty,
	})
	switch got.Outcome {
	case OutcomeReshuffle:
		if got.Buried == nil || got.Buried.Bin == nil {
			t.Fatalf("OutcomeReshuffle with no buried carrier: %+v", got)
		}
		return got.Buried.Bin.ID, got.Buried.LaneID
	case OutcomeFound:
		if got.Bin == nil {
			t.Fatalf("OutcomeFound with no bin: %+v", got)
		}
		return got.Bin.ID, 0
	default:
		t.Fatalf("outcome = %v cause=%q — the fixture no longer produces a buried empty pick",
			got.Outcome, got.QueueCause)
		return 0, 0
	}
}

// ── (a) EXCAVATION — the park self-heals in one tick ────────────────────────
//
// The dig holds the lane AND claims its target. The claim is what does the
// work: every empty finder excludes claimed bins, so the next selection cannot
// see the held lane's carrier and diverts to the free lane by itself.
//
// This case needs no fix, and pinning it is what stops MG3-1b from being
// measured against it.
func TestPin_DigBlind_ExcavationSelfHealsOnTheClaim(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	heldLane, _, heldEmpty, freeEmpty := twoLaneAllBuried(t, db, "EXC", "EXC-45x58")

	group, err := db.GetNodeByName("EXC-GRP")
	testutil.MustNoErr(t, err, "group")

	// Before the dig: the finder prefers the held lane's carrier. This is the
	// dig-blindness itself, stated before anything is held.
	got, _ := selectedEmpty(t, d, group.Name, sd.LineNode.Name)
	if got != heldEmpty {
		t.Fatalf("pre-dig selection = bin %d, want %d — the fixture's tiebreak no longer "+
			"points the finder at the lane about to be held", got, heldEmpty)
	}

	// The excavation takes the lane AND claims its target, in that order,
	// exactly as CreateCompoundChildren does inside its transaction.
	digger := digHolder(t, db, "EXC-digger")
	if !d.laneLock.TryLock(heldLane, digger.ID) {
		t.Fatal("could not take the dig lock")
	}
	_, err = db.Exec(`UPDATE bins SET claimed_by = $1 WHERE id = $2`, digger.ID, heldEmpty)
	testutil.MustNoErr(t, err, "the compound claims its target")

	got, _ = selectedEmpty(t, d, group.Name, sd.LineNode.Name)
	if got != freeEmpty {
		t.Errorf("post-claim selection = bin %d, want the free lane's %d. The excavation case "+
			"heals through the CLAIM, not through any dig awareness — every empty finder "+
			"excludes claimed bins, and that is the whole mechanism", got, freeEmpty)
	}
}

// ── (b) §R.101 SOURCE LOCK — REVERSED BY MG3-1b ────────────────────────────
//
// A mouth row held by a demand. No compound, so nothing is claimed, so there
// was nothing for the finder to skip: it re-picked the same buried carrier in
// the same held lane every tick, indefinitely, while a free lane with an
// equally diggable empty sat one row away and was never considered. Not bounded
// by any dig's duration, because the hold is not a dig that finishes.
//
// THIS ASSERTION IS INVERTED FROM WHAT MG3-0 PINNED, and the inversion IS the
// deliverable. The pinned text read "re-picks the same carrier forever"; it now
// reads "diverts to the free lane". Everything else about the fixture is
// unchanged, so the diff is exactly the behaviour change and nothing else.
//
// FREE-SIBLING DIVERSION, the first of the post-fix trio.
func TestDigExclusion_SourceLockDivertsToTheFreeLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	heldLane, _, heldEmpty, freeEmpty := twoLaneAllBuried(t, db, "SRC", "SRC-45x58")

	group, err := db.GetNodeByName("SRC-GRP")
	testutil.MustNoErr(t, err, "group")

	// The hold, with NO claim on anything. That absence is the whole difference
	// from the excavation case above.
	holder := digHolder(t, db, "SRC-source-lock")
	if !d.laneLock.TryLock(heldLane, holder.ID) {
		t.Fatal("could not take the source lock")
	}

	// Three ticks, unchanged state. The free lane every time — the exclusion is
	// a property of the query, not a retry that eventually wanders there.
	for i := 0; i < 3; i++ {
		got, _ := selectedEmpty(t, d, group.Name, sd.LineNode.Name)
		if got == heldEmpty {
			t.Fatalf("tick %d selected bin %d, in dig-held lane %d, while bin %d sat equally "+
				"diggable in a FREE lane. That is the source-lock loop: nothing is claimed, "+
				"so nothing hides the carrier, and the order re-parks under lane-locked "+
				"forever because no dig is running on its behalf",
				i+1, heldEmpty, heldLane, freeEmpty)
		}
		if got != freeEmpty {
			t.Fatalf("tick %d selected bin %d, want the free lane's %d", i+1, got, freeEmpty)
		}
	}
	t.Logf("REVERSED: three consecutive selections all chose bin %d in the FREE lane, with "+
		"bin %d hidden behind a source lock on lane %d. Before MG3-1b every one of these "+
		"chose the held carrier and parked.", freeEmpty, heldEmpty, heldLane)
}

// ── NARROWNESS — one dug lane hides only itself ─────────────────────────────
//
// The second of the post-fix trio, and the one that keeps the fix from being
// worse than the defect. Excluding the whole GROUP when one of its lanes is dug
// would starve every order that could legitimately have been served from a
// sibling — a wait with no cause anybody could name, in exchange for avoiding a
// wait that at least had one.
func TestDigExclusion_HidesTheDugLaneAndNothingElse(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	heldLane, _, heldEmpty, freeEmpty := twoLaneAllBuried(t, db, "NARROW", "NARROW-45x58")

	group, err := db.GetNodeByName("NARROW-GRP")
	testutil.MustNoErr(t, err, "group")

	holder := digHolder(t, db, "NARROW-holder")
	if !d.laneLock.TryLock(heldLane, holder.ID) {
		t.Fatal("could not take the lock")
	}

	got, _ := selectedEmpty(t, d, group.Name, sd.LineNode.Name)
	if got != freeEmpty {
		t.Fatalf("selected bin %d, want the sibling lane's %d. One dug lane must hide "+
			"itself and nothing else — a group-wide exclusion trades a nameable wait for "+
			"an unnameable one", got, freeEmpty)
	}
	_ = heldEmpty
}

// ── OWN / LANE-OWNER EXEMPTION ──────────────────────────────────────────────
//
// The third, and the one dig_exclusion.go exists because of. In expose mode the
// lane lock is TRANSFERRED to the complex parent to protect the carrier the dig
// just uncovered. If the exclusion were owner-blind, that parent would resume,
// re-resolve, and be shut out of the lane BY ITS OWN LOCK — unable to see the
// carrier its own dig exposed for it. It would resolve to the next buried one
// and dig again, and the ring arrests.
//
// So the owner is exempt, and so is the order the lock is held on behalf of.
func TestDigExclusion_TheDigsOwnerStillSeesItsLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	heldLane, _, heldEmpty, freeEmpty := twoLaneAllBuried(t, db, "OWNER", "OWNER-45x58")

	group, err := db.GetNodeByName("OWNER-GRP")
	testutil.MustNoErr(t, err, "group")

	owner := digHolder(t, db, "OWNER-the-dig")
	if !d.laneLock.TryLock(heldLane, owner.ID) {
		t.Fatal("could not take the lock")
	}

	// Remove the free lane's carrier, so the ONLY candidate is inside the held
	// lane. An owner-blind exclusion finds nothing; the owner must find its own.
	_, err = db.Exec(`DELETE FROM bins WHERE id = $1`, freeEmpty)
	testutil.MustNoErr(t, err, "remove the free-lane carrier")

	got := d.finder.FindSourceForNeed(SourceNeed{
		SourceNode: group.Name, DeliveryNode: sd.LineNode.Name, Intent: IntentEmpty,
		Asker: reservations.AskerFor(owner.ID, owner.ID),
	})
	if got.Outcome == OutcomeWait {
		t.Fatalf("the dig's OWNER was shut out of its own lane (cause %q). That is the "+
			"arrest dig_exclusion.go was written about: the lock that protects the carrier "+
			"hid it from the only order allowed to take it", got.QueueCause)
	}
	binID := int64(0)
	if got.Buried != nil && got.Buried.Bin != nil {
		binID = got.Buried.Bin.ID
	} else if got.Bin != nil {
		binID = got.Bin.ID
	}
	if binID != heldEmpty {
		t.Errorf("owner selected bin %d, want its own lane's %d", binID, heldEmpty)
	}
}
