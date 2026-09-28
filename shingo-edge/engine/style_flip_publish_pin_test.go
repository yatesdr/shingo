package engine

// style_flip_publish_pin_test.go — the running style reaches Core on every
// flip (Lane F). Core learns a process's active style only from the
// plant.claims mirror, and the mirror's Active flag is rebuilt by a publish.
// This file pins what each flip door does to that subject.

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/messaging"
)

// plantClaimsRows returns the unsent plant.claims outbox rows' payloads, in
// id order — what Core would receive for the claims mirror.
func plantClaimsRows(t *testing.T, f *cutoverFixture) [][]byte {
	t.Helper()
	msgs, err := f.db.ListUnsentOutboxByType([]string{protocol.SubjectPlantClaims})
	testutil.MustNoErr(t, err, "list unsent plant.claims")
	out := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Payload)
	}
	return out
}

// decodePlantClaimsReport unwraps one plant.claims payload into its report.
func decodePlantClaimsReport(t *testing.T, payload []byte) protocol.PlantClaimsReport {
	t.Helper()
	var env protocol.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var data protocol.Data
	if err := env.DecodePayload(&data); err != nil {
		t.Fatalf("decode data wrapper: %v", err)
	}
	if data.Subject != protocol.SubjectPlantClaims {
		t.Fatalf("data subject = %q, want %q", data.Subject, protocol.SubjectPlantClaims)
	}
	var report protocol.PlantClaimsReport
	if err := json.Unmarshal(data.Body, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	return report
}

// activeStyleName returns the name of the report's active style, or "" when
// none is flagged.
func activeStyleName(report protocol.PlantClaimsReport) string {
	for _, st := range report.Styles {
		if st.Active {
			return st.StyleID
		}
	}
	return ""
}

// wirePlantClaimsPublisher wires the REAL plant-claims publisher to the
// fixture's engine exactly as the composition root (cmd/shingoedge/main.go)
// does, so these pins exercise the real publish path, not a stub.
func wirePlantClaimsPublisher(t *testing.T, f *cutoverFixture) {
	t.Helper()
	pub := messaging.NewPlantClaimsPublisher(f.db, "stn-test", 0)
	f.eng.SetPlantClaimsFunc(func(processID int64) {
		if err := pub.PublishChanged(processID); err != nil {
			t.Errorf("plant_claims publish: %v", err)
		}
	})
}

// THE ADMIN DOOR AND THE RUNNING STYLE.
//
// A flip through the admin door (SetProcessActiveStyle) strands the piles
// AND enqueues exactly ONE plant.claims row for that process, with the NEW
// style flagged Active: Core's mirror follows within one message. A re-set
// of the style the process already runs is not a flip — no strand, no
// publish, no row.
func TestStyleFlip_AdminDoorPublishesPlantClaims(t *testing.T) {
	t.Parallel()
	f := newCutoverFixture(t, "FLIP-PUB")
	wirePlantClaimsPublisher(t, f)
	ackOutbox(t, f.db)

	testutil.MustNoErr(t, f.eng.SetProcessActiveStyle(f.processID, &f.toStyle), "admin flip")

	rows := plantClaimsRows(t, f)
	if len(rows) != 1 {
		t.Fatalf("admin flip enqueued %d plant.claims rows, want 1", len(rows))
	}
	report := decodePlantClaimsReport(t, rows[0])
	if report.ProcessID != "FLIP-PUB-PROC" {
		t.Errorf("plant.claims report process = %q, want FLIP-PUB-PROC", report.ProcessID)
	}
	if got := activeStyleName(report); got != "FLIP-PUB-TO" {
		t.Errorf("plant.claims active style = %q, want FLIP-PUB-TO", got)
	}

	// A re-set of the style now running is not a flip: no second publish.
	testutil.MustNoErr(t, f.eng.SetProcessActiveStyle(f.processID, &f.toStyle), "re-set running style")
	if rows := plantClaimsRows(t, f); len(rows) != 1 {
		t.Errorf("re-set enqueued %d more rows, want still 1 — a re-set is not a flip", len(rows)-1)
	}
}

// THE CUTOVER DOOR AND THE RUNNING STYLE.
//
// The changeover's cutover is the second door to the same verb: completing
// the cutover enqueues exactly ONE plant.claims row with the to-style
// flagged Active. Core's is_active follows within one message of the
// operator pressing Complete Cutover, not up to 60 minutes later.
func TestStyleFlip_CutoverDoorPublishesPlantClaims(t *testing.T) {
	t.Parallel()
	f := newCutoverFixture(t, "FLIP-CUT")
	wirePlantClaimsPublisher(t, f)
	ackOutbox(t, f.db)

	f.cutover(t)

	rows := plantClaimsRows(t, f)
	if len(rows) != 1 {
		t.Fatalf("cutover enqueued %d plant.claims rows, want 1", len(rows))
	}
	report := decodePlantClaimsReport(t, rows[0])
	if report.ProcessID != "FLIP-CUT-PROC" {
		t.Errorf("plant.claims report process = internal id %q, want FLIP-CUT-PROC", report.ProcessID)
	}
	if got := activeStyleName(report); got != "FLIP-CUT-TO" {
		t.Errorf("plant.claims active style = %q, want FLIP-CUT-TO", got)
	}
}

// THE VERB'S CONTRACT AT THE ENGINE BOUNDARY (guard for the forbidigo
// pattern's msg: no flip can bypass the verb's publish by going straight to
// db.SetActiveStyle).
//
// The notify fires only on real flips. The engine itself does not publish:
// it rings the injected notifier (composition root wires it to the
// publisher). With no notifier wired (test default), a flip still succeeds
// and enqueues nothing.
func TestStyleFlip_NoNotifierWiredFlipStillSucceeds(t *testing.T) {
	t.Parallel()
	f := newCutoverFixture(t, "FLIP-NIL")
	ackOutbox(t, f.db)

	testutil.MustNoErr(t, f.eng.SetProcessActiveStyle(f.processID, &f.toStyle), "flip with no notifier")
	proc, err := f.db.GetProcess(f.processID)
	testutil.MustNoErr(t, err, "get process")
	if proc.ActiveStyleID == nil || *proc.ActiveStyleID != f.toStyle {
		t.Fatalf("active style not flipped: %+v", proc.ActiveStyleID)
	}
	if rows := plantClaimsRows(t, f); len(rows) != 0 {
		t.Errorf("nil notifier enqueued %d rows, want 0 — the engine does not publish itself", len(rows))
	}
}
