//go:build docker

package dispatch

// Differential parity harness for the complex-order bin-claim path.
//
// The instrument seeds identical real-Postgres state and runs both claim paths
// against it — the inline claimComplexBins loop and BuildComplexPlan ->
// ApplyComplexPlan — then asserts they agree on everything that matters:
//
//   - the SET of bins claimed for the order,
//   - the order_bins junction rows (per-bin source and destination),
//   - the terminal disposition (nil / claim_failed / no_source_bin / no_bin),
//   - and the primary bin written to Order.BinID.
//
// Identical state is achieved by materializing each case twice in one database
// under two namespaces ("L" for the legacy run, "A" for the plan/apply run);
// outcomes are normalized back to namespace-free identities before comparison,
// so a claim of "slot 2 at SMKT" matches across the two runs. Because both
// paths share ListBinsByNode and the real CAS (store/bins Claim), the only thing
// under test is whether the plan-driven path reproduces the inline loop.
//
// Two drivers:
//   - TestComplexClaimParity_Battery: an enumerated battery pinning every named
//     scenario and terminal disposition.
//   - TestComplexClaimParity_Fuzz: the generator (complex_parity_gen_test.go)
//     driving >=10,000 reproducible cases per run, full parity each.
//
// True CAS races (a candidate that reads claimable but loses the write) cannot
// be made deterministic without a production test-seam, so they are not in the
// cross-implementation parity drivers. Instead TestComplexClaimParity_RaceInjection
// fires two dispatchers at the same bin in the same instant via the simulator's
// ParallelGroup barrier and asserts each path INDEPENDENTLY routes the contention
// correctly (loser requeues as claim_failed, never fails as no_bin; a sibling
// bin lets the loser recover). That proves the harness can inject real races and
// that both implementations handle them, which is what the parity drivers can't.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// claimOutcome is the namespace-free result of running one claim path on one
// materialized case. Two outcomes equal => the paths agree.
type claimOutcome struct {
	code     string   // "" | "claim_failed" | "no_source_bin" | "no_bin"
	primary  string   // normalized label of Order.BinID, "" if none
	claimed  []string // sorted normalized labels of bins claimed by the order
	junction []string // sorted "srcNode>destNode=binLabel" rows
}

// parityHarness holds the per-DB fixtures shared across cases.
type parityHarness struct {
	db             *store.DB
	d              *Dispatcher
	binTypeID      int64
	blockerOrderID int64 // owns the pre-claimed (read-reject) bins
	parentOrderID  int64 // parent for compound-child cases
}

func newParityHarness(t *testing.T) *parityHarness {
	t.Helper()
	db := testdb.Open(t)
	// SetupStandardData creates the DEFAULT bin type and PART-A payload the
	// cases reuse; its STORAGE-A1/LINE1-IN nodes are unused here.
	testdb.SetupStandardData(t, db)
	bt, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		t.Fatalf("get DEFAULT bin type: %v", err)
	}
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// A real order to own the pre-claimed read-reject bins, and a real parent
	// for compound-child cases — keeps every claimed_by a valid reference.
	blocker := &orders.Order{EdgeUUID: "parity-blocker", StationID: "line-1", OrderType: OrderTypeComplex, Status: StatusQueued, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(blocker), "create blocker order")
	parent := &orders.Order{EdgeUUID: "parity-parent", StationID: "line-1", OrderType: OrderTypeComplex, Status: StatusQueued, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent order")

	return &parityHarness{db: db, d: d, binTypeID: bt.ID, blockerOrderID: blocker.ID, parentOrderID: parent.ID}
}

// nsNode / nsLabel build the namespaced physical identifiers; normNode /
// normLabel strip the namespace back off for comparison.
func nsNode(ns, caseNode string) string { return ns + "_" + caseNode }
func nsLabel(ns, caseNode string, slot int) string {
	return fmt.Sprintf("%s|%s|%d", ns, caseNode, slot)
}
func normNode(physical string) string {
	if len(physical) > 2 && (physical[:2] == "L_" || physical[:2] == "A_") {
		return physical[2:]
	}
	return physical
}
func normLabel(label string) string {
	if len(label) > 2 && (label[:2] == "L|" || label[:2] == "A|") {
		return label[2:]
	}
	return label
}

// createBinForState materializes one candidate bin in the seeded state.
func (h *parityHarness) createBinForState(t *testing.T, nodeID int64, label string, st genBinState, payload string) {
	t.Helper()
	b := &bins.Bin{BinTypeID: h.binTypeID, Label: label, NodeID: &nodeID, Status: "available"}
	switch st {
	case genFull, genClaimed, genLocked:
		b.PayloadCode = payload
	case genEmpty:
		b.PayloadCode = ""
	case genMismatch:
		b.PayloadCode = payload + "-X" // a different payload -> read reject
	}
	testutil.MustNoErr(t, h.db.CreateBin(b), "create bin "+label)
	switch st {
	case genClaimed:
		testutil.MustNoErr(t, h.db.ClaimBin(b.ID, h.blockerOrderID), "pre-claim bin "+label)
	case genLocked:
		testutil.MustNoErr(t, h.db.LockBin(b.ID, "parity-harness"), "lock bin "+label)
	}
}

// materialize creates the nodes, bins, and order for one case under a namespace,
// returning the order and the namespaced step slice the claim path consumes.
func (h *parityHarness) materialize(t *testing.T, c parityCase, ns string) (*orders.Order, []resolvedStep) {
	t.Helper()

	// Scope every physical name by both the namespace (L/A) and the case name
	// so many cases share one database without colliding. Normalization strips
	// only the namespace, so the case-scoped remainder is identical across the
	// two runs and compares equal.
	scoped := func(node string) string { return c.name + "_" + node }

	// Nodes: every node referenced by any step.
	created := map[string]bool{}
	for _, s := range c.steps {
		if s.Node == "" || created[s.Node] {
			continue
		}
		created[s.Node] = true
		testutil.MustNoErr(t, h.db.CreateNode(&nodes.Node{Name: nsNode(ns, scoped(s.Node)), Enabled: true}), "create node "+s.Node)
	}

	// Bins per pickup, in slot order.
	for _, p := range c.pickups {
		node, err := h.db.GetNodeByDotName(nsNode(ns, scoped(p.node)))
		if err != nil {
			t.Fatalf("resolve pickup node %s: %v", p.node, err)
		}
		for slot, st := range p.bins {
			h.createBinForState(t, node.ID, nsLabel(ns, scoped(p.node), slot), st, c.payloadCode)
		}
	}

	// Namespaced steps.
	steps := make([]resolvedStep, len(c.steps))
	for i, s := range c.steps {
		ns2 := s
		if s.Node != "" {
			ns2.Node = nsNode(ns, scoped(s.Node))
		}
		steps[i] = ns2
	}

	src, del := extractEndpoints(steps)
	// Exercise both the explicit-ProcessNode path and the ProcessNode==""
	// source fallback: leave it empty when the process node is the source.
	processNode := nsNode(ns, scoped(c.processNode))
	if processNode == src {
		processNode = ""
	}
	stepsJSON, _ := json.Marshal(steps)
	order := &orders.Order{
		EdgeUUID:     ns + "-" + c.name,
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     1,
		PayloadCode:  c.payloadCode,
		SourceNode:   src,
		DeliveryNode: del,
		ProcessNode:  processNode,
		StepsJSON:    string(stepsJSON),
	}
	if c.compoundChild {
		order.ParentOrderID = &h.parentOrderID
	}
	testutil.MustNoErr(t, h.db.CreateOrder(order), "create order "+order.EdgeUUID)
	return order, steps
}

// captureOutcome reads the durable state the claim path produced and normalizes
// it for comparison.
func (h *parityHarness) captureOutcome(t *testing.T, order *orders.Order, runErr error) claimOutcome {
	t.Helper()
	var out claimOutcome
	var pe *planningError
	if errors.As(runErr, &pe) {
		out.code = pe.Code
	}

	if o, err := h.db.GetOrder(order.ID); err == nil && o != nil && o.BinID != nil {
		if b, err := h.db.GetBin(*o.BinID); err == nil && b != nil {
			out.primary = normLabel(b.Label)
		}
	}

	claimedBins, err := h.db.ListBinsByClaim(order.ID)
	testutil.MustNoErr(t, err, "list claimed bins")
	for _, b := range claimedBins {
		out.claimed = append(out.claimed, normLabel(b.Label))
	}
	sort.Strings(out.claimed)

	obs, err := h.db.ListOrderBins(order.ID)
	testutil.MustNoErr(t, err, "list order_bins")
	for _, ob := range obs {
		label := ""
		if b, err := h.db.GetBin(ob.BinID); err == nil && b != nil {
			label = normLabel(b.Label)
		}
		out.junction = append(out.junction, fmt.Sprintf("%s>%s=%s", normNode(ob.NodeName), normNode(ob.DestNode), label))
	}
	sort.Strings(out.junction)
	return out
}

// runLegacy / runApply materialize the case under their namespace and run the
// respective claim path, returning the normalized outcome.
func (h *parityHarness) runLegacy(t *testing.T, c parityCase) claimOutcome {
	order, steps := h.materialize(t, c, "L")
	err := h.d.claimComplexBins(order, steps, c.payloadCode, nil)
	return h.captureOutcome(t, order, err)
}

func (h *parityHarness) runApply(t *testing.T, c parityCase) claimOutcome {
	order, steps := h.materialize(t, c, "A")
	processNode := order.ProcessNode
	if processNode == "" {
		processNode = order.SourceNode
	}
	plan := BuildComplexPlan(steps, h.d.snapshotPickupBins(steps), c.payloadCode, processNode)
	err := h.d.ApplyComplexPlan(order, plan, c.payloadCode, nil)
	return h.captureOutcome(t, order, err)
}

// assertParity runs both paths on the case and fails on any divergence.
func (h *parityHarness) assertParity(t *testing.T, c parityCase) {
	t.Helper()
	legacy := h.runLegacy(t, c)
	apply := h.runApply(t, c)
	if !reflect.DeepEqual(legacy, apply) {
		t.Errorf("PARITY DIVERGENCE on %s:\n  legacy: code=%q primary=%q claimed=%v junction=%v\n  apply : code=%q primary=%q claimed=%v junction=%v",
			c.name, legacy.code, legacy.primary, legacy.claimed, legacy.junction,
			apply.code, apply.primary, apply.claimed, apply.junction)
	}
}

// --- Enumerated battery -----------------------------------------------------

func TestComplexClaimParity_Battery(t *testing.T) {
	h := newParityHarness(t)

	full := func(n int) []genBinState {
		s := make([]genBinState, n)
		for i := range s {
			s[i] = genFull
		}
		return s
	}

	cases := []parityCase{
		{
			name: "E1_single_happy", payloadCode: "PART-A", processNode: "SMKT",
			steps:   []resolvedStep{{Action: protocol.ActionPickup, Node: "SMKT"}, {Action: protocol.ActionDropoff, Node: "LINE"}},
			pickups: []genPickup{{node: "SMKT", bins: full(1)}},
		},
		{
			name: "E2_walk_past_reject", payloadCode: "PART-A", processNode: "SMKT",
			steps:   []resolvedStep{{Action: protocol.ActionPickup, Node: "SMKT"}, {Action: protocol.ActionDropoff, Node: "LINE"}},
			pickups: []genPickup{{node: "SMKT", bins: []genBinState{genClaimed, genMismatch, genFull}}},
		},
		{
			name: "E3_all_empty_no_source_bin", payloadCode: "PART-A", processNode: "SMKT",
			steps:   []resolvedStep{{Action: protocol.ActionPickup, Node: "SMKT"}, {Action: protocol.ActionDropoff, Node: "LINE"}},
			pickups: []genPickup{{node: "SMKT", bins: nil}},
		},
		{
			name: "E4_all_rejected_no_bin", payloadCode: "PART-A", processNode: "SMKT",
			steps:   []resolvedStep{{Action: protocol.ActionPickup, Node: "SMKT"}, {Action: protocol.ActionDropoff, Node: "LINE"}},
			pickups: []genPickup{{node: "SMKT", bins: []genBinState{genClaimed, genLocked, genMismatch}}},
		},
		{
			name: "E5_partial_claim", payloadCode: "PART-A", processNode: "LINE",
			steps: []resolvedStep{
				{Action: protocol.ActionPickup, Node: "LINE"}, {Action: protocol.ActionDropoff, Node: "STORE"},
				{Action: protocol.ActionPickup, Node: "SMKT"}, {Action: protocol.ActionDropoff, Node: "LINE"},
			},
			pickups: []genPickup{{node: "LINE", bins: full(1)}, {node: "SMKT", bins: nil}},
		},
		{
			name: "E6_empty_leg_claims_empty", payloadCode: "PART-A", processNode: "LINE",
			steps: []resolvedStep{
				{Action: protocol.ActionPickup, Node: "LINE"}, {Action: protocol.ActionDropoff, Node: "STORE"},
				{Action: protocol.ActionPickup, Node: "SMKT", Empty: true}, {Action: protocol.ActionDropoff, Node: "LINE"},
			},
			pickups: []genPickup{{node: "LINE", bins: full(1)}, {node: "SMKT", empty: true, bins: []genBinState{genFull, genEmpty}}},
		},
		{
			name: "E7_empty_leg_no_carrier", payloadCode: "PART-A", processNode: "SMKT",
			steps:   []resolvedStep{{Action: protocol.ActionPickup, Node: "SMKT", Empty: true}, {Action: protocol.ActionDropoff, Node: "LINE"}},
			pickups: []genPickup{{node: "SMKT", empty: true, bins: []genBinState{genFull}}},
		},
		{
			name: "E8_same_node_double_pick", payloadCode: "PART-A", processNode: "SMKT",
			steps: []resolvedStep{
				{Action: protocol.ActionPickup, Node: "SMKT"}, {Action: protocol.ActionDropoff, Node: "LINE"},
				{Action: protocol.ActionPickup, Node: "SMKT"}, {Action: protocol.ActionDropoff, Node: "STORE"},
			},
			pickups: []genPickup{{node: "SMKT", bins: full(3)}},
		},
		{
			name: "E9_swap_process_node_primary", payloadCode: "PART-A", processNode: "LINE",
			steps: []resolvedStep{
				{Action: protocol.ActionPickup, Node: "SMKT"}, {Action: protocol.ActionDropoff, Node: "LINE"},
				{Action: protocol.ActionPickup, Node: "LINE"}, {Action: protocol.ActionDropoff, Node: "STORE"},
			},
			pickups: []genPickup{{node: "SMKT", bins: full(1)}, {node: "LINE", bins: full(1)}},
		},
		{
			name: "E10_compound_child_no_junction", payloadCode: "PART-A", processNode: "SMKT", compoundChild: true,
			steps: []resolvedStep{
				{Action: protocol.ActionPickup, Node: "SMKT"}, {Action: protocol.ActionDropoff, Node: "LINE"},
				{Action: protocol.ActionPickup, Node: "STORE"}, {Action: protocol.ActionDropoff, Node: "LINE"},
			},
			pickups: []genPickup{{node: "SMKT", bins: full(1)}, {node: "STORE", bins: full(1)}},
		},
		{
			name: "E11_ab_backfill_two_nodes", payloadCode: "PART-A", processNode: "POSA",
			steps: []resolvedStep{
				{Action: protocol.ActionPickup, Node: "POSA"}, {Action: protocol.ActionDropoff, Node: "LINE"},
				{Action: protocol.ActionPickup, Node: "POSB"}, {Action: protocol.ActionDropoff, Node: "LINE"},
			},
			pickups: []genPickup{{node: "POSA", bins: full(2)}, {node: "POSB", bins: full(2)}},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) { h.assertParity(t, c) })
	}
}

// --- Fuzz driver ------------------------------------------------------------

func TestComplexClaimParity_Fuzz(t *testing.T) {
	n := 10000
	seed := int64(1)
	if v := os.Getenv("PARITY_FUZZ_N"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			n = parsed
		}
	}
	if v := os.Getenv("PARITY_FUZZ_SEED"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			seed = parsed
		}
	}
	if testing.Short() && os.Getenv("PARITY_FUZZ_N") == "" {
		n = 1000
	}

	h := newParityHarness(t)
	cases := generateCases(seed, n)
	diverged := 0
	for i, c := range cases {
		legacy := h.runLegacy(t, c)
		apply := h.runApply(t, c)
		if !reflect.DeepEqual(legacy, apply) {
			diverged++
			t.Errorf("PARITY DIVERGENCE seed=%d case#%d (%s):\n  legacy: code=%q primary=%q claimed=%v junction=%v\n  apply : code=%q primary=%q claimed=%v junction=%v",
				seed, i, c.name, legacy.code, legacy.primary, legacy.claimed, legacy.junction,
				apply.code, apply.primary, apply.claimed, apply.junction)
			if diverged >= 20 {
				t.Fatalf("stopping after %d divergences (seed=%d) — re-run with PARITY_FUZZ_SEED=%d to reproduce", diverged, seed, seed)
			}
		}
	}
	t.Logf("fuzz parity: %d cases, seed=%d, %d divergences", n, seed, diverged)
}

// --- Race injection (ParallelGroup) ----------------------------------------

// raceClaimer is one of the two claim paths, run as a contender goroutine.
type raceClaimer func(h *parityHarness, order *orders.Order, steps []resolvedStep, payload string) error

func claimLegacy(h *parityHarness, order *orders.Order, steps []resolvedStep, payload string) error {
	return h.d.claimComplexBins(order, steps, payload, nil)
}
func claimApply(h *parityHarness, order *orders.Order, steps []resolvedStep, payload string) error {
	processNode := order.ProcessNode
	if processNode == "" {
		processNode = order.SourceNode
	}
	plan := BuildComplexPlan(steps, h.d.snapshotPickupBins(steps), payload, processNode)
	return h.d.ApplyComplexPlan(order, plan, payload, nil)
}

func errCode(err error) string {
	var pe *planningError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// TestComplexClaimParity_RaceInjection fires two dispatchers at the same node in
// the same instant against the real CAS, for each claim path independently. It
// proves the harness can inject real races and that both paths route them the
// same way: with a single claimable bin the loser requeues (claim_failed, NOT
// no_bin); with a sibling the loser recovers and both succeed.
func TestComplexClaimParity_RaceInjection(t *testing.T) {
	for _, impl := range []struct {
		name  string
		claim raceClaimer
	}{
		{"legacy", claimLegacy},
		{"apply", claimApply},
	} {
		impl := impl
		t.Run(impl.name, func(t *testing.T) {
			t.Run("single_bin_contention", func(t *testing.T) {
				// Several dispatchers contend for ONE bin, repeated across
				// iterations to reliably exercise both race outcomes:
				//   - a loser that lost the CAS itself (read passed, write lost)
				//     must route to claim_failed (requeue), never no_bin;
				//   - a loser that only read the bin AFTER the winner claimed it
				//     read-rejects to no_bin — a legitimate, identical-to-legacy
				//     outcome, not a misroute.
				// Every iteration must yield exactly one winner and no double
				// claim; across iterations at least one true CAS loss
				// (claim_failed) must be observed, proving the harness injects a
				// real read-pass/CAS-fail race and that it is classified
				// correctly.
				h := newParityHarness(t)
				const contenders = 4
				const iterations = 30
				totalClaimFailed := 0
				for iter := range iterations {
					node := fmt.Sprintf("RACE_%d", iter)
					testutil.MustNoErr(t, h.db.CreateNode(&nodes.Node{Name: nsNode("X", node), Enabled: true}), "create race node")
					n, _ := h.db.GetNodeByDotName(nsNode("X", node))
					h.createBinForState(t, n.ID, nsLabel("X", node, 0), genFull, "PART-A")

					steps := []resolvedStep{{Action: protocol.ActionPickup, Node: nsNode("X", node)}, {Action: protocol.ActionDropoff, Node: nsNode("X", node)}}
					stepsJSON, _ := json.Marshal(steps)
					racers := make([]*orders.Order, contenders)
					for k := range racers {
						o := &orders.Order{EdgeUUID: fmt.Sprintf("race-%d-%d", iter, k), StationID: "line-1", OrderType: OrderTypeComplex, Status: StatusQueued, Quantity: 1, PayloadCode: "PART-A", SourceNode: nsNode("X", node), DeliveryNode: nsNode("X", node), StepsJSON: string(stepsJSON)}
						testutil.MustNoErr(t, h.db.CreateOrder(o), o.EdgeUUID)
						racers[k] = o
					}

					results := make([]error, contenders)
					simulator.ParallelGroup(contenders, func(i int) {
						results[i] = impl.claim(h, racers[i], steps, "PART-A")
					})

					winners := 0
					binOwners := map[int64]bool{}
					for k, o := range racers {
						switch errCode(results[k]) {
						case "":
							winners++
							claimed, _ := h.db.ListBinsByClaim(o.ID)
							if len(claimed) != 1 {
								t.Errorf("%s iter %d: winner order %d claimed %d bins, want 1", impl.name, iter, o.ID, len(claimed))
								continue
							}
							if binOwners[claimed[0].ID] {
								t.Errorf("%s iter %d: bin %d double-claimed — CAS did not arbitrate", impl.name, iter, claimed[0].ID)
							}
							binOwners[claimed[0].ID] = true
						case "claim_failed":
							totalClaimFailed++
						case "no_bin":
							// Read-reject after the winner's claim — legitimate.
						default:
							t.Errorf("%s iter %d: order %d unexpected code %q", impl.name, iter, o.ID, errCode(results[k]))
						}
					}
					if winners != 1 {
						t.Errorf("%s iter %d: want exactly 1 winner, got %d", impl.name, iter, winners)
					}
				}
				if totalClaimFailed == 0 {
					t.Errorf("%s: no claim_failed observed across %d iterations of %d-way contention — a true read-pass/CAS-fail race was never injected", impl.name, iterations, contenders)
				} else {
					t.Logf("%s: observed %d true CAS losses (claim_failed) across %d iterations", impl.name, totalClaimFailed, iterations)
				}
			})

			t.Run("sibling_lets_loser_recover", func(t *testing.T) {
				h := newParityHarness(t)
				ns := "Y"
				node := "RACE2"
				testutil.MustNoErr(t, h.db.CreateNode(&nodes.Node{Name: nsNode(ns, node), Enabled: true}), "create race node")
				dst := "DST2"
				testutil.MustNoErr(t, h.db.CreateNode(&nodes.Node{Name: nsNode(ns, dst), Enabled: true}), "create dst node")
				n, _ := h.db.GetNodeByDotName(nsNode(ns, node))
				// Two claimable bins; two orders can both succeed on distinct bins.
				h.createBinForState(t, n.ID, nsLabel(ns, node, 0), genFull, "PART-A")
				h.createBinForState(t, n.ID, nsLabel(ns, node, 1), genFull, "PART-A")

				steps := []resolvedStep{{Action: protocol.ActionPickup, Node: nsNode(ns, node)}, {Action: protocol.ActionDropoff, Node: nsNode(ns, dst)}}
				stepsJSON, _ := json.Marshal(steps)
				mk := func(uuid string) *orders.Order {
					o := &orders.Order{EdgeUUID: uuid, StationID: "line-1", OrderType: OrderTypeComplex, Status: StatusQueued, Quantity: 1, PayloadCode: "PART-A", SourceNode: nsNode(ns, node), DeliveryNode: nsNode(ns, dst), StepsJSON: string(stepsJSON)}
					testutil.MustNoErr(t, h.db.CreateOrder(o), "create racer "+uuid)
					return o
				}
				o1, o2 := mk("race-2a"), mk("race-2b")

				var mu sync.Mutex
				results := map[int64]error{}
				orderList := []*orders.Order{o1, o2}
				simulator.ParallelGroup(2, func(i int) {
					err := impl.claim(h, orderList[i], steps, "PART-A")
					mu.Lock()
					results[orderList[i].ID] = err
					mu.Unlock()
				})

				claimedBy := map[int64]bool{}
				for _, o := range orderList {
					if code := errCode(results[o.ID]); code != "" {
						t.Errorf("%s: order %d failed with %q — both should claim a distinct sibling", impl.name, o.ID, code)
						continue
					}
					claimed, _ := h.db.ListBinsByClaim(o.ID)
					if len(claimed) != 1 {
						t.Errorf("%s: order %d claimed %d bins, want 1", impl.name, o.ID, len(claimed))
						continue
					}
					if claimedBy[claimed[0].ID] {
						t.Errorf("%s: bin %d claimed by two orders — CAS did not arbitrate", impl.name, claimed[0].ID)
					}
					claimedBy[claimed[0].ID] = true
				}
			})
		})
	}
}
