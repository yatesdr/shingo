package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// process_delete_test.go — deleting a process closes its demands.
//
// The store-level pins (store/processes_delete_cascade_test.go) say what the
// delete itself does: it drops the open rows and sends nothing. These say what
// the engine door above it does, which is the half Core can see.

// openEpisodesFor seeds n open episodes on one process NAME, the way a cell's
// mint path leaves them, and returns their origin ids in key order. Seeded
// directly rather than through openCellEpisode because what is under test is the
// CLOSE of whatever is open, not how it came to be open — and a direct seed
// leaves the outbox empty, so every message on it afterwards is the delete's.
func openEpisodesFor(t *testing.T, db *store.DB, processName string, n int) map[string]string {
	t.Helper()
	byKey := map[string]string{}
	for i := 0; i < n; i++ {
		payload := "SYN-PANEL-" + string(rune('A'+i))
		key := protocol.CellEpisodeKey(processName, payload, protocol.ClaimRoleConsume)
		expected := 2
		row := &store.OpenOrigin{
			EpisodeKey: key, OriginID: "origin-" + key, Kind: protocol.EpisodeKindCell,
			Direction: protocol.ClaimRoleConsume, TriggerKind: protocol.EpisodeTriggerAutoreorder,
			TriggerRef: "claim:1", ProcessID: processName, CoreNodeName: "SYN_NODE01",
			PayloadCode: payload, Threshold: 50, ExpectedOrders: &expected,
			OpenedAt: time.Now().UTC(),
		}
		if err := db.OpenDemandOrigin(row); err != nil {
			t.Fatalf("open demand origin %s: %v", key, err)
		}
		byKey[key] = row.OriginID
	}
	return byKey
}

func newProcess(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	id, err := db.CreateProcess(name, "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process %q: %v", name, err)
	}
	return id
}

// THE LANE. A deleted process's episodes reach Core as CLOSED, with a reason
// that says what happened.
//
// Before this, the delete dropped the open rows inside its own transaction and
// sent nothing, so Core held every one of those episodes open against a process
// that no longer existed until its childless pass aged them out `unattributed` —
// a reason that means "this episode never had an order" and describes nothing
// about a process being deleted.
//
// claim_removed is the honest reason: the need did not recover, it stopped being
// asked. closed_by is `notification` because an event told us — the operator's
// delete — which is what separates this from the reconciler finding the same
// fact by sweeping.
func TestDeleteProcess_ClosesEveryOpenEpisode(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)

	pid := newProcess(t, db, "DEL-PROC")
	seeded := openEpisodesFor(t, db, "DEL-PROC", 3)

	// Same table, different process. The close is keyed on the name, and this is
	// the row that proves it.
	newProcess(t, db, "DEL-BYSTANDER")
	bystander := openEpisodesFor(t, db, "DEL-BYSTANDER", 1)

	if err := eng.DeleteProcess(pid); err != nil {
		t.Fatalf("DeleteProcess: %v", err)
	}

	if _, err := db.GetProcess(pid); err == nil {
		t.Error("the process row survived its own delete")
	}
	left, err := db.ListOpenDemandOriginsForProcess("DEL-PROC")
	if err != nil {
		t.Fatalf("list open episodes: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("%d episode(s) still open for a process that is gone", len(left))
	}
	others, err := db.ListOpenDemandOriginsForProcess("DEL-BYSTANDER")
	if err != nil {
		t.Fatalf("list bystander episodes: %v", err)
	}
	if len(others) != 1 {
		t.Errorf("another process's episodes went with it: %d left, want 1", len(others))
	}

	states := decodeOriginStates(t, db)
	if len(states) != len(seeded) {
		t.Fatalf("%d demand.origin message(s) on the outbox, want %d — one close per open episode",
			len(states), len(seeded))
	}
	closed := map[string]protocol.DemandOriginState{}
	for _, st := range states {
		if st.ClosedAt == nil {
			t.Errorf("origin %s reached Core without a closed_at — its presence IS the close", st.OriginID)
		}
		if st.CloseReason != protocol.CloseReasonClaimRemoved {
			t.Errorf("origin %s close_reason = %q, want %q", st.OriginID, st.CloseReason, protocol.CloseReasonClaimRemoved)
		}
		if st.ClosedBy != protocol.ClosedByNotification {
			t.Errorf("origin %s closed_by = %q, want %q — an event told us, a sweep did not find it",
				st.OriginID, st.ClosedBy, protocol.ClosedByNotification)
		}
		closed[st.EpisodeKey] = st
	}
	for key, origin := range seeded {
		st, ok := closed[key]
		if !ok {
			t.Errorf("episode %s was deleted without a close reaching Core", key)
			continue
		}
		if st.OriginID != origin {
			t.Errorf("episode %s closed under origin %s, want %s", key, st.OriginID, origin)
		}
		if st.Revision < 2 {
			t.Errorf("episode %s closed at revision %d — the close must outrank the open at Core",
				key, st.Revision)
		}
	}
	for key := range bystander {
		if _, ok := closed[key]; ok {
			t.Errorf("the bystander's episode %s was closed by another process's delete", key)
		}
	}
}

// A PILE NO LONGER BLOCKS THE DELETE. The process's episodes close, the
// process goes, and its lineside pile goes with it.
// Flipped under the brief's U3 (EnsureNoLinesideStock is deleted; deleting a
// process deletes its piles): this was TestDeleteProcess_
// RefusedWhileStockedClosesNothing, which pinned ErrProcessHasStock refusing the
// delete before any close.
func TestDeleteProcess_DeletesItsPilesAndClosesItsEpisodes(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)

	pid := newProcess(t, db, "DEL-STOCKED")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, CoreNodeName: "SYN_NODE01", Code: "N1", Name: "N1", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.CaptureLinesideBucket(nodeID, "SYN-PANEL-A", 240); err != nil {
		t.Fatalf("capture pile: %v", err)
	}
	openEpisodesFor(t, db, "DEL-STOCKED", 2)

	if err := eng.DeleteProcess(pid); err != nil {
		t.Fatalf("DeleteProcess = %v, want nil: a pile is deleted with its process", err)
	}
	if _, err := db.GetProcess(pid); err == nil {
		t.Error("the process survived its delete")
	}
	open, err := db.ListOpenDemandOriginsForProcess("DEL-STOCKED")
	if err != nil {
		t.Fatalf("list open episodes: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("%d episode(s) still open after the delete, want 0", len(open))
	}
	rows, err := db.ListLinesideBuckets(nodeID)
	if err != nil {
		t.Fatalf("list piles: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("piles after the delete = %+v, want none", rows)
	}
}

// A CLOSE THAT NEVER REACHED THE OUTBOX MUST NOT TAKE THE PROCESS WITH IT.
//
// closeEpisode keeps the open row when the enqueue fails, precisely so the
// reconciling sweep can close it again later. Deleting the process anyway would
// destroy that row inside the delete's own transaction and lose the close
// outright — with no sweep able to notice, because the sweep reads that table
// and the claims it would re-derive the close from are gone too.
//
// So the delete refuses, and what is left is a state the plant already
// understands: the episode open, the process alive, the operator's retry able to
// finish the job. Dropping the outbox table is the bluntest genuine enqueue
// failure available and needs no injection seam.
func TestDeleteProcess_RefusesWhenTheCloseCannotBeEnqueued(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)

	pid := newProcess(t, db, "DEL-ENQFAIL")
	openEpisodesFor(t, db, "DEL-ENQFAIL", 1)
	if _, err := db.Exec(`DROP TABLE outbox`); err != nil {
		t.Fatalf("drop outbox: %v", err)
	}

	if err := eng.DeleteProcess(pid); err == nil {
		t.Fatal("DeleteProcess succeeded with no way to tell Core the episode ended")
	}
	if _, err := db.GetProcess(pid); err != nil {
		t.Errorf("the process was deleted anyway: %v", err)
	}
	open, err := db.ListOpenDemandOriginsForProcess("DEL-ENQFAIL")
	if err != nil {
		t.Fatalf("list open episodes: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("%d episode(s) open, want the one whose close never got enqueued", len(open))
	}
	if open[0].Revision < 2 {
		t.Errorf("revision = %d, want >= 2 so the eventual re-send wins at Core", open[0].Revision)
	}
}

// Deleting an already-deleted process is not an error, and stays silent. A
// double-click is not a reason to tell Core anything.
func TestDeleteProcess_MissingIsSilent(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)

	if err := eng.DeleteProcess(99999); err != nil {
		t.Fatalf("deleting a missing process: %v", err)
	}
	if states := decodeOriginStates(t, db); len(states) != 0 {
		t.Errorf("deleting a missing process put %d message(s) on the outbox, want 0", len(states))
	}
}

// THE EDGE IS A PI AND THE STORE IS PINNED TO ONE CONNECTION (store.Open sets
// MaxOpenConns(1)), so the number of statements a path issues is what an
// operator feels as a hang — every other reader is behind them.
//
// This is a delete-time cost and only a delete-time cost: one list for the
// process, then four statements per open episode (the revision bump, the row
// read, the outbox INSERT, the open-row DELETE), and nothing on any tick.
//
// The two reads before them are the ordering this lane needs. The stock COUNT is
// asked here as well as inside the delete, so a refusal lands before the first
// close rather than after it; the name is read because the episodes are keyed on
// it. Both are at a config action — a process is deleted by hand, rarely — and
// both are indexed single-row reads.
//
// The numbers are pinned rather than bounded so that a per-episode read appearing
// anywhere on this path fails the gate instead of being absorbed.
func TestDeleteProcess_StatementCount(t *testing.T) {
	baseline := 13 // 10 in the store's delete + the stock precheck, the name, the list
	perEpisode := 4

	for _, episodes := range []int{0, 1, 5} {
		t.Run(map[int]string{0: "none", 1: "one", 5: "five"}[episodes], func(t *testing.T) {
			db, counter := testEngineDBCounting(t)
			eng := testEngine(t, db)
			name := "DEL-COUNT"
			pid := newProcess(t, db, name)
			openEpisodesFor(t, db, name, episodes)

			counter.Reset()
			if err := eng.DeleteProcess(pid); err != nil {
				t.Fatalf("DeleteProcess: %v", err)
			}

			want := int64(baseline + perEpisode*episodes)
			if got := counter.Count(); got != want {
				t.Errorf("%d open episode(s): %d statements, want %d", episodes, got, want)
			}
		})
	}
}
