package plan

import (
	"errors"
	"fmt"

	"shingocore/dispatch/binresolver"
	"shingocore/domain"
	"shingocore/store/nodes"
)

// destination.go — the choice of destination (v4 §2.4): where a carrier bound
// for a group is put down.
//
// THE CHOOSING HALF ONLY (SYNTH §2.2, "the planner chooses; acquiring
// reserves"). Every door that settles a group destination asked the group
// resolver itself and then wrote the answer: intake (the order's first
// destination, and the overflow when the group is at its level), the scanner's
// dropoff settle and its dug-lane re-aim (ReserveStorageDropoff), and the
// plain-path move planner. They ask here now; the delivery_node write, the slot
// reservation and the release of the old one stay with the door that asked, and
// so does every log line that says what the door did with the answer.

// errNoResolver is what a planner built without a group resolver answers. Every
// caller guards on its own resolver first, as it always did, so no production
// path reaches it.
var errNoResolver = errors.New("planner has no group resolver")

// StoreDestination chooses the child of a group (an NGRP, or a LANE) that a
// store puts its carrier on, with the carrier the caller could state: the one
// chooser's answer (ChooseDrop; RULINGS-C2c-run-through §3.2, "every choice of
// a drop in a group goes through ChooseDrop"), asked once.
//
// FOR A DOOR THAT ASKS BEFORE IT HOLDS AN ORDER ROW (intake, the overflow, the
// move door), OR THAT MAY NOT WRITE (the plain transport's decide). Asked for no
// order, the chooser offers no stranger's paper — rank and commitment are an
// order's — so its answer is always a free slot; a paper take offered to a
// caller that cannot make one reads as a full group, and acquiring's settle
// (StoreDrop) makes it on the order's next pass. A caller that can write asks
// StoreDrop, which names the take for it to make.
func (p *Planner) StoreDestination(group *nodes.Node, payloadCode string, stated binresolver.BinTypeStatement,
	asker domain.DigAsker) (*binresolver.ResolveResult, error) {
	defer observe(p.db, "store-destination")()
	choice, err := p.StoreDrop(group, payloadCode, stated, asker)
	if err != nil {
		return nil, err
	}
	if choice.Take != nil {
		// Unreachable for the callers above (no order, no take). A caller that
		// could take paper and asked here would drop the take on the floor, so it
		// is told there is no free slot, in the capacity sentence callers read.
		return nil, fmt.Errorf("no available slot in node group %s", group.Name)
	}
	return &binresolver.ResolveResult{Node: choice.Slot}, nil
}

// StoreDrop is StoreDestination for a caller holding an order row (acquiring's
// settle and its dug-lane re-aim, the plain move's deferred group): the
// chooser's whole answer, so a slot that is a stranger's paper comes back with
// the take named (ruling 2: taken by rank), for the caller's store door to make.
func (p *Planner) StoreDrop(group *nodes.Node, payloadCode string, stated binresolver.BinTypeStatement,
	asker domain.DigAsker) (DropChoice, error) {
	if p.resolver == nil {
		return DropChoice{}, errNoResolver
	}
	return p.ChooseDrop(group, DropNeed{Payload: payloadCode, Stated: stated, Asker: asker})
}

// OverflowDestination chooses where a push goes when its group is at its level:
// the child the group's overflow destination would take, or false. One hop,
// never a chain; a group naming itself, an overflow that does not exist and an
// overflow with no room all answer false, and the push parks.
//
// Moved from dispatch's LifecycleService.tryOverflow (B1); the door keeps the
// log line and the stamp.
//
// stated is asked only once an overflow group is found: working out the carrier
// costs reads (the intake door's binTypeAtIntake), and a group with no overflow
// configured, which is nearly every group, must not pay for them.
func (p *Planner) OverflowDestination(group *nodes.Node, payloadCode string, stated func() binresolver.BinTypeStatement,
	asker domain.DigAsker) (string, bool) {
	defer observe(p.db, "overflow-destination")()
	dest, why := OverflowGroupOf(p.db, group)
	if dest == nil {
		if why != "" {
			p.debug("intake: %s — parking instead", why)
		}
		return "", false
	}
	overflow := dest.Name
	// The overflow destination is the same placement one group over, so it gets
	// the same carrier answer rather than a second, weaker one.
	result, err := p.StoreDestination(dest, payloadCode, stated(), asker)
	if err != nil || result == nil || result.Node == nil {
		p.debug("intake: overflow %s of %s has no room either (%v) — parking",
			overflow, group.Name, err)
		return "", false
	}
	return result.Node.Name, true
}

// OverflowReader is what OverflowGroupOf reads: the group's property and the
// node it names.
type OverflowReader interface {
	GetNodeProperty(nodeID int64, key string) string
	GetNodeByDotName(name string) (*nodes.Node, error)
}

// OverflowGroupOf is a maintained group's configured overflow destination: the
// node it names, or nil. With nil, why is "" when none is configured (a real
// answer, not a missing one) and a sentence when one is configured and
// unusable.
//
// THE LOOKUP HALF OF OverflowDestination, and the only copy. The cancel-return
// policy (engine/cancel_return.go, through dispatch.OverflowGroupOf) follows the
// same overflow for a bin it is returning to a group at its level, and two
// lookups would be two answers the first time either grows a rule.
func OverflowGroupOf(db OverflowReader, group *nodes.Node) (*nodes.Node, string) {
	overflow := db.GetNodeProperty(group.ID, nodes.PropOverflowDestination)
	if overflow == "" {
		return nil, ""
	}
	dest, err := db.GetNodeByDotName(overflow)
	if err != nil || dest == nil {
		return nil, fmt.Sprintf("overflow %q of %s does not resolve (%v)", overflow, group.Name, err)
	}
	if dest.ID == group.ID {
		// A group naming itself is the one-hop rule's degenerate case, and it is
		// worth refusing explicitly: without this it would re-resolve the same
		// full group and return the same capacity error, which reads as a
		// mysterious no-op rather than a configuration mistake.
		return nil, fmt.Sprintf("overflow of %s names itself", group.Name)
	}
	return dest, ""
}

// MaintainedSlot chooses the free slot of a maintained group that one keeper
// ask fills: the group resolver's store mode, asked with the level's carrier
// type (the one door that has always named its carrier) and as nobody's dig.
//
// The group resolver directly, not StoreDestination: a maintained group is an
// NGRP (its config save refuses anything else), so the child listing the
// general resolver makes before delegating would be a read the keeper never
// made. The resolver's per-child count + inflight >= 1 check is what bounds the
// keeper's loop, which is why the keeper births each ask before asking again.
//
// Moved from engine (Maintainer.createAsks' ResolveStore) unchanged (B3).
func (p *Planner) MaintainedSlot(group *nodes.Node, binTypeID int64) (*binresolver.ResolveResult, error) {
	defer observe(p.db, "maintained-slot")()
	return p.groupStore(group, "", binresolver.KnownBinType(binTypeID))
}

// GroupStoreSlot chooses the free child of a node group a carrier is stored in:
// the group resolver's store mode, asked with the carrier the caller states and
// as nobody's dig. The cancel-return's store into a claim's declared source
// asks it (engine storeInto), and reads its refusal: a maintained group at its
// level answers binresolver.ErrAtDeclaredLevel, which is what sends the bin
// one hop to the group's overflow.
func (p *Planner) GroupStoreSlot(group *nodes.Node, payloadCode string, stated binresolver.BinTypeStatement) (*binresolver.ResolveResult, error) {
	defer observe(p.db, "group-store-slot")()
	return p.groupStore(group, payloadCode, stated)
}

// groupStore is MaintainedSlot's and GroupStoreSlot's one ask of the chooser
// (ChooseDrop, RULINGS-C2c-run-through §3.2), as nobody's dig and taking the
// group resolver's store arm directly (see MaintainedSlot for why no child
// listing). Asked for no order, it is offered no paper; its refusal is the
// resolver's own, so ErrAtDeclaredLevel still reads through.
func (p *Planner) groupStore(group *nodes.Node, payloadCode string, stated binresolver.BinTypeStatement) (*binresolver.ResolveResult, error) {
	choice, err := p.ChooseDrop(group, DropNeed{Payload: payloadCode, Stated: stated, Asker: domain.Anyone, direct: true})
	if err != nil {
		return nil, err
	}
	return &binresolver.ResolveResult{Node: choice.Slot}, nil
}
