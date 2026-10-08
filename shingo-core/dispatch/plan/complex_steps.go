package plan

import (
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// complex_steps.go — the leg shape of a complex order: every step's node
// resolved to a concrete one (a group to a child, a blank pickup to a plant-wide
// source), with the carrier each dropoff puts down carried forward.
//
// Moved from dispatch/complex_steps.go unchanged (B1). The Dispatcher's
// resolveComplexSteps and reResolveComplexSteps are now one-line faces over
// these, and read nothing the planner does not.

// stepReader is what BinTypeAtNode reads: a node by name, and what stands on it.
type stepReader interface {
	GetNodeByDotName(name string) (*nodes.Node, error)
	ListBinsByNode(nodeID int64) ([]*bins.Bin, error)
}

// Planner is the planner's parts that choose a destination or shape legs, over
// one Reader and the one picker. It reads, and it never holds or writes.
type Planner struct {
	db       Reader
	finder   *Finder
	resolver binresolver.NodeResolver
	dbg      func(string, ...any)
}

// NewPlanner builds the planner over the reader and the picker it shares with
// its caller: one picker, so complex sourcing cannot drift from simple sourcing.
// resolver may be nil, as it may for the picker.
func NewPlanner(db Reader, finder *Finder, resolver binresolver.NodeResolver, dbg func(string, ...any)) *Planner {
	return &Planner{db: db, finder: finder, resolver: resolver, dbg: dbg}
}

func (p *Planner) debug(format string, args ...any) {
	if p.dbg != nil {
		p.dbg(format, args...)
	}
}

// ComplexSteps validates and resolves all steps, returning concrete node names.
//
// Pure function — does NOT side-effect sendError on failure. Callers
// decide how to surface the error (intake may queue on capacity
// errors via classifyResolutionError; the scanner replay path
// re-runs resolution per tick).
//
// Error shape: "step N: <reason>" — index info preserved so callers
// don't need to re-format. The wrapped reason from resolveStepNode
// preserves the original resolver substring so
// classifyResolutionError's ResolutionCapacity branch can match.
func (p *Planner) ComplexSteps(steps []protocol.ComplexOrderStep, payloadCode string, asker domain.DigAsker) ([]Step, error) {
	defer observe(p.db, "complex-steps")()
	var resolved []Step
	// The carrier the most recently resolved pickup is holding, which is the one
	// the next dropoff puts down. See the storeType comment in resolveStepNode
	// for why the dropoff cannot work this out for itself at intake.
	var carrier *int64
	var drops []groupDrop
	for i, step := range steps {
		// MG3-4: where this leg's carrier is GOING, for the pickup that has not
		// happened yet. See resolveStepNode.
		nextDrop := NextDropoffNode(steps, i)
		switch step.Action {
		case protocol.ActionPickup, protocol.ActionDropoff:
			// Blank dropoff = deferred destination (placeForDedicatedLoader resolves it
			// after intake). Pass it through unchanged, same as reResolveComplexSteps.
			if step.Action == protocol.ActionDropoff && step.Node == "" {
				resolved = append(resolved, Step{Action: protocol.ActionDropoff, Empty: step.Empty,
					PayloadCode: step.PayloadCode, ExclusiveSlot: step.ExclusiveSlot})
				continue
			}
			stepOrderPayload := payloadCode
			if step.Action == protocol.ActionDropoff {
				stepOrderPayload = DropPayload(i, step.PayloadCode, payloadCode,
					func(j int) bool { return steps[j].Action == protocol.ActionPickup },
					func(j int) bool { return steps[j].Empty })
			}
			nodeName, group, err := p.StepNode(step, stepOrderPayload, asker, nextDrop, carrier)
			if err != nil {
				return nil, fmt.Errorf("step %d: %w", i, err)
			}
			// A GROUP DROP NEVER WALLS THIS PLAN'S OWN LATER PICKUP (Q1), asked
			// once every step has its slot (unwallOwnPickups).
			if step.Action == protocol.ActionDropoff && group != "" {
				drops = append(drops, groupDrop{i: len(resolved), group: group,
					payload: StepPayload(step, stepOrderPayload),
					stated:  binresolver.BinTypeFrom(carrier, "complex step: no preceding pickup resolved to exactly one carrier")})
			}
			if step.Action == protocol.ActionPickup {
				carrier = BinTypeAtNode(p.db, nodeName)
			}
			resolved = append(resolved, Step{Action: step.Action, Node: nodeName, Group: group, Empty: step.Empty,
				PayloadCode: step.PayloadCode, ExclusiveSlot: step.ExclusiveSlot})
		case protocol.ActionWait:
			// Wait may optionally include a node (drive-to-and-hold).
			// If present, resolve it; otherwise it's a bare wait (split point only).
			// THE AUTHOR'S KIND IS CARRIED, NOT RE-DERIVED. This is a
			// translation from the wire type to Core's, not a new author: the
			// station stamped who owns its wait when it built the plan, and
			// dropping the stamp here would put the guessing back exactly where
			// it was — the fence would see an unowned wait and read it as the
			// station's by the drain-window default, which is right today by
			// accident and wrong the moment that window closes.
			if step.Node != "" {
				// nil carrier: a wait puts nothing down, so there is nothing for the
				// bin-type fence to narrow on.
				nodeName, group, err := p.StepNode(step, payloadCode, asker, nextDrop, nil)
				if err != nil {
					return nil, fmt.Errorf("step %d: %w", i, err)
				}
				resolved = append(resolved, Step{
					Action: protocol.ActionWait, Node: nodeName, Group: group, WaitKind: step.WaitKind,
				})
			} else {
				resolved = append(resolved, Step{Action: protocol.ActionWait, WaitKind: step.WaitKind})
			}
		default:
			return nil, fmt.Errorf("step %d: unknown step action: %s", i, step.Action)
		}
	}
	if _, err := p.unwallOwnPickups(resolved, drops, asker); err != nil {
		return nil, err
	}
	return resolved, nil
}

// ReComplexSteps walks an already-resolved step list and
// re-resolves any step whose node still references a synthetic NGRP.
// Used by DispatchPreparedComplex on the scanner replay path: when
// intake queued an order because the NGRP was saturated, the original
// NGRP names sit in stepsJSON until a later tick succeeds.
//
// Returns:
//   - newSteps: resolution-applied step list (concrete child names
//     where NGRP resolution succeeded).
//   - changed: true if any step's Node value changed from the input.
//     The dispatcher persists the new stepsJSON when changed=true so
//     subsequent ticks don't redo the resolution work and so claim
//     proceeds against the locked-in children.
//   - err: the first resolution error encountered. Caller distinguishes
//     capacity (queue, retry next tick), buried (replay reshuffle), and
//     other errors (fail) via classifyResolutionError.
func (p *Planner) ReComplexSteps(steps []Step, need Need) (newSteps []Step, changed bool, err error) {
	payloadCode, asker := need.PayloadCode, need.Asker
	defer observe(p.db, "complex-steps-replay")()
	newSteps = make([]Step, 0, len(steps))
	var drops []groupDrop
	for i, step := range steps {
		if step.Node == "" {
			newSteps = append(newSteps, step)
			continue
		}
		if step.WaitKind == WaitKindLane {
			// A LANE WAIT IS EXEMPT, EXPLICITLY. Its node is an RDS map point,
			// not a Core node — deliberately, so a point that never holds a bin
			// stays out of the node graph (PropLaneGatePoint). So the lookup
			// below cannot find it and it would ride the "node vanished —
			// unrecoverable" arm: the right OUTCOME (passed through untouched)
			// reached by a branch that means something else.
			//
			// That matters beyond tidiness. That arm is where a genuinely
			// missing node lands, and a reader debugging one should not have to
			// know that every gated plan takes it too. Naming the case also
			// makes it fail loudly if a plant ever configures a gate point that
			// HAPPENS to collide with a Core node name — without this, such a
			// wait would be looked up, possibly found to be an NGRP, and
			// re-resolved into a storage slot, silently re-aiming the gate.
			newSteps = append(newSteps, step)
			continue
		}
		node, lookupErr := p.db.GetNodeByDotName(step.Node)
		if lookupErr != nil || node == nil {
			// Node vanished from Core — unrecoverable. Fall through to
			// the original step; the claim path will surface a usable
			// error.
			newSteps = append(newSteps, step)
			continue
		}
		if !(node.IsSynthetic && node.NodeTypeCode == protocol.NodeClassNGRP) {
			// Already a concrete node — no re-resolution needed.
			newSteps = append(newSteps, step)
			continue
		}
		// Step still references an NGRP; re-attempt resolution. Carry Empty so
		// the produce empty-leg distinction survives replay re-resolution.
		ps := protocol.ComplexOrderStep{Action: step.Action, Node: step.Node, Empty: step.Empty,
			PayloadCode: step.PayloadCode, ExclusiveSlot: step.ExclusiveSlot}
		// Same carrier question as intake, asked of the already-resolved list.
		// The resolver would derive one from the asker here (this path has a real
		// order), but a complex PARENT owns legs rather than cargo — its bin_id is
		// NULL by design and its holds can name two carriers at once — so the
		// paired pickup is the better answer and this prefers it.
		stepOrderPayload := payloadCode
		if step.Action == protocol.ActionDropoff {
			stepOrderPayload = DropPayload(i, step.PayloadCode, payloadCode,
				func(j int) bool { return steps[j].Action == protocol.ActionPickup },
				func(j int) bool { return steps[j].Empty })
		}
		newName, group, resolveErr := p.StepNode(ps, stepOrderPayload, asker, "",
			p.BinTypeBeforeStep(steps, i))
		// A GROUP DROP NEVER WALLS THIS PLAN'S OWN LATER PICKUP (Q1), on replay as
		// at intake, asked once every step has its slot (unwallOwnPickups).
		if resolveErr == nil && step.Action == protocol.ActionDropoff && group != "" {
			drops = append(drops, groupDrop{i: len(newSteps), group: group,
				payload: StepPayload(ps, stepOrderPayload),
				stated: binresolver.BinTypeFrom(p.BinTypeBeforeStep(steps, i),
					"complex step: no preceding pickup resolved to exactly one carrier")})
		}
		var vacate *Vacate
		if resolveErr != nil {
			// THE VACATED-SLOT RULE, WHERE THIS WOULD WAIT. A group drop with no
			// free child may still land on the child a committed lift empties first.
			// Only on capacity, and only with the order in hand (the dispatch pass):
			// intake is unchanged, since the pair is not formed there yet.
			if need.Order == 0 || step.Action != protocol.ActionDropoff || CapacityShape(resolveErr) == "" {
				return steps, false, fmt.Errorf("step %d: %w", i, resolveErr)
			}
			sofar := append(append([]Step(nil), newSteps...), steps[i:]...)
			name, st := p.VacatedGroupChild(need, sofar, i, node)
			if st == nil {
				return steps, false, fmt.Errorf("step %d: %w", i, resolveErr)
			}
			newName, group, vacate = name, step.Node, st
		}
		if newName != step.Node {
			changed = true
		}
		// The rebuild drops any stamp the step carried: it names a node this
		// resolve may have moved off. A stamp only this resolve just granted rides.
		// Who owns a wait survives the rebuild (WaitKind, WaitLane): only the node
		// is re-resolved.
		newSteps = append(newSteps, Step{Action: step.Action, Node: newName, Group: group, Empty: step.Empty,
			PayloadCode:   step.PayloadCode,
			ExclusiveSlot: step.ExclusiveSlot,
			WaitKind:      step.WaitKind,
			WaitLane:      step.WaitLane,
			Vacate:        vacate,
			Anchor:        step.Anchor})
	}
	moved, err := p.unwallOwnPickups(newSteps, drops, asker)
	if err != nil {
		return steps, false, err
	}
	return newSteps, changed || moved, nil
}

// StepPayload is the payload THIS step's bin selection resolves against.
//
// A leg may name its own, and exactly one kind does: the refill leg of a
// changeover swap. That order carries the FROM-style payload because its
// opening pickup has to find the bin physically on the line, while the carrier
// it fetches has to suit the style arriving. Marking the leg Empty drops the
// full-bin content match but not bin-type compatibility, which resolves against
// whatever payload reaches PayloadBinTypeRuleArm — so before the step
// could say, the press was handed a carrier of the type it was leaving
// (sim 2026-08-24, N1-c).
//
// Everything else says nothing and gets the order's payload, which is what it
// has always got.
func StepPayload(step protocol.ComplexOrderStep, orderPayload string) string {
	if step.PayloadCode != "" {
		return step.PayloadCode
	}
	return orderPayload
}

// ResolvedStepPayload is StepPayload for the resolved form, and it sits here
// rather than beside its caller so the two spellings of one rule cannot drift.
// Step carries the payload through a replay precisely so the answer
// survives; reading the order's payload instead is how a changeover leg ends up
// sourcing the style being left.
func ResolvedStepPayload(step Step, orderPayload string) string {
	if step.PayloadCode != "" {
		return step.PayloadCode
	}
	return orderPayload
}

// DropPayload is the payload a dropoff at step i asks its group with: its own,
// if it names one; NONE, if the carrier it puts down is EMPTY (the nearest
// pickup ahead of it is an Empty pickup); otherwise the order's (C2b Q4).
//
// An empty may be stored wherever an empty may stand (payloadAllowedAt passes an
// empty payload), so asking with the order's payload refused it every lane that
// declares another one: a consume press's swap stamped with the payload it is
// leaving lifts an empty off the press, sets it down in the market, and waited
// under ngrp-resolve for a lane declared for that payload. A full set down
// mid-route still carries the order's payload, and is still fenced by it.
//
// pickupEmpty(j) answers whether step j is an Empty pickup, and false for every
// other step, so the request's steps and the resolved plan share this walk.
func DropPayload(i int, stepPayload, orderPayload string, isPickup, pickupEmpty func(j int) bool) string {
	if stepPayload != "" {
		return stepPayload
	}
	for j := i - 1; j >= 0; j-- {
		if isPickup(j) {
			if pickupEmpty(j) {
				return ""
			}
			break
		}
	}
	return orderPayload
}

// BinTypeBeforeStep names the carrier the nearest PICKUP ahead of index i is
// holding — the one a dropoff at i is about to put down.
//
// Backwards from i rather than tracked in a variable, because this runs on the
// REPLAY path, where only the steps still naming a group are re-resolved and the
// walk therefore skips most of the list. A carried variable would be updated on
// the iterations that happen to re-resolve and stale on the ones that do not.
// The lists are a handful of steps long, so the scan costs nothing.
func (p *Planner) BinTypeBeforeStep(steps []Step, i int) *int64 {
	defer observe(p.db, "bin-type-before-step")()
	for j := i - 1; j >= 0; j-- {
		if steps[j].Action != protocol.ActionPickup || steps[j].Node == "" {
			continue
		}
		return BinTypeAtNode(p.db, steps[j].Node)
	}
	return nil
}

// BinTypeAtNode names the carrier standing at a just-resolved pickup slot,
// so the dropoff that follows can be fenced by type at intake.
//
// EXACTLY ONE, OR NOTHING — the same rule typeFromDestinationPosition uses for
// the same reason. A slot holding one carrier is a statement about what this leg
// is about to carry. Zero means the pickup resolved against something this read
// cannot see (a lane whose slot rows live a level down, a group child picked for
// a bin not yet placed), and many means the answer is a guess. Both return nil,
// which resolves the dropoff untyped exactly as it did before.
//
// BEST EFFORT THROUGHOUT. Every error path returns nil rather than failing the
// intake: this is a narrowing, and a narrowing that cannot be computed must not
// be able to refuse an order Edge just asked for.
func BinTypeAtNode(db stepReader, nodeName string) *int64 {
	defer observe(db, "bin-type-at-node")()
	if nodeName == "" {
		return nil
	}
	node, err := db.GetNodeByDotName(nodeName)
	if err != nil || node == nil {
		return nil
	}
	binsAt, err := db.ListBinsByNode(node.ID)
	if err != nil || len(binsAt) != 1 {
		return nil
	}
	id := binsAt[0].BinTypeID
	return &id
}

// StepNode resolves a single step's node. If the node is a synthetic
// group (NGRP), it is automatically resolved via the group resolver. If the
// node is concrete, it is returned directly. If no node is provided, the
// global fallback resolves via payload code.
//
// Every source-finding path in this function now routes through the shared
// dispatch.SourceFinder (C(i)): the last inline copies of the plant-wide bin
// finders are gone, and the forbidigo carve-out that marked them
// (exclusions rule #7, complex_steps.go arm) is deleted with them. The
// Reshuffle→blind-dispatch mappings below preserve pre-fold behaviour
// byte-for-byte; surfacing the burial is a C(ii) decision.
func (p *Planner) StepNode(step protocol.ComplexOrderStep, orderPayload string,
	asker domain.DigAsker, nextDropoff string, carrierTypeID *int64) (string, string, error) {
	defer observe(p.db, "step-node")()
	payloadCode := StepPayload(step, orderPayload)
	if step.Node != "" {
		node, err := p.db.GetNodeByDotName(step.Node)
		if err != nil {
			return "", "", fmt.Errorf("node %q not found", step.Node)
		}
		// Auto-detect group nodes and resolve to a concrete slot.
		if node.IsSynthetic && node.NodeTypeCode == protocol.NodeClassNGRP && p.resolver != nil {
			// Empty pickup leg (produce node's "bring an empty to fill"):
			// resolve to a slot holding an EMPTY compatible carrier, not a
			// payload-matching full. Mirrors the source finder's group-scoped
			// empty tier (planTransport, empty intent), which also bypasses the
			// full-retrieve resolver for empties. (Pre-fix every complex pickup resolved as
			// OrderTypeRetrieve — a full — which delivered a full to produce
			// nodes; step.Empty now carries the distinction the old comment
			// here said it would need.)
			if step.Action == protocol.ActionPickup && step.Empty {
				// Through the shared finder (tier 3 runs the same
				// FindEmptyCompatibleBinInGroup this used to call inline, with
				// excludeID=0 because the need carries no DeliveryNode).
				res := p.finder.Pick(Need{
					SourceNode:  step.Node,
					PayloadCode: payloadCode,
					ProcessNode: nextDropoff,
					Intent:      IntentEmpty,
					Asker:       asker,
				})
				switch res.Outcome {
				case OutcomeFound:
					return res.Node.Name, step.Node, nil
				case OutcomeReshuffle:
					// Pre-fold behaviour, preserved BYTE-FOR-BYTE for C(i): the
					// inline call never checked slot accessibility, so a buried
					// empty was dispatched at blind. The finder's tier-6 check
					// now SEES the burial; acting on it (reshuffle instead of a
					// robot fault at an unreachable slot) is a C(ii) decision,
					// not a ratchet side effect.
					return res.Buried.Slot.Name, step.Node, nil
				default:
					// Wait and Structural both collapse to the resolution error
					// this caller has always returned. Known string delta, DB-
					// error path only: the inline code distinguished a db error
					// ("cannot resolve empty in group") from no-bin; the finder
					// folds both into Wait.
					return "", "", fmt.Errorf("no empty carrier in group %s", step.Node)
				}
			}
			// Per STEP, not per order: one complex order's steps go both ways,
			// which is why the resolver takes a direction rather than the
			// order's type.
			mode := binresolver.ResolveModeRetrieve
			if step.Action == protocol.ActionDropoff {
				mode = binresolver.ResolveModeStore
			}
			// THE CARRIER TYPE, FOR THE DROPOFF THAT HAS ONE. At intake the order
			// row does not exist yet (complex_intake.go passes reservations.Anyone
			// for exactly that reason), so the resolver's own derivation has no id
			// to work from and the per-node Allowed Bin Types fence would be
			// skipped — on a destination choice that is then LOCKED IN, because
			// reResolveComplexSteps only re-resolves steps still naming a group.
			//
			// The pickup that precedes it is the answer: it resolves to a slot
			// holding a specific carrier, and that carrier is the one this leg puts
			// down. Nil on a retrieve, and nil when the pickup could not be pinned
			// to exactly one bin, which resolves untyped as it always did.
			storeCarrier := binresolver.NoBinType
			if mode == binresolver.ResolveModeStore {
				storeCarrier = binresolver.BinTypeFrom(carrierTypeID,
					"complex step: no preceding pickup resolved to exactly one carrier")
			}
			result, err := p.resolver.Resolve(node, mode, payloadCode, storeCarrier, asker, nil)
			if err != nil {
				return "", "", fmt.Errorf("cannot resolve group %s: %w", step.Node, err)
			}
			return result.Node.Name, step.Node, nil
		}
		return node.Name, "", nil
	}
	// Global fallback: when Edge sends no node, resolve the pickup SOURCE using
	// the payload code (FindSourceBinFIFO / FindEmptyCompatibleBin). Blank
	// dropoffs are deferred and short-circuited by the callers, so they never
	// reach here — there is no dropoff fallback.
	if payloadCode != "" {
		switch step.Action {
		case protocol.ActionPickup:
			// Global fallback resolver: no order-level destination context here
			// (we are picking the source), so no node to exclude. Pass 0.
			// Empty leg sources an empty carrier (FindEmptyCompatibleBin),
			// matching the NGRP empty branch above; otherwise a full (FIFO).
			// Through the shared finder: a need with NO source node and
			// NodeLocal=false skips tiers 1-4 and lands exactly on the tier-5
			// plant-wide calls this used to make inline (preferZone "" and
			// excludeID 0 because the need carries no DeliveryNode).
			intent := IntentFull
			if step.Empty {
				intent = IntentEmpty
			}
			res := p.finder.Pick(Need{
				PayloadCode: payloadCode,
				Intent:      intent,
				Asker:       asker,
				ProcessNode: nextDropoff,
			})
			switch res.Outcome {
			case OutcomeFound:
				p.debug("resolveStepNode: global fallback pickup → %s (bin %d)", res.Node.Name, res.Bin.ID)
				return res.Node.Name, "", nil
			case OutcomeReshuffle:
				// Pre-fold blind dispatch preserved — see the NGRP branch above.
				return res.Buried.Slot.Name, "", nil
			default:
				if step.Empty {
					return "", "", fmt.Errorf("no empty carrier for payload %q", payloadCode)
				}
				return "", "", fmt.Errorf("no source bin for payload %q", payloadCode)
			}
		}
	}
	return "", "", fmt.Errorf("step requires either node or payload_code for resolution")
}

// NextDropoffNode names where the carrier a leg is picking up is GOING — the
// first dropoff at or after this step that names a node.
//
// ── MG3-4: OUTFLOW TYPING, AND WHY THE DESTINATION IS THE INPUT ─────────────
//
// A press empty pull has no payload to reason from: it is asking for an empty
// carrier, and "empty" says nothing about which type. The only fact available at
// selection time is the position it is going TO, and that position's effective
// bin types say what fits there.
//
// This threads the destination in. The finder decides what to do with it —
// exactly one effective type at the position means that is the want; zero or
// many leaves today's behaviour untouched, because "this slot fits anything" and
// "nobody has said" are both answered correctly by not narrowing.
//
// IT IS ALSO THE FENCE'S INPUT. A supported press reaches its own maintained
// group through the supports list, keyed on this same node. Before MG3-4 a
// complex leg carried no process identity at all, so every press-index swap was
// an outsider at every group — including the one built to serve it.
//
// INERT ON EMPTY CONFIG, deliberately. A plant with no per-position bin types
// and no maintained groups threads a node name that changes no decision. That is
// why it lands regardless of what the census says about how many positions are
// typed today: the wire has to exist before the config is worth populating.
//
// The FIRST dropoff at or after the step, not the last: a multi-leg plan drops
// at several places, and the one that matters for a pickup is where that
// carrier is set down next.
func NextDropoffNode(steps []protocol.ComplexOrderStep, from int) string {
	for i := from; i < len(steps); i++ {
		if steps[i].Action == protocol.ActionDropoff && steps[i].Node != "" {
			return steps[i].Node
		}
	}
	return ""
}
