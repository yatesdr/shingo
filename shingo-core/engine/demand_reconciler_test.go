//go:build docker

package engine

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/demands"
)

// registerBinding writes a demand_registry row the way every production path
// does — through SyncRegistry — so the sweep's "is this binding still live?"
// question is asked of the same table the plant answers it from.
func registerBinding(t *testing.T, db *store.DB, b thresholdEntry) {
	t.Helper()
	if _, err := db.SyncDemandRegistry(b.stationID, []demands.RegistryEntry{{
		StationID:             b.stationID,
		CoreNodeName:          b.coreNodeName,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           b.payloadCode,
		ReplenishUOPThreshold: b.threshold,
	}}); err != nil {
		t.Fatalf("register binding: %v", err)
	}
}

// registerActiveEdge makes the station REACHABLE. Without it a zero child count
// is a missing input rather than a finding, and the childless pass correctly
// refuses to act — so any test about childlessness has to establish that Core
// could have heard from this Edge in the first place.
func registerActiveEdge(t *testing.T, db *store.DB, stationID string) {
	t.Helper()
	if err := db.RegisterEdge(stationID, "test-host", "test", nil); err != nil {
		t.Fatalf("register edge: %v", err)
	}
	if _, err := db.UpdateHeartbeat(stationID); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

// backdateEpisode ages an open episode past a grace period. Tests pin their own
// inputs: driving this off the wall clock would mean waiting fifteen minutes or
// asserting nothing.
func backdateEpisode(t *testing.T, db *store.DB, originID string, age time.Duration) {
	t.Helper()
	if _, err := db.Exec(`UPDATE demand_origins SET opened_at = $1 WHERE origin_id = $2`,
		time.Now().UTC().Add(-age), originID); err != nil {
		t.Fatalf("backdate episode: %v", err)
	}
}

func insertOrderWithOrigin(t *testing.T, db *store.DB, originID, class string, age time.Duration) {
	t.Helper()
	var oid any
	if originID != "" {
		oid = originID
	}
	if _, err := db.Exec(`
		INSERT INTO orders (edge_uuid, station_id, origin_id, origin_class, created_at)
		VALUES ($1, 'PLANT.LINE1', $2, $3, $4)`,
		uuid.NewString(), oid, class, time.Now().UTC().Add(-age)); err != nil {
		t.Fatalf("insert order: %v", err)
	}
}

func mustGetOrigin(t *testing.T, db *store.DB, originID string) *store.DemandOrigin {
	t.Helper()
	got, err := db.GetDemandOrigin(originID)
	if err != nil {
		t.Fatalf("read episode %s: %v", originID, err)
	}
	if got == nil {
		t.Fatalf("episode %s vanished", originID)
	}
	return got
}

// THE SWEEP'S ENTIRE REASON TO EXIST, and the test is built to be able to fail.
//
// SyncRegistry replaces a station's whole demand_registry in one transaction
// and emits a RegistryChange only when a threshold VALUE moved. Three of its
// call sites discard the change list outright, and the worst of them is the
// stale-edge reaper, which calls SyncDemandRegistry(station, nil) — every
// binding at that station deleted, `_` on the changes, nothing fired, nothing
// logged. That is reproduced here EXACTLY: the notification path is not broken
// with a flag or a stub, it simply is not invoked, because in production it is
// not invoked either. You cannot wire up an absence.
//
// Without the sweep this episode stays open forever: the monitor's rising edge
// only runs for bindings that still exist, engagePayloads only rebuilds
// payloads somebody told it about, and nothing else looks.
func TestDemandReconciler_ClosesWhatNoNotificationPathEverSees(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	m := eng.thresholdMonitor
	b := episodeBinding(t, eng, "PANEL-RC1", 18)
	registerBinding(t, db, b)

	m.checkBindings([]thresholdEntry{b}, 40, "below_threshold", false)
	open, err := db.ListOpenThresholdEpisodes()
	if err != nil || len(open) != 1 {
		t.Fatalf("no episode opened: %d (%v)", len(open), err)
	}
	originID := open[0].OriginID

	// The stale-edge reaper, verbatim: core_handler.go's
	// `if _, err := h.db.SyncDemandRegistry(sid, nil); err != nil`.
	if _, err := db.SyncDemandRegistry(b.stationID, nil); err != nil {
		t.Fatalf("reap registry: %v", err)
	}
	// And prove the premise rather than assuming it — if some path HAD closed
	// the episode here, the assertions below would be measuring that path.
	if pre := mustGetOrigin(t, db, originID); pre.ClosedAt != nil {
		t.Fatalf("a notification path closed the episode before the sweep ran (reason=%q) — this test is no longer about the sweep",
			pre.CloseReason)
	}

	eng.reconcileDemandEpisodes()

	got := mustGetOrigin(t, db, originID)
	if got.ClosedAt == nil {
		t.Fatal("the sweep left an episode open whose binding no longer exists — a demand that ended renders as a permanent alarm")
	}
	if got.CloseReason != protocol.CloseReasonThresholdRemoved {
		t.Errorf("close_reason = %q, want %q — the need did not recover, it stopped being watched",
			got.CloseReason, protocol.CloseReasonThresholdRemoved)
	}
	// closed_by is what makes the sweep's share of the closing measurable. If
	// this said "notification" the surface could not tell a healthy plant from
	// one where every notification path has silently stopped firing.
	if got.ClosedBy != protocol.ClosedBySweep {
		t.Errorf("closed_by = %q, want %q", got.ClosedBy, protocol.ClosedBySweep)
	}
	// And the monitor released its hold, so the next crossing mints a fresh
	// episode instead of failing against the partial unique index forever.
	if held := m.currentThresholdOrigin(bindingKey(b.stationID, b.coreNodeName, b.payloadCode)); held != "" {
		t.Errorf("monitor still holds %q after the sweep closed the episode", held)
	}
}

// A SWEEP THAT CLOSES EVERYTHING PASSES THE TEST ABOVE. This is the other
// direction, and §11 is explicit that it is the more dangerous one: a
// reconciler that closes live episodes is worse than none, because the
// notification paths re-open them and the surface fills with false short
// episodes nobody can distinguish from real thrash.
//
// Both halves of the precondition rule are asserted here.
//
// The binding is present, so the episode stays open — that is the rule.
//
// And the LEVEL HAS RECOVERED while the episode is still open, which is the
// half that is easy to get wrong. The rising edge in checkBindings owns the
// level close; a sweep that also read the total would be a second opinion on a
// question that already has an answer, and the two would race — the sweep
// closing what the next delta re-opens. The sweep must not care.
func TestDemandReconciler_LeavesAnEpisodeWhosePreconditionHolds(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	m := eng.thresholdMonitor
	b := episodeBinding(t, eng, "PANEL-RC2", 18)
	registerBinding(t, db, b)
	registerActiveEdge(t, db, b.stationID)

	m.checkBindings([]thresholdEntry{b}, 40, "below_threshold", false)
	open, _ := db.ListOpenThresholdEpisodes()
	if len(open) != 1 {
		t.Fatalf("no episode opened: %d", len(open))
	}
	originID := open[0].OriginID

	// Well past the childless grace, and with the total back above threshold —
	// so the ONLY thing keeping this episode open is that its binding still
	// exists and its rising edge has not been evaluated. Give the sweep every
	// excuse to close it.
	backdateEpisode(t, db, originID, 24*time.Hour)
	insertOrderWithOrigin(t, db, originID, protocol.OriginClassAttached, time.Minute)

	eng.reconcileDemandEpisodes()

	got := mustGetOrigin(t, db, originID)
	if got.ClosedAt != nil {
		t.Fatalf("the sweep closed a live episode (reason=%q, by=%q) — the binding still exists, so the demand is still being watched",
			got.CloseReason, got.ClosedBy)
	}
	if held := m.currentThresholdOrigin(bindingKey(b.stationID, b.coreNodeName, b.payloadCode)); held != originID {
		t.Errorf("the sweep dropped the monitor's hold on a live episode: held %q, want %s", held, originID)
	}
}

// A childless episode is a demand nothing was ever done about, and it has to
// end on its own or the deploy-skew case renders the whole surface as an
// emergency: a new Core against an older Edge gets every order back with no
// origin on it, so EVERY episode has zero children.
func TestDemandReconciler_ChildlessEpisodeClosesUnattributed(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	m := eng.thresholdMonitor
	b := episodeBinding(t, eng, "PANEL-RC3", 18)
	registerBinding(t, db, b)
	registerActiveEdge(t, db, b.stationID)

	m.checkBindings([]thresholdEntry{b}, 40, "below_threshold", false)
	open, _ := db.ListOpenThresholdEpisodes()
	if len(open) != 1 {
		t.Fatalf("no episode opened: %d", len(open))
	}
	originID := open[0].OriginID
	backdateEpisode(t, db, originID, time.Hour)

	eng.reconcileDemandEpisodes()

	got := mustGetOrigin(t, db, originID)
	if got.ClosedAt == nil {
		t.Fatal("an hour-old episode with zero orders against it stayed open")
	}
	if got.CloseReason != protocol.CloseReasonUnattributed {
		t.Errorf("close_reason = %q, want %q — nothing recovered and nothing was cancelled; no order was ever attributed to it",
			got.CloseReason, protocol.CloseReasonUnattributed)
	}
	if got.ClosedBy != protocol.ClosedBySweep {
		t.Errorf("closed_by = %q, want %q", got.ClosedBy, protocol.ClosedBySweep)
	}

	// The grace is not decoration. A young childless episode is a demand whose
	// orders have not been created YET, and closing it would be the reconciler
	// racing the thing it is meant to be a floor under.
	young := episodeBinding(t, eng, "PANEL-RC3B", 18)
	young.coreNodeName = "SLN_003"
	registerBinding(t, db, young)
	m.checkBindings([]thresholdEntry{young}, 40, "below_threshold", false)
	eng.reconcileDemandEpisodes()
	for _, o := range mustListOpen(t, db) {
		if o.PayloadCode == young.payloadCode {
			return
		}
	}
	t.Error("the sweep closed an episode that opened seconds ago — the childless grace is not being applied")
}

func mustListOpen(t *testing.T, db *store.DB) []store.DemandOrigin {
	t.Helper()
	open, err := db.ListOpenThresholdEpisodes()
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	return open
}

// A CHECK MUST KNOW WHETHER IT HAD THE INPUT TO CHECK.
//
// Zero children means "no order was attributed to this demand" only if Core
// could have received one. When the owning Edge is unreachable it is not a
// finding, it is a missing input — and rendering a missing input as a finding
// is the failure this branch has now paid for repeatedly. So the episode is
// decorated, never closed: "this episode's Edge has been unreachable since X"
// is an honest unknown, and an unknown is not a false alarm.
func TestDemandReconciler_ChildlessOnAnUnreachableEdgeIsNotAFinding(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	// A cell episode — Edge-authored, arriving over the state-transfer seam,
	// for a station Core has never heard a heartbeat from.
	originID := uuid.NewString()
	if err := db.UpsertDemandOrigin(store.DemandOrigin{
		OriginID:   originID,
		Revision:   1,
		EpisodeKey: protocol.CellEpisodeKey("PLANT.DARK", 7, "PANEL-RC4", protocol.EpisodeDirectionSupply),
		Kind:       protocol.EpisodeKindCell,
		Direction:  protocol.EpisodeDirectionSupply,
		StationID:  "PLANT.DARK",
		ProcessID:  7,
		OpenedAt:   time.Now().UTC().Add(-6 * time.Hour),
	}); err != nil {
		t.Fatalf("upsert cell episode: %v", err)
	}

	eng.reconcileDemandEpisodes()

	if got := mustGetOrigin(t, db, originID); got.ClosedAt != nil {
		t.Fatalf("the sweep closed a cell episode on an unreachable Edge (reason=%q) — Core does not own that precondition and cannot know the episode ended",
			got.CloseReason)
	}

	// And once the Edge IS reachable, the same episode becomes decidable. This
	// half is what makes the assertion above about REACHABILITY rather than
	// about the sweep simply never closing cell episodes.
	registerActiveEdge(t, db, "PLANT.DARK")
	eng.reconcileDemandEpisodes()
	got := mustGetOrigin(t, db, originID)
	if got.ClosedAt == nil {
		t.Fatal("with the Edge reachable, a six-hour childless episode must close — otherwise the guard above is untested")
	}
	if got.CloseReason != protocol.CloseReasonUnattributed {
		t.Errorf("close_reason = %q, want %q", got.CloseReason, protocol.CloseReasonUnattributed)
	}
}

// CORE'S INFERENCE MUST NOT OUTRANK EDGE'S TRUTH.
//
// Edge authors cell and changeover episodes and stamps a monotonic revision on
// every change; Core's upsert applies a message only when it is strictly newer.
// So an aging close that bumped the revision would push Core past the number
// Edge is about to send, and the REAL close — the one that says
// claim_removed, or changeover_complete — would lose the comparison and be
// discarded in silence. The surface would then permanently report
// `unattributed`/`sweep` for an episode whose owner knows exactly how it ended.
//
// This is the same reasoning SupersedeOpenEpisode is built on, and it is why
// the aging close is a placeholder by construction rather than by intent.
func TestDemandReconciler_InferredCloseStepsAsideForTheRealOne(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	registerActiveEdge(t, db, "PLANT.LINE1")

	originID := uuid.NewString()
	key := protocol.CellEpisodeKey("PLANT.LINE1", 11, "PANEL-RC5", protocol.EpisodeDirectionEvacuate)
	base := store.DemandOrigin{
		OriginID:   originID,
		Revision:   1,
		EpisodeKey: key,
		Kind:       protocol.EpisodeKindCell,
		Direction:  protocol.EpisodeDirectionEvacuate,
		StationID:  "PLANT.LINE1",
		ProcessID:  11,
		OpenedAt:   time.Now().UTC().Add(-2 * time.Hour),
	}
	if err := db.UpsertDemandOrigin(base); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	eng.reconcileDemandEpisodes()
	if got := mustGetOrigin(t, db, originID); got.CloseReason != protocol.CloseReasonUnattributed {
		t.Fatalf("setup: sweep did not close the childless episode (reason=%q)", got.CloseReason)
	}

	// Edge's real close, at the revision it actually stamped: 2, the next one
	// after the open. If the sweep had bumped, this arrives at 2 against a
	// local 2 and the guard drops it.
	closedAt := time.Now().UTC()
	truth := base
	truth.Revision = 2
	truth.ClosedAt = &closedAt
	truth.CloseReason = protocol.CloseReasonClaimRemoved
	truth.ClosedBy = protocol.ClosedByNotification
	if err := db.UpsertDemandOrigin(truth); err != nil {
		t.Fatalf("upsert real close: %v", err)
	}

	got := mustGetOrigin(t, db, originID)
	if got.CloseReason != protocol.CloseReasonClaimRemoved {
		t.Errorf("close_reason = %q, want %q — Core's placeholder outranked the owner's truth and the real close was discarded by the revision guard",
			got.CloseReason, protocol.CloseReasonClaimRemoved)
	}
}

// Orphans age out, because an alarm that never clears is indistinguishable from
// a broken one — and there is no deferred attach, so the finding set otherwise
// only grows.
func TestDemandReconciler_AgesOutOldOrphansOnly(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	insertOrderWithOrigin(t, db, "", protocol.OriginClassOrphan, 48*time.Hour)
	insertOrderWithOrigin(t, db, "", protocol.OriginClassOrphan, time.Hour)
	// Not a finding and must never be touched: no_demand is stamped at the
	// create site by orders that are structurally originless.
	insertOrderWithOrigin(t, db, "", protocol.OriginClassNoDemand, 48*time.Hour)

	eng.reconcileDemandEpisodes()

	counts := map[string]int{}
	rows, err := db.Query(`SELECT origin_class, COUNT(*) FROM orders GROUP BY origin_class`)
	if err != nil {
		t.Fatalf("count classes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts[c] = n
	}
	if counts[protocol.OriginClassOrphanAged] != 1 {
		t.Errorf("orphan_aged = %d, want 1 — the two-day-old orphan should have stopped being asked about",
			counts[protocol.OriginClassOrphanAged])
	}
	if counts[protocol.OriginClassOrphan] != 1 {
		t.Errorf("orphan = %d, want 1 — the one-hour-old orphan is still a live finding",
			counts[protocol.OriginClassOrphan])
	}
	if counts[protocol.OriginClassNoDemand] != 1 {
		t.Errorf("no_demand = %d, want 1 — the sweep must not touch orders that were never a finding",
			counts[protocol.OriginClassNoDemand])
	}
}
