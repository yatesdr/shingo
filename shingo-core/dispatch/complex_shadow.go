package dispatch

import (
	"log"

	"shingo/protocol"
	binsstore "shingocore/store/bins"
	"shingocore/store/orders"
)

// ComplexPlan shadow comparison.
//
// Shadow mode validates, in production and with zero behavior change, that the
// pure BuildComplexPlan would make the same primary-claim decision the live
// inline claimComplexBins loop actually makes — and, via the persisted re-read
// below, that the claim actually reached the order row. When plan and live agree
// across a representative volume of real orders, the dispatcher can later be cut
// over to consume a Plan directly with confidence.
//
// The detection target is the silent-claim-failure class (ALN_002 → SMN_003,
// 2026-04-23): the live path "succeeds" but Order.BinID ends up nil or pointing
// at the wrong bin — either in memory, or set in memory yet never persisted — so
// the later manifest sync falls through to a fallback and the line bin reads
// empty/zeroed. The single value that went wrong in those incidents is
// Order.BinID (the primary claim), so that is what the shadow compares.
//
// Two things are deliberately out of scope here:
//   - The full per-bin set of a multi-pickup order (the order_bins junction).
//     The current scope compares only the primary claim; full-parity validation
//     of every claimed bin is future work, for when the dispatcher actually
//     consumes the Plan instead of the inline loop.
//   - Post-dispatch manifest resets — a bin's count being zeroed AFTER a correct
//     dispatch. That is a runtime-lifecycle problem, not a dispatch-time claim
//     decision, so it needs its own monitor and is not observable here. It is
//     tracked separately in SHINGO_TODO.md as the complex-order BinID lifecycle
//     invariant monitor.
//
// Everything here is read-only and log-only. snapshotPickupBins and the order
// re-read issue extra read queries (the live loop re-lists the same nodes when
// it claims); that is an accepted diagnostic cost on the complex-dispatch path,
// which is not hot.

// snapshotPickupBins lists the candidate bins at each distinct pickup node,
// keyed by dot-name, from the PRE-claim state. It must be called before
// claimComplexBins mutates bin ownership so BuildComplexPlan sees the same
// candidates the live loop will. Best-effort and read-only: a node that fails
// to resolve or list is simply omitted, which BuildComplexPlan renders as the
// same "no bins at node" skip the live loop records on the identical failure.
func (d *Dispatcher) snapshotPickupBins(steps []resolvedStep) map[string][]*binsstore.Bin {
	out := make(map[string][]*binsstore.Bin)
	for _, s := range steps {
		if s.Action != protocol.ActionPickup {
			continue
		}
		if _, done := out[s.Node]; done {
			continue
		}
		node, err := d.db.GetNodeByDotName(s.Node)
		if err != nil {
			continue
		}
		listed, err := d.db.ListBinsByNode(node.ID)
		if err != nil {
			continue
		}
		out[s.Node] = listed
	}
	return out
}

// shadowComparePlan builds the pure plan from the pre-claim candidate snapshot
// and logs where the plan's primary-claim decision diverges from what
// claimComplexBins persisted. Call it only after a SUCCESSFUL claim (err == nil)
// — that is where silent partial failures hide (the order proceeded yet
// Order.BinID is wrong or never reached the row). Log-only; it never alters
// dispatch.
//
// The primary comparison is against the PERSISTED Order.BinID (re-read from the
// row), so it observes a failed UpdateOrderBinID at dispatch time, not just an
// in-memory divergence. The in-memory value claimComplexBins set is kept as a
// secondary signal that pins a persistence failure specifically. A primary
// divergence that is not an outright silent failure is most often a claim race
// or candidate skew between the snapshot and the live list — logged distinctly
// so it is not mistaken for a planner logic bug.
func (d *Dispatcher) shadowComparePlan(order *orders.Order, steps []resolvedStep, binsByNode map[string][]*binsstore.Bin) {
	// Mirror claimComplexBins' processNode derivation: explicit ProcessNode for
	// swap orders, else SourceNode. (An earlier version passed ProcessNode raw,
	// so its IsProcessNode flagging was wrong whenever ProcessNode was empty.)
	processNode := order.ProcessNode
	if processNode == "" {
		processNode = order.SourceNode
	}

	plan := BuildComplexPlan(steps, binsByNode, order.PayloadCode, processNode)
	if plan == nil {
		return
	}

	// Cheap endpoint sanity assert: the plan re-derives endpoints from the same
	// resolved steps, so this only fires if the persisted Source/Delivery drift
	// from a fresh extract (e.g. an intake/replay update that didn't re-persist).
	if plan.SourceNode != order.SourceNode || plan.DeliveryNode != order.DeliveryNode {
		log.Printf("dispatch: complex-plan shadow endpoint mismatch order=%d plan=%s->%s persisted=%s->%s",
			order.ID, plan.SourceNode, plan.DeliveryNode, order.SourceNode, order.DeliveryNode)
	}

	planPrimary := plan.primaryBinID(processNode)

	// In-memory primary: the bin claimComplexBins set on the order struct in this
	// process — the secondary signal, used below to pin a persistence failure.
	var inMemoryPrimary int64
	if order.BinID != nil {
		inMemoryPrimary = *order.BinID
	}

	// Persisted primary: re-read the row so the comparison reflects what actually
	// landed in the DB. Best-effort — a failed re-read logs and falls back to the
	// in-memory value (it must never affect dispatch).
	persistedPrimary := inMemoryPrimary
	if persisted, err := d.db.GetOrder(order.ID); err != nil {
		log.Printf("dispatch: complex-plan shadow order=%d persisted re-read failed, using in-memory BinID: %v", order.ID, err)
	} else if persisted == nil || persisted.BinID == nil {
		persistedPrimary = 0
	} else {
		persistedPrimary = *persisted.BinID
	}

	// Primary comparison: the plan's primary claim vs the PERSISTED Order.BinID.
	switch {
	case planPrimary == persistedPrimary:
		// Agreement — the common case once parity holds. Nothing to report.
	case planPrimary != 0 && persistedPrimary == 0:
		// The plan, from the same candidates, would have set a primary bin, but
		// the row carries none after a successful claim — whether the live loop
		// never claimed it or set it in memory and failed to persist. This is the
		// silent-claim-failure signature.
		log.Printf("dispatch: complex-plan shadow SILENT-CLAIM-FAILURE order=%d plan would claim bin %d as primary but persisted Order.BinID is nil (plan claims=%d skips=%d)",
			order.ID, planPrimary, len(plan.BinClaims), len(plan.Skips))
	case planPrimary == 0 && persistedPrimary != 0:
		// The inverse: the row carries a primary the planner skipped. Indicates
		// the planner under-claims relative to the live loop (a planner-side gap).
		log.Printf("dispatch: complex-plan shadow order=%d planner under-claims: persisted Order.BinID=%d but plan claimed no primary (plan skips=%d)",
			order.ID, persistedPrimary, len(plan.Skips))
	default:
		// Both chose a primary but a different bin — most likely a claim race or
		// snapshot/candidate skew, not a logic divergence.
		log.Printf("dispatch: complex-plan shadow order=%d primary-claim mismatch plan=%d persisted=%d (likely claim race or candidate skew)",
			order.ID, planPrimary, persistedPrimary)
	}

	// Persistence check (secondary): the bin claimComplexBins set in memory did
	// not reach the row — a dispatch-time UpdateOrderBinID failure, which lets the
	// order proceed with a BinID the DB never recorded (one of the original
	// incident's silent paths). Distinct from the plan-vs-persisted divergence
	// above, which can also be a claim race or planner difference.
	if inMemoryPrimary != persistedPrimary {
		log.Printf("dispatch: complex-plan shadow order=%d BinID not persisted: in-memory=%d persisted=%d — UpdateOrderBinID did not stick",
			order.ID, inMemoryPrimary, persistedPrimary)
	}
}

// primaryBinID returns the bin the plan would write to Order.BinID: the claim
// at processNode if one exists, else the first claim — mirroring the primaryIdx
// selection in claimComplexBins. Returns 0 when the plan claimed nothing.
func (p *ComplexPlan) primaryBinID(processNode string) int64 {
	if len(p.BinClaims) == 0 {
		return 0
	}
	for _, c := range p.BinClaims {
		if c.NodeName == processNode {
			return c.BinID
		}
	}
	return p.BinClaims[0].BinID
}
