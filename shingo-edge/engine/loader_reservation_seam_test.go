package engine

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"shingoedge/orders"
	"strings"
	"sync"
	"sync/atomic"
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

// TestRace_LoaderBudget_ConcurrentSignalsAndOperator is the seam's concurrency
// gate. A demand signal (Kafka path → tryCreateL1) and an operator REQUEST (HTTP
// path → RequestEmptyBin) hammer ONE loader from many goroutines. The seam must
// serialise count→fire per loader so the loader's in-flight empties never exceed
// its budget (1 here — a single delivery node). Run under -race: the
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
				_, _ = eng.tryCreateL1(dl, "P1", L1LoopThreshold, 2, "", orders.Origin{})
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

// mustSharedLoader builds a SINGLE-window shared loader for the seam tests.
// ReservationTarget resolves it to that one window with a budget of 1, whether or
// not multi-window delivery is enabled. Use mustMultiWindowLoader when the test
// needs a budget above 1.
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

// TestReserveLoaderEmpties_PropNeverExceedsBudget drives a deterministic
// randomized sequence of reservations (two payloads, with occasional
// completions) to exercise the seam's per-payload dedup and its loader-capacity
// cap together. The invariant — in-flight at the loader <= budget — must hold
// after EVERY step.
//
// SCOPE, because this test is easy to over-read. It covers the ORDER-COUNTING
// budget only, on a single-window loader at budget 1:
//
//   - No coreClient is installed, so Available() is false and the seam's
//     resident-bin gate never runs. Occupancy is covered by
//     TestReserveLoaderBins_PropOccupancyLive.
//   - Budget above 1 is covered by TestMultiWindow_DemandOfN_ExactlyNAcrossWindows
//     and the occupancy-live property, both multi-window.
//
// (An earlier version of this comment said the budget=N property "lands in C4
// when ReservationTarget widens to the window cluster." That shipped —
// ReservationTarget spreads across windows whenever multi-window delivery is on,
// which is the default.)
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

// occupancyStub is a MUTABLE /api/telemetry/node-bins stub: the test flips which
// windows hold a bin between steps and the seam reads current state on each
// reserve. nodeBinsStub is fixed at construction and cannot model a bin arriving
// mid-run, which is the situation the resident-bin gate exists for.
type occupancyStub struct {
	mu       sync.Mutex
	occupied map[string]bool
	hits     atomic.Int64
	srv      *httptest.Server
}

func newOccupancyStub(t *testing.T) *occupancyStub {
	t.Helper()
	s := &occupancyStub{occupied: map[string]bool{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/telemetry/node-bins" {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		s.hits.Add(1)
		s.mu.Lock()
		defer s.mu.Unlock()
		rows := []map[string]any{}
		for n := range strings.SplitSeq(r.URL.Query().Get("nodes"), ",") {
			row := map[string]any{"node_name": n, "occupied": s.occupied[n]}
			if s.occupied[n] {
				row["uop_remaining"] = 0 // an EMPTY carrier still occupies the window
			}
			rows = append(rows, row)
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *occupancyStub) set(node string, occupied bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.occupied[node] = occupied
}

func (s *occupancyStub) isOccupied(node string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.occupied[node]
}

// TestReserveLoaderBins_PropOccupancyLive is the never-2N property run with the
// resident-bin gate LIVE, across a multi-window loader at budget > 1.
//
// The existing PropNeverExceedsBudget covers neither: it installs no coreClient,
// so Available() is false and the whole occupancy block is skipped, and it pins
// budget=1 on a single node. The property it proves is therefore the order-only
// budget — the one thing the 2026-07-31 incident did NOT violate. This test
// covers the gap.
//
// Two properties hold at every step:
//
//	A — in-flight empties across the window set never exceed the budget.
//	B — no window gains an empty while Core reports it occupied. Measured as a
//	    per-window delta across each demand, because that is the only form the
//	    property can take: a bin arriving at a window that already has an empty
//	    inbound is legitimate and must not fail the run.
//
// Demands go through tryCreateL1, the production caller, rather than a
// hand-rolled fire closure — same idiom as the multi-window tests. Occupancy
// flips between steps, so the run exercises bins arriving and leaving underneath
// in-flight orders rather than a fixed world.
func TestReserveLoaderBins_PropOccupancyLive(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	mw := true
	eng.cfg.LoadersMultiWindow = &mw

	windows := []string{"OCC-W1", "OCC-W2", "OCC-W3"}
	seedWindowNodes(t, db, "OCC-PROC", windows)
	loader := mustMultiWindowLoader(t, "OCC-LDR", windows, "P1", "P2")

	stub := newOccupancyStub(t)
	eng.coreClient = NewCoreClient(stub.srv.URL)

	// Fixture guard: this test earns its keep only at budget > 1. If the loader
	// shape or the multi-window default changes so it funnels to one anchor, the
	// run would silently degrade into a duplicate of the existing property test.
	if _, budget := loader.ReservationTarget("", "P1", eng.multiWindowEnabled()); budget != len(windows) {
		t.Fatalf("fixture: ReservationTarget budget = %d, want %d (multi-window spread); "+
			"budget=1 is already covered by TestReserveLoaderEmpties_PropNeverExceedsBudget", budget, len(windows))
	}

	payloads := []string{"P1", "P2"}
	rng := rand.New(rand.NewSource(20260801))

	for step := range 300 {
		switch rng.Intn(4) {
		case 0, 1: // a demand for a random payload
			payload := payloads[rng.Intn(len(payloads))]
			_, before := windowCounts(t, db, windows)
			if _, err := eng.tryCreateL1(loader, domain.PayloadCode(payload), L1LoopThreshold, rng.Intn(len(windows)+1), "", orders.Origin{}); err != nil {
				t.Fatalf("step %d: tryCreateL1: %v", step, err)
			}
			_, after := windowCounts(t, db, windows)
			for _, w := range windows { // property B
				if after[w] > before[w] && stub.isOccupied(w) {
					t.Errorf("step %d: window %q gained an empty (%d→%d) for %s while Core reported it OCCUPIED",
						step, w, before[w], after[w], payload)
				}
			}
		case 2: // a bin arrives at, or is taken from, a window
			stub.set(windows[rng.Intn(len(windows))], rng.Intn(2) == 0)
		case 3: // terminalize a random in-flight empty, freeing budget
			list, err := db.ListActiveOrdersByDeliveryNodeSet(windows)
			if err != nil {
				t.Fatalf("step %d: list: %v", step, err)
			}
			if len(list) > 0 {
				victim := list[rng.Intn(len(list))]
				if err := db.UpdateOrderStatus(victim.ID, string(protocol.StatusConfirmed)); err != nil {
					t.Fatalf("step %d: terminalize: %v", step, err)
				}
			}
		}
		if got := inFlightEmpties(t, db, windows); got > len(windows) { // property A
			t.Fatalf("step %d: in-flight %d exceeds budget %d", step, got, len(windows))
		}
	}

	// Without this the test can pass while proving nothing: a stub that is never
	// reached leaves the occupancy block skipped and both properties trivially
	// true. This is precisely how the existing property test came to look like
	// coverage of a path it never executed.
	if stub.hits.Load() == 0 {
		t.Fatal("occupancy stub was never called — the resident-bin gate did not run, so this is the order-only property under a new name")
	}
}

// inFlightFulls counts non-terminal U1 full-in orders (retrieve_empty=false)
// across a delivery set — the consume-side mirror of inFlightEmpties.
func inFlightFulls(t *testing.T, db *store.DB, nodes []string) int {
	t.Helper()
	list, err := db.ListActiveOrdersByDeliveryNodeSet(nodes)
	if err != nil {
		t.Fatalf("ListActiveOrdersByDeliveryNodeSet(%v): %v", nodes, err)
	}
	n := 0
	for _, o := range list {
		if !o.RetrieveEmpty {
			n++
		}
	}
	return n
}

// nodeBinsPayloadStub serves per-node occupancy WITH payload codes. Neither
// existing stub can do this: nodeBinsStub reports node_name but no payload, and
// fakeCoreBinServer reports a payload but no node_name. The unloader guard needs
// both at once. Nodes absent from `resident` report unoccupied.
func nodeBinsPayloadStub(t *testing.T, resident map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/telemetry/node-bins" {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		rows := []map[string]any{}
		for n := range strings.SplitSeq(r.URL.Query().Get("nodes"), ",") {
			row := map[string]any{"node_name": n, "occupied": false}
			if p, ok := resident[n]; ok {
				row["occupied"] = true
				row["payload_code"] = p
			}
			rows = append(rows, row)
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCreateUnloaderFullIn_PayloadSpecificGuard pins what the consume side
// ACTUALLY does when a full is parked on an unloader window, which is not what
// the guard alone suggests.
//
// unloaderHasUsableFullPresent is payload-specific: it suppresses only when the
// resident full matches the payload asked for. But it is not the only gate. The
// seam's resident-bin count (operator_demand_loader.go) is payload-AGNOSTIC — it
// charges ANY occupied window against the budget. So the payload-specific guard
// is only observable where the loader has a free window left to route to.
//
// The three cases below separate the two gates:
//
//	multi-window, same payload   → guard suppresses (never reaches the seam)
//	multi-window, other payload  → guard passes, seam routes to the FREE window
//	single-window, other payload → guard passes, seam suppresses on budget
//
// The third case is the one worth knowing before the consume producer moves to
// Core. Core's CheckDropoffCapacity is an any-occupancy predicate, and the
// standing assumption has been that porting this path would widen the predicate
// from payload-specific to any-occupancy. For a SINGLE-WINDOW unloader that
// widening has already happened: the effective behavior is any-occupancy today,
// because the seam's count suppresses before payload is ever consulted. The
// widening is real only for multi-window unloaders.
func TestCreateUnloaderFullIn_PayloadSpecificGuard(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		windows     []string
		askPayload  string
		wantCreated int
		why         string
	}{
		{
			name: "multi_window_same_payload_suppresses", windows: []string{"ULD-W1", "ULD-W2"},
			askPayload: "PART-A", wantCreated: 0,
			why: "a full of PART-A already stands on W1; another is useless until the operator clears it",
		},
		{
			name: "multi_window_other_payload_routes_to_free_window", windows: []string{"ULD-W1", "ULD-W2"},
			askPayload: "PART-B", wantCreated: 1,
			why: "the resident full is PART-A, W2 is free, so an unserved PART-B demand still pulls",
		},
		{
			name: "single_window_other_payload_suppressed_by_budget", windows: []string{"ULD-W1"},
			askPayload: "PART-B", wantCreated: 0,
			why: "guard passes on payload, but the only window is occupied so the seam's any-occupancy count leaves no headroom",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			mw := true
			eng.cfg.LoadersMultiWindow = &mw
			seedWindowNodes(t, db, "ULD-PROC", tc.windows)

			// Core reports a resident FULL of PART-A on the first window only.
			eng.coreClient = NewCoreClient(nodeBinsPayloadStub(t, map[string]string{"ULD-W1": "PART-A"}).URL)

			ws := make([]domain.Window, len(tc.windows))
			for i, w := range tc.windows {
				ws[i] = domain.Window{Node: domain.NodeID(w)}
			}
			unloader, err := domain.NewSharedWindowLoader("ULD", "ULD", domain.RoleConsume, domain.ReplenishmentOperator,
				ws, []domain.PayloadCode{"PART-A", "PART-B"}, domain.WithInboundSource("FG-SUPER"))
			if err != nil {
				t.Fatalf("build unloader: %v", err)
			}

			eng.createUnloaderFullInViaSeam(unloader, tc.askPayload)

			if got := inFlightFulls(t, db, tc.windows); got != tc.wantCreated {
				t.Fatalf("U1 for %s with a resident PART-A full on ULD-W1: created %d, want %d — %s",
					tc.askPayload, got, tc.wantCreated, tc.why)
			}
		})
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

// TestReserveLoaderBins_FiresWhenCoreNotConfigured covers the arm where no Core
// base URL is set at all: Available() is false, the seam skips the occupancy gate
// and falls back to the order-only count. This is the one arm where firing is
// defensible — an unconfigured Core is a deployment state, not a failed read, so
// no question went unanswered. Core's plan-time dropoff gate still queues rather
// than dispatches a redundant empty.
//
// Renamed from _FiresWhenCoreUnreachable, which claimed coverage of an
// unreachable Core that it never exercised: a nil base URL never reaches the
// network. The genuinely unreachable arms are pinned by
// TestReserveLoaderBins_FiresWhenOccupancyReadFails below.
func TestReserveLoaderBins_FiresWhenCoreNotConfigured(t *testing.T) {
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
		t.Fatalf("Core not configured: gate must skip and fire from the order-only count; fired %d, want 1", created)
	}
}

// nodeBinsBrokenStub serves Core's /api/telemetry/node-bins in a chosen failure
// mode, counting hits in `hits`. Both modes are arms of FetchNodeBins that
// collapse to (nil, nil), which the seam reads as "no bin is resident":
//
//	"status"  — HTTP 500: Core is up but erroring.
//	"garbage" — HTTP 200 with a truncated body: a partial or corrupted write.
//
// The hit counter is load-bearing, not decoration. These stubs back tests whose
// pass condition is that the seam fires anyway, so EVERY defect in the stub —
// wrong path, wrong URL, server never started — produces the same passing
// result as the behavior under test. Counting hits is what separates "the read
// failed as designed" from "the test never asked."
func nodeBinsBrokenStub(t *testing.T, mode string, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/telemetry/node-bins" {
			t.Errorf("nodeBinsBrokenStub: unexpected path %q", r.URL.Path)
			return
		}
		hits.Add(1)
		switch mode {
		case "status":
			w.WriteHeader(http.StatusInternalServerError)
		case "garbage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{"))
		default:
			t.Errorf("nodeBinsBrokenStub: unknown mode %q", mode)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deadCoreURL returns the URL of an httptest server that has already been shut
// down, so a request fails at the transport layer instead of returning a
// response — the "Core process is gone" arm. Same shape as
// TestCoreClient_FetchPayloadManifest_NetworkError uses inline.
func deadCoreURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u := srv.URL
	srv.Close()
	return u
}

// TestReserveLoaderBins_FiresWhenOccupancyReadFails pins TODAY'S behavior on the
// three arms that produced the 2026-07-31 Springfield over-ordering incident:
// Core is configured, the seam asks whether the window is occupied, and the read
// fails. FetchNodeBins returns (nil, nil) on all three, so the seam cannot
// distinguish "Core could not answer" from "no bin is resident" and fires an
// empty into a window that already holds one.
//
// These assertions are DELIBERATELY the buggy behavior. They exist so that the
// fail-closed change is observed flipping them rather than merely asserted to.
// When the three-state contract lands, each want becomes 0 and this test becomes
// the regression guard for the fix.
//
// What was and was not covered before this test: resident-occupied and
// window-empty have had coverage since SMN_014, and the not-configured arm is
// covered above. No test covered a CONFIGURED Core whose read fails, which is
// the incident's own shape.
func TestReserveLoaderBins_FiresWhenOccupancyReadFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// setup returns the client to install and a check that the seam really
		// reached the failure being modelled — see nodeBinsBrokenStub on why a
		// bare pass assertion is not enough here.
		setup func(*testing.T) (*CoreClient, func(*testing.T))
		why   string
	}{
		{
			name: "transport_error",
			setup: func(t *testing.T) (*CoreClient, func(*testing.T)) {
				c := NewCoreClient(deadCoreURL(t))
				// The server is gone, so hits cannot be counted. Assert instead
				// that the client is configured — that is what distinguishes
				// this from the not-configured arm, where the seam never dials.
				return c, func(t *testing.T) {
					if !c.Available() {
						t.Fatal("client reports unavailable; this case must exercise a CONFIGURED Core that fails to answer, not the not-configured arm")
					}
				}
			},
			why: "Core process gone: http.Get errors and FetchNodeBins swallows it",
		},
		{
			name: "non_200",
			setup: func(t *testing.T) (*CoreClient, func(*testing.T)) {
				var hits atomic.Int64
				c := NewCoreClient(nodeBinsBrokenStub(t, "status", &hits).URL)
				return c, func(t *testing.T) {
					if hits.Load() == 0 {
						t.Fatal("stub was never called; the seam did not ask Core, so this test proves nothing about a failed read")
					}
				}
			},
			why: "Core up but erroring: status != 200 returns nil without saying why",
		},
		{
			name: "undecodable_body",
			setup: func(t *testing.T) (*CoreClient, func(*testing.T)) {
				var hits atomic.Int64
				c := NewCoreClient(nodeBinsBrokenStub(t, "garbage", &hits).URL)
				return c, func(t *testing.T) {
					if hits.Load() == 0 {
						t.Fatal("stub was never called; the seam did not ask Core, so this test proves nothing about a failed read")
					}
				}
			},
			why: "200 with a truncated body: the decode error is discarded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			nodeID := seedCapManualSwap(t, db, "READFAIL", "LOADER-1", protocol.ClaimRoleProduce, []string{"P1"}, 0, false)
			loader := mustSharedLoader(t, "LOADER-1", "P1")
			client, verifyAsked := tc.setup(t)
			eng.coreClient = client

			created, err := eng.reserveLoaderBins(loader, "P1", 1, "", true, fireOneEmptyPerWindow(eng, nodeID))
			if err != nil {
				t.Fatalf("reserveLoaderBins: %v", err)
			}
			verifyAsked(t)
			if created != 1 {
				t.Fatalf("occupancy read failed (%s): seam created %d empties, want 1.\n"+
					"This test pins the CURRENT fail-open, not desired behavior. If it now reports 0, "+
					"the fail-closed fix has landed: change want to 0 and rewrite the doc comment.",
					tc.why, created)
			}
		})
	}
}
