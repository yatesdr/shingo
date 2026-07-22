package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
)

// loader_occupied_slot_test.go — the reservation seam must spend a slot's budget
// on the BIN STANDING ON IT, not just on orders inbound to it.
//
// The gap this pins shut (Springfield SMN_014, 2026-07-22): a dedicated home held
// an unloaded empty carrier. An unloaded carrier carries 0 UOP, so the plant-wide
// threshold monitor read the loop as empty and kept signalling; the seam counted
// only in-flight ORDERS, saw none, and fired another empty at the same one-bin
// slot. Core's dropoff-capacity check then parked it at waiting_for_slot, where it
// sat until the operator caught up — and the same demand re-fired on every delta.
//
// The loader's demand is "an empty carrier is standing at this home for the
// operator to fill". Once it is standing there the demand is MET; what the plant
// needs next is the operator, not more transport.

// fakeNodeBinsServer serves /api/telemetry/node-bins for the given node→occupied
// map, in the shape BinAtLineside matches on (one row per requested node, keyed by
// node_name). Nodes absent from the map are reported unoccupied, which is also how
// Core answers for a known-but-empty slot.
func fakeNodeBinsServer(t *testing.T, occupied map[string]bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/telemetry/node-bins" {
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		node := r.URL.Query().Get("nodes")
		row := map[string]any{"node_name": node, "occupied": occupied[node]}
		if occupied[node] {
			row["bin_id"] = 1
			row["bin_label"] = "CARRIER-0001"
		}
		json.NewEncoder(w).Encode([]map[string]any{row})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// dedicatedHomeLoaderInfo builds a one-home dedicated_positions produce loader —
// the Springfield "Supermarket Dedicated Locations" shape reduced to a single
// position, which is the unit the seam reserves against (budget 1 per home).
func dedicatedHomeLoaderInfo(coreNode, payload string, uopThreshold int) protocol.LoaderInfo {
	return protocol.LoaderInfo{
		Name:          coreNode,
		LoaderKey:     "loader:" + coreNode,
		Role:          "produce",
		Layout:        "dedicated_positions",
		Replenishment: "threshold",
		InboundSource: "EMPTY-SUPER",
		OutboundDest:  "FG-MARKET",
		ConfigGen:     1,
		Positions: []protocol.LoaderPosition{{
			CoreNodeName: coreNode,
			Kind:         "dedicated",
			PayloadCode:  payload,
			UOPThreshold: uopThreshold,
		}},
	}
}

// countEmptiesTo counts non-terminal retrieve_empty orders bound for a node.
func countEmptiesTo(t *testing.T, db *store.DB, node string) int {
	t.Helper()
	list, err := db.ListActiveOrdersByDeliveryNodeSet([]string{node})
	if err != nil {
		t.Fatalf("ListActiveOrdersByDeliveryNodeSet(%s): %v", node, err)
	}
	n := 0
	for _, o := range list {
		if o.RetrieveEmpty {
			n++
		}
	}
	return n
}

// occupiedSlotFixture wires a dedicated home loader whose single position reports
// the given occupancy to Core, and returns the engine plus the resolved loader.
func occupiedSlotFixture(t *testing.T, node string, occupied bool) (*Engine, *store.DB, *domain.Loader) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	seedCapManualSwap(t, db, "PROC-"+node, node, protocol.ClaimRoleProduce, []string{"P1"}, 0, false)
	seedCoreLoader(t, eng, dedicatedHomeLoaderInfo(node, "P1", 100))
	eng.coreClient = NewCoreClient(fakeNodeBinsServer(t, map[string]bool{node: occupied}).URL)

	l, err := eng.loaders().LoaderAt(domain.NodeID(node), domain.RoleProduce)
	if err != nil || l == nil {
		t.Fatalf("loader did not resolve for %s: %v", node, err)
	}
	return eng, db, l
}

// TestReserveLoaderBins_OccupiedHomeFiresNothing is the guard. A home already
// holding a carrier has no room for another, so the reservation must spend its
// budget and fire zero — no matter how far below threshold the loop reads.
func TestReserveLoaderBins_OccupiedHomeFiresNothing(t *testing.T) {
	t.Parallel()
	eng, db, l := occupiedSlotFixture(t, "HOME-OCC", true)

	created, err := eng.tryCreateL1(l, "P1", L1LoopThreshold, 1, "HOME-OCC")
	if err != nil {
		t.Fatalf("tryCreateL1: %v", err)
	}
	if created != 0 {
		t.Errorf("created = %d L1 into an occupied home; want 0 — the carrier is already there, the operator just has not loaded it", created)
	}
	if n := countEmptiesTo(t, db, "HOME-OCC"); n != 0 {
		t.Errorf("in-flight empties at the occupied home = %d, want 0", n)
	}
}

// TestReserveLoaderBins_EmptyHomeStillFires is the other half: the guard must not
// suppress a genuine replenishment. An empty home below threshold still gets its
// carrier, so the fix cannot starve the loop it was meant to keep honest.
func TestReserveLoaderBins_EmptyHomeStillFires(t *testing.T) {
	t.Parallel()
	eng, db, l := occupiedSlotFixture(t, "HOME-FREE", false)

	created, err := eng.tryCreateL1(l, "P1", L1LoopThreshold, 1, "HOME-FREE")
	if err != nil {
		t.Fatalf("tryCreateL1: %v", err)
	}
	if created != 1 {
		t.Errorf("created = %d L1 into a FREE home; want 1 — an empty home below threshold must still be replenished", created)
	}
	if n := countEmptiesTo(t, db, "HOME-FREE"); n != 1 {
		t.Errorf("in-flight empties at the free home = %d, want 1", n)
	}
}

// TestReserveLoaderBins_UnknownOccupancyFiresNothing pins the FAIL-CLOSED
// direction. When Core is configured but will not answer, "unknown" must not read
// as "empty" — a suppression guard that passes on an unknown answer suppresses
// nothing exactly when the plant is in trouble. The seam surfaces the error and
// fires none; the next signal retries.
func TestReserveLoaderBins_UnknownOccupancyFiresNothing(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	seedCapManualSwap(t, db, "PROC-DEAD", "HOME-DEAD", protocol.ClaimRoleProduce, []string{"P1"}, 0, false)
	seedCoreLoader(t, eng, dedicatedHomeLoaderInfo("HOME-DEAD", "P1", 100))

	// A server that is configured but refuses to answer — the Core-unreachable
	// shape BinAtLineside reports as (nil, false, err), distinct from a confirmed
	// empty. (httptest closed immediately so the dial fails.)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	eng.coreClient = NewCoreClient(deadURL)

	l, err := eng.loaders().LoaderAt("HOME-DEAD", domain.RoleProduce)
	if err != nil || l == nil {
		t.Fatalf("loader did not resolve: %v", err)
	}

	created, terr := eng.tryCreateL1(l, "P1", L1LoopThreshold, 1, "HOME-DEAD")
	if terr == nil {
		t.Error("tryCreateL1 returned nil error on unknown occupancy; want the fail-closed error")
	}
	if created != 0 {
		t.Errorf("created = %d L1 on unknown occupancy; want 0 (fail closed)", created)
	}
	if n := countEmptiesTo(t, db, "HOME-DEAD"); n != 0 {
		t.Errorf("in-flight empties after a failed occupancy read = %d, want 0", n)
	}
}

// TestReserveLoaderBins_NoCoreAPIKeepsPriorBehaviour: an Edge with no Core API
// wired never had this guard, so it must degrade to the pre-change behaviour
// rather than wedging replenishment plant-wide. This is the case every existing
// seam test runs in (testEngine builds no coreClient), pinned explicitly so the
// nil-client contract is not lost to a refactor.
func TestReserveLoaderBins_NoCoreAPIKeepsPriorBehaviour(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	seedCapManualSwap(t, db, "PROC-NOAPI", "HOME-NOAPI", protocol.ClaimRoleProduce, []string{"P1"}, 0, false)
	seedCoreLoader(t, eng, dedicatedHomeLoaderInfo("HOME-NOAPI", "P1", 100))
	if eng.coreClient.Available() {
		t.Fatal("fixture invariant: testEngine must build no Core client")
	}

	l, err := eng.loaders().LoaderAt("HOME-NOAPI", domain.RoleProduce)
	if err != nil || l == nil {
		t.Fatalf("loader did not resolve: %v", err)
	}

	created, terr := eng.tryCreateL1(l, "P1", L1LoopThreshold, 1, "HOME-NOAPI")
	if terr != nil {
		t.Fatalf("tryCreateL1 with no Core API: %v", terr)
	}
	if created != 1 {
		t.Errorf("created = %d with no Core API; want 1 (guard unavailable → prior behaviour)", created)
	}
}
