package plan

import (
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/domain"
	"shingocore/store/nodes"
)

// push.go — where a cart of a kanban's loop goes: the one chooser behind every door a push
// into a kanban can meet (intake, the scanner's gate arm and its resolve, the cancel-return's
// store, the lift's divert).
//
// THE SHAPE IS WIDER THAN AN UNLOADER. A pusher is any node the door's read calls the loop's: a
// window of the kanban's own unloader, a position it supports, a slot of its own. Another kind
// of pusher is one more arm of that read.
//
// A LOOP CART ALWAYS LANDS. The level governs the keeper's asks, never a return: this is what
// keeps a changeover off the family from holding every window. In order, stopping at the first
// that takes it:
//
//	the kanban, whenever a slot admits the cart's type
//	each source in rank order, while short of the type
//	nothing: the push parks named at the kanban as a full group does, visible, re-asked
//	every pass, and its cause names the family
//
// A kanban has no overflow: a sized loop always has a slot, and pool past room is a warning at
// its save. A cart from outside the loop answers no door here and keeps the cap.
//
// ONE FIT. The door's read says which of the lines has a slot for the cart, so a line with none
// costs nothing to pass, and the door pays for one fit wherever the cart goes.
//
// ONE HOP. A source's sources are not asked: "the cart went three groups away from where
// anybody expected" is worse than a park an operator can see.
//
// NO LEDGER. A source declaring no level of the type is short of nothing. Two kanbans sharing a
// source both see its count: whoever borrowed, the next cart that fits repays it.
//
// UNDER A ROBOT ONLY AT A RELEASE. Intake, the scanner and the cancel-return ask for an
// order no robot has; the lift's divert (dispatch's placeByContentsAtRelease) asks for an
// order whose robot stands at the line, and the drop it is given is moved with the rest of
// that route planned again (dispatch's repoint). A person's named drop never comes here
// (dispatch.WindowPushShaped).

// PushDB is what the chooser reads beyond the resolver's store: the door's one
// statement (store.DB.ReadKanbanDoor).
type PushDB interface {
	ReadKanbanDoor(kanbanID, binTypeID int64, source string, self int64) (*nodes.KanbanDoor, error)
}

// PushRefusals is a pass's memo of the (group, type) resolves that found no slot,
// shared by every push asked in it: a refusal for one type says nothing about
// another's. Nil asks every time.
type PushRefusals map[PushRefusal]bool

// PushRefusal keys the memo: the group a resolve was asked of, and the type.
type PushRefusal struct {
	Target int64
	Type   int64
}

// PushAsk is one push at a door.
type PushAsk struct {
	// Group is the group the push names.
	Group *nodes.Node
	// Source is the node the push lifts from, as the order names it. For a cancelled
	// push's return, the cancelled order's.
	Source string
	// Stated is the cart.
	Stated binresolver.BinTypeStatement
	// Asker is the order asking, domain.Anyone for one with no row yet.
	Asker domain.DigAsker
	// Refused is the pass's memo; nil outside a pass.
	Refused PushRefusals
}

// PushChoice is the chooser's answer. Applies false: the source is no part of the kanban's loop
// (or its group declares no level), the chooser read one statement, and the door keeps its own
// path, which is the cap's. Applies with no Slot: nothing
// takes the cart now, and it parks named at the group.
type PushChoice struct {
	Applies bool
	Slot    *nodes.Node
	// Into is the group the slot stands in.
	Into *nodes.Node
	// Why is the door's sentence for a hop away from the group named, "" for none.
	Why string
	// Err says why nothing took it: the capacity sentence a parked store reads.
	Err error
	// RoomOnly says the push parks because no slot admits it: as a full group does, never at
	// a level. Every park here is one: a loop cart is not refused at its level.
	RoomOnly bool
	// Family is the cart's family, named by the park.
	Family string
}

// ChoosePush answers one push; see the file's head. A failed read parks the push
// rather than guessing a place for it: the push is asked again on the next pass.
func (p *Planner) ChoosePush(ask PushAsk) PushChoice {
	defer observe(p.db, "choose-push")()
	typeID := ask.Stated.TypeID()
	state := ask.Stated.State()
	if typeID == nil || ask.Source == "" || state == protocol.CarrierLoaded || state == protocol.CarrierBare {
		return PushChoice{}
	}
	door, err := p.db.ReadKanbanDoor(ask.Group.ID, *typeID, ask.Source, ask.Asker.OrderID)
	if err != nil {
		p.debug("plan: door of %s unreadable (%v) — the push parks", ask.Group.Name, err)
		return PushChoice{Applies: true, Err: fmt.Errorf("door of %s unreadable: %w", ask.Group.Name, err)}
	}
	if door == nil {
		return PushChoice{}
	}
	take := func(l *nodes.DoorLine, loop bool, why string) (PushChoice, bool) {
		if l.FreeSlots == 0 {
			return PushChoice{}, false
		}
		slot := p.slotInLine(l, ask, *typeID, loop)
		if slot == nil {
			return PushChoice{}, false
		}
		return PushChoice{Applies: true, Slot: slot, Into: l.Group, Why: why}, true
	}
	if c, ok := take(&door.Kanban, door.Loop, ""); ok {
		return c
	}
	for i := range door.Sources {
		s := &door.Sources[i]
		if s.Short(state) <= 0 {
			continue
		}
		if c, ok := take(s, false, fmt.Sprintf("%s has no slot for it; %s is the first of its sources short of it", door.Kanban.Group.Name, s.Group.Name)); ok {
			return c
		}
	}
	p.debug("plan: no slot admits a %s from %s — it parks named at %s", state, ask.Source, door.Kanban.Group.Name)
	return PushChoice{Applies: true, Err: fmt.Errorf("no slot of %s admits a cart of %s", door.Kanban.Group.Name, door.Family), RoomOnly: true, Family: door.Family}
}

// slotInLine is the free slot of one line's group, nil for none: the group's own
// resolve over one read of its children, with the line in hand for its cap. A slot
// that would take a stranger's paper is none here: intake has no order row to take
// it with, and a parked push is asked again.
func (p *Planner) slotInLine(l *nodes.DoorLine, ask PushAsk, typeID int64, loop bool) *nodes.Node {
	key := PushRefusal{Target: l.Group.ID, Type: typeID}
	if ask.Refused[key] {
		return nil
	}
	if !l.Group.Enabled || l.Group.NodeTypeCode != protocol.NodeClassNGRP {
		return nil
	}
	choice, err := p.overGroup(l.Group, l).ChooseDrop(l.Group, DropNeed{Stated: ask.Stated, Asker: ask.Asker, Loop: loop, direct: true})
	if err != nil || choice.Take != nil || choice.Slot == nil {
		if ask.Refused != nil {
			ask.Refused[key] = true
		}
		p.debug("plan: %s has no slot for the push (%v)", l.Group.Name, err)
		return nil
	}
	return choice.Slot
}

// overGroup is the planner reading one group through one read of its children, when
// it was given a way to take one (SetSlotViews). A nil line holds nothing for the
// group's cap, and the view reads it.
func (p *Planner) overGroup(group *nodes.Node, l *nodes.DoorLine) *Planner {
	if p.slotViews == nil {
		return p
	}
	return p.Over(p.slotViews(group.ID, l))
}
