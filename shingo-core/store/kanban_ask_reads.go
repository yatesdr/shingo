package store

import (
	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// kanban_ask_reads.go — one short kanban's asks in one keeper tick: the slot
// chooser (plan.KanbanSlot) reading through one read of the group, so a burst of
// asks pays for the group once and a free child costs no statement.
//
// GroupPassReads answers the lane questions from its one read of the group. A
// kanban is flat, so the chooser's questions here are the flat-child arm's, per
// free child: how many bins, how many live orders name it, whose slot row, which
// types it admits. Those are answered from the same read and one read of the
// children's types. They live on this view and not on GroupPassReads because a gate
// pass notes what a release takes and nothing it does makes a flat child's count
// true for the next waiter; here the keeper is the only asker and it notes what it
// asks (NoteAsked). A fresh one per kanban per tick: nothing outlives the tick.

// KanbanAskReads is the view; see above.
type KanbanAskReads struct {
	*GroupPassReads
	residents  map[string]int
	types      map[int64][]*bins.BinType
	typesRead  bool
	groupProps map[string]string
}

// KanbanAskReads is a fresh view over a kanban, its levels and residents already
// in the tick's hand so the chooser's cap asks no statement for them.
func (db *DB) KanbanAskReads(kanbanID int64, levels []KanbanLevel, residents map[string]int) *KanbanAskReads {
	pass := db.GroupPassReads(kanbanID)
	pass.levels[kanbanID] = levels
	return &KanbanAskReads{GroupPassReads: pass, residents: residents}
}

// CountEmptyBinsOfTypeInGroup answers the cap's count at no height said from the
// tick's residents. No ask in the tick moves a resident.
func (r *KanbanAskReads) CountEmptyBinsOfTypeInGroup(code string, groupNodeID int64, h domain.Height) (int, error) {
	if groupNodeID == r.groupID && h == (domain.Height{}) {
		if n, ok := r.residents[code]; ok {
			return n, nil
		}
	}
	return r.DB.CountEmptyBinsOfTypeInGroup(code, groupNodeID, h)
}

// GetNodeProperty reads the group's properties once, whichever is asked first.
func (r *KanbanAskReads) GetNodeProperty(nodeID int64, key string) string {
	if nodeID != r.groupID {
		return r.GroupPassReads.GetNodeProperty(nodeID, key)
	}
	if r.groupProps == nil {
		r.groupProps = map[string]string{}
		all, err := nodes.ListProperties(r.DB.DB, nodeID)
		if err != nil {
			return r.GroupPassReads.GetNodeProperty(nodeID, key)
		}
		for _, p := range all {
			r.groupProps[p.Key] = p.Value
		}
	}
	return r.groupProps[key]
}

// BinTypeBareOf answers nothing and reads nothing: what this view is asked to place is a level's
// type or a door's cart, and neither is ever a bare marker (plan.Planner.ChoosePush turns a bare
// cart away before it reads). A slot's list then says no with no read of the type.
func (r *KanbanAskReads) BinTypeBareOf(int64) (*int64, error) { return nil, nil }

// CountBinsByNode answers a child from the group's one read.
func (r *KanbanAskReads) CountBinsByNode(nodeID int64) (int, error) {
	if s := r.slotState(); s != nil {
		if n, ok := s.BinsAt(nodeID); ok {
			return n, nil
		}
	}
	return r.DB.CountBinsByNode(nodeID)
}

// CountActiveOrdersByDeliveryNodeExcluding answers a child from the group's one
// read, by its name.
func (r *KanbanAskReads) CountActiveOrdersByDeliveryNodeExcluding(nodeName string, excludeID int64) (int, error) {
	if s := r.slotState(); s != nil {
		if n, ok := s.LiveNaming(nodeName, excludeID); ok {
			return n, nil
		}
	}
	return r.DB.CountActiveOrdersByDeliveryNodeExcluding(nodeName, excludeID)
}

// SlotSpokenForByStranger answers a child from the group's one read.
func (r *KanbanAskReads) SlotSpokenForByStranger(nodeID, owner int64) (bool, error) {
	if s := r.slotState(); s != nil {
		if spoken, ok := s.SpokenForByStranger(nodeID, owner); ok {
			return spoken, nil
		}
	}
	return r.DB.SlotSpokenForByStranger(nodeID, owner)
}

// GetEffectiveBinTypes answers a child of the group from one read of the
// children's types (bins.EffectiveTypesOfChildren).
func (r *KanbanAskReads) GetEffectiveBinTypes(nodeID int64) ([]*bins.BinType, error) {
	s := r.slotState()
	if s == nil {
		return r.GroupPassReads.GetEffectiveBinTypes(nodeID)
	}
	if !s.HasLane(nodeID) {
		return r.GroupPassReads.GetEffectiveBinTypes(nodeID)
	}
	if !r.typesRead {
		types, err := bins.EffectiveTypesOfChildren(r.DB.DB, r.groupID)
		if err != nil {
			return r.GroupPassReads.GetEffectiveBinTypes(nodeID)
		}
		r.types, r.typesRead = types, true
	}
	return r.types[nodeID], nil
}

// NoteAsked writes an ask born on a child into the group's read, so the next
// choice moves on with no statement.
func (r *KanbanAskReads) NoteAsked(slotID, orderID int64) {
	if r.state != nil {
		r.state.NoteNaming(slotID, orderID)
	}
}

// NoteSlotTaken writes a child another order took since the chooser read it free.
func (r *KanbanAskReads) NoteSlotTaken(slotID int64) {
	r.NoteAsked(slotID, slotTakenByStranger)
}

// slotTakenByStranger stands for the order that took a slot when its id is not in
// hand: order ids are positive.
const slotTakenByStranger = -1
