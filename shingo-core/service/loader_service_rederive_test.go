//go:build docker

package service

import (
	"bytes"
	"io"
	"log"
	"strings"
	"sync"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/demands"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// recordingNotifier keeps every change batch rederive hands the monitor, one
// slice per call, so a test can tell "told nothing" from "told an empty list".
type recordingNotifier struct{ calls [][]demands.RegistryChange }

func (r *recordingNotifier) OnThresholdChanges(c []demands.RegistryChange) {
	r.calls = append(r.calls, c)
}

func (r *recordingNotifier) last() []demands.RegistryChange {
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[len(r.calls)-1]
}

// enrollRegisteredEdge makes stationID part of rederive's target set: a
// registered edge with no registry rows yet.
func enrollRegisteredEdge(t *testing.T, db *store.DB, stationID string) {
	t.Helper()
	_, err := db.EnrollEdge(stationID, "", stationID)
	testutil.MustNoErr(t, err, "enroll edge")
	_, err = db.RegisterEdge(stationID, "test-host", "test-inst", "test", "")
	testutil.MustNoErr(t, err, "register edge")
}

func stationRegistryRows(t *testing.T, db *store.DB, stationID string) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT count(*) FROM demand_registry WHERE station_id=$1`, stationID).Scan(&n),
		"count demand_registry")
	return n
}

// TestLoaderServiceRederivePin_ThresholdEditNotifiesMonitor pins what the
// config-edit writer tells the monitor across a threshold's life: set (0->10),
// edited (10->25), and the loader archived (25->0, rows gone).
func TestLoaderServiceRederivePin_ThresholdEditNotifiesMonitor(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	rec := &recordingNotifier{}
	svc := NewLoaderService(db, rec)
	enrollRegisteredEdge(t, db, "ST-RD-PIN")

	id, err := db.CreateLoader(loaders.Loader{Name: "RD-PIN", Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold})
	testutil.MustNoErr(t, err, "create loader")
	slot := &nodes.Node{Name: "RD-PIN-SLOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")

	want := func(step string, old, new int) {
		t.Helper()
		c := rec.last()
		if len(c) != 1 || c[0].StationID != "ST-RD-PIN" || c[0].CoreNodeName != "RD-PIN-SLOT" ||
			c[0].PayloadCode != "P-RD" || c[0].OldThreshold != old || c[0].NewThreshold != new {
			t.Fatalf("%s: monitor told %+v, want ST-RD-PIN/RD-PIN-SLOT/P-RD %d->%d", step, c, old, new)
		}
	}

	testutil.MustNoErr(t, svc.SetHome(id, slot.ID, "P-RD", "", 10), "SetHome 10")
	want("set", 0, 10)
	if n := stationRegistryRows(t, db, "ST-RD-PIN"); n != 1 {
		t.Fatalf("rows after set = %d, want 1", n)
	}

	testutil.MustNoErr(t, svc.SetHome(id, slot.ID, "P-RD", "", 25), "SetHome 25")
	want("edit", 10, 25)

	calls := len(rec.calls)
	testutil.MustNoErr(t, svc.ReorderHomes(id, []int64{slot.ID}), "ReorderHomes")
	if len(rec.calls) != calls {
		t.Fatalf("a re-derive that moved no threshold notified the monitor: %+v", rec.last())
	}

	testutil.MustNoErr(t, svc.Delete(id), "Delete")
	want("archive", 25, 0)
	if n := stationRegistryRows(t, db, "ST-RD-PIN"); n != 0 {
		t.Fatalf("rows after archive = %d, want 0 — an archived loader must leave the registry", n)
	}
}

// lockedBuf is a log sink safe against a background goroutine logging while
// the test reads it.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog tees the standard logger into a buffer for the rest of the test.
// Callers must not be t.Parallel: the logger is process-global.
func captureLog(t *testing.T) *lockedBuf {
	t.Helper()
	b := &lockedBuf{}
	prev := log.Writer()
	log.SetOutput(io.MultiWriter(prev, b))
	t.Cleanup(func() { log.SetOutput(prev) })
	return b
}

// orphanNode deletes a node a loader home still points at, the way a map
// re-import or a hand edit leaves one: the FK is dropped first because the
// schema would otherwise refuse the delete.
func orphanNode(t *testing.T, db *store.DB, nodeID int64) {
	t.Helper()
	_, err := db.DB.Exec(`ALTER TABLE bin_loader_homes DROP CONSTRAINT IF EXISTS bin_loader_homes_position_node_id_fkey`)
	testutil.MustNoErr(t, err, "drop fk")
	_, err = db.DB.Exec(`DELETE FROM nodes WHERE id=$1`, nodeID)
	testutil.MustNoErr(t, err, "delete node")
}

// TestLoaderServiceRederive_UnresolvableInputsKeepRows is the refusal. A
// derivation that yields nothing because it could not resolve its inputs is not
// evidence the station has no demand, so it must not delete the rows it has:
// no rows lost, the monitor told nothing, and one line saying it refused.
//
// Two shapes of "could not resolve": a dedicated home whose node is gone, and a
// shared_window loader whose windows all point at nodes that are gone (it has
// payloads but no address to put them at).
//
// Not parallel: it reads the process-global logger.
func TestLoaderServiceRederive_UnresolvableInputsKeepRows(t *testing.T) {
	cases := []struct {
		name   string
		layout string
		skip   string
	}{
		{"dedicated home node gone", loaders.LayoutDedicatedPositions, "node_gone=1"},
		{"shared window unresolvable", loaders.LayoutSharedWindow, "window_unresolved=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			rec := &recordingNotifier{}
			svc := NewLoaderService(db, rec)
			const st = "ST-RD-RED"
			enrollRegisteredEdge(t, db, st)

			id, err := db.CreateLoader(loaders.Loader{Name: "RD-RED", Role: loaders.RoleProduce,
				Layout: tc.layout, Replenishment: loaders.ReplenishmentThreshold})
			testutil.MustNoErr(t, err, "create loader")
			slot := &nodes.Node{Name: "RD-RED-SLOT", Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(slot), "create slot")
			if tc.layout == loaders.LayoutSharedWindow {
				testutil.MustNoErr(t, svc.SetHome(id, slot.ID, "", "", 0), "SetHome window")
				testutil.MustNoErr(t, svc.SetPayload(id, "P-RED", 10), "SetPayload")
			} else {
				testutil.MustNoErr(t, svc.SetHome(id, slot.ID, "P-RED", "", 10), "SetHome")
			}
			if n := stationRegistryRows(t, db, st); n != 1 {
				t.Fatalf("seeded rows = %d, want 1", n)
			}

			orphanNode(t, db, slot.ID)
			logs := captureLog(t)
			calls := len(rec.calls)
			testutil.MustNoErr(t, svc.Update(LoaderUpdate{ID: id, Name: "RD-RED"}), "Update")

			if n := stationRegistryRows(t, db, st); n != 1 {
				t.Errorf("rows after an unresolvable derive = %d, want 1 — a derivation that could not resolve its inputs deleted the station's demand", n)
			}
			if len(rec.calls) != calls {
				t.Errorf("monitor told %+v after a refused derive, want nothing", rec.last())
			}
			out := logs.String()
			if !strings.Contains(out, "station="+st) || !strings.Contains(out, "outcome=refused") || !strings.Contains(out, tc.skip) {
				t.Errorf("log = %q, want one derive line for %s with outcome=refused and %s", out, st, tc.skip)
			}
		})
	}
}
