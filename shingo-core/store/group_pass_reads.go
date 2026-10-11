package store

import (
	"fmt"
	"log"

	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

// GroupPassReads is one group pass's view of the store for its releases' chooser: the
// store itself, with the reads that no release inside the pass can change
// memoised — the group's properties and declared level, each lane's and slot's
// declared payloads and carrier types, the bins on a node, and an order's
// carrier — and the group's SLOT STATE read once (nodes.GroupSlotState).
//
// THE GROUP'S SLOT STATE ONCE PER EVENT (RULINGS-C2c-run-through §3.3: "the
// per-pass cache serves every waiter after the first at 0 reads"). The store
// selector's questions name the asker (its own claim, row, delivery and dig are
// exempt), so they used to be read per waiter: the group's children
// (ListChildNodesUnlocked), each lane's store slot (one StoreSlotsInGroup per
// owner), its slots and bin count, and the paper on it (PaperSlotsInGroup).
// They are answered from one read of the group with every party named, per
// asker in memory. A release inside the pass takes a slot and moves no bin; it
// is written into the read (NoteReleased, NoteGivenBack), so the next waiter
// sees it with no statement.
//
// A fresh one per pass, so nothing outlives the mutex it was read under.
type GroupPassReads struct {
	*DB
	groupID   int64
	state     *nodes.GroupSlotState
	stateRead bool
	props     map[passPropKey]string
	levels    map[int64][]nodes.MaintainLevel
	payloads  map[int64][]*payloads.Payload
	binTypes  map[int64][]*bins.BinType
	bare      map[int64]*int64
	binsAt    map[int64][]*bins.Bin
	orderType map[int64]*int64
}

type passPropKey struct {
	node int64
	key  string
}

// GroupPassReads is a fresh read view for one gate pass over groupID (dispatch
// evaluateGroupReleasesPass); see the type.
func (db *DB) GroupPassReads(groupID int64) *GroupPassReads {
	return &GroupPassReads{DB: db, groupID: groupID, props: map[passPropKey]string{}, levels: map[int64][]nodes.MaintainLevel{},
		payloads: map[int64][]*payloads.Payload{}, binTypes: map[int64][]*bins.BinType{}, bare: map[int64]*int64{},
		binsAt: map[int64][]*bins.Bin{}, orderType: map[int64]*int64{}}
}

// slotState is the group's one read, taken on the first ask that needs it. A
// failed read is nil, and every question then asks the store as it always did.
func (r *GroupPassReads) slotState() *nodes.GroupSlotState {
	if !r.stateRead {
		r.stateRead = true
		s, err := nodes.ReadGroupSlotState(r.DB.DB, r.groupID)
		if err != nil {
			log.Printf("lane gate: group %d's slot state unreadable (%v) — asking per waiter", r.groupID, err)
			return nil
		}
		r.state = s
	}
	return r.state
}

// copied hands out copies of the read's nodes, so a caller that edits a node it
// was given edits nothing the next waiter is answered from.
func copied(in []*nodes.Node) []*nodes.Node {
	out := make([]*nodes.Node, len(in))
	for i, n := range in {
		c := *n
		out[i] = &c
	}
	return out
}

// GetNodeProperty memoises a node property for the pass.
func (r *GroupPassReads) GetNodeProperty(nodeID int64, key string) string {
	k := passPropKey{nodeID, key}
	if v, ok := r.props[k]; ok {
		return v
	}
	v := r.DB.GetNodeProperty(nodeID, key)
	r.props[k] = v
	return v
}

// ListMaintainLevels memoises a group's declared levels for the pass.
func (r *GroupPassReads) ListMaintainLevels(groupNodeID int64) ([]nodes.MaintainLevel, error) {
	if v, ok := r.levels[groupNodeID]; ok {
		return v, nil
	}
	v, err := r.DB.ListMaintainLevels(groupNodeID)
	if err == nil {
		r.levels[groupNodeID] = v
	}
	return v, err
}

// GetEffectivePayloads memoises a node's declared payloads for the pass.
func (r *GroupPassReads) GetEffectivePayloads(nodeID int64) ([]*payloads.Payload, error) {
	if v, ok := r.payloads[nodeID]; ok {
		return v, nil
	}
	v, err := r.DB.GetEffectivePayloads(nodeID)
	if err == nil {
		r.payloads[nodeID] = v
	}
	return v, err
}

// GetEffectiveBinTypes memoises a node's declared carrier types for the pass.
func (r *GroupPassReads) GetEffectiveBinTypes(nodeID int64) ([]*bins.BinType, error) {
	if v, ok := r.binTypes[nodeID]; ok {
		return v, nil
	}
	v, err := r.DB.GetEffectiveBinTypes(nodeID)
	if err == nil {
		r.binTypes[nodeID] = v
	}
	return v, err
}

// BinTypeBareOf memoises a bare marker's carrier for the pass.
func (r *GroupPassReads) BinTypeBareOf(typeID int64) (*int64, error) {
	if v, ok := r.bare[typeID]; ok {
		return v, nil
	}
	v, err := r.DB.BinTypeBareOf(typeID)
	if err == nil {
		r.bare[typeID] = v
	}
	return v, err
}

// ListChildNodesUnlocked answers the pass's group from its one read: the
// children no dig keeps asker out of. Any other node asks the store.
func (r *GroupPassReads) ListChildNodesUnlocked(parentID int64, asker reservations.DigAsker) ([]*nodes.Node, error) {
	if parentID == r.groupID {
		if s := r.slotState(); s != nil {
			return copied(s.ChildrenUnlocked(asker)), nil
		}
	}
	return r.DB.ListChildNodesUnlocked(parentID, asker)
}

// ListLaneSlots answers a lane of the pass's group from its one read.
func (r *GroupPassReads) ListLaneSlots(laneID int64) ([]*nodes.Node, error) {
	if s := r.slotState(); s != nil && s.HasLane(laneID) {
		return copied(s.LaneSlots(laneID)), nil
	}
	return r.DB.ListLaneSlots(laneID)
}

// CountBinsInLane answers a lane of the pass's group from its one read.
func (r *GroupPassReads) CountBinsInLane(laneID int64) (int, error) {
	if s := r.slotState(); s != nil && s.HasLane(laneID) {
		return s.LaneBins(laneID), nil
	}
	return r.DB.CountBinsInLane(laneID)
}

// ListBinsByNode memoises the bins on a node for the pass.
func (r *GroupPassReads) ListBinsByNode(nodeID int64) ([]*bins.Bin, error) {
	if v, ok := r.binsAt[nodeID]; ok {
		return v, nil
	}
	v, err := r.DB.ListBinsByNode(nodeID)
	if err == nil {
		r.binsAt[nodeID] = v
	}
	return v, err
}

// BinTypeForOrder memoises an order's carrier for the pass.
func (r *GroupPassReads) BinTypeForOrder(orderID int64) (*int64, error) {
	if v, ok := r.orderType[orderID]; ok {
		return v, nil
	}
	v, err := r.DB.BinTypeForOrder(orderID)
	if err == nil {
		r.orderType[orderID] = v
	}
	return v, err
}

// FindStoreSlotInLaneExcluding answers the store selector for a lane of the
// pass's group from its one read, for this owner: the answer
// nodes.FindStoreSlotInLaneExcluding gives, miss diagnosis included
// (ErrLaneClosedByClaim). A lane outside the group asks the store.
func (r *GroupPassReads) FindStoreSlotInLaneExcluding(laneID, owner int64) (*nodes.Node, error) {
	s := r.slotState()
	if s == nil || !s.HasLane(laneID) {
		return r.DB.FindStoreSlotInLaneExcluding(laneID, owner)
	}
	slot, open := s.StoreSlot(laneID, owner)
	if slot == nil {
		if open {
			return nil, fmt.Errorf("no empty slot in lane %d: %w", laneID, nodes.ErrLaneClosedByClaim)
		}
		return nil, fmt.Errorf("no empty slot in lane %d", laneID)
	}
	c := *slot
	return &c, nil
}

// PaperSlotsInGroup answers the pass's group from its one read.
func (r *GroupPassReads) PaperSlotsInGroup(groupID, owner int64) ([]nodes.PaperSlot, error) {
	if groupID == r.groupID {
		if s := r.slotState(); s != nil {
			out := s.PaperSlots(owner)
			for i := range out {
				c := *out[i].Slot
				out[i].Slot = &c
			}
			return out, nil
		}
	}
	return r.DB.PaperSlotsInGroup(groupID, owner)
}

// NoteReleased writes a release the pass made into the group's read: orderID
// took slotID as armor and let go of leftID; final, its drop is the plan's last
// (nodes.GroupSlotState.NoteReleased).
func (r *GroupPassReads) NoteReleased(orderID, slotID, leftID int64, final bool) {
	if r != nil && r.state != nil {
		r.state.NoteReleased(orderID, slotID, leftID, final)
	}
}

// NoteGivenBack writes a release's give-back into the group's read.
func (r *GroupPassReads) NoteGivenBack(orderID, slotID int64) {
	if r != nil && r.state != nil {
		r.state.NoteGivenBack(orderID, slotID)
	}
}
