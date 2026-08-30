//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// lane_hold_kind_docker_test.go — THE PIN THE §R.101 SPLIT DID NOT HAVE.
//
// ── WHAT §R.101 DID, AND WHAT IT DID NOT TELL THE READERS ─────────────────
//
// §R.101 generalized every demand's SOURCE hold from ModeOutbound to ModeDig:
// "a demand that resolves onto a bin owns that lane until the bin leaves by its
// mover". Generalizing a value is an extraction, and law 3's rider says an
// extraction owes an argument that the sites share ONE QUESTION. mode='dig' was
// answering two:
//
//	is this lane held EXCLUSIVELY   — admitMouth, and every keep-out decision
//	is an EXCAVATION running here   — the causes, and admission's refusal name
//
// Before §R.101 those were the same set of rows. After it, every ordinary
// retrieve holding the lane it sources from answered YES to the second, and the
// readers duly reported an excavation that had never been planned. It also made
// CauseLaneHeldTraffic nearly unreachable, since a source lock outranked it on
// any lane a demand had resolved onto.
//
// ── WHAT THIS FILE ASSERTS, AND WHAT IT DELIBERATELY DOES NOT ─────────────
//
// It asserts that the two questions now get two answers, AND — first, because it
// is the one that could hurt — that the exclusivity answer did not move. §R.101
// is a ruling: a demand owns the lane it sourced from. Reading the excavation
// question where a keep-out is decided would reverse it and let a second order
// into a lane a demand owns. So IsLocked stays true for a source lock here, on
// purpose, and that assertion is as load-bearing as the split itself.
//
// ── THE SUB-TEST THAT IS NOT A CONVENIENCE ────────────────────────────────
//
// "a demand that digs the lane it already sourced from" is the one that failed
// before this change, and it failed silently. The brief proposed reading
// reserved_by on the grounds that it was "already half-built — written at five
// sites and read at none". The census confirms the read half; the WRITE half was
// not sound. Measured against the database, not argued:
//
//	after the §R.101 source hold      mode="dig" reserved_by="lanegate"
//	after the SAME order's TryLockFor mode="dig" reserved_by="lanegate"   ← stale
//
// admitMouth returns admitIdempotent when the owner already holds the lane in
// the requested mode, and the idempotent arm wrote nothing at all. Before §R.101
// that case arrived holding an OUTBOUND row, so the verdict was admitUpgrade and
// that arm's UPDATE rewrote the tag; §R.101 made the modes equal and turned the
// upgrade into a no-op. A genuine excavation then wore the source-lock tag —
// the wrong-name failure pointing the wrong way, which is the expensive
// direction. AcquireLanesFor now promotes the tag on that arm, never demotes it.
//
// MUTATION (driven this session, fires): delete the promote block in
// AcquireLanesFor's admitIdempotent arm. The third sub-test below reports the
// lane holding no excavation while the order is digging it.
func TestLaneHoldKind_ASourceLockIsNotAnExcavation(t *testing.T) {
	t.Parallel()

	t.Run("a demand's source hold holds the lane exclusively and is NOT an excavation", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		testdb.SetupStandardData(t, db)
		_, laneID, _ := gatedLane(t, db, "HKIND-SRC", "")
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

		demand := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "hkind-src" })
		adm := d.acquireOrderLanes(demand.ID, []laneHold{{laneID: laneID, mode: reservations.ModeDig}})
		admitted, err := adm.admitted, adm.err
		if err != nil || !admitted {
			t.Fatalf("the demand could not take its own source hold: admitted=%v err=%v", admitted, err)
		}

		// THE EXCLUSIVITY ANSWER MUST NOT HAVE MOVED. This is §R.101 itself, and
		// it is asserted first because reading the excavation question here
		// instead is the mistake that would let a second order into a lane a
		// demand owns.
		if !d.laneLock.IsLocked(laneID) {
			t.Fatal("a demand's §R.101 source hold stopped excluding other orders. The lane is held " +
				"and IsLocked is the keep-out read — reporting free here admits a second order into a " +
				"single-file corridor whose bin the first one has already resolved onto, which is the " +
				"ruling reversed rather than renamed")
		}

		owner, err := d.laneLock.ExcavationOwner(laneID)
		if err != nil {
			t.Fatalf("read the excavation owner: %v", err)
		}
		if owner != 0 {
			t.Fatalf("order %d holds lane %d as a §R.101 SOURCE lock, and the lane reports excavation "+
				"owner %d. No reshuffle was planned, no parent exists and no legs will run — so an "+
				"engineer sent here by a lane-dig-active wait finds nothing to look at. That is the "+
				"alarm-with-the-wrong-name failure, not a missing alarm",
				demand.ID, laneID, owner)
		}
	})

	t.Run("a dig's lock IS an excavation", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		testdb.SetupStandardData(t, db)
		_, laneID, _ := gatedLane(t, db, "HKIND-DIG", "")
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

		dig := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "hkind-dig" })
		if !d.laneLock.TryLock(laneID, dig.ID) {
			t.Fatal("the dig could not take the lane")
		}

		owner, err := d.laneLock.ExcavationOwner(laneID)
		if err != nil {
			t.Fatalf("read the excavation owner: %v", err)
		}
		if owner != dig.ID {
			t.Fatalf("excavation owner of lane %d = %d, want the digging order %d. Every production "+
				"dig comes through TryLockFor, so if this reads 0 the split has stopped seeing "+
				"reshuffles at all and every dig wait is about to be labelled a source lock",
				laneID, owner, dig.ID)
		}
	})

	t.Run("a demand that digs the lane it already sourced from is an excavation", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		testdb.SetupStandardData(t, db)
		_, laneID, _ := gatedLane(t, db, "HKIND-SELF", "")
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

		demand := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "hkind-self" })
		// 1. The §R.101 source hold, exactly as resolveOrderLaneHolds takes it.
		adm := d.acquireOrderLanes(demand.ID, []laneHold{{laneID: laneID, mode: reservations.ModeDig}})
		admitted, err := adm.admitted, adm.err
		if err != nil || !admitted {
			t.Fatalf("source hold not admitted: admitted=%v err=%v", admitted, err)
		}
		// 2. The bin turns out to be buried and planBuriedReshuffle re-parents the
		//    demand onto its OWN excavation — the plain dig shape, same order id.
		if !d.laneLock.TryLockFor(laneID, demand.ID, reservations.AskerFor(demand.ID, demand.ID)) {
			t.Fatal("the demand could not take its own lane for its own dig")
		}

		owner, err := d.laneLock.ExcavationOwner(laneID)
		if err != nil {
			t.Fatalf("read the excavation owner: %v", err)
		}
		if owner != demand.ID {
			t.Fatalf("order %d took its source hold on lane %d and then dug that lane for itself, and "+
				"the lane reports excavation owner %d. The row is the SAME row — admitMouth answers "+
				"idempotent because the mode already matches — so unless the acquire promotes the tag, "+
				"a real reshuffle keeps the source-lock name for its whole life and every wait behind "+
				"it tells the engineer there is no dig here",
				demand.ID, laneID, owner)
		}
	})
}

// TestDigWait_SentenceNamesADigOnlyWhenOneIsDigging is the OPERATOR-facing half.
//
// digWaitFor feeds digClause, which renders the literal words "— dig %d is
// working this lane" onto a rearranging wait. It read DigOwner, which since
// §R.101 answers for any exclusive holder — so a plain retrieve sourcing from
// the lane was announced to the floor as an excavation, with an id attached. An
// id makes a false sentence actionable, which is worse than the vaguer sentence
// it replaced.
//
// THIS PIN EXISTS BECAUSE THE SUITE DID NOT NOTICE. Reverting digWaitFor to
// DigOwner leaves the entire dispatch docker suite green — driven, not assumed —
// because every test that reaches this clause plants its hold with TryLock and
// both readers agree on a real dig. The disagreement only shows on a source
// lock, and nothing built one here until now.
//
// MUTATION (driven this session, fires): put DigOwner back in digWaitFor. The
// first sub-test reports the sentence naming a dig.
func TestDigWait_SentenceNamesADigOnlyWhenOneIsDigging(t *testing.T) {
	t.Parallel()

	t.Run("a source lock puts no dig in the operator's sentence", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		testdb.SetupStandardData(t, db)
		_, laneID, _ := gatedLane(t, db, "DWAIT-SRC", "")
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

		holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "dwait-src" })
		if err := reservations.AcquireLanesFor(db.DB, holder.ID, reservations.ModeDig,
			reservations.Anyone, laneGateReservedBy, laneID); err != nil {
			t.Fatalf("plant the source lock: %v", err)
		}

		if got := digWaitFor(d.laneLock, laneID); got != 0 {
			t.Fatalf("digWaitFor = %d on a lane held by a §R.101 SOURCE lock, want 0. Order %d is "+
				"sourcing from this lane, not excavating it — no reshuffle exists to name",
				got, holder.ID)
		}
		sentence := rearrangingSentence(QueueParams{
			Lane: "DWAIT-SRC", Payload: "PART-1", DigOrderID: digWaitFor(d.laneLock, laneID)})
		if strings.Contains(sentence, "dig ") {
			t.Errorf("the operator reads %q. There is no dig: the lane is held by a demand that "+
				"resolved onto a bin in it, and naming an excavation with an id is a false sentence "+
				"somebody can act on — worse than the lane alone, which is what this renders now",
				sentence)
		}
	})

	t.Run("a real excavation is still named", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		testdb.SetupStandardData(t, db)
		_, laneID, _ := gatedLane(t, db, "DWAIT-DIG", "")
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

		dig := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "dwait-dig" })
		if !d.laneLock.TryLock(laneID, dig.ID) {
			t.Fatal("the dig could not take the lane")
		}

		if got := digWaitFor(d.laneLock, laneID); got != dig.ID {
			t.Fatalf("digWaitFor = %d, want the digging order %d. Without this the sub-test above is "+
				"satisfied by a clause that has stopped appearing at all, and §R.111's complaint — a "+
				"wait that names no excavation leaves the operator with one word and a lookup — is "+
				"back with the fix's name on it", got, dig.ID)
		}
		sentence := rearrangingSentence(QueueParams{
			Lane: "DWAIT-DIG", Payload: "PART-1", DigOrderID: digWaitFor(d.laneLock, laneID)})
		if !strings.Contains(sentence, "is working this lane") {
			t.Errorf("the operator reads %q, and a real excavation is running — the clause must be "+
				"there", sentence)
		}
	})
}

// TestAdmit_ForeignSourceLockRefusesUnderItsOwnName is admission arm 1's half of
// the split: the refusal is unchanged and only the word for it moves.
//
// Both halves are asserted together because either alone misleads. A test that
// only checked the name would pass just as well if the arm had stopped refusing.
//
// MUTATION (driven this session, fires): return CauseLaneDigActive
// unconditionally from the arm. This test fails on the cause.
//
// A NOTE THIS FILE GOT WRONG FIRST TIME, corrected by the gate rather than by
// reading: it claimed "every other admission test stays green, because they all
// plant their holds with AcquireLanes(…, "test", …) and a foreign tag is not an
// excavation either". Two of them do exactly that AND assert the resulting
// cause — TestAdmit_ForeignDigRefusesAndOwnDigDoesNot and TestLaneGate_CauseDig
// — so they failed until their fixtures were tagged. The tag is now part of
// what a dig fixture MEANS; a new one that omits it will be read as a source
// lock, and that is the first thing to check when a dig test reports
// lane-held-source.
func TestAdmit_ForeignSourceLockRefusesUnderItsOwnName(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	_, laneID, s0 := gatedLane(t, db, "ADM-SRC", "")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	order := admitOrder(t, db, "adm-src", s0)

	// A stranger's §R.101 SOURCE hold — the lane gate's tag, not the lock's.
	stranger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "adm-src-holder" })
	if err := reservations.AcquireLanesFor(db.DB, stranger.ID, reservations.ModeDig,
		reservations.Anyone, laneGateReservedBy, laneID); err != nil {
		t.Fatalf("plant a source lock: %v", err)
	}

	v, err := d.admit(admissionSituation{order: order, sourceNode: s0})
	if err != nil {
		t.Fatalf("admit against a foreign source lock: %v", err)
	}
	if v.Admitted() {
		t.Fatal("admitted into a lane another demand has resolved onto. §R.101 rules that a demand " +
			"owns the lane it sources from until the bin leaves by its mover — the naming split must " +
			"not weaken the keep-out, only the word for it")
	}
	if v.Cause() != CauseLaneHeldSource {
		t.Errorf("cause = %q, want %q. The holder is an ordinary demand with a source hold: no "+
			"reshuffle was planned and none will run, so %q sends the next engineer looking for an "+
			"excavation that does not exist — and it clears on a different event, in a different "+
			"time, from a dig",
			v.Cause(), CauseLaneHeldSource, CauseLaneDigActive)
	}

	// AND A REAL EXCAVATION KEEPS ITS OWN NAME. Without this the test above is
	// satisfied by a split that has collapsed the other way round.
	if _, err := reservations.ReleaseLanesByOwner(db.DB, stranger.ID); err != nil {
		t.Fatalf("release the source lock: %v", err)
	}
	digger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "adm-src-digger" })
	if !d.laneLock.TryLock(laneID, digger.ID) {
		t.Fatal("the dig could not take the lane")
	}
	v, err = d.admit(admissionSituation{order: order, sourceNode: s0})
	if err != nil {
		t.Fatalf("admit against a foreign excavation: %v", err)
	}
	if v.Admitted() || v.Cause() != CauseLaneDigActive {
		t.Errorf("admitted=%v cause=%q against a real reshuffle, want refused with %q",
			v.Admitted(), v.Cause(), CauseLaneDigActive)
	}
}
