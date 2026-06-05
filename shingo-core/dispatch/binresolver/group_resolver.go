package binresolver

import (
	"errors"
	"fmt"
	"time"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/nodes"
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
	LaneLock *LaneLock
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
func (r *GroupResolver) ResolveRetrieve(group *nodes.Node, payloadCode string) (*ResolveResult, error) {
	algo := r.getGroupAlgorithm(group.ID, "retrieve_algorithm", RetrieveFIFO)
	strategy := retrieveStrategies[algo]
	return r.scanForBestBin(group, payloadCode, strategy)
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
	checkBuried            func(r *GroupResolver, children []*nodes.Node, payloadCode string) (buried *bins.Bin, slot *nodes.Node, laneID int64)
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
func checkOldestBuried(r *GroupResolver, children []*nodes.Node, payloadCode string) (*bins.Bin, *nodes.Node, int64) {
	var best *bins.Bin
	var bestSlot *nodes.Node
	var bestLaneID int64
	var bestTime time.Time

	for _, child := range children {
		if !child.Enabled || child.NodeTypeCode != protocol.NodeClassLANE {
			continue
		}
		if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
			continue
		}
		buried, slot, err := r.DB.FindOldestBuriedBin(child.ID, payloadCode)
		if err != nil || buried == nil {
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
func checkShallowestBuried(r *GroupResolver, children []*nodes.Node, payloadCode string) (*bins.Bin, *nodes.Node, int64) {
	for _, child := range children {
		if !child.Enabled || child.NodeTypeCode != protocol.NodeClassLANE {
			continue
		}
		if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
			continue
		}
		buried, slot, err := r.DB.FindBuriedBin(child.ID, payloadCode)
		if err == nil && buried != nil {
			return buried, slot, child.ID
		}
	}
	return nil, nil, 0
}

// scanForBestBin is the shared scanner for all retrieve algorithms. It iterates
// child nodes, finds accessible bins, optionally probes for buried bins, and
// delegates the algorithm-specific decisions to the strategy.
func (r *GroupResolver) scanForBestBin(group *nodes.Node, payloadCode string, s retrieveStrategy) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(group.ID)
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
			if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
				continue
			}

			b, err := r.DB.FindSourceBinInLane(child.ID, payloadCode)
			if err != nil {
				r.dbg("%s: FindSourceBinInLane lane=%s: %v", s.label, child.Name, err)
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
				if !isBinAvailableForRetrieve(b, payloadCode) {
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
		buried, buriedSlot, buriedLaneID := s.checkBuried(r, children, payloadCode)
		if buried != nil && s.shouldTriggerBuried(buried, binTimestamp(buried), bestBin, bestTime) {
			r.dbg("%s: buried bin %d (%s) triggers reshuffle in lane %d",
				s.label, buried.ID, binTimestamp(buried).Format(time.RFC3339), buriedLaneID)
			return nil, &BuriedError{Bin: buried, Slot: buriedSlot, LaneID: buriedLaneID}
		}
	}

	if bestBin != nil {
		return &ResolveResult{Node: bestNode, Bin: bestBin}, nil
	}

	return nil, r.classifyEmptyGroup(group, children, payloadCode)
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
	group *nodes.Node, children []*nodes.Node, payloadCode string,
) error {
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
func (r *GroupResolver) ResolveStore(group *nodes.Node, payloadCode string, binTypeID *int64) (*ResolveResult, error) {
	algo := r.getGroupAlgorithm(group.ID, "store_algorithm", StoreLKND)
	switch algo {
	case StoreDPTH:
		return r.resolveStoreDPTH(group, payloadCode, binTypeID)
	default:
		return r.resolveStoreLKND(group, payloadCode, binTypeID)
	}
}

// resolveStoreLKND consolidates matching payload codes first, then picks the emptiest slot.
func (r *GroupResolver) resolveStoreLKND(group *nodes.Node, payloadCode string, binTypeID *int64) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(group.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	var candidates []storageCandidate

	for _, child := range children {
		if !child.Enabled {
			continue
		}

		if child.NodeTypeCode == protocol.NodeClassLANE {
			if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
				continue
			}

			// Skip lanes with payload restrictions that don't match
			if payloadCode != "" {
				lanePayloads, _ := r.DB.GetEffectivePayloads(child.ID)
				if len(lanePayloads) > 0 {
					match := false
					for _, lp := range lanePayloads {
						if lp.Code == payloadCode {
							match = true
							break
						}
					}
					if !match {
						continue
					}
				}
			}

			// Skip lanes with bin type restrictions that don't match
			if binTypeID != nil {
				if !r.binTypeAllowed(child.ID, *binTypeID) {
					continue
				}
			}

			slot, err := r.DB.FindStoreSlotInLane(child.ID)
			if err != nil {
				r.dbg("LKND: FindStoreSlotInLane lane=%s: %v", child.Name, err)
				continue // lane is full
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

			candidates = append(candidates, storageCandidate{node: slot, hasMatch: hasMatch, count: count})
		} else if !child.IsSynthetic {
			if child.ClaimedBy != nil {
				continue // slot already claimed by another order's dispatch
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

			candidates = append(candidates, storageCandidate{node: child, hasMatch: hasMatch, count: count})
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no available slot in node group %s", group.Name)
	}

	return &ResolveResult{Node: bestStorageCandidate(candidates)}, nil
}

// resolveStoreDPTH packs back-to-front regardless of payload. Prefers lanes over direct children.
func (r *GroupResolver) resolveStoreDPTH(group *nodes.Node, payloadCode string, binTypeID *int64) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(group.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", group.Name, err)
	}

	// First pass: try lanes (deepest empty slot)
	for _, child := range children {
		if !child.Enabled || child.NodeTypeCode != protocol.NodeClassLANE {
			continue
		}
		if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) {
			continue
		}

		// Skip lanes with payload restrictions that don't match
		if payloadCode != "" {
			lanePayloads, _ := r.DB.GetEffectivePayloads(child.ID)
			if len(lanePayloads) > 0 {
				match := false
				for _, lp := range lanePayloads {
					if lp.Code == payloadCode {
						match = true
						break
					}
				}
				if !match {
					continue
				}
			}
		}

		// Skip lanes with bin type restrictions that don't match
		if binTypeID != nil {
			if !r.binTypeAllowed(child.ID, *binTypeID) {
				continue
			}
		}

		slot, err := r.DB.FindStoreSlotInLane(child.ID)
		if err != nil {
			r.dbg("DPTH: FindStoreSlotInLane lane=%s: %v", child.Name, err)
			continue // lane is full
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

	return nil, fmt.Errorf("no available slot in node group %s", group.Name)
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
