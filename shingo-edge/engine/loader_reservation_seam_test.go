package engine

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
)

// inFlightEmpties counts non-terminal retrieve_empty orders across a delivery set
// — the quantity the never-2N invariant bounds.
func inFlightEmpties(t *testing.T, db *store.DB, nodes []string) int {
	t.Helper()
	list, err := db.ListActiveOrdersByDeliveryNodeSet(nodes)
	if err != nil {
		t.Fatalf("ListActiveOrdersByDeliveryNodeSet(%v): %v", nodes, err)
	}
	n := 0
	for _, o := range list {
		if o.RetrieveEmpty {
			n++
		}
	}
	return n
}

// TestRace_LoaderBudget_ConcurrentSignalsAndOperator is the C1 gate. A demand
// signal (Kafka path → tryCreateL1) and an operator REQUEST (HTTP path →
// RequestEmptyBin) hammer ONE loader from many goroutines. The reservation seam
// must serialise count→fire per loader so the loader's in-flight empties never
// exceed its budget (1 in C1, the single delivery node). Run under -race: the
// detector covers the seam's own shared state (the keyed mutex map), the
// assertion covers the logical never-2N invariant. Pre-seam this races to 2+.
func TestRace_LoaderBudget_ConcurrentSignalsAndOperator(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedCapManualSwap(t, db, "RACE", "LOADER-1", protocol.ClaimRoleProduce, []string{"P1"}, 2, false)
	// Seed the Core-loader cache so BOTH the automatic path (tryCreateL1) and the
	// operator path (RequestEmptyBin) resolve the SAME aggregate loader — and lock
	// the same loader_key mutex. (Without this both paths no-op/error and the race
	// would be vacuous.)
	seedCoreLoader(t, eng, sharedLoaderInfo("LOADER-1", "produce", "threshold", "P1", 0, 100))
	dl, err := eng.loaders().LoaderAt("LOADER-1", domain.RoleProduce)
	if err != nil || dl == nil {
		t.Fatalf("loader did not resolve from the aggregate: %v", err)
	}

	const goroutines = 24
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(g int) {
			defer wg.Done()
			if g%2 == 0 {
				// automatic/threshold path: wants 2, seam caps to the budget (1)
				_, _ = eng.tryCreateL1(dl, "P1", L1LoopThreshold, 2, "")
			} else {
				// operator path: a single empty request through the same seam
				_, _ = eng.RequestEmptyBin(nodeID, "P1")
			}
		}(g)
	}
	wg.Wait()

	if got := inFlightEmpties(t, db, []string{"LOADER-1"}); got > 1 {
		t.Fatalf("loader budget violated: %d in-flight empties at a 1-slot loader after %d concurrent ops (want <= 1)", got, goroutines)
	}
}

// TestReserveLoaderEmpties_PropNeverExceedsBudget is PropLoaderBudgetNeverExceeded:
// mustSharedLoader builds a single-window shared loader for the seam tests (the
// C3 shape: ReservationTarget funnels to the anchor, budget 1).
func mustSharedLoader(t *testing.T, id string, payloads ...string) *domain.Loader {
	t.Helper()
	ps := make([]domain.PayloadCode, len(payloads))
	for i, p := range payloads {
		ps[i] = domain.PayloadCode(p)
	}
	l, err := domain.NewSharedWindowLoader(domain.LoaderID(id), id, domain.RoleProduce, domain.ReplenishmentThreshold,
		[]domain.Window{{Node: domain.NodeID(id)}}, ps, domain.WithInboundSource("EMPTY-SUPER"))
	if err != nil {
		t.Fatalf("build loader %s: %v", id, err)
	}
	return l
}

// a deterministic randomized sequence of reservations (across two payloads, with
// occasional completions) exercises the seam's per-payload dedup AND loader
// capacity cap together. The invariant `in-flight at the loader <= budget` must
// hold after EVERY step. Budget is 1 in C3 — a shared loader funnels to its
// anchor (ReservationTarget); the budget=N multi-window property lands in C4 when
// ReservationTarget widens to the window cluster.
func TestReserveLoaderEmpties_PropNeverExceedsBudget(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedCapManualSwap(t, db, "PROP", "PROP-LDR", protocol.ClaimRoleProduce, []string{"P1", "P2"}, 0, false)
	loader := mustSharedLoader(t, "PROP-LDR", "P1", "P2")

	const budget = 1
	payloads := []domain.PayloadCode{"P1", "P2"}
	nodes := []string{"PROP-LDR"}
	rng := rand.New(rand.NewSource(20260612))

	reserve := func(payload domain.PayloadCode, want int) {
		_, err := eng.reserveLoaderBins(loader, payload, want, "", true, func(deliveryNodes []string) (int, error) {
			made := 0
			for _, deliveryNode := range deliveryNodes {
				if _, cerr := eng.orderMgr.CreateRetrieveOrder(&nodeID, true, 1, deliveryNode, "EMPTY-SUPER", "", "standard", string(payload), false, true); cerr != nil {
					return made, cerr
				}
				made++
			}
			return made, nil
		})
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
	}

	for step := range 200 {
		switch rng.Intn(3) {
		case 0, 1: // reserve a random want for a random payload
			reserve(payloads[rng.Intn(len(payloads))], rng.Intn(budget+2))
		case 2: // complete (terminalize) a random in-flight empty, freeing budget
			list, err := db.ListActiveOrdersByDeliveryNodeSet(nodes)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(list) > 0 {
				victim := list[rng.Intn(len(list))]
				if err := db.UpdateOrderStatus(victim.ID, string(protocol.StatusConfirmed)); err != nil {
					t.Fatalf("terminalize: %v", err)
				}
			}
		}
		if got := inFlightEmpties(t, db, nodes); got > budget {
			t.Fatalf("step %d: in-flight %d exceeds budget %d", step, got, budget)
		}
	}
}

// TestReserveLoaderEmpties_EmitDuringReservation_NoDeadlock pins the re-entrancy
// rule. `fire` runs while the loader's mutex is held and CreateRetrieveOrder
// fires EmitOrderCreated synchronously on the in-process bus; a subscriber that
// re-enters the seam for a DIFFERENT loader (a distinct lock) must proceed, and
// the whole reservation must complete — never self-deadlock. (Same-loader
// re-entry is the forbidden case the rule documents; it cannot be unit-tested
// without hanging, which is the point of the rule.)
func TestReserveLoaderEmpties_EmitDuringReservation_NoDeadlock(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	var reentered bool
	eng.Events.Subscribe(func(evt Event) {
		if evt.Type != EventOrderCreated {
			return
		}
		// Re-enter the seam for a DIFFERENT loader from inside the synchronous
		// emit — a separate lock, so it must not deadlock. (eventbus.Emit
		// dispatches subscribers inline on the emitting goroutine, so this runs
		// while DLK-A's lock is still held.)
		_, _ = eng.reserveLoaderBins(mustSharedLoader(t, "DLK-B", "P1"), "P1", 1, "", true, func([]string) (int, error) {
			reentered = true
			return 0, nil // no fire — we exercise the lock, not order creation
		})
	})

	done := make(chan struct{})
	go func() {
		_, _ = eng.reserveLoaderBins(mustSharedLoader(t, "DLK-A", "P1"), "P1", 1, "", true, func(deliveryNodes []string) (int, error) {
			// In production CreateRetrieveOrder fires EmitOrderCreated synchronously
			// here, under the lock. The test order-emitter is a no-op, so emit it
			// directly to exercise a synchronous subscriber callback in the locked
			// region — the exact re-entrancy hazard the pinned rule governs.
			eng.Events.Emit(Event{Type: EventOrderCreated, Payload: OrderCreatedEvent{OrderID: 1, OrderType: protocol.OrderTypeRetrieve}})
			return len(deliveryNodes), nil
		})
		close(done)
	}()

	select {
	case <-done:
		if !reentered {
			t.Fatal("subscriber did not run — the emit-during-reservation path was not exercised")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: a reservation that emitted under the loader lock did not complete within 5s")
	}
}

// nodeBinsStub serves Core's /api/telemetry/node-bins, reporting the given
// occupancy for occupiedNode and Occupied=false for every other requested node —
// the minimal Core telemetry the seam's resident-empty gate reads. A resident
// empty is modelled as occupied with uop_remaining=0 (Core marks a window Occupied
// for ANY resident bin, empty included).
func nodeBinsStub(t *testing.T, occupiedNode string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/telemetry/node-bins" {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		rows := []map[string]any{}
		for n := range strings.SplitSeq(r.URL.Query().Get("nodes"), ",") {
			row := map[string]any{"node_name": n, "occupied": false}
			if n == occupiedNode {
				row["occupied"] = true
				row["uop_remaining"] = 0 // an EMPTY carrier still occupies the window
			}
			rows = append(rows, row)
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fireOneEmptyPerWindow is the reserve `fire` closure the resident-gate tests share:
// it creates one retrieve_empty per delivery window and reports how many it made.
func fireOneEmptyPerWindow(eng *Engine, nodeID int64) func([]string) (int, error) {
	return func(deliveryNodes []string) (int, error) {
		made := 0
		for _, dn := range deliveryNodes {
			if _, cerr := eng.orderMgr.CreateRetrieveOrder(&nodeID, true, 1, dn, "EMPTY-SUPER", "", "standard", "P1", false, true); cerr != nil {
				return made, cerr
			}
			made++
		}
		return made, nil
	}
}

// TestReserveLoaderBins_SuppressesWhenWindowHasResidentEmpty is the Springfield
// SMN_014 regression (2026-07-23). A 0-UOP empty already stands on the loader's
// only window, so system UOP reads 0 < threshold and Core keeps signalling — but
// another empty is useless: the loader operator just needs to LOAD the one that's
// there. With ZERO inbound orders the order-count dedup can't suppress it; the seam
// must count the resident bin as occupying the window and fire nothing.
func TestReserveLoaderBins_SuppressesWhenWindowHasResidentEmpty(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedCapManualSwap(t, db, "RESIDENT", "LOADER-1", protocol.ClaimRoleProduce, []string{"P1"}, 0, false)
	loader := mustSharedLoader(t, "LOADER-1", "P1")
	eng.coreClient = NewCoreClient(nodeBinsStub(t, "LOADER-1").URL)

	created, err := eng.reserveLoaderBins(loader, "P1", 1, "", true, fireOneEmptyPerWindow(eng, nodeID))
	if err != nil {
		t.Fatalf("reserveLoaderBins: %v", err)
	}
	if created != 0 {
		t.Fatalf("resident empty present but seam fired %d empties; want 0 (operator must load the resident carrier)", created)
	}
	if got := inFlightEmpties(t, db, []string{"LOADER-1"}); got != 0 {
		t.Fatalf("in-flight empties = %d after suppressed reserve; want 0", got)
	}
}

// TestReserveLoaderBins_FiresWhenWindowEmpty is the negative control: an
// unoccupied window still gets its empty, so the resident gate can't over-suppress.
func TestReserveLoaderBins_FiresWhenWindowEmpty(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedCapManualSwap(t, db, "EMPTYWIN", "LOADER-1", protocol.ClaimRoleProduce, []string{"P1"}, 0, false)
	loader := mustSharedLoader(t, "LOADER-1", "P1")
	eng.coreClient = NewCoreClient(nodeBinsStub(t, "OTHER-NODE").URL) // LOADER-1 reported empty

	created, err := eng.reserveLoaderBins(loader, "P1", 1, "", true, fireOneEmptyPerWindow(eng, nodeID))
	if err != nil {
		t.Fatalf("reserveLoaderBins: %v", err)
	}
	if created != 1 {
		t.Fatalf("empty window but seam fired %d empties; want 1", created)
	}
}

// TestReserveLoaderBins_FiresWhenCoreUnreachable documents the fail-open contract:
// when Core telemetry can't be read (no base URL), the gate is skipped and the seam
// falls back to the order-only count — no worse than before, Core's dropoff guard
// still backstops any redundant empty.
func TestReserveLoaderBins_FiresWhenCoreUnreachable(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedCapManualSwap(t, db, "NOCORE", "LOADER-1", protocol.ClaimRoleProduce, []string{"P1"}, 0, false)
	loader := mustSharedLoader(t, "LOADER-1", "P1")
	eng.coreClient = NewCoreClient("") // Core telemetry unavailable

	created, err := eng.reserveLoaderBins(loader, "P1", 1, "", true, fireOneEmptyPerWindow(eng, nodeID))
	if err != nil {
		t.Fatalf("reserveLoaderBins: %v", err)
	}
	if created != 1 {
		t.Fatalf("Core unreachable: gate must skip and fire from the order-only count; fired %d, want 1", created)
	}
}
