package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	ordermgr "shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// seedRuntimeUOP puts parts on the press so the "no parts to finalize" guard
// cannot be mistaken for whatever a test is actually asserting.
func seedRuntimeUOP(t *testing.T, db *store.DB, nodeID int64) {
	t.Helper()
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")
	claim := requestedClaimAtNode(db, node)
	if claim == nil {
		t.Fatal("no active claim — seed contract changed")
	}
	cID := claim.ID
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &cID, 50), "set runtime uop")
}

// originOnOrderRequest reads the OriginID off the OrderRequest envelope the
// create enqueued. Edge's order row carries Core's stamp, not ours, so the
// envelope is the only place our origin is observable at create time.
func originOnOrderRequest(t *testing.T, db *store.DB, orderUUID string) string {
	t.Helper()
	msgs, err := db.ListUnsentOutboxByType([]string{string(protocol.TypeOrderRequest)})
	testutil.MustNoErr(t, err, "read outbox")
	for _, m := range msgs {
		var env protocol.Envelope
		if err := json.Unmarshal(m.Payload, &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		var req protocol.OrderRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			continue
		}
		if req.OrderUUID == orderUUID {
			return req.OriginID
		}
	}
	t.Fatalf("no OrderRequest envelope for order %s on the outbox", orderUUID)
	return ""
}

// The chain ahead of the swap: a two_robot_press_index cell whose head is
// occupied but whose paired position is vacant gets that position's empty and
// its swap behind it, which Core holds until the empty lands (W5). These pin
// the planner half.

const (
	primeHead   = "PRODUCE-NODE"
	primePaired = "PRODUCE-NODE-BACK"
	primeSecond = "PRODUCE-NODE-C"
	primeSource = "EMPTY-STORAGE"
)

// pressIndexFixtures returns the produce fixtures with a press-index claim and,
// optionally, a third position.
func pressIndexFixtures(secondPaired string) (*processes.Node, *processes.RuntimeState, *processes.NodeClaim) {
	node, runtime, claim := produceFixtures(protocol.SwapModeTwoRobotPressIndex)
	claim.SecondPairedCoreNode = secondPaired
	return node, runtime, claim
}

func TestBuildProducePlan_VacantOnDeck_PrimeAheadOfTheSwap(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures("")

	occ := map[string]bool{primeHead: true, primePaired: false}
	plan, err := BuildProducePlan(node, runtime, claim, occ, Deck{})
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if plan.Dispatch == nil || plan.Dispatch.StepsA == nil || plan.Dispatch.StepsB == nil {
		t.Fatalf("head occupied + paired vacant builds the swap, queued behind the prime; Dispatch = %+v", plan.Dispatch)
	}
	if len(plan.Chain.Prime) != 1 || plan.Chain.Prime[0] != primePaired || len(plan.Chain.Clear) != 0 {
		t.Fatalf("chain = %+v, want exactly one prime for %s", plan.Chain, primePaired)
	}
	// expected_orders for the episode: the prime and both swap legs.
	if n := plan.OrderCount(); n != 3 {
		t.Errorf("OrderCount() = %d, want 3 — the prime and both legs", n)
	}
}

func TestBuildProducePlan_FullCellStillSwaps(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures("")

	occ := map[string]bool{primeHead: true, primePaired: true}
	plan, err := BuildProducePlan(node, runtime, claim, occ, Deck{})
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if plan.Chain.Count() != 0 {
		t.Fatalf("a fully occupied cell has no chain; chain = %+v", plan.Chain)
	}
	if plan.Dispatch == nil || plan.Dispatch.StepsA == nil || plan.Dispatch.StepsB == nil {
		t.Fatalf("the ordinary press-index swap must be unchanged; Dispatch = %+v", plan.Dispatch)
	}
	if n := plan.OrderCount(); n != 2 {
		t.Errorf("OrderCount() = %d, want 2 (both swap legs)", n)
	}
}

func TestBuildProducePlan_ThreePositionPrimesBoth(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures(primeSecond)

	occ := map[string]bool{primeHead: true, primePaired: false, primeSecond: false}
	plan, err := BuildProducePlan(node, runtime, claim, occ, Deck{})
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if plan.Dispatch == nil {
		t.Fatalf("the swap is built behind the chain; Dispatch = nil")
	}
	if len(plan.Chain.Prime) != 2 || plan.Chain.Prime[0] != primePaired || plan.Chain.Prime[1] != primeSecond {
		t.Fatalf("primes = %v, want [%s %s]", plan.Chain.Prime, primePaired, primeSecond)
	}
	if n := plan.OrderCount(); n != 4 {
		t.Errorf("OrderCount() = %d, want 4 (two primes and both legs)", n)
	}
}

// A cold press reads RemainingUOPCached == 0 — and at Springfield the counter
// tag is not wired at all, so it reads 0 always. The planner asks no count: it
// builds the chain and the swap, and the finalizing request drops the swap and
// keeps the chain (TestRequestProduceSwap_ColdPressPrimesInOneTap).
func TestBuildProducePlan_ColdPressPlansTheChain(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures("")
	runtime.RemainingUOPCached = 0

	occ := map[string]bool{primeHead: true, primePaired: false}
	plan, err := BuildProducePlan(node, runtime, claim, occ, Deck{})
	if err != nil {
		t.Fatalf("a cold press with a vacant paired position must still plan its chain, not refuse: %v", err)
	}
	if len(plan.Chain.Prime) != 1 || plan.Dispatch == nil {
		t.Fatalf("chain = %+v, Dispatch = %+v; want one prime and the swap (the request decides the count)",
			plan.Chain, plan.Dispatch)
	}
}

// A cold press with NOTHING to prime, asked through the two buttons: the produce
// request finalizes a filled bin and refuses with nothing counted; the empty-bin
// request asks for an empty whatever the count and still swaps. The refusal is
// the request's, not the planner's.
func TestRequest_ColdPressWithFullCell(t *testing.T) {
	t.Parallel()
	t.Run("produce request refuses", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		eng := testEngine(t, db)
		nodeID := seedPressIndexProduce(t, db)
		bridgeCore(t, eng, pressIndexBinsStub(t, "PRESS-HEAD", "PRESS-DECK").URL)
		holdPlaces(t, eng, occupiedAt("PRESS-HEAD", "PRESS-DECK"))
		_, err := eng.RequestProduceSwap(nodeID, 0)
		if err == nil || !strings.Contains(err.Error(), "has no parts to finalize") {
			t.Fatalf("produce request = %v, want the no-parts refusal", err)
		}
		if n := countOrdersByType(t, db, string(protocol.OrderTypeComplex)); n != 0 {
			t.Errorf("complex orders = %d after the refusal, want 0", n)
		}
	})
	t.Run("empty-bin request swaps", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		eng := testEngine(t, db)
		nodeID := seedPressIndexProduce(t, db)
		bridgeCore(t, eng, pressIndexBinsStub(t, "PRESS-HEAD", "PRESS-DECK").URL)
		holdPlaces(t, eng, occupiedAt("PRESS-HEAD", "PRESS-DECK"))
		if _, err := eng.RequestEmptyBin(nodeID, "WIDGET-A"); err != nil {
			t.Fatalf("empty-bin request: %v", err)
		}
		if n := countOrdersByType(t, db, string(protocol.OrderTypeComplex)); n != 2 {
			t.Errorf("complex orders = %d, want the two-leg swap", n)
		}
	})
}

func TestBuildProducePlan_UnknownOccupancyPrimesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		occ  map[string]bool
	}{
		{"nil_map", nil},
		{"empty_map", map[string]bool{}},
		{"head_known_paired_missing", map[string]bool{primeHead: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node, runtime, claim := pressIndexFixtures("")
			plan, err := BuildProducePlan(node, runtime, claim, tc.occ, Deck{})
			if err != nil {
				t.Fatalf("BuildProducePlan: %v", err)
			}
			if plan.Chain.Count() != 0 || plan.Dispatch == nil {
				t.Fatalf("a missing occupancy entry reads as OCCUPIED: no chain, the swap; chain=%+v Dispatch=%+v",
					plan.Chain, plan.Dispatch)
			}
		})
	}
}

// The chatter guard: an empty already inbound to the vacant position means the
// next request adds no prime. The swap is built: Core holds it for the carrier
// coming to its on-deck (W5), so the press indexes the moment the empty lands.
func TestBuildProducePlan_AlreadyPrimedAddsNoPrime(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures("")

	occ := map[string]bool{primeHead: true, primePaired: false}
	primed := map[string]bool{primePaired: true}
	plan, err := BuildProducePlan(node, runtime, claim, occ, Deck{Coming: primed})
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if plan.Chain.Count() != 0 {
		t.Fatalf("a position with an empty already inbound must not be primed again; chain=%+v", plan.Chain)
	}
	if plan.Dispatch == nil || plan.OrderCount() != 2 {
		t.Fatalf("the swap is queued while the prime travels: Dispatch=%+v orders=%d, want the two legs",
			plan.Dispatch, plan.OrderCount())
	}
}

// One of three vacant, two already inbound: prime only the one that needs it.
func TestBuildProducePlan_PrimesOnlyTheUnprimedPosition(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures(primeSecond)

	occ := map[string]bool{primeHead: true, primePaired: false, primeSecond: false}
	primed := map[string]bool{primePaired: true}
	plan, err := BuildProducePlan(node, runtime, claim, occ, Deck{Coming: primed})
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if len(plan.Chain.Prime) != 1 || plan.Chain.Prime[0] != primeSecond {
		t.Fatalf("primes = %v, want exactly one for %s", plan.Chain.Prime, primeSecond)
	}
}

// The chain is press-index-only. Every other swap mode keeps its existing
// behavior whatever the paired position reads.
func TestBuildProducePlan_OtherModesHaveNoChain(t *testing.T) {
	t.Parallel()
	for _, mode := range []protocol.SwapMode{"sequential", "single_robot", "two_robot"} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			node, runtime, claim := produceFixtures(mode)
			occ := map[string]bool{primeHead: true, primePaired: false}
			plan, err := BuildProducePlan(node, runtime, claim, occ, Deck{})
			if err != nil {
				t.Fatalf("BuildProducePlan: %v", err)
			}
			if plan.Chain.Count() != 0 {
				t.Fatalf("%s must not prime; chain=%+v", mode, plan.Chain)
			}
			if plan.Dispatch == nil {
				t.Fatalf("%s must still dispatch", mode)
			}
		})
	}
}

// A prime with nowhere to pull from is a refusal, not a silent order at ""
// — the same reading the consume side's downgrade gives a blank InboundSource;
// and a clear with nowhere to go is one too.
func TestBuildProducePlan_NoInboundSourceRefuses(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures("")
	claim.InboundSource = ""

	occ := map[string]bool{primeHead: true, primePaired: false}
	if _, err := BuildProducePlan(node, runtime, claim, occ, Deck{}); err == nil {
		t.Fatal("want an error when a prime is needed and the claim has no inbound source")
	}
	_, _, claim = pressIndexFixtures("")
	claim.OutboundDestination = ""
	wrong := Deck{Rows: map[string]protocol.NodeBinsRow{primePaired: {Node: primePaired,
		State: protocol.BinsStateOccupied, Occupied: true, BinID: 9, PayloadCode: "OTHER", Stocked: true}}}
	occ = map[string]bool{primeHead: true, primePaired: true}
	if _, err := BuildProducePlan(node, runtime, claim, occ, wrong); err == nil ||
		!strings.Contains(err.Error(), "no outbound destination") {
		t.Fatalf("a clear with no outbound destination = %v, want the refusal", err)
	}
}

// ── apply level ─────────────────────────────────────────────────────────

// TestApplyProducePlan_ChainOnly pins what the impure half does with a plan
// whose swap was dropped (a cold press): a retrieve_empty (not a move — a move
// hunts a full bin in an empties pool), the merged auto-confirm signal, the
// episode's origin carried onto the row, no runtime order slots written, and
// ProcessNodeID set on the result.
func TestApplyProducePlan_ChainOnly(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	eng := testEngine(t, db)

	runtime, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime")

	plan := &ProducePlan{Chain: Readiness{Prime: []string{"INDEX-B"}}}
	origin := ordermgr.Attached("episode-abc")

	res, err := eng.applyProducePlan(node, runtime, claim, plan, origin)
	testutil.MustNoErr(t, err, "applyProducePlan")

	if res.ProcessNodeID != nodeID {
		t.Errorf("ProcessNodeID = %d, want %d", res.ProcessNodeID, nodeID)
	}
	if res.Order != nil || res.OrderA != nil || res.OrderB != nil {
		t.Errorf("a round with no swap mints no legs; Order=%v OrderA=%v OrderB=%v", res.Order, res.OrderA, res.OrderB)
	}
	if len(res.PrimeOrders) != 1 {
		t.Fatalf("PrimeOrders = %+v, want one", res.PrimeOrders)
	}

	po := res.PrimeOrders[0]
	if !po.RetrieveEmpty {
		t.Errorf("prime must be RetrieveEmpty=true — a move would hunt a FULL bin in an empties pool")
	}
	if po.DeliveryNode != "INDEX-B" {
		t.Errorf("prime DeliveryNode = %q, want INDEX-B", po.DeliveryNode)
	}
	if po.SourceNode != "MARKET-EMPTIES" {
		t.Errorf("prime SourceNode = %q, want MARKET-EMPTIES", po.SourceNode)
	}
	// cfg.Web.AutoConfirm is true in testEngine and the seeded claim's own flag
	// is false, so this arm only shows the merge is at least as permissive as
	// the config. TestApplyProducePlan_PrimeAutoConfirmIsMerged below is what
	// stops a hard-coded true passing: with BOTH inputs false it must be false.
	if !po.AutoConfirm {
		t.Errorf("prime AutoConfirm = false; want the merged claim||config signal")
	}
	// order.origin_id on Edge is what CORE stamped back, so a freshly created
	// row is correctly blank. The origin we pass travels on the OrderRequest
	// envelope, and that is where it has to be asserted.
	if got := originOnOrderRequest(t, db, po.UUID); got != "episode-abc" {
		t.Errorf("OrderRequest OriginID = %q, want the episode's origin carried through", got)
	}

	// Runtime order slots belong to the head node's swap-cycle machinery. A
	// prime is not a swap, so neither slot may be stamped.
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "re-read runtime")
	if rt.ActiveOrderID != nil || rt.StagedOrderID != nil {
		t.Errorf("primes must not write runtime order slots; active=%v staged=%v", rt.ActiveOrderID, rt.StagedOrderID)
	}
}

// TestRequestProduceSwap_PrimeThenSwapOnce is the end-to-end chatter guard: the
// first request primes the vacant position and queues the swap behind it, in
// that order (the outbox's), and a second request while both are in flight
// creates nothing: it meets the swap. Counting rows rather than inspecting the
// plan is the point — this is what the operator's double-tap produces.
func TestRequestProduceSwap_PrimeThenSwapOnce(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	seedRuntimeUOP(t, db, nodeID)

	eng := testEngine(t, db)
	// Core reports the head occupied and INDEX-B vacant — the incident's shape.
	holdPlaces(t, eng, occupiedAt("PRESS"))
	eng.SetCoreNodes([]protocol.NodeInfo{{Name: "PRESS"}, {Name: "INDEX-B"}, {Name: "MARKET-EMPTIES"}})

	res, err := eng.RequestProduceSwap(nodeID, 0)
	testutil.MustNoErr(t, err, "first RequestProduceSwap")
	if len(res.PrimeOrders) != 1 || res.OrderA == nil || res.OrderB == nil {
		t.Fatalf("first request: primes %+v, legs %v/%v; want one prime and the swap behind it",
			res.PrimeOrders, res.OrderA, res.OrderB)
	}
	if !(res.PrimeOrders[0].ID < res.OrderA.ID && res.OrderA.ID < res.OrderB.ID) {
		t.Errorf("creation order: prime %d, legs %d and %d; want the prime first — Core ingests by outbox id",
			res.PrimeOrders[0].ID, res.OrderA.ID, res.OrderB.ID)
	}

	if _, err := eng.RequestProduceSwap(nodeID, 0); err == nil {
		t.Fatalf("second request: want a refusal while the swap is in flight")
	}
	all, err := db.ListOrders()
	testutil.MustNoErr(t, err, "list orders")
	empties := 0
	for _, o := range all {
		if o.RetrieveEmpty && o.DeliveryNode == "INDEX-B" {
			empties++
		}
	}
	if empties != 1 {
		t.Errorf("retrieve_empty rows at INDEX-B = %d, want exactly 1 across two requests", empties)
	}
}

// TestRequestProduceSwap_ColdPressPrimesInOneTap: a cold produce press (its
// count 0, its line holding a bin, its on-deck vacant) gets its prime and no
// swap, in one tap: the count check drops the swap, not the chain.
func TestRequestProduceSwap_ColdPressPrimesInOneTap(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")

	eng := testEngine(t, db)
	holdPlaces(t, eng, occupiedAt("PRESS"))
	eng.SetCoreNodes([]protocol.NodeInfo{{Name: "PRESS"}, {Name: "INDEX-B"}, {Name: "MARKET-EMPTIES"}})

	res, err := eng.RequestProduceSwap(nodeID, 0)
	testutil.MustNoErr(t, err, "the cold press's one tap")
	if len(res.PrimeOrders) != 1 || res.OrderA != nil || res.OrderB != nil || res.Order != nil {
		t.Fatalf("cold press: primes %+v, legs %v/%v; want the prime and no swap", res.PrimeOrders, res.OrderA, res.OrderB)
	}
	if n := countOrdersByType(t, db, string(protocol.OrderTypeComplex)); n != 0 {
		t.Errorf("complex orders = %d, want none: nothing counted, nothing to finalize", n)
	}
}

// A paired position Core does not know reads as EMPTY on the wire — Core
// answers "there is no bin at a place that does not exist" with a present
// entry and occupied=false. Priming on that sentence sends a carrier at a
// typo every cycle, so the name must resolve in Core's synced node set first.
func TestRequestProduceSwap_UnknownPairedNodeIsNotPrimed(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")

	eng := testEngine(t, db)
	holdPlaces(t, eng, occupiedAt("PRESS"))
	// A non-empty node set that does NOT contain INDEX-B: that is evidence.
	eng.SetCoreNodes([]protocol.NodeInfo{{Name: "PRESS"}, {Name: "MARKET-EMPTIES"}})
	// Parts on the press, so the cold-press guard cannot be what stops the
	// prime — the unknown-node read has to be.
	seedRuntimeUOP(t, db, nodeID)

	res, err := eng.RequestProduceSwap(nodeID, 0)
	testutil.MustNoErr(t, err, "RequestProduceSwap")
	if len(res.PrimeOrders) != 0 {
		t.Errorf("PrimeOrders = %+v, want none — INDEX-B is not a node Core knows", res.PrimeOrders)
	}
	if res.OrderA == nil || res.OrderB == nil {
		t.Errorf("an unknown paired position reads as occupied, so the ordinary swap proceeds; got %+v", res)
	}
}

// The sibling of the above, and the reason it cannot simply refuse: an EMPTY
// node set is not evidence that a name is wrong. A fresh Edge, a restart or a
// Kafka gap all present that way, while Core still answers node-bins over a
// different transport, so the check is skipped and the prime fires on
// telemetry alone.
func TestRequestProduceSwap_EmptyCoreNodeSetStillPrimes(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")

	eng := testEngine(t, db)
	holdPlaces(t, eng, occupiedAt("PRESS"))
	// No SetCoreNodes call at all — Core has not been heard from.

	res, err := eng.RequestProduceSwap(nodeID, 0)
	testutil.MustNoErr(t, err, "RequestProduceSwap")
	if len(res.PrimeOrders) != 1 {
		t.Errorf("PrimeOrders = %+v, want one — an unheard-from Core must not suppress the prime", res.PrimeOrders)
	}
}

// TestApplyProducePlan_PrimeAutoConfirmIsMerged pins the merge across the whole
// matrix rather than at one point. Asserting only the config-true case is
// exactly the leaning test that goes vacuous the day someone flips a default:
// a hard-coded `true` satisfies it. The false/false row is the one that bites.
func TestApplyProducePlan_PrimeAutoConfirmIsMerged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		claimAC, cfgAC bool
		want           bool
	}{
		{"neither", false, false, false},
		{"config_only", false, true, true},
		{"claim_only", true, false, true},
		{"both", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
			_, err := db.EnsureProcessNodeRuntime(nodeID)
			testutil.MustNoErr(t, err, "ensure runtime")

			eng := testEngine(t, db)
			eng.cfg.Web.AutoConfirm = tc.cfgAC
			claim.AutoConfirm = tc.claimAC

			runtime, err := db.GetProcessNodeRuntime(nodeID)
			testutil.MustNoErr(t, err, "get runtime")

			plan := &ProducePlan{Chain: Readiness{Prime: []string{"INDEX-B"}}}
			res, err := eng.applyProducePlan(node, runtime, claim, plan, ordermgr.Origin{})
			testutil.MustNoErr(t, err, "applyProducePlan")
			if len(res.PrimeOrders) != 1 {
				t.Fatalf("PrimeOrders = %+v, want one", res.PrimeOrders)
			}
			if got := res.PrimeOrders[0].AutoConfirm; got != tc.want {
				t.Errorf("claim=%v cfg=%v: prime AutoConfirm = %v, want %v (claim || config)",
					tc.claimAC, tc.cfgAC, got, tc.want)
			}
		})
	}
}
