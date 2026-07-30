package store

import (
	"errors"
	"testing"
)

// supply_refusals_test.go — the storage half of the supplier→customer channel.
//
// LAYOUT-AGNOSTIC BY CONSTRUCTION, which is why these do NOT run twice. §6 asks
// for every strand A test to run once per board layout, and that is right for
// the view and the endpoint — a shared window's payload card and a dedicated
// home's position card reach the key differently. They reach the SAME key
// though, and this file is below that join: nothing here can tell the layouts
// apart, so a second pass would assert the same rows twice and imply a
// distinction that does not exist. The layout tests belong on the card.

func TestSupplyRefusal_OpenIsIdempotentOnTheCard(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	if err := db.OpenSupplyRefusal("SMN_014", "PART-A", "Bin Loader"); err != nil {
		t.Fatalf("first refusal: %v", err)
	}
	first, err := db.GetSupplyRefusal("SMN_014", "PART-A")
	if err != nil {
		t.Fatalf("get after first: %v", err)
	}

	// A second press is the same statement said twice — not a new one. It must
	// not mint a second row, and it must not restart the clock on the first: the
	// refusal that stands is the one already made, and its timestamp is what the
	// cell was told.
	if err := db.OpenSupplyRefusal("SMN_014", "PART-A", "Someone Else"); err != nil {
		t.Fatalf("second refusal: %v", err)
	}
	second, err := db.GetSupplyRefusal("SMN_014", "PART-A")
	if err != nil {
		t.Fatalf("get after second: %v", err)
	}
	if !second.RefusedAt.Equal(first.RefusedAt) {
		t.Errorf("refused_at moved on a second press: %v → %v", first.RefusedAt, second.RefusedAt)
	}
	if second.RefusedBy != first.RefusedBy {
		t.Errorf("refused_by overwritten on a second press: %q → %q", first.RefusedBy, second.RefusedBy)
	}

	open, err := db.ListOpenSupplyRefusals()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 1 {
		t.Errorf("got %d open rows, want exactly 1 — the card is the key", len(open))
	}
}

func TestSupplyRefusal_KeyIsTheCardNotThePayload(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// The same payload refused at two different windows is two statements by two
	// people about two racks. "SMN_014 says no" is not "the loader says no".
	for _, node := range []string{"SMN_014", "SMN_015"} {
		if err := db.OpenSupplyRefusal(node, "PART-A", "Bin Loader"); err != nil {
			t.Fatalf("refuse at %s: %v", node, err)
		}
	}
	open, err := db.ListOpenSupplyRefusals()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("got %d rows, want 2 — one per (window, payload)", len(open))
	}

	// And clearing one must not clear the other.
	if err := db.DeleteSupplyRefusal("SMN_014", "PART-A"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetSupplyRefusal("SMN_015", "PART-A"); err != nil {
		t.Errorf("the other window's refusal was cleared too: %v", err)
	}
}

func TestSupplyRefusal_AckRecordsFirstAnswerOnly(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	if err := db.OpenSupplyRefusal("SMN_014", "PART-A", "Bin Loader"); err != nil {
		t.Fatalf("refuse: %v", err)
	}

	// ack_at IS NULL is a real state — told, not answered — and it is the state
	// the whole project started from: the information was on a screen and nobody
	// had acted on it.
	before, _ := db.GetSupplyRefusal("SMN_014", "PART-A")
	if before.Answered() {
		t.Fatal("a fresh refusal reports as answered")
	}

	ok, err := db.AckSupplyRefusal("SMN_014", "PART-A", "wait", "SNF2")
	if err != nil || !ok {
		t.Fatalf("first ack: ok=%v err=%v", ok, err)
	}
	after, _ := db.GetSupplyRefusal("SMN_014", "PART-A")
	if !after.Answered() || after.AckChoice != "wait" || after.AckProcessID != "SNF2" {
		t.Errorf("ack not recorded: answered=%v choice=%q process=%q",
			after.Answered(), after.AckChoice, after.AckProcessID)
	}

	// A second answer can only be a double-tap or a second tab on the same
	// board. The operator's first answer is the real one, and the caller is told
	// it did not take rather than being handed a silent success.
	ok, err = db.AckSupplyRefusal("SMN_014", "PART-A", "changeover", "SNF3")
	if err != nil {
		t.Fatalf("second ack errored: %v", err)
	}
	if ok {
		t.Error("second ack reported as recorded — first answer must win")
	}
	final, _ := db.GetSupplyRefusal("SMN_014", "PART-A")
	if final.AckChoice != "wait" || final.AckProcessID != "SNF2" {
		t.Errorf("second ack overwrote the first: choice=%q process=%q",
			final.AckChoice, final.AckProcessID)
	}
}

func TestSupplyRefusal_DeleteTakesTheAckWithIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	if err := db.OpenSupplyRefusal("SMN_014", "PART-A", "Bin Loader"); err != nil {
		t.Fatalf("refuse: %v", err)
	}
	if _, err := db.AckSupplyRefusal("SMN_014", "PART-A", "wait", "SNF2"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := db.DeleteSupplyRefusal("SMN_014", "PART-A"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetSupplyRefusal("SMN_014", "PART-A"); !errors.Is(err, ErrNoOpenRefusal) {
		t.Fatalf("row survived the delete: %v", err)
	}

	// THE POINT OF THE TEST. A row left behind carrying ack_choice='wait' would
	// make the NEXT refusal look already-answered, and the cell's modal — which
	// fires on "refused and not yet answered" — would never fire again for that
	// card. Both endings delete: a LOAD (the parts arrived) and UNDO (mis-tap).
	if err := db.OpenSupplyRefusal("SMN_014", "PART-A", "Bin Loader"); err != nil {
		t.Fatalf("re-refuse: %v", err)
	}
	again, err := db.GetSupplyRefusal("SMN_014", "PART-A")
	if err != nil {
		t.Fatalf("get after re-refuse: %v", err)
	}
	if again.Answered() {
		t.Error("a NEW refusal came back already-answered — the previous ack outlived its " +
			"refusal and the cell would never be asked again")
	}
}

func TestSupplyRefusal_DeleteIsHarmlessWhenNothingIsOpen(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	// The clear-on-load hook fires on every LOAD at a window, and almost every
	// load happens with no refusal standing. That must be a cheap no-op, not an
	// error the caller has to filter.
	if err := db.DeleteSupplyRefusal("SMN_014", "PART-A"); err != nil {
		t.Errorf("delete with nothing open: %v", err)
	}
}
