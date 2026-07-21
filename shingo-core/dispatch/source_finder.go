package dispatch

// source_finder.go — the one shared source-finding seam behind BOTH intake
// planning (planRetrieve / planRetrieveEmpty / planMove) and the fulfillment
// scanner's replay path. One pure seam both callers share.
//
// Why it exists: the scanner's inline finder had drifted from the intake
// planners — it dropped the dedicated-loader-pool and group-scoped-empty tiers
// and mis-classified the NGRP error path — so an order that queued at intake was
// re-sourced with different (wrong) scoping on replay, silently re-opening two
// previously-fixed bugs (loader oldest-first/partial-buffer consumption;
// supermarket/lane empty isolation). One finder, one tier cascade, both callers
// route through it. A forbidigo rule (.golangci.yml) forbids the raw
// db.FindSourceBinFIFO / db.FindEmptyCompatibleBin fallbacks outside this file so
// the drift cannot silently reappear.
//
// The finder is PURE: it finds a bin, it never claims, transitions, or writes an
// order. The caller owns the capacity gate, the claim (ClaimForDispatch), the
// MoveToSourcing transition, result assembly, and fleet dispatch. Disposition
// (queue vs reshuffle vs terminal) lives INSIDE the finder as a closed outcome
// enum — that is deliberate: the NGRP error-path drift lived in the caller's
// error handling, so the classifier must live in the seam.

import (
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch/binsource"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// Intent distinguishes what kind of bin the caller needs. It is keyed on the
// order's data, never on OrderType==Complex or StepsJSON.
//
// FindSource has exactly two callers — PlanningService (simple retrieve /
// retrieve_empty / move intake) and the fulfillment scanner's replay. The
// Allocator is NOT one of them: it sources complex orders through its own
// findAvailableForNeed, which reads a single node. Do not assume complex
// pickups are covered by anything in this file.
type Intent int

const (
	// IntentFull needs a bin holding the order's payload (retrieve, move).
	IntentFull Intent = iota
	// IntentEmpty needs an empty compatible carrier (retrieve_empty).
	IntentEmpty
)

// Outcome is the closed disposition set FindSource returns.
type Outcome int

const (
	// OutcomeFound — a bin was located; Bin and Node are both set.
	OutcomeFound Outcome = iota
	// OutcomeWait — no bin available now; the caller queues with QueueReason.
	OutcomeWait
	// OutcomeReshuffle — the only candidate is buried; Buried carries the plan input.
	OutcomeReshuffle
	// OutcomeStructural — a permanent/terminal failure; TermCode + Err describe it.
	OutcomeStructural
)

// SourceResult is the closed result of FindSource. Bin and Node are returned
// together on OutcomeFound so the caller never re-resolves the node (which
// deleted two of the scanner's three ad-hoc rollbacks).
type SourceResult struct {
	Outcome Outcome

	// OutcomeFound.
	Bin  *bins.Bin
	Node *nodes.Node

	// OutcomeWait: the structured category the order is parked under + the
	// params the operator sentence is generated from. Replaces a pre-formatted
	// reason string so the caller parks through the formatter door (the same
	// code surfaces from every finder tier). Cause is the engineer-only scope
	// tag (which tier waited); the sentence is built by the caller from
	// QueueCode + QueueParams.
	QueueCode   protocol.QueueCode
	QueueCause  string
	QueueParams QueueParams

	// OutcomeReshuffle: the buried bin + its slot/lane for reshuffle planning.
	Buried *BuriedError

	// OutcomeStructural: TermCode is the planningError code the intake caller
	// re-raises verbatim (the queue_reason/skip-reason strings are a persisted,
	// compared contract); Err is the underlying error. The scanner maps any
	// structural outcome to its "structural" fail path.
	TermCode string
	Err      error
}

// FinderDB is the narrow store surface the finder needs. *store.DB satisfies it
// structurally; the assertion below catches a drift in the store method set, and
// finder tests drop a fake in to prove tier scoping (e.g. "FindSourceBinFIFO is
// never called while the loader pool is empty").
type FinderDB interface {
	GetNodeByDotName(name string) (*nodes.Node, error)
	GetNode(id int64) (*nodes.Node, error)
	ListBinsByNode(nodeID int64) ([]*bins.Bin, error)
	ListBinsByNodes(nodeIDs []int64) ([]*bins.Bin, error)
	FindSourceBinFIFO(payloadCode string, excludeNodeID int64) (*bins.Bin, error)
	FindEmptyCompatibleBin(payloadCode, preferZone string, excludeNodeID int64) (*bins.Bin, error)
	FindEmptyCompatibleBinInGroup(payloadCode string, groupNodeID, excludeNodeID int64) (*bins.Bin, error)
	IsSlotAccessible(slotNodeID int64) (bool, error)
	GetLoaderHomeByPositionNode(positionNodeID int64) (*loaders.Home, error)
	GetLoader(id int64) (*loaders.Loader, error)
	ListLoaderHomes(loaderID int64) ([]loaders.Home, error)
}

var _ FinderDB = (*store.DB)(nil)

// SourceFinder is the shared source-finding engine.
type SourceFinder struct {
	db       FinderDB
	resolver NodeResolver // may be nil (tier 1 self-guards)
	dbg      func(string, ...any)
}

// NewSourceFinder constructs a SourceFinder. resolver may be nil — the NGRP tier
// self-guards on it, exactly as the intake planners do (s.resolver != nil).
func NewSourceFinder(db FinderDB, resolver NodeResolver, dbg func(string, ...any)) *SourceFinder {
	return &SourceFinder{db: db, resolver: resolver, dbg: dbg}
}

func (f *SourceFinder) debug(format string, args ...any) {
	if f.dbg != nil {
		f.dbg(format, args...)
	}
}

// FindSource runs the tier cascade for one order and one intent and returns a
// closed outcome. It never claims, transitions, or writes.
//
// Tier cascade (intake's order — reproduced faithfully per intent/shape so
// replay cannot drift from intake):
//
//  1. NGRP synthetic source  → resolver.Resolve, classified   (full intent)
//  2. dedicated-loader pool  → sourceFromDedicatedLoader        (Drain/Fill)
//  3. group/lane-scoped empty → FindEmptyCompatibleBinInGroup   (empty intent)
//  4. concrete-node candidate → ListBinsByNode                  (move-shaped)
//  5. plant-wide fallback    → FindSourceBinFIFO / FindEmptyCompatibleBin
//  6. post-find buried check → IsSlotAccessible                 (empty intent)
//
// A move sources node-locally (tiers 1,2,4) and never falls through to the
// plant-wide scan; a retrieve_empty scoped to a synthetic source queues rather
// than widening; an NGRP capacity/buried error queues (or reshuffles) scoped —
// none of these fall through to tier 5. Those "no fall-through" edges are the
// bugs the collapse fixes; keep them exact.
func (f *SourceFinder) FindSource(order *orders.Order, intent Intent) SourceResult {
	payloadCode := order.PayloadCode
	// Move-shaped: a node-local source relocates the bin AT a concrete source
	// node (tier 4) and never scans plant-wide. Stage 4 keys this on the sourcing
	// intent data (SourceIntentLocal), stamped at intake, not on OrderType.
	moveShaped := order.SourceIntent == SourceIntentLocal

	// Destination resolved once — excludeNodeID (prevent same-node retrieve) and
	// preferZone (zone-preferring empty fallback). Kills the four open-coded
	// copies (planning_service ×2, scanner ×2).
	var (
		excludeID  int64
		preferZone string
	)
	if order.DeliveryNode != "" {
		if dest, err := f.db.GetNodeByDotName(order.DeliveryNode); err == nil && dest != nil {
			excludeID = dest.ID
			preferZone = dest.Zone
		}
	}

	// Source node resolved once (tiers 1–4). A lookup miss leaves it nil; tiers
	// gate on nil and fall through to the plant-wide scan (retrieve) or queue
	// (move — no plant-wide fallback).
	var srcNode *nodes.Node
	if order.SourceNode != "" {
		srcNode, _ = f.db.GetNodeByDotName(order.SourceNode)
	}

	var (
		bin     *bins.Bin
		binNode *nodes.Node
	)

	// ── Tier 1: NGRP synthetic source (full intent only) ──────────────────
	// Empties never route through the retrieve resolver: ResolveRetrieve is
	// payload-match-required and rejects PayloadCode=="" bins, so an empty pull
	// on an NGRP source falls to the group-scoped empty tier (planRetrieveEmpty's
	// comment). Errors route through the SAME classifier intake uses — this is
	// where the A4 drift lived (the scanner checked only *StructuralError and
	// fell through to plant-wide FIFO on a capacity/buried error).
	if intent == IntentFull && srcNode != nil && srcNode.IsSynthetic &&
		srcNode.NodeTypeCode == protocol.NodeClassNGRP && f.resolver != nil {
		result, err := f.resolver.Resolve(srcNode, OrderTypeRetrieve, payloadCode, nil)
		if err != nil {
			switch class, payload := classifyResolutionError(err); class {
			case ResolutionBuried:
				return SourceResult{Outcome: OutcomeReshuffle, Buried: payload.(*BuriedError)}
			case ResolutionStructural:
				return SourceResult{Outcome: OutcomeStructural, TermCode: codeStructural, Err: payload.(*StructuralError)}
			default:
				// Capacity / Transient / Fatal all QUEUE SCOPED — never fall
				// through to the plant-wide scan. (Intake queues here too.)
				f.debug("finder: no source in group %s for payload=%s, waiting", order.SourceNode, payloadCode)
				return SourceResult{
					Outcome:     OutcomeWait,
					QueueCode:   protocol.QueueWaitingForMaterial,
					QueueCause:  "finder-group-empty",
					QueueParams: QueueParams{Payload: payloadCode, Destination: order.SourceNode},
				}
			}
		}
		if result.Bin == nil {
			// Resolver returned a node but no concrete bin — queue and retry.
			// Matches planMove's defensive branch; safe for retrieve, where
			// ResolveRetrieve always carries a Bin on success.
			return SourceResult{
				Outcome:     OutcomeWait,
				QueueCode:   protocol.QueueWaitingForMaterial,
				QueueCause:  "finder-group-empty",
				QueueParams: QueueParams{Payload: payloadCode, Destination: order.SourceNode},
			}
		}
		bin = result.Bin
	}

	// ── Tier 2: dedicated-loader pool ─────────────────────────────────────
	// Drain (full) / Fill (empty). A payload-less move (full intent, blank
	// payload) skips the pool source — it is a direct relocation of the physical
	// bin at the position, handled by the concrete-node tier below. This mirrors
	// planMove:580 (`isLoaderPos && payloadCode != ""`); planRetrieve and
	// planRetrieveEmpty always carry a payload/intent that reaches here.
	if bin == nil && order.SourceNode != "" && (intent == IntentEmpty || payloadCode != "") {
		loaderIntent := binsource.Drain
		if intent == IntentEmpty {
			loaderIntent = binsource.Fill
		}
		chosen, node, isLoaderPos, lerr := f.sourceFromDedicatedLoader(order.SourceNode, payloadCode, loaderIntent)
		if lerr != nil {
			return SourceResult{Outcome: OutcomeStructural, TermCode: codeLoaderSource, Err: lerr}
		}
		if isLoaderPos {
			if chosen == nil {
				// Loader position, no eligible bin of X in the pool — QUEUE; do
				// NOT fall through to the plant-wide scan (the no-fall-through
				// invariant). Scoping oldest-part-first / partial-buffer is the
				// whole point of the loader pool.
				f.debug("finder: loader pool for %s has no %q, waiting", order.SourceNode, payloadCode)
				return SourceResult{
					Outcome:     OutcomeWait,
					QueueCode:   protocol.QueueWaitingForMaterial,
					QueueCause:  "finder-pool-empty",
					QueueParams: QueueParams{Payload: payloadCode, Destination: order.SourceNode},
				}
			}
			bin, binNode = chosen, node
		}
	}

	// ── Tier 3: group/lane-scoped empty (empty intent, synthetic source) ──
	// Restricts empties to descendants of the SourceNode NGRP/LANE so a
	// multi-supermarket setup doesn't pull from the wrong supermarket or the
	// empty-tote return area. On no-empty it QUEUES scoped — no fall-through to
	// the plant-wide empty scan.
	if bin == nil && intent == IntentEmpty && srcNode != nil && srcNode.IsSynthetic &&
		(srcNode.NodeTypeCode == protocol.NodeClassNGRP || srcNode.NodeTypeCode == protocol.NodeClassLANE) {
		groupBin, gerr := f.db.FindEmptyCompatibleBinInGroup(payloadCode, srcNode.ID, excludeID)
		if gerr != nil || groupBin == nil {
			f.debug("finder: no empty in group %s for payload=%s, waiting", order.SourceNode, payloadCode)
			return SourceResult{
				Outcome:     OutcomeWait,
				QueueCode:   protocol.QueueWaitingForMaterial,
				QueueCause:  "finder-group-empty",
				QueueParams: QueueParams{Kind: "empty", Payload: payloadCode, Destination: order.SourceNode},
			}
		}
		bin = groupBin
	}

	// ── Tier 4: concrete-node candidates (move-shaped, full intent) ───────
	// A move sources the bin parked AT its concrete source node — the first
	// available candidate (BinUnavailableReason=="", which skips the payload
	// check for a payload-less move, exactly as claimFirstAvailable does at
	// intake). No plant-wide fallback: not-found queues, it never widens.
	if bin == nil && moveShaped && intent == IntentFull && srcNode != nil {
		candidates, _ := f.db.ListBinsByNode(srcNode.ID)
		for _, b := range candidates {
			if BinUnavailableReason(b, payloadCode) != "" {
				continue
			}
			bin, binNode = b, srcNode
			break
		}
		if bin == nil {
			return SourceResult{
				Outcome:     OutcomeWait,
				QueueCode:   protocol.QueueWaitingForMaterial,
				QueueCause:  "finder-node-empty",
				QueueParams: QueueParams{Payload: payloadCode, Destination: order.SourceNode},
			}
		}
	}

	// ── Tier 5: plant-wide fallback (retrieve-shaped only) ────────────────
	// Move-shaped needs never reach here (tier 4 is terminal for a move).
	if bin == nil && !moveShaped {
		if intent == IntentFull {
			b, err := f.db.FindSourceBinFIFO(payloadCode, excludeID)
			if err != nil || b == nil {
				return SourceResult{
					Outcome:     OutcomeWait,
					QueueCode:   protocol.QueueWaitingForMaterial,
					QueueCause:  "finder-plant-empty",
					QueueParams: QueueParams{Payload: payloadCode},
				}
			}
			bin = b
		} else {
			b, err := f.db.FindEmptyCompatibleBin(payloadCode, preferZone, excludeID)
			if err != nil || b == nil {
				return SourceResult{
					Outcome:     OutcomeWait,
					QueueCode:   protocol.QueueWaitingForMaterial,
					QueueCause:  "finder-plant-empty",
					QueueParams: QueueParams{Kind: "empty", Payload: payloadCode},
				}
			}
			bin = b
		}
	}

	if bin == nil {
		params := QueueParams{Payload: payloadCode}
		cause := "finder-plant-empty"
		if intent == IntentEmpty {
			params = QueueParams{Kind: "empty", Payload: payloadCode}
		}
		return SourceResult{
			Outcome:     OutcomeWait,
			QueueCode:   protocol.QueueWaitingForMaterial,
			QueueCause:  cause,
			QueueParams: params,
		}
	}

	// Resolve the bin's node if a tier set `bin` without one (tiers 1 and 5).
	// A missing/unreadable node is the terminal codeNode intake raises.
	if binNode == nil {
		if bin.NodeID == nil {
			return SourceResult{Outcome: OutcomeStructural, TermCode: codeNode, Err: fmt.Errorf("source bin %d has no node", bin.ID)}
		}
		n, err := f.db.GetNode(*bin.NodeID)
		if err != nil {
			return SourceResult{Outcome: OutcomeStructural, TermCode: codeNode, Err: fmt.Errorf("resolve node for bin %d: %w", bin.ID, err)}
		}
		binNode = n
	}

	// ── Tier 6: post-find buried check (empty intent only) ────────────────
	// Preserves planRetrieveEmpty's last-resort reshuffle (:421-434): the empty
	// finder prefers lane-mouth empties, so a buried empty landing here means
	// every compatible empty is buried — dig this one out rather than dispatch a
	// robot to an unreachable slot. The full-retrieve path has no post-find
	// buried check (the NGRP resolver detects buried internally; a FIFO result
	// is not lane-buried).
	if intent == IntentEmpty && bin.NodeID != nil {
		if accessible, err := f.db.IsSlotAccessible(*bin.NodeID); err == nil && !accessible {
			if slot, serr := f.db.GetNode(*bin.NodeID); serr == nil && slot.ParentID != nil {
				if lane, lerr := f.db.GetNode(*slot.ParentID); lerr == nil && lane.NodeTypeCode == protocol.NodeClassLANE {
					f.debug("finder: empty bin %d buried at slot %s in lane %s, reshuffle", bin.ID, slot.Name, lane.Name)
					return SourceResult{Outcome: OutcomeReshuffle, Buried: &BuriedError{Bin: bin, Slot: slot, LaneID: lane.ID}}
				}
			}
		}
	}

	return SourceResult{Outcome: OutcomeFound, Bin: bin, Node: binNode}
}

// sourceFromDedicatedLoader is the dedicated-home-loader source path, moved onto
// the finder (from PlanningService) so the loader tier is compile-time
// unreachable from anywhere else. If sourceNodeName is a position on a
// dedicated_positions loader, it ranks the loader's WHOLE pool — its
// payload-pinned home positions AND its buffer slots (home_kind=buffer) — with
// binsource.Source and returns the chosen bin plus the node it sits at. That is
// what lets a cell bound to one home position consume a partial of X parked in
// the buffer: sourcing is over the loader's pool, not one slot. A shared_window
// (market) loader's windows live in the same table but are layout-gated out
// below (D5) so a window name never enters the flat-pool ranker.
//
//   - isLoaderPos=false → not a loader position; the caller falls back to its
//     normal (supermarket / global) sourcing, unchanged.
//   - isLoaderPos=true, bin=nil → a loader position but no eligible bin of X in
//     the pool; the caller QUEUES (must not fall through to the global scan).
//   - isLoaderPos=true, bin!=nil → the chosen bin and the node it is parked at.
func (f *SourceFinder) sourceFromDedicatedLoader(sourceNodeName, payloadCode string, intent binsource.Intent) (bin *bins.Bin, binNode *nodes.Node, isLoaderPos bool, err error) {
	srcNode, err := f.db.GetNodeByDotName(sourceNodeName)
	if err != nil {
		// A real lookup error must NOT be reported as "not a loader position" —
		// that would fall the caller through to the plant-wide scan (the very
		// bug this path fixes). Propagate so the order queues instead.
		return nil, nil, false, fmt.Errorf("resolve source node %s: %w", sourceNodeName, err)
	}
	if srcNode == nil {
		return nil, nil, false, nil // name doesn't resolve to a node → not a loader position
	}
	home, err := f.db.GetLoaderHomeByPositionNode(srcNode.ID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("resolve loader for node %s: %w", sourceNodeName, err)
	}
	if home == nil {
		return nil, nil, false, nil // not a loader position at all
	}
	// Layout gate (D5 / M3): Source ranks dedicated_positions loaders only. A
	// shared_window loader ALSO stores its windows in bin_loader_homes, so
	// without this a window node name would be ranked as a flat pool, bypassing
	// the supermarket/seam semantics that govern a market loader. A non-dedicated
	// (or vanished/archived) loader → treat as "not a loader source" and fall through.
	loader, err := f.db.GetLoader(home.LoaderID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("resolve loader %d for node %s: %w", home.LoaderID, sourceNodeName, err)
	}
	if loader == nil || loader.Layout != loaders.LayoutDedicatedPositions {
		return nil, nil, false, nil
	}

	// Pool = the loader's sourceable members: pinned home positions + buffer
	// slots (kept partials). An UNPINNED home (home_kind=home, no payload yet) is
	// inert and excluded (InSourcePool) so a stray bin on a half-configured
	// position is never sourced — the D4 buffer/unpinned-home disambiguation.
	members, err := f.db.ListLoaderHomes(home.LoaderID)
	if err != nil {
		return nil, nil, true, fmt.Errorf("list loader %d members: %w", home.LoaderID, err)
	}
	// Collect the sourceable members' node ids, then read every bin across them
	// in ONE query (ListBinsByNodes) rather than N per-member reads on the hot
	// path. A read error now FAILS the source (propagates → the order queues)
	// instead of being swallowed per member — a swallowed read silently shrank
	// the pool and could mis-source.
	poolNodes := make([]int64, 0, len(members))
	for _, m := range members {
		if !m.InSourcePool() {
			continue // unpinned home — inert, not a buffer
		}
		poolNodes = append(poolNodes, m.PositionNodeID)
	}
	slotBins, err := f.db.ListBinsByNodes(poolNodes)
	if err != nil {
		return nil, nil, true, fmt.Errorf("list bins for loader %d pool: %w", home.LoaderID, err)
	}
	cands := make([]binsource.Cand, 0, len(slotBins))
	byID := make(map[int64]*bins.Bin, len(slotBins))
	for _, b := range slotBins {
		cands = append(cands, candFromBin(b))
		byID[b.ID] = b
	}

	best, ok := binsource.Source(cands, binsource.Want{Payload: payloadCode, Intent: intent})
	if !ok {
		return nil, nil, true, nil // loader position, no eligible bin of X → caller queues
	}
	chosen := byID[best.BinID]
	if chosen == nil || chosen.NodeID == nil {
		// Defensive: Source only returns a BinID it was handed, and a pool bin
		// always carries the node it was read at — but never deref a nil node id.
		return nil, nil, true, fmt.Errorf("loader %d chose bin %d with no resolvable node", home.LoaderID, best.BinID)
	}
	node, err := f.db.GetNode(*chosen.NodeID)
	if err != nil {
		return nil, nil, true, fmt.Errorf("resolve node for bin %d: %w", chosen.ID, err)
	}
	return chosen, node, true, nil
}
