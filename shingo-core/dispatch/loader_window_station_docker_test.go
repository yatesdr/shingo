//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// TestWindowStation_NeverARemovedStation: a window whose node assignment names
// a station Remove station took off is not addressed to it (closing ruling
// 2026-11-22: after the removal Core addresses nothing to the removed uid). The
// plant's one Edge left takes the window's notices, as for a window that names
// no one station.
func TestWindowStation_NeverARemovedStation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const live, gone = "stn-ws-live", "stn-ws-gone"
	for _, uid := range []string{live, gone} {
		_, err := db.EnrollEdge(uid, uid)
		testutil.MustNoErr(t, err, "enroll "+uid)
	}
	w := &nodes.Node{Name: "WS-W1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(w), "window")
	testutil.MustNoErr(t, db.AssignNodeToStation(w.ID, gone), "the window names the station")
	d := &Dispatcher{db: db}
	if got := d.windowStation(w); got != gone {
		t.Fatalf("setup: the window is addressed to %q, want %q", got, gone)
	}
	_, err := db.Exec(`UPDATE edge_registry SET status = 'stale' WHERE station_uid = $1`, gone)
	testutil.MustNoErr(t, err, "stale")
	_, err = db.RemoveStaleStation(gone, "test")
	testutil.MustNoErr(t, err, "remove the station")
	if got := d.windowStation(w); got != live {
		t.Errorf("after the removal the window is addressed to %q, want the plant's one Edge left, %q", got, live)
	}
}
