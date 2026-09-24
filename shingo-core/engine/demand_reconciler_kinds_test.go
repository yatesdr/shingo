//go:build docker

package engine

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/fleet/simulator"
	"shingocore/store"
)

// demand_reconciler_kinds_test.go — WHICH KINDS THE CHILDLESS PASS MAY CLOSE.
//
// Its evidence is "Core never heard an order attributed to this demand", which
// only means something for demand whose orders arrive from somewhere else: the
// Edge-authored kinds, cell and changeover. The Core-authored kinds, maintain
// and threshold, create their own orders in this process with the origin
// already stamped, so zero children there is not a missing message.

// A CHANGEOVER EPISODE IS EDGE-AUTHORED, SO ZERO CHILDREN PAST THE GRACE, ON AN
// EDGE CORE HAS HEARD FROM RECENTLY, IS A FINDING: it closes `unattributed`.
func TestDemandReconciler_ChildlessChangeoverEpisodeCloses(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	const station = "PLANT.CO"
	registerActiveEdge(t, db, station)

	originID := uuid.NewString()
	if err := db.UpsertDemandOrigin(store.DemandOrigin{
		OriginID:   originID,
		Revision:   1,
		EpisodeKey: protocol.ChangeoverEpisodeKey(station, 42),
		Kind:       protocol.EpisodeKindChangeover,
		StationID:  station,
		OpenedAt:   time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("upsert changeover episode: %v", err)
	}

	eng.reconcileDemandEpisodes()

	got := mustGetOrigin(t, db, originID)
	if got.ClosedAt == nil || got.CloseReason != protocol.CloseReasonUnattributed {
		t.Errorf("childless changeover episode: closed=%v reason=%q, want closed %q",
			got.ClosedAt != nil, got.CloseReason, protocol.CloseReasonUnattributed)
	}
}

// A THRESHOLD EPISODE IS CORE-AUTHORED, SO THE PASS LEAVES IT ALONE.
//
// fireSignalCached creates the orders in this process and stamps the open
// origin on them, so a threshold episode with no children is a demand whose
// orders were not wanted yet: debounce held the fire, the loader's windows
// were full, or its config refused. That is the maintain exemption's
// reasoning word for word. Closing it wrote an `unattributed` row the monitor
// never heard about. The monitor then kept stamping the closed origin on new
// orders, and never minted a replacement while the level stayed below.
// Springfield: 865 threshold episodes closed this way, 30% of all of them, and
// 195 orders stamped with an origin that was already closed.
func TestDemandReconciler_LeavesThresholdEpisodesToTheMonitor(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	m := eng.thresholdMonitor
	b := episodeBinding(t, eng, "PANEL-RCK1", 18)
	registerBinding(t, db, b)
	registerActiveEdge(t, db, b.stationID)

	m.checkBindings([]thresholdEntry{b}, 40, "below_threshold")
	open, _ := db.ListOpenThresholdEpisodes()
	if len(open) != 1 {
		t.Fatalf("setup: no threshold episode opened: %d", len(open))
	}
	originID := open[0].OriginID
	backdateEpisode(t, db, originID, time.Hour)

	eng.reconcileDemandEpisodes()

	if got := mustGetOrigin(t, db, originID); got.ClosedAt != nil {
		t.Errorf("the childless pass closed a threshold episode as %q by %q — the monitor owns ending these",
			got.CloseReason, got.ClosedBy)
	}
}
