package dispatch

import (
	"log"
	"slices"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/fleet"
	"shingocore/store/bins"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// ── CARRIER KEYS ON THE BLOCKS: WHICH STEP SENDS ITS FAMILY'S KEY ────────────
//
// A family's lift key goes on the lift of a cart of that family, and its wait
// key on a wait that stands directly in front of that lift at the same node:
// the one place the robot stands at a cart it is about to recognize. Every
// other step sends the default (seerrds.BinTaskForAction): a set-down, and a
// wait made while carrying a cart (a lane gate point, a dig dwell, a turn wait
// in front of a set-down), which sends what it sends today.
//
// ONE BUILDER. orderBlocks is the only caller of stepsToBlocks, and the four
// sites that send an order's blocks call it: the sealed create
// (dispatchToFleetCore), the unsealed create (buildUnsealedCreate), the
// ungated complex create (buildComplexCommit) and the append (appendSegment).
// A fifth site that built its own blocks would send the default key where a
// family names one, the way a load sequence was once dropped by a path that
// did not know it. Pinned by TestCensus_OneBlockBuilder.

// blockRun is a run of an order's plan sent in one fleet call: its steps as
// sent, the block number before the first (blockOffset), and how the keys find
// what the rule reads. at and families are asked only when a family names a
// key, so a plant with none decodes no plan and reads no cart.
type blockRun struct {
	steps  []resolvedStep
	offset int
	// at is the plan the order's cart rows point into, and steps[0]'s position
	// in it; ok=false when it cannot be read.
	at func() (plan []resolvedStep, start int, ok bool)
	// families is a cart's carrier family by bin id, asked only for a cart a
	// step lifts.
	families func(binID int64) (int64, bool)
}

// createRun is a create's run: the head of plan, whose rows the order holds,
// with the carts the create read for its robot group (capabilityBins), so the
// keys read no cart again.
func createRun(plan, head []resolvedStep, carts []*bins.Bin) blockRun {
	return blockRun{
		steps: head,
		at:    func() ([]resolvedStep, int, bool) { return plan, 0, true },
		families: func(bin int64) (int64, bool) {
			for _, c := range carts {
				if c.ID == bin {
					return c.BinFamilyID, true
				}
			}
			return 0, false
		},
	}
}

// appendRun is an append's run: segment, cut from the order's stored plan
// (order.StepsJSON, the plan ClaimAppend witnesses) after its wait
// order.WaitIndex. The plan position is segmentStart; the block number is
// blockOffset. The two differ when a bare wait, which sends no block, stands
// before the segment (TestCarrierKeys_AnAppendNamesItsStepByPlanPosition). The
// carts' families are one CartsOfOrder read, made on the first ask.
func (d *Dispatcher) appendRun(order *orders.Order, segment []resolvedStep, blockOffset int) blockRun {
	var families map[int64]int64
	read := false
	return blockRun{
		steps:  segment,
		offset: blockOffset,
		at: func() ([]resolvedStep, int, bool) {
			plan, ok := decodeSteps(order.StepsJSON)
			if !ok {
				return nil, 0, false
			}
			start := segmentStart(plan, order.WaitIndex)
			return plan, start, start >= 0
		},
		families: func(bin int64) (int64, bool) {
			if !read {
				read = true
				carts, err := d.db.CartsOfOrder(order.ID)
				if err != nil {
					log.Printf("dispatch: order %d: read its carts for their carrier keys: %v — its appended blocks send the default keys",
						order.ID, err)
				}
				families = make(map[int64]int64, len(carts))
				for _, c := range carts {
					families[c.ID] = c.BinFamilyID
				}
			}
			fam, ok := families[bin]
			return fam, ok
		},
	}
}

// orderBlocks is the one builder of an order's blocks (stepsToBlocks' only
// caller): the run's blocks, each lift and each wait in front of one sending
// its cart's family's key where Core's copy of the RDS scene lists that key at
// the block's location, every other block what it sends today. With no family
// keyed it reads nothing and passes no names: today's bytes
// (TestCarrierKeys_EveryKeyBlankIsToday).
func (d *Dispatcher) orderBlocks(order *orders.Order, vendorOrderID string, run blockRun, loadSeq *payloads.LoadSequence) []fleet.OrderBlock {
	return stepsToBlocks(vendorOrderID, run.steps, d.runKeyNames(order, run, loadSeq), run.offset, loadSeq)
}

// runKeyNames is the name each step of the run sends (stepsToBlocks' names):
// the rule's, less the pickup a load sequence names itself, less every name the
// scene copy does not list at its location. nil when no family is keyed, and
// then nothing is read.
func (d *Dispatcher) runKeyNames(order *orders.Order, run blockRun, loadSeq *payloads.LoadSequence) []string {
	keys := d.carrierKeys()
	if len(keys) == 0 {
		return nil
	}
	plan, start, ok := run.at()
	if !ok {
		log.Printf("dispatch: order %d: its stored plan does not place the segment it appends — its blocks send the default keys", order.ID)
		return nil
	}
	names := carrierKeyNames(plan, start, run.steps, order.Carts, run.families, keys)
	if names != nil && loadSeq != nil {
		if i := loadLegAt(run.steps); i >= 0 {
			names[i] = ""
		}
	}
	return d.namesTheSceneLists(order.ID, run.steps, names)
}

// namesTheSceneLists keeps each name that Core's copy of the RDS scene lists at
// its step's location, in every row the copy holds for that location
// (domain.ScenePoint.ListsBinTask), and blanks the rest: a location with no
// row, without the key, or whose binTask property has no value sends the
// default, as it does today. So an engineer adds a size's keys location by
// location, and a location still without them gets today's block rather than
// an order RDS refuses. One read for the run's locations, made only when a step
// named a key; a read that fails sends the default for the whole run, said
// once. nil when no name is kept. Pinned by
// TestCarrierKeys_ALocationTheSceneDoesNotListGetsTheDefault and
// TestCarrierKeys_AFailedSceneReadSendsTheDefaultOnce.
func (d *Dispatcher) namesTheSceneLists(orderID int64, steps []resolvedStep, names []string) []string {
	var locations []string
	for i, n := range names {
		if n != "" && !slices.Contains(locations, steps[i].Node) {
			locations = append(locations, steps[i].Node)
		}
	}
	if len(locations) == 0 {
		return nil
	}
	points, err := d.db.SceneLocationsNamed(locations)
	if err != nil {
		log.Printf("dispatch: order %d: read the scene's binTask keys at %v: %v — its blocks send the default keys",
			orderID, locations, err)
		return nil
	}
	at := make(map[string][]*domain.ScenePoint, len(locations))
	for _, p := range points {
		at[p.InstanceName] = append(at[p.InstanceName], p)
	}
	kept := false
	for i, n := range names {
		if n == "" {
			continue
		}
		rows := at[steps[i].Node]
		if len(rows) == 0 || slices.ContainsFunc(rows, func(p *domain.ScenePoint) bool { return !p.ListsBinTask(n) }) {
			names[i] = ""
			continue
		}
		kept = true
	}
	if !kept {
		return nil
	}
	return names
}

// carrierKeyNames is that rule, written once: the key each step of seg sends,
// by step ("" for the default), or nil when every step sends the default.
//
// seg is a run of the order's plan as it is sent, and start is its first step's
// position in plan. The rule reads seg's own steps (a redirect may have
// re-pointed a node in it: patchRedirectSegments) and, for the wait rule's
// look-ahead past seg's last step, the plan's next. rows are the order's carts
// (order_bins), familyOf a cart's carrier family by bin id, asked only for a
// cart a step lifts, and keys the families that name a key.
//
// It keys on the pick step, never on the bin: one cart can be lifted twice in
// one order (a relay, the carryover round trip), each lift its own row. A step
// no row lifts at, a cart with no family, a blank key, and rows at one step
// whose keys disagree all send the default. Pinned by
// TestCarrierKeyNames_TheRule.
func carrierKeyNames(plan []resolvedStep, start int, seg []resolvedStep, rows []domain.Cart,
	familyOf func(binID int64) (int64, bool), keys map[int64]familyKeys) []string {
	var names []string
	for k, s := range seg {
		var name string
		switch {
		case s.Action == protocol.ActionPickup:
			name = keyLiftedAt(start+k, rows, familyOf, keys, func(f familyKeys) string { return f.lift })
		case s.Action == protocol.ActionWait && s.Node != "":
			next, ok := stepAfter(plan, start, seg, k)
			if ok && next.Action == protocol.ActionPickup && next.Node == s.Node {
				name = keyLiftedAt(start+k+1, rows, familyOf, keys, func(f familyKeys) string { return f.wait })
			}
		}
		if name == "" {
			continue
		}
		if names == nil {
			names = make([]string, len(seg))
		}
		names[k] = name
	}
	return names
}

// stepAfter is the step after seg[k]: seg's next, or past seg's last, the plan's
// step after it (seg starts at plan position start).
func stepAfter(plan []resolvedStep, start int, seg []resolvedStep, k int) (resolvedStep, bool) {
	if k+1 < len(seg) {
		return seg[k+1], true
	}
	if p := start + len(seg); p >= 0 && p < len(plan) {
		return plan[p], true
	}
	return resolvedStep{}, false
}

// keyLiftedAt is the key (key picks the lift's or the wait's) of the carts the
// order lifts at plan step p, when they agree on one; "" when no row lifts
// there, a cart has no family, or two carts' keys differ.
func keyLiftedAt(p int, rows []domain.Cart, familyOf func(int64) (int64, bool), keys map[int64]familyKeys,
	key func(familyKeys) string) string {
	name, seen := "", false
	for _, r := range rows {
		if r.PickStep == nil || *r.PickStep != p {
			continue
		}
		k := ""
		if fam, ok := familyOf(r.BinID); ok {
			k = key(keys[fam])
		}
		if seen && k != name {
			return ""
		}
		name, seen = k, true
	}
	return name
}
