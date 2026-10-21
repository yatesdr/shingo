package engine

import (
	"math/rand"
	"shingoedge/orders"
	"slices"
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
	l, err := domain.NewSharedWindowLoader(domain.LoaderID(id), domain.RoleProduce, domain.ReplenishmentThreshold,
		[]domain.Window{{Node: domain.NodeID(id)}}, ps, domain.WithInboundSource("EMPTY-SUPER"))
	if err != nil {
		t.Fatalf("build loader %s: %v", id, err)
	}
	return l
}

// TestWithLoaderBudget_PropNeverExceedsBudget drives a deterministic
// randomized sequence of reservations (two payloads, with occasional
// completions) to exercise the seam's per-payload dedup and its loader-capacity
// cap together. The invariant — in-flight at the loader <= budget — must hold
// after EVERY step.
//
// SCOPE, because this test is easy to over-read. It covers the ORDER-COUNTING
// budget only, on a single-window loader at budget 1:
//
//   - The bins table holds the window empty throughout, so the resident-bin
//     gate always passes and the order count is the bound. Occupancy is
//     covered by TestWithLoaderBudget_PropOccupancyLive.
//   - Budget above 1 is covered by the occupancy-live property, multi-window.
//
// (An earlier version of this comment said the budget=N property "lands in C4
// when ReservationTarget widens to the window cluster." That shipped —
// ReservationTarget spreads across windows whenever multi-window delivery is on,
// which is the default.)
func TestWithLoaderBudget_PropNeverExceedsBudget(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedCapManualSwap(t, db, "PROP", "PROP-LDR", protocol.ClaimRoleProduce, []string{"P1", "P2"}, 0, false)
	loader := mustSharedLoader(t, "PROP-LDR", "P1", "P2")
	holdCoreRows(t, eng, map[string]NodeBinInfo{}, "PROP-LDR")

	const budget = 1
	payloads := []domain.PayloadCode{"P1", "P2"}
	nodes := []string{"PROP-LDR"}
	rng := rand.New(rand.NewSource(20260612))

	reserve := func(payload domain.PayloadCode, want int) {
		_, err := eng.withLoaderBudget(loader, payload, want, "", true, func(deliveryNodes []string) (int, error) {
			made := 0
			for _, deliveryNode := range deliveryNodes {
				if _, cerr := eng.orderMgr.CreateRetrieveOrder(&nodeID, true, 1, deliveryNode, "EMPTY-SUPER", "", "standard", string(payload), false, true, orders.NoDemand()); cerr != nil {
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

// occupancyTable is the bins table as a MUTABLE fixture: the test flips which
// windows hold a bin between steps and pushes the table on each flip, and the
// seam reads the current table on each reserve.
type occupancyTable struct {
	mu       sync.Mutex
	occupied map[string]bool
}

func (s *occupancyTable) set(t *testing.T, eng *Engine, windows []string, node string, occupied bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.occupied[node] = occupied
	rows := map[string]NodeBinInfo{}
	for _, w := range windows {
		if s.occupied[w] {
			rows[w] = NodeBinInfo{Occupied: true, BinID: 77} // an EMPTY carrier still occupies the window
		}
	}
	holdCoreRows(t, eng, rows, windows...)
}

func (s *occupancyTable) isOccupied(node string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.occupied[node]
}

// TestWithLoaderBudget_PropOccupancyLive is the never-2N property run with the
// resident-bin gate LIVE, across a multi-window loader at budget > 1.
//
// The existing PropNeverExceedsBudget covers neither: it holds no bins table, so
// it pins the seam refusing (budget=1 on a single node, the order-only count). The property it proves is therefore the order-only
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
// Demands go through withLoaderBudget with a fire closure that orders one empty
// per window it is handed, as its remaining empties caller (CreateRetrieveForAPI
// at a loader window) does; an operator's empties are Core's to size now
// (loader.retrieve). Occupancy
// flips between steps, so the run exercises bins arriving and leaving underneath
// in-flight orders rather than a fixed world.
func TestWithLoaderBudget_PropOccupancyLive(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	windows := []string{"OCC-W1", "OCC-W2", "OCC-W3"}
	seedWindowNodes(t, db, "OCC-PROC", windows)
	loader := mustMultiWindowLoader(t, "OCC-LDR", windows, "P1", "P2")

	stub := &occupancyTable{occupied: map[string]bool{}}
	stub.set(t, eng, windows, windows[0], false)

	// Fixture guard: this test earns its keep only at budget > 1. If the loader
	// shape or the multi-window default changes so it funnels to one anchor, the
	// run would silently degrade into a duplicate of the existing property test.
	if _, budget := loader.ReservationTarget("", "P1", eng.multiWindowFor(loader)); budget != len(windows) {
		t.Fatalf("fixture: ReservationTarget budget = %d, want %d (multi-window spread); "+
			"budget=1 is already covered by TestWithLoaderBudget_PropNeverExceedsBudget", budget, len(windows))
	}

	payloads := []string{"P1", "P2"}
	rng := rand.New(rand.NewSource(20260801))

	fired, occupiedSteps := 0, 0
	for step := range 300 {
		switch rng.Intn(4) {
		case 0, 1: // a demand for a random payload
			payload := payloads[rng.Intn(len(payloads))]
			_, before := windowCounts(t, db, windows)
			if slices.ContainsFunc(windows, stub.isOccupied) {
				occupiedSteps++
			}
			if n, err := eng.withLoaderBudget(loader, domain.PayloadCode(payload), rng.Intn(len(windows)+1), "", true, func(deliveryNodes []string) (int, error) {
				made := 0
				for _, dn := range deliveryNodes {
					if _, cerr := eng.orderMgr.CreateRetrieveOrder(nil, true, 1, dn, "EMPTY-SUPER", "", "standard", payload, false, true, orders.NoDemand()); cerr != nil {
						return made, cerr
					}
					made++
				}
				return made, nil
			}); err != nil {
				t.Fatalf("step %d: withLoaderBudget: %v", step, err)
			} else {
				fired += n
			}
			_, after := windowCounts(t, db, windows)
			for _, w := range windows { // property B
				if after[w] > before[w] && stub.isOccupied(w) {
					t.Errorf("step %d: window %q gained an empty (%d→%d) for %s while the table held it OCCUPIED",
						step, w, before[w], after[w], payload)
				}
			}
		case 2: // a bin arrives at, or is taken from, a window
			stub.set(t, eng, windows, windows[rng.Intn(len(windows))], rng.Intn(2) == 0)
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

	// Without this the test can pass while proving nothing: a run in which no
	// window was ever occupied leaves property B trivially true.
	if fired == 0 || occupiedSteps == 0 {
		t.Fatalf("run exercised nothing: %d fires, %d demands with a window occupied", fired, occupiedSteps)
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

// TestCreateUnloaderFullIn_PayloadSpecificGuard pins what the consume side
// ACTUALLY does when a full is parked on an unloader window, which is not what
// the guard alone suggests.
//
// unloaderHasUsableFullPresent is payload-specific: it suppresses only when the
// resident full matches the payload asked for. But it is not the only gate. The
// seam's resident-bin count (decideLoaderBudget) is payload-AGNOSTIC — it
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
			seedWindowNodes(t, db, "ULD-PROC", tc.windows)

			// Core's table holds a resident FULL of PART-A on the first window only.
			holdCoreRows(t, eng, map[string]NodeBinInfo{"ULD-W1": {Occupied: true, PayloadCode: "PART-A", BinID: 77}}, tc.windows...)

			ws := make([]domain.Window, len(tc.windows))
			for i, w := range tc.windows {
				ws[i] = domain.Window{Node: domain.NodeID(w)}
			}
			unloader, err := domain.NewSharedWindowLoader("ULD", domain.RoleConsume, domain.ReplenishmentOperator,
				ws, []domain.PayloadCode{"PART-A", "PART-B"}, domain.WithInboundSource("FG-SUPER"))
			if err != nil {
				t.Fatalf("build unloader: %v", err)
			}

			eng.createUnloaderFullIns(unloader, []domain.PayloadCode{domain.PayloadCode(tc.askPayload)})

			if got := inFlightFulls(t, db, tc.windows); got != tc.wantCreated {
				t.Fatalf("U1 for %s with a resident PART-A full on ULD-W1: created %d, want %d — %s",
					tc.askPayload, got, tc.wantCreated, tc.why)
			}
		})
	}
}

// TestCreateUnloaderFullIn_HoldsWhenOccupancyReadFails pins that the consume
// side inherits the fail-closed rule rather than needing its own copy of it.
//
// The U1 reads the bins table once per unloader. The parked-full guard reads it
// and does not refuse; decideLoaderBudget's caller, sharing withLoaderBudget's
// rule, refuses on it. This test is what says the refusal lives there: the U1
// holds with the guard NOT refusing, on a table it may not decide from (none
// held — before the first push, or past the ack age).
//
// It is also the guard against the shortcut. If someone later routes a U1 around
// that refusal for speed, this goes red, and the missing refusal has to be put
// back where the shortcut is.
func TestCreateUnloaderFullIn_HoldsWhenOccupancyReadFails(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	windows := []string{"ULD-RF-W1", "ULD-RF-W2"}
	seedWindowNodes(t, db, "ULD-RF-PROC", windows)

	ws := make([]domain.Window, len(windows))
	for i, w := range windows {
		ws[i] = domain.Window{Node: domain.NodeID(w)}
	}
	unloader, err := domain.NewSharedWindowLoader("ULD-RF", domain.RoleConsume, domain.ReplenishmentOperator,
		ws, []domain.PayloadCode{"PART-A"}, domain.WithInboundSource("FG-SUPER"))
	if err != nil {
		t.Fatalf("build unloader: %v", err)
	}
	if ok, _ := eng.BinsActable(); ok {
		t.Fatal("fixture: the table is actable; this case needs one the Edge may not decide from")
	}

	eng.createUnloaderFullIns(unloader, []domain.PayloadCode{"PART-A"})

	if got := inFlightFulls(t, db, windows); got != 0 {
		t.Fatalf("table not actable: U1 created %d fulls, want 0 — "+
			"nothing had checked whether a full was already parked on those windows", got)
	}
}

// TestWithLoaderBudget_EmitDuringReservation_NoDeadlock pins the re-entrancy
// rule. `fire` runs while the loader's mutex is held and CreateRetrieveOrder
// fires EmitOrderCreated synchronously on the in-process bus; a subscriber runs
// inside the locked region, and the whole reservation must complete.
//
// CONVERTED (D2 Lane 0, LOCKS.md). It pinned that a subscriber re-entering the
// seam for a DIFFERENT loader proceeds. Two loaders' locks taken in no fixed
// order are two goroutines away from waiting on each other (one holds A and
// wants B, the other the reverse), and no production subscriber re-enters the
// seam at all (LOCKS.md, E C2). The lock order now forbids any second loader
// lock under one; the guard enforces it, and
// TestWithLoaderBudget_ASecondLoaderLockUnderOneIsRefused pins that.
func TestWithLoaderBudget_EmitDuringReservation_NoDeadlock(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	holdCoreRows(t, eng, map[string]NodeBinInfo{}, "DLK-A")
	var ran bool
	eng.Events.Subscribe(func(evt Event) {
		if evt.Type == EventOrderCreated {
			ran = true // the subscriber runs inside the locked region
		}
	})

	done := make(chan struct{})
	go func() {
		_, _ = eng.withLoaderBudget(mustSharedLoader(t, "DLK-A", "P1"), "P1", 1, "", true, func(deliveryNodes []string) (int, error) {
			// In production CreateRetrieveOrder fires EmitOrderCreated synchronously
			// here, under the lock. The test order-emitter is a no-op, so emit it
			// directly to exercise a synchronous subscriber callback in the locked
			// region.
			eng.Events.Emit(Event{Type: EventOrderCreated, Payload: OrderCreatedEvent{OrderID: 1, OrderType: protocol.OrderTypeRetrieve}})
			return len(deliveryNodes), nil
		})
		close(done)
	}()

	select {
	case <-done:
		if !ran {
			t.Fatal("subscriber did not run — the emit-during-reservation path was not exercised")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: a reservation that emitted under the loader lock did not complete within 5s")
	}
}

// fireOneEmptyPerWindow is the reserve `fire` closure the resident-gate tests share:
// it creates one retrieve_empty per delivery window and reports how many it made.
func fireOneEmptyPerWindow(eng *Engine, nodeID int64) func([]string) (int, error) {
	return func(deliveryNodes []string) (int, error) {
		made := 0
		for _, dn := range deliveryNodes {
			if _, cerr := eng.orderMgr.CreateRetrieveOrder(&nodeID, true, 1, dn, "EMPTY-SUPER", "", "standard", "P1", false, true, orders.NoDemand()); cerr != nil {
				return made, cerr
			}
			made++
		}
		return made, nil
	}
}

// TestWithLoaderBudget_SuppressesWhenWindowHasResidentEmpty is the Springfield
// SMN_014 regression (2026-07-23). A 0-UOP empty already stands on the loader's
// only window, so system UOP reads 0 < threshold and Core keeps signalling — but
// another empty is useless: the loader operator just needs to LOAD the one that's
// there. With ZERO inbound orders the order-count dedup can't suppress it; the seam
// must count the resident bin as occupying the window and fire nothing.
func TestWithLoaderBudget_SuppressesWhenWindowHasResidentEmpty(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedCapManualSwap(t, db, "RESIDENT", "LOADER-1", protocol.ClaimRoleProduce, []string{"P1"}, 0, false)
	loader := mustSharedLoader(t, "LOADER-1", "P1")
	holdCoreRows(t, eng, map[string]NodeBinInfo{"LOADER-1": {Occupied: true, BinID: 77}}) // an EMPTY carrier still occupies the window

	created, err := eng.withLoaderBudget(loader, "P1", 1, "", true, fireOneEmptyPerWindow(eng, nodeID))
	if err != nil {
		t.Fatalf("withLoaderBudget: %v", err)
	}
	if created != 0 {
		t.Fatalf("resident empty present but seam fired %d empties; want 0 (operator must load the resident carrier)", created)
	}
	if got := inFlightEmpties(t, db, []string{"LOADER-1"}); got != 0 {
		t.Fatalf("in-flight empties = %d after suppressed reserve; want 0", got)
	}
}

// TestWithLoaderBudget_FiresWhenWindowEmpty is the negative control: an
// unoccupied window still gets its empty, so the resident gate can't over-suppress.
func TestWithLoaderBudget_FiresWhenWindowEmpty(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedCapManualSwap(t, db, "EMPTYWIN", "LOADER-1", protocol.ClaimRoleProduce, []string{"P1"}, 0, false)
	loader := mustSharedLoader(t, "LOADER-1", "P1")
	holdCoreRows(t, eng, map[string]NodeBinInfo{}, "LOADER-1") // LOADER-1 reported empty

	created, err := eng.withLoaderBudget(loader, "P1", 1, "", true, fireOneEmptyPerWindow(eng, nodeID))
	if err != nil {
		t.Fatalf("withLoaderBudget: %v", err)
	}
	if created != 1 {
		t.Fatalf("empty window but seam fired %d empties; want 1", created)
	}
}

// TestWithLoaderBudget_HoldsWhenOccupancyReadFails covers the shape of the
// 2026-07-31 Springfield over-ordering incident: the seam asks whether the
// window is occupied and cannot know. On the table that is a copy the Edge may
// not decide from (none held, here; past the ack age, or answering another
// watch list, the same).
//
// The seam's whole job is to not put a second carrier on a window that already
// holds one, and it cannot do that job without knowing what is on the windows.
// Firing regardless was never a decision; it was the HTTP read being unable to
// report that it had failed. The next push or ack makes the table actable, so
// holding costs one cycle.
//
// There is no arm that fires without the table: an Edge with no Core configured
// once kept firing on its order count alone, and that arm is gone with the HTTP
// read.
func TestWithLoaderBudget_HoldsWhenOccupancyReadFails(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedCapManualSwap(t, db, "READFAIL", "LOADER-1", protocol.ClaimRoleProduce, []string{"P1"}, 0, false)
	loader := mustSharedLoader(t, "LOADER-1", "P1")
	if ok, _ := eng.BinsActable(); ok {
		t.Fatal("fixture: the table is actable; this case needs one the Edge may not decide from")
	}

	created, err := eng.withLoaderBudget(loader, "P1", 1, "", true, fireOneEmptyPerWindow(eng, nodeID))
	if err != nil {
		t.Fatalf("withLoaderBudget: %v", err)
	}
	if created != 0 {
		t.Fatalf("table not actable: seam created %d empties, want 0.\n"+
			"The seam fired without knowing what is on the windows — this is the "+
			"2026-07-31 over-ordering incident's exact shape.", created)
	}
}
