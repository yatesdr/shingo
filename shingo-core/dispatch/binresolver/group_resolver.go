package binresolver

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// ErrBuried indicates the target bin exists but is blocked by shallower bins.
var ErrBuried = errors.New("bin is buried")

// StructuralError indicates a permanent resolution failure — the group
// structure cannot satisfy the request regardless of inventory changes.
type StructuralError struct {
	Group   string
	Payload string
	Reason  string
}

func (e *StructuralError) Error() string {
	return fmt.Sprintf("structural: %s (group=%s, payload=%s)",
		e.Reason, e.Group, e.Payload)
}

// BuriedError provides detail about a buried bin for reshuffle planning.
type BuriedError struct {
	Bin    *bins.Bin
	Slot   *nodes.Node
	LaneID int64
}

func (e *BuriedError) Error() string {
	return fmt.Sprintf("bin %d is buried at slot %s in lane %d", e.Bin.ID, e.Slot.Name, e.LaneID)
}

func (e *BuriedError) Unwrap() error { return ErrBuried }

// Retrieval algorithm codes.
const (
	RetrieveFIFO = "FIFO" // strict FIFO: globally oldest bin, proactive reshuffle when buried is older
	RetrieveCOST = "COST" // cost-optimized: oldest accessible bin, reshuffle only when none accessible
	RetrieveFAVL = "FAVL" // first available unclaimed bin, no reshuffle
)

// Storage algorithm codes.
const (
	StoreLKND = "LKND" // Like Kind: consolidate matching payload codes, then emptiest
	StoreDPTH = "DPTH" // Depth First: pack back-to-front regardless of payload
)

// GroupResolver handles NGRP → LANE → Slot and NGRP → direct child resolution.
//
// DB is the narrow Store interface (satisfied by *store.DB); see
// store.go. This lets per-algorithm tests drive the resolver with a
// fake and avoid database fixtures.
type GroupResolver struct {
	DB       Store
	DebugLog func(string, ...any)
}

func (r *GroupResolver) dbg(format string, args ...any) {
	if fn := r.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// getGroupAlgorithm reads an algorithm property from the node group,
// returning defaultVal if unset. When ASRS is explicitly disabled on the
// group (asrs_enabled="off"), the configured algorithm is ignored and the
// default applies — that is what the operator's "Enable ASRS" toggle means
// at runtime. Unset asrs_enabled (every existing group) leaves behavior
// unchanged.
func (r *GroupResolver) getGroupAlgorithm(groupID int64, key, defaultVal string) string {
	if r.DB.GetNodeProperty(groupID, "asrs_enabled") == "off" {
		return defaultVal
	}
	v := r.DB.GetNodeProperty(groupID, key)
	if v == "" {
		return defaultVal
	}
	return v
}

// ResolveRetrieve finds the best accessible bin across all lanes and direct children.
//
// asker is the order this resolution is for. It is what keeps a dig from
// hiding a lane from the order the dig was RUN for — pass reservations.Anyone
// only when there is genuinely no order behind the call, which reproduces the
// owner-blind behaviour this parameter was added to end.
func (r *GroupResolver) ResolveRetrieve(group *nodes.Node, payloadCode string, asker reservations.DigAsker,
	accept BinFilter) (*ResolveResult, error) {
	algo := r.getGroupAlgorithm(group.ID, "retrieve_algorithm", RetrieveFIFO)
	strategy := retrieveStrategies[algo]
	return r.scanForBestBin(group, payloadCode, strategy, asker, accept)
}

// retrieveStrategy controls how a retrieve algorithm scores accessible bins,
// whether it checks for buried bins, and how it decides between accessible vs buried.
type retrieveStrategy struct {
	label      string
	firstMatch bool
	// skipBuriedIfAccessible skips the buried-bin DB scan when an accessible
	// bin was found. COST sets this because it only reshuffles when no
	// accessible bin exists; FIFO clears it because it reshuffles even when
	// an accessible bin is found if the buried bin is older.
	skipBuriedIfAccessible bool
	checkBuried            func(r *GroupResolver, children []*nodes.Node, payloadCode string, accept BinFilter) (buried *bins.Bin, slot *nodes.Node, laneID int64)
	shouldTriggerBuried    func(buried *bins.Bin, buriedTime time.Time, accessible *bins.Bin, accessibleTime time.Time) bool
}

var retrieveStrategies = map[string]retrieveStrategy{
	RetrieveFIFO: {
		label:       "FIFO",
		firstMatch:  false,
		checkBuried: checkOldestBuried,
		shouldTriggerBuried: func(buried *bins.Bin, buriedTime time.Time, accessible *bins.Bin, accessibleTime time.Time) bool {
			return accessible == nil || buriedTime.Before(accessibleTime)
		},
	},
	RetrieveCOST: {
		label:                  "COST",
		firstMatch:             false,
		skipBuriedIfAccessible: true,
		checkBuried:            checkShallowestBuried,
		shouldTriggerBuried: func(buried *bins.Bin, buriedTime time.Time, accessible *bins.Bin, accessibleTime time.Time) bool {
			return accessible == nil
		},
	},
	RetrieveFAVL: {
		label:      "FAVL",
		firstMatch: true,
	},
}

// checkOldestBuried scans all lanes for the globally oldest buried bin.
// THE FILTER RIDES HERE TOO, AND THE CODE SAID SO BEFORE IT WAS WRITTEN.
// requiresFullCarrier's own note warned that teaching the resolver to prefer
// fulls would need the fullness rule inside it AND INSIDE THE BURIED-BIN
// LOOKUPS BEHIND IT, or a dig would be spent exposing a carrier that check then
// declines. (That note has since been rewritten to record the surgery as done —
// dispatch/source_finder.go, at the requiresFullCarrier guard — so it no longer
// reads as a warning against doing it.) Filtering only the accessible scan is
// precisely the half-fix it described: the
// accessible partials vanish, bestBin goes nil, the buried arm fires on an
// UNFILTERED lookup, and a whole excavation is spent exposing a carrier the
// caller refuses on arrival. A dig is the largest action this system takes; it
// must not be spent on a bin nobody can use.
func checkOldestBuried(r *GroupResolver, children []*nodes.Node, payloadCode string, accept BinFilter) (*bins.Bin, *nodes.Node, int64) {
	var best *bins.Bin
	var bestSlot *nodes.Node
	var bestLaneID int64
	var bestTime time.Time

	for _, child := range children {
		if !child.Enabled || child.NodeTypeCode != protocol.NodeClassLANE {
			continue
		}
		buried, slot, err := r.DB.FindOldestBuriedBin(child.ID, payloadCode)
		if err != nil || buried == nil || !accept.accepts(buried) {
			continue
		}
		bTime := binTimestamp(buried)
		if best == nil || bTime.Before(bestTime) {
			best = buried
			bestSlot = slot
			bestLaneID = child.ID
			bestTime = bTime
		}
	}
	return best, bestSlot, bestLaneID
}

// checkShallowestBuried scans lanes for the shallowest buried bin (cheapest to unblock).
func checkShallowestBuried(r *GroupResolver, children []*nodes.Node, payloadCode string, accept BinFilter) (*bins.Bin, *nodes.Node, int64) {
	for _, child := range children {
		if !child.Enabled || child.NodeTypeCode != protocol.NodeClassLANE {
			continue
		}
		buried, slot, err := r.DB.FindBuriedBin(child.ID, payloadCode)
		if err == nil && buried != nil && accept.accepts(buried) {
			return buried, slot, child.ID
		}
	}
	return nil, nil, 0
}

// scanForBestBin is the shared scanner for all retrieve algorithms. It iterates
// child nodes, finds accessible bins, optionally probes for buried bins, and
// delegates the algorithm-specific decisions to the strategy.
//
// ── DIRECT CHILDREN ONLY, AND THE PLANT HAS A SECOND ANSWER ──────────────────
//
// This walks one level: a LANE child is searched via FindSourceBinInLane, a
// non-synthetic child is a slot and is read directly, and a SYNTHETIC child that
// is not a LANE — a NESTED GROUP — falls through both arms below and is silently
// skipped. Bins inside a group inside this group are invisible here.
//
// The group-scoped EMPTY finders answer the same question differently: they
// recurse the whole subtree (bins.FindEmptyOfTypeInGroup /
// FindEmptyCompatibleInGroup, over nodetree.DescendantsOf), so a nested group's
// slots ARE in scope for them.
//
// So "what is in this group" has two live answers, and which one you get depends
// on whether you are retrieving a LOADED carrier (here, nesting invisible) or
// sourcing an EMPTY one (there, nesting visible). Neither is wrong on its own;
// they have simply never been reconciled.
//
// NESTING SEMANTICS FOR SOURCING IS AN OPEN OWNER RULING. It is not decided, and
// this comment is not deciding it. Maintained groups sidestep the question
// entirely — they are refused at save time unless they are flat — but every
// other group in the plant still lives with the disagreement, so the first
// person who needs nested sourcing to behave one specific way has to get that
// ruling rather than fix whichever site they happened to open.
func (r *GroupResolver) scanForBestBin(group *nodes.Node, payloadCode string, s retrieveStrategy,
	asker reservations.DigAsker, accept BinFilter) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodesUnlocked(group.ID, asker)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	var bestBin *bins.Bin
	var bestNode *nodes.Node
	var bestTime time.Time

	for _, child := range children {
		if !child.Enabled {
			continue
		}

		if child.NodeTypeCode == protocol.NodeClassLANE {
			b, err := r.DB.FindSourceBinInLane(child.ID, payloadCode)
			if err != nil {
				r.dbg("%s: FindSourceBinInLane lane=%s: %v", s.label, child.Name, err)
				continue
			}
			// A bin the caller cannot use is not a candidate. Skipping the LANE
			// rather than looking deeper in it is deliberate: anything behind
			// this bin is buried, and exposing it is an excavation, which is a
			// different decision made somewhere else. See BinFilter.
			if !accept.accepts(b) {
				r.dbg("%s: lane=%s holds bin %d, which this caller cannot use — looking elsewhere",
					s.label, child.Name, b.ID)
				continue
			}

			if s.firstMatch {
				slot, _ := r.DB.GetNode(*b.NodeID)
				return &ResolveResult{Node: slot, Bin: b}, nil
			}

			bTime := binTimestamp(b)
			if bestBin == nil || bTime.Before(bestTime) {
				bestBin = b
				bestTime = bTime
				slot, err := r.DB.GetNode(*b.NodeID)
				if err != nil {
					r.dbg("%s: GetNode for bin %d slot: %v", s.label, b.ID, err)
				}
				bestNode = slot
			}
		} else if !child.IsSynthetic {
			nodeBins, err := r.DB.ListBinsByNode(child.ID)
			if err != nil {
				r.dbg("%s: ListBinsByNode node=%s: %v", s.label, child.Name, err)
				continue
			}
			for _, b := range nodeBins {
				if !isBinAvailableForRetrieve(b, payloadCode) || !accept.accepts(b) {
					continue
				}
				if s.firstMatch {
					return &ResolveResult{Node: child, Bin: b}, nil
				}
				bTime := binTimestamp(b)
				if bestBin == nil || bTime.Before(bestTime) {
					bestBin = b
					bestTime = bTime
					bestNode = child
				}
			}
		}
	}

	if s.checkBuried != nil && !(s.skipBuriedIfAccessible && bestBin != nil) {
		buried, buriedSlot, buriedLaneID := s.checkBuried(r, children, payloadCode, accept)
		if buried != nil && s.shouldTriggerBuried(buried, binTimestamp(buried), bestBin, bestTime) {
			r.dbg("%s: buried bin %d (%s) triggers reshuffle in lane %d",
				s.label, buried.ID, binTimestamp(buried).Format(time.RFC3339), buriedLaneID)
			return nil, &BuriedError{Bin: buried, Slot: buriedSlot, LaneID: buriedLaneID}
		}
	}

	if bestBin != nil {
		return &ResolveResult{Node: bestNode, Bin: bestBin}, nil
	}

	return nil, r.classifyEmptyGroup(group, payloadCode)
}

// binTimestamp returns the effective timestamp for a bin (LoadedAt if set, else CreatedAt).
func binTimestamp(b *bins.Bin) time.Time {
	if b.LoadedAt != nil {
		return *b.LoadedAt
	}
	return b.CreatedAt
}

// classifyEmptyGroup determines whether a group resolution failure is
// structural (permanent) or transient (inventory may arrive).
//
// Intentionally looser than the resolution loop. The loop skips lanes for
// multiple reasons (locked, full, buried, payload mismatch). This helper
// only checks structural capability — not whether bins are available now.
// A false "transient" is safer than a false "structural".
//
// On any DB error during classification, returns transient.
func (r *GroupResolver) classifyEmptyGroup(
	group *nodes.Node, payloadCode string,
) error {
	// Reads the UNFILTERED children on purpose. The scan above walks
	// ListChildNodesUnlocked, which drops dig-held lanes in the query — but a
	// dig-held lane is still a CONFIGURED lane, and this helper answers a
	// configuration question. Classifying off the filtered set would make a
	// group whose only lane is mid-dig report "no enabled child nodes", i.e.
	// StructuralError, i.e. TERMINAL: a dig would kill every order aimed at that
	// group instead of making them wait. The golden suite caught exactly that.
	//
	// The extra read costs nothing where it sits. This runs only after
	// resolution has already failed to find anything, and the payload-capability
	// loop below already issues a query per child.
	children, err := r.DB.ListChildNodes(group.ID)
	if err != nil {
		r.dbg("classifyEmptyGroup: ListChildNodes(%d) error: %v, defaulting to transient", group.ID, err)
		return fmt.Errorf("no bin of requested payload in node group %s", group.Name)
	}

	hasEnabled := false
	for _, child := range children {
		if child.Enabled {
			hasEnabled = true
			break
		}
	}
	if !hasEnabled {
		return &StructuralError{
			Group: group.Name, Payload: payloadCode,
			Reason: "group has no enabled child nodes",
		}
	}

	if payloadCode != "" {
		hasCapable := false
		for _, child := range children {
			if !child.Enabled {
				continue
			}
			payloads, err := r.DB.GetEffectivePayloads(child.ID)
			if err != nil {
				r.dbg("classifyEmptyGroup: GetEffectivePayloads(%d) error: %v, "+
					"defaulting to transient", child.ID, err)
				return fmt.Errorf("no bin of requested payload in node group %s",
					group.Name)
			}
			if len(payloads) == 0 {
				hasCapable = true
				break
			}
			for _, p := range payloads {
				if p.Code == payloadCode {
					hasCapable = true
					break
				}
			}
			if hasCapable {
				break
			}
		}
		if !hasCapable {
			return &StructuralError{
				Group: group.Name, Payload: payloadCode,
				Reason: "no child node accepts this payload type",
			}
		}
	}

	return fmt.Errorf("no bin of requested payload in node group %s", group.Name)
}

// ResolveStore finds the best slot for storing a bin in a node group.
func (r *GroupResolver) ResolveStore(group *nodes.Node, payloadCode string, binTypeID *int64, asker reservations.DigAsker) (*ResolveResult, error) {
	// ── MG4-1: THE LEVEL IS A CAP, AND THIS IS WHERE IT BINDS ───────────────
	//
	// A maintained group holds a declared number of empty carriers. The keeper
	// tops UP to that number; this refuses a store that would push PAST it.
	// Without both halves the level is only a floor, and a group configured to
	// hold four would accept a fifth, a sixth, and every carrier anybody wanted
	// to put down — which is how a press empty bank becomes the place the plant
	// parks its overflow.
	//
	// ResolutionCapacity, which means QUEUE-ON-FULL: the caller parks the push
	// and retries, inheriting the whole park / re-resolve / revert path a full
	// group already has. It is not an error and nothing is cancelled. A push that
	// finds every maintained destination at level is backpressure, which is
	// uncomfortable and correct.
	//
	// EVALUATED PER RESOLVE, NOT PER CHILD. The level is a property of the GROUP
	// — four carriers across it, wherever they stand — so a per-child evaluation
	// would be asking a question the configuration does not answer.
	levels, err := r.DB.ListMaintainLevels(group.ID)
	if err != nil {
		return nil, fmt.Errorf("read declared level for %s: %w", group.Name, err)
	}

	// ── A DECLARED LEVEL SAYS WHAT THE GROUP HOLDS, AND IT IS EMPTIES ───────
	//
	// Every maintain level counts EMPTY carriers of a bin type
	// (CountEmptyBinsOfTypeInGroup) and the keeper tops the group up with empty
	// carriers. Declaring one is therefore a declaration that the group is an
	// empties bank, and a store carrying a payload does not belong in it — it is
	// not merely a poor fit, it is a carrier the keeper's own count cannot see,
	// occupying a slot the keeper will then try to refill.
	//
	// FAIL-SAFE, AND SAID PLAINLY. The path that produced labelled stores into a
	// press empties bank was an ordering race at the operator's CLEAR
	// (shingo-edge operator_bin_ops.go), and that race is closed. So this arm's
	// runtime population may well be empty, and a run that never fires it has
	// proved nothing about it either way — it is certified by a unit test, not by
	// a board read. It is here because the refusal is one line and the failure it
	// catches is a payload-bearing carrier parked in an empties bank, where
	// nothing looks for it.
	//
	// CAPACITY-SHAPED ON PURPOSE, so the maintained group's configured overflow
	// destination catches it at admission and the carrier goes somewhere real
	// instead of parking (LifecycleService.tryOverflow). A group with no overflow
	// configured parks under its own cause, which names the condition rather than
	// reading as a slot shortage.
	//
	// THE KEEPER CANNOT BE REFUSED BY THIS. Its own stores pass an empty
	// payloadCode (maintainer.go), which is what an empties bank is for.
	if payloadCode != "" && len(levels) > 0 {
		return nil, fmt.Errorf("payload %s cannot be stored in empties-only node group %s", payloadCode, group.Name)
	}

	if full, err := r.atDeclaredLevel(group, binTypeID, levels); err != nil {
		return nil, err
	} else if full {
		return nil, fmt.Errorf("no available slot in node group %s", group.Name)
	}

	algo := r.getGroupAlgorithm(group.ID, "store_algorithm", StoreLKND)
	switch algo {
	case StoreDPTH:
		return r.resolveStoreDPTH(group, payloadCode, binTypeID, asker)
	default:
		return r.resolveStoreLKND(group, payloadCode, binTypeID, asker)
	}
}

// ── THE STORE RESOLVERS ARE OWNER-AWARE, AND THE OWNER WAS ALREADY IN HAND ──
//
// Both arms below ask the slot selector through FindStoreSlotInLaneExcluding
// with asker.OrderID, not the blind FindStoreSlotInLane. The asker was already a
// parameter — it was threaded for the dig-lock question — so this costs nothing
// and closes a trap that is documented at the selector and was reachable here.
//
// The blind form's guards are owner-BLIND: an order that already holds a slot in
// a lane matches its own claim, its own reservation AND its own delivery_node, so
// its own slot is invisible to it and the resolver returns the next-best one,
// which is SHALLOWER. Any re-resolve through here therefore walked an order
// forward out of the slot it was holding and toward the mouth, which is the
// motion that builds a lane bubble.
//
// reservations.Anyone carries OrderID 0, and order ids are positive, so every
// call site that has no order in hand keeps exactly the behaviour it had. This
// is the same convention nodes.FindStoreSlotInLaneExcluding documents for its own
// exclude parameter.

// resolveStoreLKND consolidates matching payload codes first, then picks the emptiest slot.
func (r *GroupResolver) resolveStoreLKND(group *nodes.Node, payloadCode string, binTypeID *int64, asker reservations.DigAsker) (*ResolveResult, error) {
	// Lanes that had a usable slot and were refused by the burial guard. Kept so
	// a group that comes up empty can say whether it is FULL or merely CLOSED —
	// two conditions with the same disposition (walk on) and completely different
	// diagnoses. See noteClosedLanes.
	var closedByClaim []string
	children, err := r.DB.ListChildNodesUnlocked(group.ID, asker)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	// Resolve-around (§13.3), off by default: only when the group enables it does
	// the ranker consult each lane's mouth. Read once — a no-op group read when
	// unset, and no per-lane mouth query happens at all when off, so the off path
	// is byte-identical.
	resolveAround := r.DB.GetNodeProperty(group.ID, PropResolveAround) == "on"

	var candidates []storageCandidate

	for _, child := range children {
		if !child.Enabled {
			continue
		}

		if child.NodeTypeCode == protocol.NodeClassLANE {
			// Skip lanes with payload restrictions that don't match
			if !r.payloadAllowedAt(child.ID, payloadCode) {
				continue
			}

			// Skip lanes with bin type restrictions that don't match
			if binTypeID != nil {
				if !r.binTypeAllowed(child.ID, *binTypeID) {
					continue
				}
			}

			slot, err := r.DB.FindStoreSlotInLaneExcluding(child.ID, asker.OrderID)
			if err != nil {
				if errors.Is(err, nodes.ErrLaneClosedByClaim) {
					closedByClaim = append(closedByClaim, child.Name)
				}
				r.dbg("LKND: FindStoreSlotInLane lane=%s: %v", child.Name, err)
				continue // lane is full, or closed to stores by a claim
			}

			count, _ := r.DB.CountBinsInLane(child.ID)
			slots, _ := r.DB.ListLaneSlots(child.ID)

			hasMatch := false
			if payloadCode != "" {
				for _, s := range slots {
					bins, _ := r.DB.ListBinsByNode(s.ID)
					for _, b := range bins {
						if b.PayloadCode == payloadCode {
							hasMatch = true
							break
						}
					}
					if hasMatch {
						break
					}
				}
			}

			// Resolve-around consults the lane's mouth only when the arm is on.
			// A read error is non-fatal — treat the lane as compatible (the arm is
			// opportunistic, never load-bearing; the mouth gate still arbitrates).
			laneCompatible := false
			if resolveAround {
				ok, cErr := r.DB.LaneAcceptsInbound(child.ID)
				if cErr != nil {
					r.dbg("LKND: LaneAcceptsInbound lane=%s: %v", child.Name, cErr)
				}
				laneCompatible = cErr != nil || ok
			}

			candidates = append(candidates, storageCandidate{node: slot, hasMatch: hasMatch, count: count, depth: nodeDepth(child), laneCompatible: laneCompatible})
		} else if !child.IsSynthetic {
			if child.ClaimedBy != nil {
				continue // slot already claimed by another order's dispatch
			}
			// Skip flat slots with payload restrictions that don't match — the
			// SAME refusal the lane branch above makes, and it was missing here.
			// A flat child can carry node_payloads exactly as a lane can, and
			// this branch read only the bin type, so a group of flat slots that
			// declared what each slot holds would accept anything into any of
			// them and then consolidate the wrong payloads together.
			if !r.payloadAllowedAt(child.ID, payloadCode) {
				continue
			}
			count, err := r.DB.CountBinsByNode(child.ID)
			if err != nil {
				r.dbg("LKND: CountBinsByNode node=%s: %v", child.Name, err)
				continue
			}
			inflight, _ := r.DB.CountActiveOrdersByDeliveryNode(child.Name)
			if count+inflight >= 1 {
				continue
			}

			// Skip nodes with bin type restrictions that don't match
			if binTypeID != nil {
				if !r.binTypeAllowed(child.ID, *binTypeID) {
					continue
				}
			}

			hasMatch := false
			if payloadCode != "" {
				bins, _ := r.DB.ListBinsByNode(child.ID)
				for _, b := range bins {
					if b.PayloadCode == payloadCode {
						hasMatch = true
						break
					}
				}
			}

			candidates = append(candidates, storageCandidate{node: child, hasMatch: hasMatch, count: count, depth: nodeDepth(child)})
		}
	}

	if len(candidates) == 0 {
		r.noteClosedLanes(group, closedByClaim)
		return nil, fmt.Errorf("no available slot in node group %s", group.Name)
	}

	return &ResolveResult{Node: bestStorageCandidate(candidates)}, nil
}

// resolveStoreDPTH packs back-to-front regardless of payload. Prefers lanes over direct children.
func (r *GroupResolver) resolveStoreDPTH(group *nodes.Node, payloadCode string, binTypeID *int64, asker reservations.DigAsker) (*ResolveResult, error) {
	// See resolveStoreLKND: lanes the burial guard refused, for the diagnosis on
	// the empty-group path.
	var closedByClaim []string
	children, err := r.DB.ListChildNodesUnlocked(group.ID, asker)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	// First pass: try lanes (deepest empty slot)
	for _, child := range children {
		if !child.Enabled || child.NodeTypeCode != protocol.NodeClassLANE {
			continue
		}

		// Skip lanes with payload restrictions that don't match
		if !r.payloadAllowedAt(child.ID, payloadCode) {
			continue
		}

		// Skip lanes with bin type restrictions that don't match
		if binTypeID != nil {
			if !r.binTypeAllowed(child.ID, *binTypeID) {
				continue
			}
		}

		slot, err := r.DB.FindStoreSlotInLaneExcluding(child.ID, asker.OrderID)
		if err != nil {
			if errors.Is(err, nodes.ErrLaneClosedByClaim) {
				closedByClaim = append(closedByClaim, child.Name)
			}
			r.dbg("DPTH: FindStoreSlotInLane lane=%s: %v", child.Name, err)
			continue // lane is full, or closed to stores by a claim
		}
		return &ResolveResult{Node: slot}, nil
	}

	// Second pass: direct children
	for _, child := range children {
		if !child.Enabled || child.IsSynthetic {
			continue
		}
		if child.ClaimedBy != nil {
			continue // slot already claimed by another order's dispatch
		}

		// Skip flat slots with payload restrictions that don't match — see the
		// same call in resolveStoreLKND's flat branch. DPTH packs back-to-front
		// "regardless of payload", but that is about ORDERING preference, not
		// about ignoring a slot that declared what it holds.
		if !r.payloadAllowedAt(child.ID, payloadCode) {
			continue
		}

		// Skip nodes with bin type restrictions that don't match
		if binTypeID != nil {
			if !r.binTypeAllowed(child.ID, *binTypeID) {
				continue
			}
		}

		count, err := r.DB.CountBinsByNode(child.ID)
		if err != nil {
			r.dbg("DPTH: CountBinsByNode node=%s: %v", child.Name, err)
			continue
		}
		inflight, _ := r.DB.CountActiveOrdersByDeliveryNode(child.Name)
		if count+inflight < 1 {
			return &ResolveResult{Node: child}, nil
		}
	}

	r.noteClosedLanes(group, closedByClaim)
	return nil, fmt.Errorf("no available slot in node group %s", group.Name)
}

// noteClosedLanes reports a group that came up empty with at least one lane
// refused by the burial guard rather than genuinely full.
//
// LOUD, and only on the whole-group failure. A single closed lane is a non-event
// — the scan walks to a sibling and the store lands — so per-lane logging would
// be noise at dispatch volume. A group where every lane is either full or closed
// is the condition that actually costs something: the store parks, and it parks
// for as long as the claims last.
//
// The message stays out of the returned error deliberately. The queue-reason
// classifier reads that message by substring and takes the group name as
// everything after "node group " to end of string (dispatch/complex.go), so any
// suffix here would surface inside the operator sentence as part of the group's
// name. The park keeps its existing shape; this line is the engineer-facing half.
//
// Repeats per resolution attempt, which for a parked store means per scanner
// tick. That is intended: a single line is a lane doing its job, and a stream of
// them is the signal — sustained closure means the claims are not clearing, which
// is a stalled robot or a stalled dig, and that is the incident to chase.
func (r *GroupResolver) noteClosedLanes(group *nodes.Node, closed []string) {
	if len(closed) == 0 {
		return
	}
	log.Printf("store slot: node group %s has no free slot; %d lane(s) closed to stores by a claimed bin deeper in them: %s",
		group.Name, len(closed), strings.Join(closed, ", "))
}

// binTypeAllowed checks whether a bin type is permitted at a node via effective bin types.
// Returns true if no restrictions are set (nil = all allowed) or if the bin type is in the set.
func (r *GroupResolver) binTypeAllowed(nodeID int64, binTypeID int64) bool {
	bts, err := r.DB.GetEffectiveBinTypes(nodeID)
	if err != nil || len(bts) == 0 {
		return true // no restrictions
	}
	for _, bt := range bts {
		if bt.ID == binTypeID {
			return true
		}
	}
	return false
}

// payloadAllowedAt reports whether a store of payloadCode may land at a child
// node — lane or flat slot alike.
//
// ONE SPELLING FOR FOUR SITES. This was written out four times, and only twice:
// both lane branches carried it and neither flat branch did, so a group of flat
// slots that declared its payloads had those declarations ignored for stores
// while an identically-configured laned group honoured them. The asymmetry was
// not a decision anybody recorded; the flat branches simply never grew the
// clause the lane branches have.
//
// AN UNDECLARED NODE ACCEPTS EVERYTHING. node_payloads is a restriction, not a
// whitelist that defaults closed — a node with no rows takes any payload, which
// is every node in every plant that has not been configured otherwise. Reversing
// that would close every default slot in the system.
//
// AN UNTYPED STORE IS NOT REFUSED EITHER. payloadCode == "" is an empty carrier,
// and an empty carrier has no payload to check against a declaration.
//
// A READ FAILURE ALLOWS. GetEffectivePayloads' error is dropped here, matching
// what all four sites did before: refusing on a transient read would close a
// group's slots for as long as the read failed, and the store has other gates
// (bin type, capacity, the mouth) behind this one.
func (r *GroupResolver) payloadAllowedAt(nodeID int64, payloadCode string) bool {
	if payloadCode == "" {
		return true
	}
	declared, _ := r.DB.GetEffectivePayloads(nodeID)
	if len(declared) == 0 {
		return true
	}
	for _, p := range declared {
		if p.Code == payloadCode {
			return true
		}
	}
	return false
}

// atDeclaredLevel reports whether a maintained group is already holding what it
// was told to hold.
//
// ── THE ASYMMETRY, STATED ───────────────────────────────────────────────────
//
// PER-TYPE WHEN THE CALLER KNOWS THE TYPE. A group declaring "four 45x58 and two
// 45x48" that is full of 45x58 must still accept a 45x48 — the levels are
// separate declarations and the cap is per declaration.
//
// GROUP-TOTAL WHEN IT DOES NOT. An untyped store carries no way to say which
// declaration it would fill, so the only honest cap is the sum: refuse when the
// group holds as many empties as every declaration together asked for. That is
// deliberately the LOOSER reading. The alternative — refuse whenever any single
// declaration is met — would turn one satisfied type into a fence against every
// other, and an untyped push has done nothing to deserve that.
//
// The asymmetry is a consequence of what the caller knows, not a policy choice,
// and it disappears the moment MG4-2 gives the untyped path a derived type.
//
// A GROUP WITH NO DECLARED LEVEL IS NOT MAINTAINED, and this is a no-op for it —
// which is every group in every plant today. The read is one query against a
// table that is empty almost everywhere.
//
// A READ FAILURE REFUSES THE STORE rather than allowing it, and that direction
// is chosen: allowing means overfilling a group past a cap somebody set, which
// nothing later corrects, while refusing means the push parks and retries. The
// error propagates rather than being swallowed into "not full" — MG3-1a's rule
// applies here too.
//
// The levels are READ BY THE CALLER and passed in, because ResolveStore needs
// the same list one line earlier to decide whether the group is an empties bank
// at all. One read, two questions.
func (r *GroupResolver) atDeclaredLevel(group *nodes.Node, binTypeID *int64, levels []nodes.MaintainLevel) (bool, error) {
	if len(levels) == 0 {
		return false, nil
	}

	if binTypeID != nil {
		for _, l := range levels {
			if l.BinTypeID != *binTypeID {
				continue
			}
			held, cerr := r.DB.CountEmptyBinsOfTypeInGroup(l.BinTypeCode, group.ID)
			if cerr != nil {
				return false, fmt.Errorf("count %s in %s: %w", l.BinTypeCode, group.Name, cerr)
			}
			return held >= l.Want, nil
		}
		// A type nobody declared. NOT refused: a maintained group is a group with
		// a level on some types, not a group closed to every other. Declaring a
		// level is saying "hold at least these"; it is not saying "and nothing
		// else may ever stand here".
		return false, nil
	}

	want, held := 0, 0
	for _, l := range levels {
		want += l.Want
		n, cerr := r.DB.CountEmptyBinsOfTypeInGroup(l.BinTypeCode, group.ID)
		if cerr != nil {
			return false, fmt.Errorf("count %s in %s: %w", l.BinTypeCode, group.Name, cerr)
		}
		held += n
	}
	return held >= want, nil
}
