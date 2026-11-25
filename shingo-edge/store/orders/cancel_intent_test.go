package orders

import (
	"testing"

	"shingo/protocol/testutil"
)

// cancel_intent_test.go — orders.cancel_intent's writers and its read fields
// (domain.CancelIntent): the cancel this station asked Core for, which the
// row carries until Core's word ends it.

const (
	cancelAsked   = `{"reason":"cancelled by operator","by":"person","env":"e-2","sent_at":"2026-11-25T12:00:00Z"}`
	cancelRefused = `{"reason":"cancelled by operator","env":"e-2","sent_at":"2026-11-25T12:00:00Z","refused":"set down"}`
)

// TestCancelIntent_ReadOnALiveRowOnly: a live row with an unanswered cancel
// reads cancel sent, with its clock; a refused one reads Core's sentence and
// not sent; an ended row reads neither, whatever its column holds.
func TestCancelIntent_ReadOnALiveRowOnly(t *testing.T) {
	db := openWindowDB(t)
	for _, r := range []struct{ uuid, status, intent string }{
		{"asked", "in_transit", cancelAsked},
		{"refused", "delivered", cancelRefused},
		{"ended", "cancelled", cancelAsked},
		{"none", "in_transit", ``},
	} {
		_, err := db.Exec(`INSERT INTO orders (uuid, status, cancel_intent) VALUES (?, ?, ?)`, r.uuid, r.status, r.intent)
		testutil.MustNoErr(t, err, "insert "+r.uuid)
	}
	for _, w := range []struct {
		uuid    string
		sent    bool
		refused string
	}{{"asked", true, ""}, {"refused", false, "set down"}, {"ended", false, ""}, {"none", false, ""}} {
		o, err := GetByUUID(db, w.uuid)
		testutil.MustNoErr(t, err, "read "+w.uuid)
		if o.CancelSent != w.sent || o.CancelRefused != w.refused || (o.CancelSentAt != nil) != w.sent {
			t.Errorf("%s reads sent=%v at %v refused=%q, want sent=%v refused=%q", w.uuid, o.CancelSent, o.CancelSentAt,
				o.CancelRefused, w.sent, w.refused)
		}
	}
}

// TestCancelIntent_TheTerminalClearsIt: Core's word that ends the order clears
// the cancel in the statement that clears release_intent; a live word keeps
// it. The Edge's own terminal write clears it too.
func TestCancelIntent_TheTerminalClearsIt(t *testing.T) {
	db := openWindowDB(t)
	ids := map[string]int64{}
	for _, uuid := range []string{"live", "ended", "own"} {
		res, err := db.Exec(`INSERT INTO orders (uuid, status, cancel_intent, release_intent, core_version) VALUES (?, 'in_transit', ?, '{"station_wait":1}', 1)`,
			uuid, cancelAsked)
		testutil.MustNoErr(t, err, "insert "+uuid)
		ids[uuid], err = res.LastInsertId()
		testutil.MustNoErr(t, err, "id of "+uuid)
	}
	wrote, err := ApplyCoreWord(db, ids["live"], CoreWordRow{Status: "staged", Version: 2})
	testutil.MustNoErr(t, err, "a live word")
	if !wrote {
		t.Fatal("the live word wrote nothing")
	}
	wrote, err = ApplyCoreWord(db, ids["ended"], CoreWordRow{Status: "cancelled", Version: 2})
	testutil.MustNoErr(t, err, "an ending word")
	if !wrote {
		t.Fatal("the ending word wrote nothing")
	}
	testutil.MustNoErr(t, UpdateStatus(db, ids["own"], "cancelled"), "the Edge's own terminal")
	for uuid, want := range map[string]string{"live": cancelAsked, "ended": "", "own": ""} {
		var got string
		testutil.MustNoErr(t, db.QueryRow(`SELECT cancel_intent FROM orders WHERE id = ?`, ids[uuid]).Scan(&got), "read "+uuid)
		if got != want {
			t.Errorf("%s holds cancel_intent %q, want %q", uuid, got, want)
		}
	}
}

// TestCancelIntent_TheWritersAnswerTheirOwn: the ask writes only on the row the
// door read (its status at its version); a refusal writes only when it answers
// the envelope the cancel last went in and the cancel is unanswered, so a
// stale or repeated one matches nothing; a resend moves the envelope the
// refusal must match.
func TestCancelIntent_TheWritersAnswerTheirOwn(t *testing.T) {
	db := openWindowDB(t)
	res, err := db.Exec(`INSERT INTO orders (uuid, status, core_version) VALUES ('door', 'in_transit', 3)`)
	testutil.MustNoErr(t, err, "insert")
	id, err := res.LastInsertId()
	testutil.MustNoErr(t, err, "id")

	if wrote, err := AskCancel(db, id, cancelAsked, "in_transit", 2); err != nil || wrote {
		t.Errorf("the ask on a row Core moved since (version 3, read at 2) wrote=%v err=%v, want nothing", wrote, err)
	}
	if wrote, err := AskCancel(db, id, cancelAsked, "in_transit", 3); err != nil || !wrote {
		t.Fatalf("the ask on the row as read wrote=%v err=%v, want it written", wrote, err)
	}
	if wrote, err := RefuseCancel(db, id, "e-1", "stale"); err != nil || wrote {
		t.Errorf("a refusal of another envelope wrote=%v err=%v, want nothing", wrote, err)
	}
	testutil.MustNoErr(t, ResentCancel(db, id, "e-3"), "the resend")
	if wrote, err := RefuseCancel(db, id, "e-2", "of the first copy"); err != nil || wrote {
		t.Errorf("a refusal of the copy before the resend wrote=%v err=%v, want nothing", wrote, err)
	}
	if wrote, err := RefuseCancel(db, id, "e-3", "set down"); err != nil || !wrote {
		t.Fatalf("the refusal of the cancel sent wrote=%v err=%v, want it written", wrote, err)
	}
	if wrote, err := RefuseCancel(db, id, "e-3", "again"); err != nil || wrote {
		t.Errorf("a repeated refusal wrote=%v err=%v, want nothing: the cancel is answered", wrote, err)
	}
	o, err := Get(db, id)
	testutil.MustNoErr(t, err, "read")
	if o.CancelSent || o.CancelRefused != "set down" {
		t.Errorf("after the refusal the row reads sent=%v refused=%q, want refused \"set down\"", o.CancelSent, o.CancelRefused)
	}
}
