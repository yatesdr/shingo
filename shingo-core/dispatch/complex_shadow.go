package dispatch

import (
	"log"

	"shingo/protocol"
	binsstore "shingocore/store/bins"
	"shingocore/store/orders"
)

// ComplexPlan shadow comparison.
//
// Runs read-only after every complex claim as a post-cutover canary: it rebuilds
// the pure BuildComplexPlan from the pre-claim snapshot and checks that what the
// live claim path (ApplyComplexPlan) persisted matches what a fresh plan
// predicts — and, via the persisted re-read below, that the claim actually
// reached the order row. A non-benign mismatch flags a real claim bug in
// production.
//
// The detection target is the silent-claim-failure class (ALN_002 → SMN_003,
// 2026-04-23): the live path "succeeds" but Order.BinID ends up nil or pointing
// at the wrong bin — either in memory, or set in memory yet never persisted — so
// the later manifest sync falls through to a fallback and the line bin reads
// empty/zeroed. The single value that went wrong in those incidents is
// Order.BinID (the primary claim), so that is what the shadow compares.
//
// Scope: the primary comparison (Order.BinID) plus the full claimed set and
// per-bin destinations of a multi-pickup order (the order_bins junction), via
// compareComplexJunction. Both are race-aware — a bin the other path now holds,
// or one consumed by a same-node double-pick, is agreement, not a divergence.
//
// One thing remains deliberately out of scope here:
//   - Post-dispatch manifest resets — a bin's count being zeroed AFTER a correct
//     dispatch. That is a runtime-lifecycle problem, not a dispatch-time claim
//     decision, so it needs its own monitor and is not observable here. It is
//     tracked separately in SHINGO_TODO.md as the complex-order BinID lifecycle
//     invariant monitor.
//
// Everything here is read-only and log-only. snapshotPickupBins and the order
// re-read issue extra read queries (the live claim path re-lists the same nodes
// when it claims); that is an accepted diagnostic cost on the complex-dispatch
// path, which is not hot.

// snapshotPickupBins lists the candidate bins at each distinct pickup node,
// keyed by dot-name, from the PRE-claim state. It must be called before
// ApplyComplexPlan mutates bin ownership so BuildComplexPlan sees the same
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
// ApplyComplexPlan persisted. Call it only after a SUCCESSFUL claim (err == nil)
// — that is where silent partial failures hide (the order proceeded yet
// Order.BinID is wrong or never reached the row). Log-only; it never alters
// dispatch.
//
// The primary comparison is against the PERSISTED Order.BinID (re-read from the
// row), so it observes a failed UpdateOrderBinID at dispatch time, not just an
// in-memory divergence. The in-memory value ApplyComplexPlan set is kept as a
// secondary signal that pins a persistence failure specifically. A primary
// divergence that is not an outright silent failure is most often a claim race
// or candidate skew between the snapshot and the live list — logged distinctly
// so it is not mistaken for a planner logic bug.
func (d *Dispatcher) shadowComparePlan(order *orders.Order, steps []resolvedStep, binsByNode map[string][]*binsstore.Bin) {
	// Mirror the live claim's processNode derivation: explicit ProcessNode for
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

	// In-memory primary: the bin ApplyComplexPlan set on the order struct in this
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
		// Both chose a primary but a different bin. Classify before crying wolf:
		// a predicted bin now held by another order is agreement-under-race; one
		// held by this order at another step is the same-node double-pick the
		// read-only planner cannot model; only a free predicted bin is a real
		// selection divergence.
		switch d.binDivergenceKind(planPrimary, order.ID) {
		case "race":
			d.dbg("complex-plan shadow order=%d primary agreement-under-race: plan bin %d taken by another order, authority claimed %d",
				order.ID, planPrimary, persistedPrimary)
		case "self":
			d.dbg("complex-plan shadow order=%d primary same-node double-pick: plan bin %d consumed by this order, authority claimed %d",
				order.ID, planPrimary, persistedPrimary)
		default:
			log.Printf("dispatch: complex-plan shadow order=%d primary-claim mismatch plan=%d persisted=%d (no race explanation)",
				order.ID, planPrimary, persistedPrimary)
		}
	}

	// Full claimed-set + junction parity (race-aware). The primary check above
	// sees only Order.BinID; this observes the second-and-later claims and the
	// per-bin destinations the cutover now drives — the columns a primary-only
	// shadow is blind to.
	d.compareComplexJunction(order, plan)

	// Persistence check (secondary): the bin ApplyComplexPlan set in memory did
	// not reach the row — a dispatch-time UpdateOrderBinID failure, which lets the
	// order proceed with a BinID the DB never recorded (one of the original
	// incident's silent paths). Distinct from the plan-vs-persisted divergence
	// above, which can also be a claim race or planner difference.
	if inMemoryPrimary != persistedPrimary {
		log.Printf("dispatch: complex-plan shadow order=%d BinID not persisted: in-memory=%d persisted=%d — UpdateOrderBinID did not stick",
			order.ID, inMemoryPrimary, persistedPrimary)
	}
}

// binDivergenceKind classifies why an authority claimed a different bin than the
// plan predicted for a step:
//
//   - "race": the predicted bin is now held by ANOTHER order, so the authority
//     correctly walked to a sibling — agreement-under-race, not a bug.
//   - "self": the predicted bin is held by THIS order at a different step — the
//     same-node double-pick the read-only planner cannot model (it builds its
//     claimed-map only after selection, so it predicts the same bin twice).
//   - "real": the predicted bin is free, so the difference is a genuine
//     selection divergence worth surfacing.
func (d *Dispatcher) binDivergenceKind(predictedBinID, orderID int64) string {
	if predictedBinID == 0 {
		return "real"
	}
	b, err := d.db.GetBin(predictedBinID)
	if err != nil || b == nil || b.ClaimedBy == nil {
		return "real"
	}
	if *b.ClaimedBy == orderID {
		return "self"
	}
	return "race"
}

// compareComplexJunction validates the full claimed set and per-bin destinations
// the cutover drives, against the pure plan, for multi-pickup orders (where the
// order_bins junction exists). It is the parity column a primary-only shadow is
// blind to: the second-and-later claims and their destinations.
//
// Race-aware and log-only. Differences a concurrent claim or a same-node
// double-pick explains are logged benign (debug); only an unexplained bin, a
// missing/extra row, or a destination mismatch on an otherwise-agreeing claim
// set is surfaced loudly. Compound children carry no junction and are skipped.
func (d *Dispatcher) compareComplexJunction(order *orders.Order, plan *ComplexPlan) {
	if len(plan.BinClaims) <= 1 || order.ParentOrderID != nil {
		return
	}
	persisted, err := d.db.ListOrderBins(order.ID)
	if err != nil {
		log.Printf("dispatch: complex-plan shadow order=%d junction re-read failed: %v", order.ID, err)
		return
	}
	persistedByStep := make(map[int]*orders.OrderBin, len(persisted))
	for _, ob := range persisted {
		persistedByStep[ob.StepIndex] = ob
	}
	plannedSteps := make(map[int]bool, len(plan.BinClaims))

	fullAgreement := len(persisted) == len(plan.BinClaims)
	for _, bc := range plan.BinClaims {
		plannedSteps[bc.StepIndex] = true
		ob, ok := persistedByStep[bc.StepIndex]
		if !ok {
			fullAgreement = false
			switch d.binDivergenceKind(bc.BinID, order.ID) {
			case "race", "self":
				d.dbg("complex-plan shadow order=%d junction step %d: plan bin %d not persisted, explained by race/double-pick", order.ID, bc.StepIndex, bc.BinID)
			default:
				log.Printf("dispatch: complex-plan shadow order=%d junction GAP step %d node=%s: plan predicted bin %d but no order_bins row (no race explanation)",
					order.ID, bc.StepIndex, bc.NodeName, bc.BinID)
			}
			continue
		}
		if ob.BinID != bc.BinID {
			fullAgreement = false
			switch d.binDivergenceKind(bc.BinID, order.ID) {
			case "race":
				d.dbg("complex-plan shadow order=%d junction step %d agreement-under-race: plan bin %d taken, authority claimed %d", order.ID, bc.StepIndex, bc.BinID, ob.BinID)
			case "self":
				d.dbg("complex-plan shadow order=%d junction step %d same-node double-pick: plan bin %d consumed by this order, authority claimed %d", order.ID, bc.StepIndex, bc.BinID, ob.BinID)
			default:
				log.Printf("dispatch: complex-plan shadow order=%d junction BIN mismatch step %d node=%s plan=%d persisted=%d (no race explanation)",
					order.ID, bc.StepIndex, bc.NodeName, bc.BinID, ob.BinID)
			}
		}
	}

	// Destinations are only comparable when the full claimed set agrees: a
	// sibling fallback on any step re-derives the destination map, so a partial
	// race would make a dest "mismatch" that is in fact correct. Under full
	// agreement both maps come from the same function over the same bins, so a
	// difference here is a genuine per-bin mis-delivery regression.
	if fullAgreement {
		for _, bc := range plan.BinClaims {
			ob := persistedByStep[bc.StepIndex]
			if expected := plan.PerBinDestinations[bc.BinID]; ob.DestNode != expected {
				log.Printf("dispatch: complex-plan shadow order=%d junction DEST mismatch step %d bin=%d plan-dest=%s persisted-dest=%s",
					order.ID, bc.StepIndex, bc.BinID, expected, ob.DestNode)
			}
		}
	}

	// Rows the plan never predicted — the authority claimed at a step the
	// read-only planner skipped (the planner-under-claims signal).
	for _, ob := range persisted {
		if !plannedSteps[ob.StepIndex] {
			log.Printf("dispatch: complex-plan shadow order=%d junction EXTRA step %d node=%s: persisted bin %d not predicted by plan",
				order.ID, ob.StepIndex, ob.NodeName, ob.BinID)
		}
	}
}

// primaryBinID returns the bin the plan would write to Order.BinID: the claim
// at processNode if one exists, else the first claim — mirroring the primaryIdx
// selection in ApplyComplexPlan. Returns 0 when the plan claimed nothing.
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
