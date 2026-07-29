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

// registerEdgeWithoutHeartbeat is the state a station is in between Core
// booting and that station's first heartbeat landing: a registry row, status
// 'active', last_heartbeat NULL. Core has never heard a word from it — and
// under a status-based reachability check it reads as perfectly healthy.
func registerEdgeWithoutHeartbeat(t *testing.T, db *store.DB, stationID string) {
	t.Helper()
	if err := db.RegisterEdge(stationID, "test-host", "test", nil); err != nil {
		t.Fatalf("register edge: %v", err)
	}
}

// silenceEdge ages a station's last heartbeat WITHOUT touching its status —
// which is exactly the tree a broken, unstarted or misconfigured MarkStaleEdges
// leaves behind, because MarkStaleEdges is the only thing that ever moves status
// off 'active'.
func silenceEdge(t *testing.T, db *store.DB, stationID string, silentFor time.Duration) {
	t.Helper()
	if _, err := db.Exec(`UPDATE edge_registry SET last_heartbeat = $1 WHERE station_id = $2`,
		time.Now().UTC().Add(-silentFor), stationID); err != nil {
		t.Fatalf("silence edge: %v", err)
	}
}

// mustEdgeStatus reads edge_registry.status back so a test can PROVE the stale
// flag was never set, rather than assuming it.
func mustEdgeStatus(t *testing.T, db *store.DB, stationID string) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM edge_registry WHERE station_id = $1`,
		stationID).Scan(&status); err != nil {
		t.Fatalf("read edge status: %v", err)
	}
	return status
}

// openCellEpisode writes an Edge-authored cell episode at revision 1, the way
// one arrives over the state-transfer seam, backdated past the childless grace.
func openCellEpisode(t *testing.T, db *store.DB, stationID, payloadCode string, processID int64, age time.Duration) string {
	t.Helper()
	originID := uuid.NewString()
	if err := db.UpsertDemandOrigin(store.DemandOrigin{
		OriginID:   originID,
		Revision:   1,
		EpisodeKey: protocol.CellEpisodeKey(stationID, processID, payloadCode, protocol.EpisodeDirectionSupply),
		Kind:       protocol.EpisodeKindCell,
		Direction:  protocol.EpisodeDirectionSupply,
		StationID:  stationID,
		ProcessID:  processID,
		OpenedAt:   time.Now().UTC().Add(-age),
	}); err != nil {
		t.Fatalf("upsert cell episode: %v", err)
	}
	return originID
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

// REACHABILITY IS A POSITIVE ASSERTION, NOT AN ABSENT FLAG.
//
// The sweep's licence to close a childless episode rests entirely on Core being
// able to say it WOULD have received a child if one had been created. The first
// version of that check asked `edge_registry.status == 'active'` — and status is
// written 'active' by Register and by every heartbeat, and moved off 'active' by
// exactly one thing: MarkStaleEdges, a 60-second loop in CoreHandler, a
// different service from the one running this sweep.
//
// So the check inferred "this Edge is fine" from the ABSENCE OF A MARK, and a
// staleness tracker that is unstarted, misconfigured or itself broken reads
// identically to a healthy plant — at which point the sweep closes EVERY OPEN
// CELL EPISODE IN THE PLANT on the strength of a signal that was never computed.
// A check must know whether it had the input to check, and that rule applies to
// the tiebreak's own input.
//
// Both halves are broken here WITHOUT breaking any code: the tree is exactly
// what production leaves behind when the stale loop does not run.
func TestDemandReconciler_ReachabilityIsAPositiveAssertionNotAnAbsentFlag(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	// (a) Registered at boot, never heartbeated. Core has heard NOTHING from
	// this station, and nothing is broken — this is the ordinary state of every
	// station for the first minute after Core starts.
	registerEdgeWithoutHeartbeat(t, db, "PLANT.MUTE")
	mute := openCellEpisode(t, db, "PLANT.MUTE", "PANEL-RC6", 3, 6*time.Hour)

	// (b) Heartbeated once, then went quiet six hours ago, and NOTHING marked it
	// stale — MarkStaleEdges never ran.
	registerActiveEdge(t, db, "PLANT.DEAF")
	silenceEdge(t, db, "PLANT.DEAF", 6*time.Hour)
	deaf := openCellEpisode(t, db, "PLANT.DEAF", "PANEL-RC7", 4, 6*time.Hour)

	// PROVE THE PREMISE. If the stale loop had run, this test would be measuring
	// the stale loop rather than the guard, and it would pass for the wrong
	// reason on a tree where the guard had been deleted.
	for _, station := range []string{"PLANT.MUTE", "PLANT.DEAF"} {
		if got := mustEdgeStatus(t, db, station); got != "active" {
			t.Fatalf("setup: %s status = %q, want \"active\" — something marked this edge stale, so this test is no longer about a staleness tracker that never ran",
				station, got)
		}
	}

	eng.reconcileDemandEpisodes()

	if got := mustGetOrigin(t, db, mute); got.ClosedAt != nil {
		t.Errorf("the sweep closed an episode on a station Core has NEVER heard from (reason=%q, by=%q) — a missing input was rendered as a finding",
			got.CloseReason, got.ClosedBy)
	}
	if got := mustGetOrigin(t, db, deaf); got.ClosedAt != nil {
		t.Errorf("the sweep closed an episode on a station silent for six hours whose status was still 'active' (reason=%q, by=%q) — reachability was read off the absence of a staleness flag",
			got.CloseReason, got.ClosedBy)
	}

	// (c) And the guard is about RECENCY, not about never closing. One heartbeat
	// lands and the same two episodes become decidable — otherwise the two
	// assertions above are satisfied by a sweep that closes nothing at all.
	if _, err := db.UpdateHeartbeat("PLANT.MUTE"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if _, err := db.UpdateHeartbeat("PLANT.DEAF"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	eng.reconcileDemandEpisodes()
	for name, id := range map[string]string{"PLANT.MUTE": mute, "PLANT.DEAF": deaf} {
		got := mustGetOrigin(t, db, id)
		if got.ClosedAt == nil {
			t.Errorf("%s: a six-hour childless episode stayed open after its Edge heartbeated — the guard is refusing to close rather than refusing to guess", name)
			continue
		}
		if got.CloseReason != protocol.CloseReasonUnattributed {
			t.Errorf("%s: close_reason = %q, want %q", name, got.CloseReason, protocol.CloseReasonUnattributed)
		}
	}
}

// THE A1 RIDER, ASSERTED RATHER THAN ASSUMED.
//
// Core closing a childless CELL episode is Core ending something it does not
// own: the precondition lives in Edge's claims, and Core cannot evaluate it. The
// only thing that makes that safe is that the close is PROVISIONAL — three
// specific facts, all three load-bearing, none of them previously pinned:
//
//   - close_reason `unattributed`, because what Core actually knows is that no
//     order was ever attributed to this demand, not that the demand recovered or
//     was cancelled;
//   - closed_by `sweep`, because the sweep's share of the closing is the only
//     signal that would say the notification paths have silently stopped firing;
//   - THE REVISION DOES NOT MOVE. Edge's real close arrives carrying the
//     revision Edge stamped on it. If Core's guess bumped, that close would lose
//     `WHERE revision < EXCLUDED.revision`, be discarded without an error, and
//     Core's guess would outrank the owner's truth forever.
//
// The no-bump rule may already hold via the general inferred-close path. "May
// already hold" is not the standard, and a rule nothing asserts is one refactor
// from being gone.
func TestDemandReconciler_ChildlessCellCloseIsProvisional(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	registerActiveEdge(t, db, "PLANT.RIDER")

	originID := openCellEpisode(t, db, "PLANT.RIDER", "PANEL-RC8", 5, 3*time.Hour)
	if pre := mustGetOrigin(t, db, originID); pre.Revision != 1 {
		t.Fatalf("setup: revision = %d, want 1 — the assertion below is about a revision that did not move from THIS number", pre.Revision)
	}

	eng.reconcileDemandEpisodes()

	got := mustGetOrigin(t, db, originID)
	if got.ClosedAt == nil {
		t.Fatal("a three-hour childless cell episode on a reachable Edge stayed open")
	}
	if got.CloseReason != protocol.CloseReasonUnattributed {
		t.Errorf("close_reason = %q, want %q", got.CloseReason, protocol.CloseReasonUnattributed)
	}
	if got.ClosedBy != protocol.ClosedBySweep {
		t.Errorf("closed_by = %q, want %q", got.ClosedBy, protocol.ClosedBySweep)
	}
	// The specific number, not "unchanged-ish". A bump to 2 is the value that
	// silently loses Edge's real close.
	if got.Revision != 1 {
		t.Errorf("revision = %d, want 1 — an inferred close bumped the revision, so Edge's real close will lose `WHERE revision < EXCLUDED.revision` and be discarded in silence",
			got.Revision)
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

	before := time.Now().UTC()
	eng.reconcileDemandEpisodes()

	// AGING DOES NOT RECLASSIFY. origin_class is what the create site decided
	// and no clock rewrites it: both orphans are still `orphan` afterwards, and
	// the fourth value does not exist. Counting classes is how a reclassifying
	// sweep would be caught, because the aged row would leave this bucket.
	counts := orderClassCounts(t, db)
	if counts[protocol.OriginClassOrphan] != 2 {
		t.Errorf("orphan = %d, want 2 — aging retires a finding, it does not rewrite how the order related to a demand at creation",
			counts[protocol.OriginClassOrphan])
	}
	if counts["orphan_aged"] != 0 {
		t.Errorf("orphan_aged = %d, want 0 — a clock mutating origin_class leaves the row unable to say what its class was at creation",
			counts["orphan_aged"])
	}
	if counts[protocol.OriginClassNoDemand] != 1 {
		t.Errorf("no_demand = %d, want 1 — the sweep must not touch orders that were never a finding",
			counts[protocol.OriginClassNoDemand])
	}

	// And the column is read BACK and asserted on the VALUE, not on presence.
	// A tree that stamped created_at instead of the sweep's clock passes any
	// "is it set" check and is wrong by two days.
	findings, err := db.ListOrphanFindings()
	if err != nil {
		t.Fatalf("list orphan findings: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("orphan findings = %d, want 2", len(findings))
	}
	fresh, aged := findings[0], findings[1] // ORDER BY orphan_aged_at NULLS FIRST
	if fresh.AgedAt != nil {
		t.Errorf("the one-hour-old orphan was aged out at %s — it is still a live finding", fresh.AgedAt)
	}
	if aged.AgedAt == nil {
		t.Fatal("the two-day-old orphan has no orphan_aged_at — it never stopped being asked about")
	}
	if aged.AgedAt.Before(before) || aged.AgedAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("orphan_aged_at = %s, want the sweep's own clock (between %s and now) — a timestamp taken from the order rather than the sweep is wrong by the whole grace period",
			aged.AgedAt.UTC(), before)
	}

	// A SECOND PASS MUST NOT MOVE IT. The sweep runs every minute forever;
	// without the `orphan_aged_at IS NULL` guard "when did this stop being asked
	// about" would permanently read "a minute ago", and Phase 6's trend line
	// would be flat by construction.
	firstStamp := *aged.AgedAt
	eng.reconcileDemandEpisodes()
	again, err := db.ListOrphanFindings()
	if err != nil {
		t.Fatalf("list orphan findings (second pass): %v", err)
	}
	if len(again) != 2 || again[1].AgedAt == nil {
		t.Fatalf("second pass lost the aged orphan: %+v", again)
	}
	if !again[1].AgedAt.Equal(firstStamp) {
		t.Errorf("orphan_aged_at moved from %s to %s on the second sweep — the timestamp is measuring the sweep, not the order",
			firstStamp, again[1].AgedAt.UTC())
	}
}

func orderClassCounts(t *testing.T, db *store.DB) map[string]int {
	t.Helper()
	rows, err := db.Query(`SELECT origin_class, COUNT(*) FROM orders GROUP BY origin_class`)
	if err != nil {
		t.Fatalf("count classes: %v", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts[c] = n
	}
	return counts
}
