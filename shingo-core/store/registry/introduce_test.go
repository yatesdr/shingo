//go:build docker

package registry_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/registry"
)

// Self-introduction — an edge may say IT EXISTS; only a human says WHAT IT IS.
//
// The enrollment deploy deleted the branch that let an unknown edge register,
// and it was right about the defect: the old station id was composed from two
// struct defaults, so every unconfigured edge asserted the SAME string. Two of
// them took turns owning one row while the clause that let them do it erased
// the evidence there had been two.
//
// What that fix got wrong is the distinction these tests pin: the hazard was
// COLLISION, not CREATION. A minted uid cannot reach another station's row.
// So creation is safe, and the tests below are the reason it is safe rather
// than the assertion that it is.

func mustIntroduce(t *testing.T, db *store.DB, uid, host string) *registry.Edge {
	t.Helper()
	e, err := registry.Introduce(db.DB, uid, host, "v-test")
	if err != nil {
		t.Fatalf("Introduce(%s): %v", uid, err)
	}
	return e
}

// TestIntroduce_LandsUnclaimedAndDistinguishable is the guard that makes
// self-introduction different from the branch that was deleted.
//
// That branch called Enroll and produced a row indistinguishable from a station
// an operator had deliberately created — "an unregistered machine silently
// became this station". The whole difference is that this row announces itself
// as unanswered, so the assertion is not "a row exists" but "the row is
// visibly NOT an enrollment".
func TestIntroduce_LandsUnclaimedAndDistinguishable(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	e := mustIntroduce(t, db, "stn-aaaaaaaaaaaaaaaa", "pi-new")

	got, err := registry.GetByUID(db.DB, e.StationUID)
	if err != nil {
		t.Fatalf("GetByUID: %v", err)
	}
	if got.ClaimedAt != nil {
		t.Fatalf("claimed_at = %v, want NULL — an introduced station must be visibly "+
			"unacknowledged, which is the entire difference from the deleted auto-enroll branch",
			got.ClaimedAt)
	}
	if got.DisplayName != "" {
		t.Errorf("display_name = %q, want empty — a name is what a human supplies, and "+
			"defaulting one here would make an unclaimed row read as an accounted-for station",
			got.DisplayName)
	}
}

// TestIntroduce_CannotTakeOverAnotherStation is the property the whole change
// rests on.
//
// The original defect was not that a machine could create a station. It was
// that it could create the SAME one as another machine, because the id was
// DERIVED — every unconfigured edge produced 'plant-a.line-1'. Introduce is
// only safe because the uid arriving is drawn, not composed, and the database
// enforces that rather than trusting it.
func TestIntroduce_CannotTakeOverAnotherStation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// A real, claimed station with history attached to it.
	const established = "stn-established00"
	if _, err := registry.Enroll(db.DB, established, "SPRINGFIELD / LINE 1", established); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	// A second box introduces itself with THE SAME uid — the collision case.
	if _, err := registry.Introduce(db.DB, established, "pi-imposter", "v-test"); err == nil {
		t.Fatal("Introduce succeeded against an existing station uid — it must never be able " +
			"to create or overwrite a row that already exists; that is the take-turns defect")
	}

	// The established station is untouched: same name, still claimed.
	got, err := registry.GetByUID(db.DB, established)
	if err != nil {
		t.Fatalf("GetByUID: %v", err)
	}
	if got.DisplayName != "SPRINGFIELD / LINE 1" {
		t.Fatalf("display_name = %q — an introduce reached an established station's row",
			got.DisplayName)
	}
	if got.ClaimedAt == nil {
		t.Fatal("claimed_at went NULL — an introduce un-acknowledged an established station")
	}
}

// TestClaim_IsTheHumanAnswerAndIsOneWay pins that claiming records the act, not
// the state.
func TestClaim_IsTheHumanAnswerAndIsOneWay(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	e := mustIntroduce(t, db, "stn-bbbbbbbbbbbbbbbb", "pi-new")

	ok, err := registry.Claim(db.DB, e.StationUID, "WELD CELL 2")
	if err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	claimed, err := registry.GetByUID(db.DB, e.StationUID)
	if err != nil {
		t.Fatalf("GetByUID: %v", err)
	}
	if claimed.ClaimedAt == nil {
		t.Fatal("claimed_at still NULL after Claim")
	}
	if claimed.DisplayName != "WELD CELL 2" {
		t.Fatalf("display_name = %q, want %q", claimed.DisplayName, "WELD CELL 2")
	}

	// A later rename must not disturb the acknowledgement: the question
	// claimed_at answers is "did anybody ever look", not "what is it called".
	first := *claimed.ClaimedAt
	if _, err := registry.SetDisplayName(db.DB, e.StationUID, "WELD CELL TWO"); err != nil {
		t.Fatalf("SetDisplayName: %v", err)
	}
	after, err := registry.GetByUID(db.DB, e.StationUID)
	if err != nil {
		t.Fatalf("GetByUID: %v", err)
	}
	if after.ClaimedAt == nil || !after.ClaimedAt.Equal(first) {
		t.Fatalf("claimed_at moved on rename: %v -> %v", first, after.ClaimedAt)
	}
}

// TestRegister_DoesNotClearTheClaim is why claimed_at is its own column.
//
// Register writes status='active' on EVERY register, so anything recorded in
// status is erased by the next one. An acknowledgement that a heartbeat can
// undo is not an acknowledgement.
func TestRegister_DoesNotClearTheClaim(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	e := mustIntroduce(t, db, "stn-cccccccccccccccc", "pi-new")
	if _, err := registry.Claim(db.DB, e.StationUID, "NAMED"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := registry.Register(db.DB, e.StationUID, "pi-new", "inst-1", "v2"); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	got, err := registry.GetByUID(db.DB, e.StationUID)
	if err != nil {
		t.Fatalf("GetByUID: %v", err)
	}
	if got.ClaimedAt == nil {
		t.Fatal("registering un-claimed the station — claimed_at must survive registration, " +
			"which is the whole reason it is not a status value")
	}
	if got.DisplayName != "NAMED" {
		t.Fatalf("display_name = %q after registers, want NAMED", got.DisplayName)
	}
}
