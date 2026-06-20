package dispatch

// Generator for complex-order claim-parity cases.
//
// This file is the input model for the differential parity harness
// (complex_parity_harness_test.go, docker-gated): it produces randomized
// (order-shape x bin-state) configurations that the harness materializes into
// real Postgres and runs through both claim paths, asserting they agree.
//
// The generator is deliberately weighted toward REAL plant traffic, not just
// structurally-valid edge shapes. The weights below are grounded in the
// Springfield complex-order distribution observed in production:
//
//   - pickups per order ran ~52% single / ~48% two-pickup, so the archetype
//     weights split pk1/pk2 in roughly that ratio.
//   - Two-pickup traffic is swap-dominated: every changeover spawns a swap-in
//     (supermarket->line->line) plus an evac-out (line->line->supermarket),
//     so the swap shapes carry the most weight inside pk2.
//   - Heavy carrier reuse means a pickup node usually holds several candidates
//     in FIFO order, so candidate counts skew to 2-3 with occasional 4.
//   - Produce-side empty legs (a press fetching an empty carrier to refill)
//     appear at plants with produce nodes; they are weighted in modestly.
//
// The edge shapes production under-samples are kept and reachable: same-node
// double-pick, walk-past-rejects, partial-claim (one node exhausted), and the
// A/B backfill swap. The all-empty / all-rejected / empty-leg-no-carrier /
// compound-child dispositions are pinned as explicit enumerated cases in the
// harness rather than left to chance, so every terminal disposition is covered
// on every run.
//
// This file carries no build tag so its self-check (TestParityGenerator_*)
// runs without Docker; the generator functions it defines are consumed by the
// docker-gated harness.

import (
	"fmt"
	"math/rand"

	"shingo/protocol"
)

// genBinState is the seeded state of one candidate bin at a pickup node. The
// harness translates each into a concrete row: claimable bins get a confirmed
// manifest matching the order payload; rejects get the property that makes
// BinUnavailableReason reject them at the read layer (so they route to the
// no_bin disposition, never to a CAS-loss race — true races are injected
// concurrently in the enumerated battery, not here).
type genBinState int

const (
	genFull     genBinState = iota // matching payload, available, unclaimed -> claimable
	genEmpty                       // empty carrier (no payload)             -> claimable; required for empty legs
	genMismatch                    // different payload                      -> read-reject
	genClaimed                     // matching payload but already claimed   -> read-reject
	genLocked                      // locked for active handling             -> read-reject
)

func (s genBinState) claimable(emptyLeg bool) bool {
	switch s {
	case genFull:
		// A full carrier is claimable on a normal leg; on an empty leg the
		// empty-only filter drops it.
		return !emptyLeg
	case genEmpty:
		// An empty carrier is claimable on an empty leg, and also on a normal
		// leg (BinUnavailableReason skips the payload check when the bin has no
		// payload) — both paths agree on that, which is the point of testing it.
		return true
	default:
		return false
	}
}

// genPickup is one pickup step's node and the candidate bins seeded there, in
// the order they will be created (and therefore the order ListBinsByNode walks
// them). The first claimable candidate is what both paths must select.
type genPickup struct {
	node  string
	empty bool // empty-leg: claim an empty carrier, drop payload context
	bins  []genBinState
}

// parityCase is one fully-described differential case. steps is the complete
// resolved sequence (pickups, dropoffs, waits) used for destination resolution
// and the wait split; pickups carries the seeded candidate state at each pickup
// node. The harness runs claimComplexBins and BuildComplexPlan->ApplyComplexPlan
// against identical materializations of this case and asserts full parity.
type parityCase struct {
	name        string
	payloadCode string
	processNode string
	steps       []resolvedStep
	pickups     []genPickup
	// compoundChild marks an order with a non-nil ParentOrderID — the junction
	// table is suppressed and Order.BinID tracks only the first claimed bin.
	compoundChild bool
}

// orderShape is a weighted archetype the generator draws from.
type orderShape int

const (
	shapeSingleRetrieve orderShape = iota // pk1: supermarket -> line
	shapeEvacOut                          // pk1: line -> supermarket
	shapeSwapInOneRobot                   // pk2: line(old)->store, replacement->line (one-robot full swap)
	shapeTwoRobotSupply                   // pk2: supermarket -> line -> line (two-robot supply leg)
	shapeABBackfill                       // pk2: two storage positions (A/B backfill)
	shapeSameNodeDouble                   // pk2: two pickups at the SAME node (double-pick edge shape)
)

// shapeWeights encodes the production-grounded archetype mix. pk1 archetypes
// sum to ~52, pk2 archetypes to ~48, matching the observed pickup distribution;
// inside pk2 the one-robot and two-robot swaps dominate, with the A/B backfill
// and the same-node double-pick as the deliberately-kept edge shapes.
var shapeWeights = []struct {
	shape  orderShape
	weight int
}{
	{shapeSingleRetrieve, 32},
	{shapeEvacOut, 20},
	{shapeSwapInOneRobot, 26},
	{shapeTwoRobotSupply, 12},
	{shapeABBackfill, 6},
	{shapeSameNodeDouble, 4},
}

// candidateCountWeights skews toward 2-3 candidates per node (FIFO reuse) with
// a long tail to 4 and a meaningful single-candidate share.
var candidateCountWeights = []int{ /*1*/ 30 /*2*/, 40 /*3*/, 20 /*4*/, 10}

// emptyLegOnSwapPercent is the share of one-robot swaps whose replacement leg
// fetches an empty carrier rather than a payload-matching full (produce-side
// refill). Springfield threw none; produce plants do, so it is kept reachable.
const emptyLegOnSwapPercent = 25

func weightedPick(r *rand.Rand, total int, weight func(i int) (idx int, w int), n int) int {
	roll := r.Intn(total)
	acc := 0
	for i := range n {
		idx, w := weight(i)
		acc += w
		if roll < acc {
			return idx
		}
	}
	return 0
}

func pickShape(r *rand.Rand) orderShape {
	total := 0
	for _, sw := range shapeWeights {
		total += sw.weight
	}
	idx := weightedPick(r, total, func(i int) (int, int) { return i, shapeWeights[i].weight }, len(shapeWeights))
	return shapeWeights[idx].shape
}

func pickCandidateCount(r *rand.Rand) int {
	total := 0
	for _, w := range candidateCountWeights {
		total += w
	}
	idx := weightedPick(r, total, func(i int) (int, int) { return i, candidateCountWeights[i] }, len(candidateCountWeights))
	return idx + 1
}

// genCandidates builds the candidate list for one pickup node. With ~35%
// probability it prefixes one or more read-rejects before the first claimable
// bin, exercising the walk-past-rejects path; with a small probability it
// leaves the node with no claimable bin at all (a partial-claim or, if every
// node lands here, an all-rejected order).
func genCandidates(r *rand.Rand, emptyLeg bool) []genBinState {
	n := pickCandidateCount(r)
	out := make([]genBinState, 0, n)

	// Rare: node has zero bins at all -> "no bins at node" skip.
	if r.Intn(100) < 6 {
		return nil
	}

	// Rare: node has bins but none claimable -> read-reject every candidate.
	allReject := r.Intn(100) < 8
	rejectStates := []genBinState{genMismatch, genClaimed, genLocked}

	// Number of leading rejects before the first claimable candidate.
	leadingRejects := 0
	if r.Intn(100) < 35 {
		leadingRejects = 1 + r.Intn(2) // 1 or 2
	}

	for i := range n {
		if allReject {
			out = append(out, rejectStates[r.Intn(len(rejectStates))])
			continue
		}
		if i < leadingRejects {
			out = append(out, rejectStates[r.Intn(len(rejectStates))])
			continue
		}
		// Claimable candidate: an empty leg needs an empty carrier; a normal
		// leg gets a full (with an occasional empty mixed in, which both paths
		// still accept — a useful agreement check).
		if emptyLeg {
			out = append(out, genEmpty)
		} else if r.Intn(100) < 12 {
			out = append(out, genEmpty)
		} else {
			out = append(out, genFull)
		}
	}
	return out
}

// generateCase draws one weighted, randomized parity case from r. The case is
// purely descriptive — no DB, no expected-outcome computation; the harness
// derives the outcome by running both real paths and comparing.
func generateCase(r *rand.Rand, idx int) parityCase {
	shape := pickShape(r)
	payload := "PART-A"

	// Distinct node names per case keep many cases isolated inside one test DB.
	line := fmt.Sprintf("c%d_LINE", idx)
	smkt := fmt.Sprintf("c%d_SMKT", idx)
	store := fmt.Sprintf("c%d_STORE", idx)
	posA := fmt.Sprintf("c%d_POSA", idx)
	posB := fmt.Sprintf("c%d_POSB", idx)

	pk := func(node string, empty bool) genPickup {
		return genPickup{node: node, empty: empty, bins: genCandidates(r, empty)}
	}

	c := parityCase{name: fmt.Sprintf("case%d", idx), payloadCode: payload}

	switch shape {
	case shapeSingleRetrieve:
		c.processNode = smkt
		c.steps = []resolvedStep{
			{Action: protocol.ActionPickup, Node: smkt},
			{Action: protocol.ActionDropoff, Node: line},
		}
		c.pickups = []genPickup{pk(smkt, false)}

	case shapeEvacOut:
		c.processNode = line
		c.steps = []resolvedStep{
			{Action: protocol.ActionPickup, Node: line},
			{Action: protocol.ActionDropoff, Node: smkt},
		}
		c.pickups = []genPickup{pk(line, false)}

	case shapeSwapInOneRobot:
		emptyLeg := r.Intn(100) < emptyLegOnSwapPercent
		c.processNode = line
		// pick old full at line -> store it -> fetch replacement -> deliver to line.
		c.steps = []resolvedStep{
			{Action: protocol.ActionWait, Node: line},
			{Action: protocol.ActionPickup, Node: line},
			{Action: protocol.ActionDropoff, Node: store},
			{Action: protocol.ActionPickup, Node: smkt, Empty: emptyLeg},
			{Action: protocol.ActionDropoff, Node: line},
		}
		c.pickups = []genPickup{pk(line, false), pk(smkt, emptyLeg)}

	case shapeTwoRobotSupply:
		// Supply leg of a two-robot swap: supermarket -> line, with a second
		// pickup modelling the replacement staging. Process node is the line.
		c.processNode = line
		c.steps = []resolvedStep{
			{Action: protocol.ActionPickup, Node: smkt},
			{Action: protocol.ActionDropoff, Node: line},
			{Action: protocol.ActionPickup, Node: line},
			{Action: protocol.ActionDropoff, Node: store},
		}
		c.pickups = []genPickup{pk(smkt, false), pk(line, false)}

	case shapeABBackfill:
		// A/B backfill: pull from two storage positions to consolidate.
		c.processNode = posA
		c.steps = []resolvedStep{
			{Action: protocol.ActionPickup, Node: posA},
			{Action: protocol.ActionDropoff, Node: line},
			{Action: protocol.ActionPickup, Node: posB},
			{Action: protocol.ActionDropoff, Node: line},
		}
		c.pickups = []genPickup{pk(posA, false), pk(posB, false)}

	case shapeSameNodeDouble:
		// Two pickups at the SAME node: the live loop's first claim consumes a
		// bin so the second must take a different one. Force >=2 claimable bins
		// so a distinct second pick is possible (the divergence target).
		c.processNode = smkt
		c.steps = []resolvedStep{
			{Action: protocol.ActionPickup, Node: smkt},
			{Action: protocol.ActionDropoff, Node: line},
			{Action: protocol.ActionPickup, Node: smkt},
			{Action: protocol.ActionDropoff, Node: store},
		}
		// One shared node materialization with several full carriers.
		shared := genPickup{node: smkt, bins: []genBinState{genFull, genFull, genFull}}
		c.pickups = []genPickup{shared}
	}

	// Compound children (ParentOrderID != nil) claim their bins but suppress the
	// order_bins junction — each child is a single-bin order. Fuzz that branch on
	// a fraction of the two-pickup shapes so junction suppression is exercised
	// across many states, not pinned by a single enumerated case.
	switch shape {
	case shapeSwapInOneRobot, shapeTwoRobotSupply, shapeABBackfill, shapeSameNodeDouble:
		if r.Intn(100) < 10 {
			c.compoundChild = true
		}
	}

	return c
}

// generateCases produces n weighted cases from a fixed seed. The seed makes a
// run fully reproducible: a divergence at case i is reproducible by re-running
// generateCases with the same seed and inspecting case i.
func generateCases(seed int64, n int) []parityCase {
	r := rand.New(rand.NewSource(seed))
	out := make([]parityCase, 0, n)
	for i := range n {
		out = append(out, generateCase(r, i))
	}
	return out
}
