package plan

import (
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/domain"
	"shingocore/store/nodes"
)

// push.go — where an unloader's cart goes: the one chooser behind the four doors
// a push into a kanban can meet (intake, the scanner's gate arm and its resolve,
// the cancel-return's store).
//
// A kanban borrows from its sources, and an unloader repays whichever is short. So
// a push from a window of the unloader whose outbound is the kanban is decided on
// Short (residents, asks and comings against the effective want), never on the cap,
// which counts residents alone and would admit a push whose gap an ask already
// covers. In order, stopping at the first that takes it:
//
//	the kanban, while short of the type
//	each source in rank order, while short of the type
//	the type's overflow (the level's, then the kanban's default)
//	nothing: the push parks named at the kanban, visible, re-asked every pass
//
// ONE HOP. A source's sources and an overflow's overflow are not asked: "the cart
// went three groups away from where anybody expected" is worse than a park an
// operator can see. Pin: TestOverflow_AFullOverflowDoesNotChain (the cap's hop), and
// TestKanbanDoor_* here.
//
// NO LEDGER. A source declaring no level of the type is short of nothing and is
// reached only as the type's overflow; the save warns. Two kanbans sharing a source
// both see its count: whoever borrowed, the next cart that fits repays it.
//
// NEVER UNDER A ROBOT, and that is the callers': each door asks only for an order no
// robot has. A person's named drop never comes here either (dispatch.WindowPushShaped).

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

// PushChoice is the chooser's answer. Applies false: the push does not have the
// window's shape (or its group declares no level), the chooser read one statement,
// and the door keeps its own path, which is the cap's. Applies with no Slot: nothing
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
	// RoomOnly says the group the push stays named at is short of the cart and only
	// has no slot free: it parks as a full group does, not as one at its level.
	RoomOnly bool
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
	take := func(l *nodes.DoorLine, why string) (PushChoice, bool) {
		slot := p.slotInLine(l, ask, *typeID)
		if slot == nil {
			return PushChoice{}, false
		}
		return PushChoice{Applies: true, Slot: slot, Into: l.Group, Why: why}, true
	}
	if door.Kanban.Short(state) > 0 {
		if c, ok := take(&door.Kanban, ""); ok {
			return c
		}
	}
	for i := range door.Sources {
		s := &door.Sources[i]
		if s.Short(state) <= 0 {
			continue
		}
		if c, ok := take(s, fmt.Sprintf("%s is the first of %s's sources short of it", s.Group.Name, ask.Group.Name)); ok {
			return c
		}
	}
	if o := door.Overflow; o != nil {
		if c, ok := take(o, fmt.Sprintf("nothing at %s takes it; overflowed to %s", ask.Group.Name, o.Group.Name)); ok {
			return c
		}
	}
	p.debug("plan: nothing takes a %s from %s — it parks named at %s", state, ask.Source, ask.Group.Name)
	return PushChoice{Applies: true, Err: &nothingTakesIt{group: ask.Group.Name}, RoomOnly: door.Kanban.Short(state) > 0}
}

// nothingTakesIt carries ErrAtDeclaredLevel with the capacity sentence every parked
// store's reader matches on.
type nothingTakesIt struct{ group string }

func (e *nothingTakesIt) Error() string        { return "no available slot in node group " + e.group }
func (e *nothingTakesIt) Is(target error) bool { return target == binresolver.ErrAtDeclaredLevel }

// slotInLine is the free slot of one line's group, nil for none: the group's own
// resolve over one read of its children, with the line in hand for its cap. A slot
// that would take a stranger's paper is none here: intake has no order row to take
// it with, and a parked push is asked again.
func (p *Planner) slotInLine(l *nodes.DoorLine, ask PushAsk, typeID int64) *nodes.Node {
	key := PushRefusal{Target: l.Group.ID, Type: typeID}
	if ask.Refused[key] {
		return nil
	}
	if !l.Group.Enabled || l.Group.NodeTypeCode != protocol.NodeClassNGRP {
		return nil
	}
	choice, err := p.overGroup(l.Group, l).ChooseDrop(l.Group, DropNeed{Stated: ask.Stated, Asker: ask.Asker, direct: true})
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
