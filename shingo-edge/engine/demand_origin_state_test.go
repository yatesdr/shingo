package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingoedge/store"
)

// decodeOriginStates pulls every demand.origin message off the outbox, in
// enqueue order, and unmarshals the state each carries.
func decodeOriginStates(t *testing.T, db *store.DB) []protocol.DemandOriginState {
	t.Helper()
	msgs, err := db.ListUnsentOutboxByType([]string{protocol.SubjectDemandOrigin})
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	var out []protocol.DemandOriginState
	for _, m := range msgs {
		var env protocol.Envelope
		if err := json.Unmarshal(m.Payload, &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		// A data envelope wraps the body in {subject, body}; the state is
		// inside that, not directly in the envelope payload.
		var d protocol.Data
		if err := json.Unmarshal(env.Payload, &d); err != nil {
			t.Fatalf("decode data wrapper: %v", err)
		}
		if d.Subject != protocol.SubjectDemandOrigin {
			continue
		}
		var st protocol.DemandOriginState
		if err := json.Unmarshal(d.Body, &st); err != nil {
			t.Fatalf("decode state: %v", err)
		}
		out = append(out, st)
	}
	return out
}

// STATE TRANSFER, NOT EVENTS. Every message carries the whole episode row and a
// monotonic revision, which is what makes the receiver's job an idempotent
// upsert instead of a state machine reconstructing episodes from a stream.
//
// The properties that buys are structural, and this pins the two that decide
// whether Core can be simple:
//
//   - Revisions are strictly increasing, so a REPEATED message is a no-op and a
//     REVERSED pair resolves by comparison. Both happen during a network event,
//     which is when nobody is reading logs.
//   - The LAST message is sufficient on its own. Lose everything except the
//     close and Core still converges — with events, a lost "opened" is
//     unrecoverable.
func TestOriginState_RevisionsIncreaseAndTheLastMessageIsSufficient(t *testing.T) {
	eng, db, procID, procName, claim := episodeFixture(t, "STATE-PROC", "ALN_010", 50)

	if _, _, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeDirectionSupply, protocol.EpisodeTriggerAutoreorder, 2, 40, false); err != nil {
		t.Fatalf("open: %v", err)
	}
	// An operator pushes twice while it is open — joins, not new demands.
	for range 2 {
		if _, _, err := eng.openCellEpisode(procID, claim,
			protocol.EpisodeDirectionSupply, protocol.EpisodeTriggerOperator, 2, 38, false); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	eng.closeCellEpisode(procID, "PANEL-B", protocol.EpisodeDirectionSupply, protocol.CloseReasonRecovered, protocol.ClosedByNotification)

	states := decodeOriginStates(t, db)
	if len(states) != 4 {
		t.Fatalf("want 4 state messages (open + 2 joins + close), got %d", len(states))
	}

	// Strictly increasing, which is the ONLY thing that decides which of two
	// arriving messages is newer. Not a timestamp: two services cannot agree on
	// one, and the whole point of the guard is settling order without agreement.
	for i := 1; i < len(states); i++ {
		if states[i].Revision <= states[i-1].Revision {
			t.Errorf("revision did not increase at message %d: %d then %d",
				i, states[i-1].Revision, states[i].Revision)
		}
	}
	// One episode throughout — a join is the same demand expressed again.
	for i, st := range states {
		if st.OriginID != states[0].OriginID {
			t.Errorf("message %d belongs to a different episode (%s != %s)", i, st.OriginID, states[0].OriginID)
		}
	}

	// THE LAST MESSAGE IS SUFFICIENT. Throw away every earlier one and the
	// survivor still carries the entire episode: identity, grain, denominator,
	// counts and the ending.
	last := states[len(states)-1]
	if last.ClosedAt == nil || last.CloseReason != protocol.CloseReasonRecovered {
		t.Error("the final message must carry the close — its presence IS the close")
	}
	if last.EpisodeKey == "" || last.Kind != protocol.EpisodeKindCell ||
		last.Direction != protocol.EpisodeDirectionSupply {
		t.Errorf("the final message lost its identity: %+v", last)
	}
	// THE NAME, not the row id. The grain assertion is the same assertion it
	// always was; what changed is which of the two values on the wire IS the
	// grain, and a test still comparing the row id would pass on a message that
	// Core cannot join to anything.
	if last.ProcessID != procName || last.PayloadCode != "PANEL-B" {
		t.Errorf("the final message lost its grain: process=%q payload=%q", last.ProcessID, last.PayloadCode)
	}
	if last.ExpectedOrders == nil || *last.ExpectedOrders != 2 {
		t.Errorf("the final message lost expected_orders: %v", last.ExpectedOrders)
	}
	if last.RerequestCount != 2 {
		t.Errorf("the final message lost the re-request count: %d", last.RerequestCount)
	}
	if last.OpenedAt.IsZero() {
		t.Error("the final message lost opened_at — duration is unknowable without it")
	}
}

// A changeover episode goes through the SAME table and the SAME emitter. Two
// emission paths for one subject is where the second one drifts.
func TestOriginState_ChangeoverUsesTheSamePath(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)

	procID, err := db.CreateProcess("CO-STATE-PROC", "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	from, err := db.CreateStyle("FROM", "", procID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	to, err := db.CreateStyle("TO", "", procID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	coID, err := eng.changeoverService.Create(procID, &from, to, "op", "", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("create changeover: %v", err)
	}
	co, err := db.GetActiveProcessChangeover(procID)
	if err != nil || co == nil {
		t.Fatalf("read changeover: %v", err)
	}

	originID := eng.openChangeoverEpisode(co, 6)
	if originID == "" {
		t.Fatal("no origin minted")
	}
	eng.closeChangeoverEpisode(coID, protocol.CloseReasonChangeoverComplete, protocol.ClosedByNotification)

	states := decodeOriginStates(t, db)
	if len(states) != 2 {
		t.Fatalf("want 2 state messages (open + close), got %d", len(states))
	}
	if states[0].Kind != protocol.EpisodeKindChangeover {
		t.Errorf("kind = %q, want changeover", states[0].Kind)
	}
	if states[1].Revision <= states[0].Revision {
		t.Error("the close must carry a higher revision than the open")
	}
	if states[1].ClosedAt == nil || states[1].CloseReason != protocol.CloseReasonChangeoverComplete {
		t.Error("the close must carry its reason — complete and cancelled are different outcomes")
	}
	if states[1].ExpectedOrders == nil || *states[1].ExpectedOrders != 6 {
		t.Errorf("the changeover close lost expected_orders: %v", states[1].ExpectedOrders)
	}

	// The back-pointer survives the close: it is the link from a changeover to
	// its demand, and clearing it would throw that away to save nothing.
	stored, err := db.GetChangeoverOriginID(coID)
	if err != nil || stored != originID {
		t.Errorf("changeover back-pointer = %q (err %v), want %s", stored, err, originID)
	}
}

// ENQUEUE FIRST, THEN DELETE. A close that is enqueued but whose row survives
// is harmless; a row deleted before its close is enqueued is a demand that
// never ends, on a surface where a long-open episode is the loudest row.
func TestOriginState_CloseIsEnqueuedBeforeTheRowGoes(t *testing.T) {
	eng, db, procID, procName, claim := episodeFixture(t, "ORDER-PROC", "ALN_011", 50)

	if _, _, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeDirectionSupply, protocol.EpisodeTriggerAutoreorder, 2, 40, false); err != nil {
		t.Fatalf("open: %v", err)
	}
	eng.closeCellEpisode(procID, "PANEL-B", protocol.EpisodeDirectionSupply, protocol.CloseReasonRecovered, protocol.ClosedByNotification)

	key := protocol.CellEpisodeKey(procName, "PANEL-B", protocol.EpisodeDirectionSupply)
	if _, err := db.GetOpenDemandOrigin(key); err != store.ErrOriginNotOpen {
		t.Errorf("the row must be gone after a close, got err=%v", err)
	}
	states := decodeOriginStates(t, db)
	if len(states) == 0 || states[len(states)-1].ClosedAt == nil {
		t.Fatal("the close was not enqueued — the episode would never end on Core")
	}
}
