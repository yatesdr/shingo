package engine

import (
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// lastOriginState decodes the most recent demand.origin message off the
// outbox. The close REASON is only observable there — it is the thing Core
// acts on — so a test that checked only "the row is gone" would pass while
// shipping the wrong reason.
//
// THE DECODE IS THREE LAYERS AND THE MIDDLE ONE IS EASY TO MISS.
// NewDataEnvelope wraps the body in a Data{subject, data} before putting it in
// the envelope's payload, so unmarshalling the envelope payload STRAIGHT into
// DemandOriginState succeeds — JSON ignores unknown fields — and yields a zero
// struct with every assertion reading empty. It cost a debugging round here.
// Hence the guards below: a decode that produced nothing must fail loudly
// rather than return a blank that a test then compares against "".
func lastOriginState(t *testing.T, db *store.DB) protocol.DemandOriginState {
	t.Helper()
	msgs, err := db.ListPendingOutbox(500)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	var got protocol.DemandOriginState
	found := false
	for _, m := range msgs {
		if m.MsgType != protocol.SubjectDemandOrigin {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(m.Payload, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		var data protocol.Data
		if err := env.DecodePayload(&data); err != nil {
			t.Fatalf("decode data layer: %v", err)
		}
		if data.Subject != protocol.SubjectDemandOrigin {
			t.Fatalf("data subject = %q, want %q", data.Subject, protocol.SubjectDemandOrigin)
		}
		var st protocol.DemandOriginState
		if err := json.Unmarshal(data.Body, &st); err != nil {
			t.Fatalf("decode origin state: %v", err)
		}
		if st.OriginID == "" || st.EpisodeKey == "" {
			t.Fatalf("decoded an EMPTY origin state from %s — the decode is wrong, not the code under test", data.Body)
		}
		got, found = st, true
	}
	if !found {
		t.Fatal("no demand.origin message on the outbox")
	}
	return got
}

// THE FIRST THING TO GET RIGHT IS NOT CLOSING THINGS.
//
// A reconciler that closes live episodes is worse than no reconciler: the
// notification paths would keep re-opening them and the surface would fill
// with short false episodes for demands that never ended. This is the guard
// against the sweep being too eager, and it is the one that matters most.
func TestReconciler_LeavesBreachedEpisodeOpen(t *testing.T) {
	eng, db, procID, claim := episodeFixture(t, "RECON-LIVE", "ALN_020", 50)

	if below, _ := eng.evaluateCellLevel(claim, 40); !below {
		t.Fatal("fixture should start below its reorder point")
	}
	if _, _, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeDirectionSupply, protocol.EpisodeTriggerAutoreorder, 2, 40, false); err != nil {
		t.Fatalf("open: %v", err)
	}

	eng.reconcileDemandEpisodes()

	key := protocol.CellEpisodeKey(eng.cfg.StationID(), procID, "PANEL-B", protocol.EpisodeDirectionSupply)
	if _, err := db.GetOpenDemandOrigin(key); err != nil {
		t.Fatalf("a still-breached episode must stay open, got err=%v", err)
	}
}

// The notification path missed: the rising edge cleared the flag, but the
// close never ran. Nothing else will ever close this episode, and an episode
// that never closes reads as a permanent unmet demand.
func TestReconciler_ClosesRecoveredEpisode(t *testing.T) {
	eng, db, procID, claim := episodeFixture(t, "RECON-RECOVERED", "ALN_021", 50)

	eng.evaluateCellLevel(claim, 40)
	if _, _, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeDirectionSupply, protocol.EpisodeTriggerAutoreorder, 2, 40, false); err != nil {
		t.Fatalf("open: %v", err)
	}
	// The level came back and cleared the edge — without the close landing.
	if err := db.SetClaimBelowReorderSince(claim.ID, nil); err != nil {
		t.Fatalf("clear falling edge: %v", err)
	}

	eng.reconcileDemandEpisodes()

	key := protocol.CellEpisodeKey(eng.cfg.StationID(), procID, "PANEL-B", protocol.EpisodeDirectionSupply)
	if _, err := db.GetOpenDemandOrigin(key); err != store.ErrOriginNotOpen {
		t.Errorf("recovered episode must be closed by the sweep, got err=%v", err)
	}
	st := lastOriginState(t, db)
	if st.CloseReason != protocol.CloseReasonRecovered {
		t.Errorf("close_reason = %q, want %q", st.CloseReason, protocol.CloseReasonRecovered)
	}
	if st.ClosedAt == nil {
		t.Error("closed state must carry closed_at")
	}
}

// THE CASE NOTHING FIRES FOR, and the reason the sweep exists. The style
// swapped and the new one does not claim this payload here. No rising edge,
// no recovery, no event — the need simply stopped being asked.
//
// It must NOT close as `recovered`: nothing recovered. A surface that said so
// would report a satisfied demand every time a claim was edited away.
func TestReconciler_ClosesEpisodeWhenClaimGone(t *testing.T) {
	eng, db, procID, claim := episodeFixture(t, "RECON-GONE", "ALN_022", 50)

	eng.evaluateCellLevel(claim, 40)
	if _, _, err := eng.openCellEpisode(procID, claim,
		protocol.EpisodeDirectionSupply, protocol.EpisodeTriggerAutoreorder, 2, 40, false); err != nil {
		t.Fatalf("open: %v", err)
	}

	// Swap the process onto a style that claims nothing. The old claim row
	// still exists, still flagged below — which is exactly the stale reading
	// the active-style scoping exists to ignore.
	otherStyle, err := db.CreateStyle("RECON-GONE-OTHER", "", procID)
	if err != nil {
		t.Fatalf("create other style: %v", err)
	}
	if err := db.SetActiveStyle(procID, &otherStyle); err != nil {
		t.Fatalf("set active style: %v", err)
	}

	eng.reconcileDemandEpisodes()

	key := protocol.CellEpisodeKey(eng.cfg.StationID(), procID, "PANEL-B", protocol.EpisodeDirectionSupply)
	if _, err := db.GetOpenDemandOrigin(key); err != store.ErrOriginNotOpen {
		t.Errorf("episode must close when its claim is no longer active, got err=%v", err)
	}
	st := lastOriginState(t, db)
	if st.CloseReason != protocol.CloseReasonClaimRemoved {
		t.Errorf("close_reason = %q, want %q — nothing recovered here", st.CloseReason, protocol.CloseReasonClaimRemoved)
	}
}

// THE GRAIN RULE, enforced by the sweep. An A/B pair is two claims on one
// process, and the process needs the payload while EITHER half is below. If
// the sweep asked only about the claim that minted the episode, half a swap
// cycle would close a demand that is still live.
func TestReconciler_KeepsEpisodeOpenWhileAnyClaimBelow(t *testing.T) {
	eng, db, procID, claimA := episodeFixture(t, "RECON-AB", "ALN_023", 50)

	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: "ALN_024", Code: "ALN_024", Name: "ALN_024",
		Sequence: 2, Enabled: true,
	}); err != nil {
		t.Fatalf("create paired node: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: claimA.StyleID, CoreNodeName: "ALN_024", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "PANEL-B",
		UOPCapacity: 300, ReorderPoint: 50, AutoReorder: true,
		InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
		InboundStaging: "SLN_001", OutboundStaging: "SLN_011",
	}); err != nil {
		t.Fatalf("upsert paired claim: %v", err)
	}
	claimB, err := db.GetStyleNodeClaimByNode(claimA.StyleID, "ALN_024")
	if err != nil || claimB == nil {
		t.Fatalf("read paired claim: %v", err)
	}

	// Both halves below, one episode for the process.
	eng.evaluateCellLevel(claimA, 40)
	eng.evaluateCellLevel(claimB, 40)
	if _, _, err := eng.openCellEpisode(procID, claimA,
		protocol.EpisodeDirectionSupply, protocol.EpisodeTriggerAutoreorder, 2, 40, false); err != nil {
		t.Fatalf("open: %v", err)
	}

	// A gets its material; B is still starving.
	if err := db.SetClaimBelowReorderSince(claimA.ID, nil); err != nil {
		t.Fatalf("clear A: %v", err)
	}

	eng.reconcileDemandEpisodes()

	key := protocol.CellEpisodeKey(eng.cfg.StationID(), procID, "PANEL-B", protocol.EpisodeDirectionSupply)
	if _, err := db.GetOpenDemandOrigin(key); err != nil {
		t.Fatalf("the process still needs the payload while B is below, got err=%v", err)
	}
}

// A changeover's two endings are different outcomes and must not merge —
// merging them hides every abandoned changeover behind the successful ones.
func TestReconciler_ClosesChangeoverOnTerminalState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state domain.ChangeoverState
		want  string
	}{
		{"completed", domain.ChangeoverCompleted, protocol.CloseReasonChangeoverComplete},
		{"cancelled", domain.ChangeoverCancelled, protocol.CloseReasonCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testEngineDB(t)
			eng := testEngine(t, db)

			procID, err := db.CreateProcess("RECON-CO-"+tc.name, "", "active_production", "", "", false, false)
			if err != nil {
				t.Fatalf("create process: %v", err)
			}
			fromStyle, err := db.CreateStyle("FROM-"+tc.name, "", procID)
			if err != nil {
				t.Fatalf("create from style: %v", err)
			}
			toStyle, err := db.CreateStyle("TO-"+tc.name, "", procID)
			if err != nil {
				t.Fatalf("create to style: %v", err)
			}
			if _, err := eng.changeoverService.Create(procID, &fromStyle, toStyle, "op", "", nil, nil, nil, nil); err != nil {
				t.Fatalf("create changeover: %v", err)
			}
			co, err := db.GetActiveProcessChangeover(procID)
			if err != nil || co == nil {
				t.Fatalf("read changeover: %v", err)
			}
			if origin := eng.openChangeoverEpisode(co, 3); origin == "" {
				t.Fatal("changeover episode did not open")
			}

			// An active changeover's episode must survive a sweep.
			eng.reconcileDemandEpisodes()
			key := protocol.ChangeoverEpisodeKey(eng.cfg.StationID(), co.ID)
			if _, err := db.GetOpenDemandOrigin(key); err != nil {
				t.Fatalf("an ACTIVE changeover must keep its episode, got err=%v", err)
			}

			if err := db.UpdateProcessChangeoverState(co.ID, tc.state); err != nil {
				t.Fatalf("set state %s: %v", tc.state, err)
			}
			eng.reconcileDemandEpisodes()

			if _, err := db.GetOpenDemandOrigin(key); err != store.ErrOriginNotOpen {
				t.Errorf("a %s changeover must close its episode, got err=%v", tc.state, err)
			}
			if st := lastOriginState(t, db); st.CloseReason != tc.want {
				t.Errorf("close_reason = %q, want %q", st.CloseReason, tc.want)
			}
		})
	}
}

// EDGE MUST NOT CLOSE CORE'S EPISODES. A threshold episode's precondition is a
// plant-wide in-loop total, which Edge cannot read — so it has no basis on
// which to declare the precondition failed, and a sweep that closed one would
// be guessing. The split exists because preconditions have owners.
func TestReconciler_IgnoresThresholdEpisodes(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)

	key := protocol.ThresholdEpisodeKey(eng.cfg.StationID(), "SLN_002", "PANEL-B")
	if err := db.OpenDemandOrigin(&store.OpenOrigin{
		EpisodeKey: key, OriginID: "11111111-1111-1111-1111-111111111111",
		Kind: protocol.EpisodeKindThreshold, CoreNodeName: "SLN_002",
		PayloadCode: "PANEL-B", OpenedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("open threshold episode: %v", err)
	}

	eng.reconcileDemandEpisodes()

	if _, err := db.GetOpenDemandOrigin(key); err != nil {
		t.Errorf("Edge's sweep must leave threshold episodes to Core, got err=%v", err)
	}
}
