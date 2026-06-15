package engine

import (
	"math/rand"
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

	const goroutines = 24
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(g int) {
			defer wg.Done()
			if g%2 == 0 {
				// demand path: wants minStock (2), seam caps to the budget (1)
				eng.MaybeCreateLoaderEmptyIn("LOADER-1", "P1")
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
	l, err := domain.NewSharedWindowLoader(domain.LoaderID(id), id, domain.RoleProduce, domain.ReplenishmentAuto,
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
