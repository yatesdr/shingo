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
	claim := findActiveClaim(db, node)
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

// The partial-empty prime: a two_robot_press_index cell whose head is occupied
// but whose paired position is bare mints a swap R2 can never source. These
// pin the planner half — never mint that swap, prime the bare position instead.

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

func TestBuildProducePlan_PartialEmpty_PrimesBarePairedPosition(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures("")

	occ := map[string]bool{primeHead: true, primePaired: false}
	plan, err := BuildProducePlan(node, runtime, claim, fixedNow, occ, nil)
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if !plan.SuppressSwap {
		t.Fatalf("head occupied + paired bare must suppress the swap; SuppressSwap = false")
	}
	if plan.Dispatch != nil {
		t.Errorf("a primes-only plan must carry no Dispatch; got %+v", plan.Dispatch)
	}
	if len(plan.Manifest) != 0 {
		t.Errorf("a primes-only plan manifests nothing (no bin is departing); got %+v", plan.Manifest)
	}
	if len(plan.PrimePairedPositions) != 1 {
		t.Fatalf("PrimePairedPositions = %+v, want exactly one entry for %s", plan.PrimePairedPositions, primePaired)
	}
	got := plan.PrimePairedPositions[0]
	if got.Source != primeSource || got.Dest != primePaired {
		t.Errorf("prime = %+v, want {Source:%s Dest:%s}", got, primeSource, primePaired)
	}
	// expected_orders for the episode: one row per prime, no swap legs.
	if n := plan.OrderCount(); n != 1 {
		t.Errorf("OrderCount() = %d, want 1 — a primes-only round creates one row per prime", n)
	}
}

func TestBuildProducePlan_PartialEmpty_FullCellStillSwaps(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures("")

	occ := map[string]bool{primeHead: true, primePaired: true}
	plan, err := BuildProducePlan(node, runtime, claim, fixedNow, occ, nil)
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if plan.SuppressSwap || len(plan.PrimePairedPositions) != 0 {
		t.Fatalf("a fully occupied cell primes nothing and swaps normally; SuppressSwap=%v primes=%+v",
			plan.SuppressSwap, plan.PrimePairedPositions)
	}
	if plan.Dispatch == nil || plan.Dispatch.StepsA == nil || plan.Dispatch.StepsB == nil {
		t.Fatalf("the ordinary press-index swap must be unchanged; Dispatch = %+v", plan.Dispatch)
	}
	if n := plan.OrderCount(); n != 2 {
		t.Errorf("OrderCount() = %d, want 2 (both swap legs)", n)
	}
}

func TestBuildProducePlan_PartialEmpty_ThreePositionPrimesBoth(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures(primeSecond)

	occ := map[string]bool{primeHead: true, primePaired: false, primeSecond: false}
	plan, err := BuildProducePlan(node, runtime, claim, fixedNow, occ, nil)
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if !plan.SuppressSwap {
		t.Fatalf("SuppressSwap = false, want true")
	}
	if len(plan.PrimePairedPositions) != 2 {
		t.Fatalf("PrimePairedPositions = %+v, want two entries on a 3-position layout", plan.PrimePairedPositions)
	}
	dests := []string{plan.PrimePairedPositions[0].Dest, plan.PrimePairedPositions[1].Dest}
	if dests[0] != primePaired || dests[1] != primeSecond {
		t.Errorf("prime dests = %v, want [%s %s]", dests, primePaired, primeSecond)
	}
	if n := plan.OrderCount(); n != 2 {
		t.Errorf("OrderCount() = %d, want 2 (one row per prime)", n)
	}
}

// TestBuildProducePlan_PartialEmpty_PrimesOnColdPress is the placement test.
//
// A cold press reads RemainingUOPCached == 0 — and at Springfield, where this
// fix is going, the counter tag is not wired at all, so it reads 0 always. The
// "no parts to finalize" guard is therefore the WRONG answer for a cell with a
// bare paired position, and the prime branch must sit above it. Move the branch
// back below that guard and this is the test that goes red; the other five pass
// either way.
func TestBuildProducePlan_PartialEmpty_PrimesOnColdPress(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures("")
	runtime.RemainingUOPCached = 0

	occ := map[string]bool{primeHead: true, primePaired: false}
	plan, err := BuildProducePlan(node, runtime, claim, fixedNow, occ, nil)
	if err != nil {
		t.Fatalf("a cold press with a bare paired position must still prime, not refuse: %v", err)
	}
	if !plan.SuppressSwap || len(plan.PrimePairedPositions) != 1 {
		t.Fatalf("SuppressSwap=%v primes=%+v, want one prime with the swap suppressed",
			plan.SuppressSwap, plan.PrimePairedPositions)
	}
}

// A cold press with NOTHING to prime still refuses — the UOP guard is intact
// for every case the prime branch does not claim.
func TestBuildProducePlan_ColdPressWithFullCellStillRefuses(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures("")
	runtime.RemainingUOPCached = 0

	occ := map[string]bool{primeHead: true, primePaired: true}
	if _, err := BuildProducePlan(node, runtime, claim, fixedNow, occ, nil); err == nil {
		t.Fatal("want the 'no parts to finalize' refusal for a cold press with a full cell")
	}
}

func TestBuildProducePlan_PartialEmpty_UnknownOccupancyPrimesNothing(t *testing.T) {
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
			plan, err := BuildProducePlan(node, runtime, claim, fixedNow, tc.occ, nil)
			if err != nil {
				t.Fatalf("BuildProducePlan: %v", err)
			}
			if plan.SuppressSwap || len(plan.PrimePairedPositions) != 0 {
				t.Fatalf("a missing occupancy entry reads as OCCUPIED, so nothing primes; SuppressSwap=%v primes=%+v",
					plan.SuppressSwap, plan.PrimePairedPositions)
			}
		})
	}
}

// The chatter guard: an empty already inbound to the bare position means the
// next request adds nothing. Without it, every press of the button while the
// first prime is still travelling mints another order row and inflates the
// episode.
func TestBuildProducePlan_PartialEmpty_AlreadyPrimedAddsNothing(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures("")

	occ := map[string]bool{primeHead: true, primePaired: false}
	primed := map[string]bool{primePaired: true}
	plan, err := BuildProducePlan(node, runtime, claim, fixedNow, occ, primed)
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if len(plan.PrimePairedPositions) != 0 {
		t.Fatalf("a position with an empty already inbound must not be primed again; primes=%+v",
			plan.PrimePairedPositions)
	}
	// ...but the swap stays suppressed. The position is still physically bare
	// until that empty lands, and R2 cannot index from a bare position whether
	// or not a carrier is on its way to it.
	if !plan.SuppressSwap {
		t.Fatalf("a bare position must suppress the swap even while its prime is in flight")
	}
	if plan.Dispatch != nil {
		t.Fatalf("a hold round must carry no Dispatch; got %+v", plan.Dispatch)
	}
	if n := plan.OrderCount(); n != 0 {
		t.Errorf("OrderCount() = %d, want 0 — a hold round creates nothing", n)
	}
}

// One of three bare, two already inbound: prime only the one that needs it.
func TestBuildProducePlan_PartialEmpty_PrimesOnlyTheUnprimedPosition(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures(primeSecond)

	occ := map[string]bool{primeHead: true, primePaired: false, primeSecond: false}
	primed := map[string]bool{primePaired: true}
	plan, err := BuildProducePlan(node, runtime, claim, fixedNow, occ, primed)
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if len(plan.PrimePairedPositions) != 1 || plan.PrimePairedPositions[0].Dest != primeSecond {
		t.Fatalf("primes = %+v, want exactly one for %s", plan.PrimePairedPositions, primeSecond)
	}
}

// The prime is press-index-only. Every other swap mode keeps its existing
// behavior whatever the paired position reads.
func TestBuildProducePlan_PartialEmpty_OtherModesUnaffected(t *testing.T) {
	t.Parallel()
	for _, mode := range []protocol.SwapMode{"sequential", "single_robot", "two_robot"} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			node, runtime, claim := produceFixtures(mode)
			occ := map[string]bool{primeHead: true, primePaired: false}
			plan, err := BuildProducePlan(node, runtime, claim, fixedNow, occ, nil)
			if err != nil {
				t.Fatalf("BuildProducePlan: %v", err)
			}
			if plan.SuppressSwap || len(plan.PrimePairedPositions) != 0 {
				t.Fatalf("%s must not prime; SuppressSwap=%v primes=%+v", mode, plan.SuppressSwap, plan.PrimePairedPositions)
			}
			if plan.Dispatch == nil {
				t.Fatalf("%s must still dispatch", mode)
			}
		})
	}
}

// A prime with nowhere to pull from is a refusal, not a silent order at ""
// — the same reading the consume side's downgrade gives a blank InboundSource.
func TestBuildProducePlan_PartialEmpty_NoInboundSourceRefuses(t *testing.T) {
	t.Parallel()
	node, runtime, claim := pressIndexFixtures("")
	claim.InboundSource = ""

	occ := map[string]bool{primeHead: true, primePaired: false}
	if _, err := BuildProducePlan(node, runtime, claim, fixedNow, occ, nil); err == nil {
		t.Fatal("want an error when a prime is needed and the claim has no inbound source")
	}
}

// ── apply level ─────────────────────────────────────────────────────────

// TestApplyProducePlan_PrimesOnly pins what the impure half does with a
// primes-only plan: a retrieve_empty (not a move — a move hunts a full bin in
// an empties pool), the merged auto-confirm signal, the episode's origin
// carried onto the row, no runtime order slots written, and ProcessNodeID set
// on the result.
func TestApplyProducePlan_PrimesOnly(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	eng := testEngine(t, db)

	runtime, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime")

	plan := &ProducePlan{
		SuppressSwap:         true,
		PrimePairedPositions: []SimplePrime{{Source: "MARKET-EMPTIES", Dest: "INDEX-B"}},
	}
	origin := ordermgr.Attached("episode-abc")

	res, err := eng.applyProducePlan(node, runtime, claim, plan, origin)
	testutil.MustNoErr(t, err, "applyProducePlan")

	if res.ProcessNodeID != nodeID {
		t.Errorf("ProcessNodeID = %d, want %d", res.ProcessNodeID, nodeID)
	}
	if res.CycleMode != protocol.SwapModeSimple {
		t.Errorf("CycleMode = %q, want %q", res.CycleMode, protocol.SwapModeSimple)
	}
	if res.Order != nil || res.OrderA != nil || res.OrderB != nil {
		t.Errorf("a primes-only round mints no swap legs; Order=%v OrderA=%v OrderB=%v", res.Order, res.OrderA, res.OrderB)
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

// TestRequestProduceSwap_PrimesOnceThenHolds is the end-to-end chatter guard:
// the first request primes the bare position, and a second request while that
// empty is still in flight creates no further row. Counting rows rather than
// inspecting the plan is the point — this is what the operator's double-tap
// actually produces.
func TestRequestProduceSwap_PrimesOnceThenHolds(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")

	eng := testEngine(t, db)
	// Core reports the head occupied and INDEX-B bare — the incident's shape.
	srv := nodeBinsStub(t, "PRESS")
	eng.coreClient = NewCoreClient(srv.URL)
	eng.SetCoreNodes([]protocol.NodeInfo{{Name: "PRESS"}, {Name: "INDEX-B"}, {Name: "MARKET-EMPTIES"}})

	res, err := eng.RequestProduceSwap(nodeID)
	testutil.MustNoErr(t, err, "first RequestProduceSwap")
	if len(res.PrimeOrders) != 1 {
		t.Fatalf("first request: PrimeOrders = %+v, want one prime (and no swap)", res.PrimeOrders)
	}
	if res.OrderA != nil || res.OrderB != nil {
		t.Fatalf("first request minted a swap; the whole point is that it must not")
	}

	// The second click neither primes again nor falls through to the swap: the
	// position is still bare, so there is nothing to index from yet.
	res2, err := eng.RequestProduceSwap(nodeID)
	if err == nil {
		t.Fatalf("second request: want a refusal while the prime is in flight, got %+v", res2)
	}
	if !strings.Contains(err.Error(), "already inbound") {
		t.Errorf("second request error = %q, want it to say an empty is already inbound", err)
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
	srv := nodeBinsStub(t, "PRESS")
	eng.coreClient = NewCoreClient(srv.URL)
	// A non-empty node set that does NOT contain INDEX-B: that is evidence.
	eng.SetCoreNodes([]protocol.NodeInfo{{Name: "PRESS"}, {Name: "MARKET-EMPTIES"}})
	// Parts on the press, so the cold-press guard cannot be what stops the
	// prime — the unknown-node read has to be.
	seedRuntimeUOP(t, db, nodeID)

	res, err := eng.RequestProduceSwap(nodeID)
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
// different transport. Suppressing the prime there would hand the cell back
// the un-sourceable swap this path exists to prevent, so the check is skipped
// and the prime fires on telemetry alone.
func TestRequestProduceSwap_EmptyCoreNodeSetStillPrimes(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")

	eng := testEngine(t, db)
	srv := nodeBinsStub(t, "PRESS")
	eng.coreClient = NewCoreClient(srv.URL)
	// No SetCoreNodes call at all — Core has not been heard from.

	res, err := eng.RequestProduceSwap(nodeID)
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

			plan := &ProducePlan{
				SuppressSwap:         true,
				PrimePairedPositions: []SimplePrime{{Source: "MARKET-EMPTIES", Dest: "INDEX-B"}},
			}
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
