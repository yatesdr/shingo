package plan

import (
	"errors"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// move.go — the bin move's plan (v4 §2.5; B2).
//
// A person moves one bin two ways. By label they name the bin, and the planner
// VALIDATES the plan they gave (ValidateMove). By node they name where to take a
// bin from, and the planner CHOOSES one (ChooseMove). Either way the answer is a
// Plan the builder reads: the bin, the node it stands on, the node it goes to
// (a group's chosen child), whether it takes a soft hold, or the rule that said
// no.
//
// The door keeps what is about the request rather than the plant: that the
// names exist, that a label names a bin at a node, that source and destination
// differ. Everything here is a fact about the plant, read, never written.

// RefusalRule names which rule refused a move.
type RefusalRule int

const (
	// RefusedDestDisabled: the destination is disabled (a dead node). Ruling,
	// v4 §2.5: "dead node dead".
	RefusedDestDisabled RefusalRule = iota + 1
	// RefusedCarrier: the destination does not take this bin's carrier (its
	// Allowed Bin Types, or no child of the group does).
	RefusedCarrier
	// RefusedDestFull: the capacity gate is blocked; Capacity says how.
	RefusedDestFull
	// RefusedClaimed: an order holds the bin HARD. Nobody takes a hard hold
	// (v4 §2.7); Holder names it.
	RefusedClaimed
	// RefusedLocked: the bin is locked for active handling.
	RefusedLocked
	// RefusedNotAtSource: the bin is not at the node the plan names.
	RefusedNotAtSource
	// RefusedNoFreeBin: auto, and no bin at the source is pickable.
	RefusedNoFreeBin
	// RefusedUnreadable: a read the verdict needs failed; Err carries it.
	RefusedUnreadable
)

// Refusal is a move plan's "not satisfiable", for a door with a person waiting.
type Refusal struct {
	Rule     RefusalRule
	Holder   int64           // RefusedClaimed
	Capacity CapacityVerdict // RefusedDestFull
	Err      error           // RefusedUnreadable, and the resolver's answer on RefusedCarrier
}

// ValidateMove checks a person's fully specified move (Q1, owner 2026-09-26):
// location, the carrier rule, the destination's capacity and the books. The
// person is authoritative over soft holds and never over a robot, so a bin
// another order has CLAIMED refuses (naming the holder); one another order has
// only pending-reserved is the issue's to take, decided by who asked
// (dispatch.Need.By). A held status (quality_hold and the like) is not a
// refusal: a person moving a held bin, to quarantine say, is the person
// deciding.
func (p *Planner) ValidateMove(g Given) Plan {
	defer observe(p.db, "move")()
	b := g.Bin
	if b.ClaimedBy != nil {
		return refused(Refusal{Rule: RefusedClaimed, Holder: *b.ClaimedBy})
	}
	if b.NodeID == nil || *b.NodeID != g.Source.ID {
		return refused(Refusal{Rule: RefusedNotAtSource})
	}
	if b.Locked {
		return refused(Refusal{Rule: RefusedLocked})
	}
	return p.moveDestination(b, g.Source, g.Dest, nil)
}

// ChooseMove picks the bin an auto move takes off source: the first, in the
// node's listing order, that PickableAt admits for anyone (Q1: "the automatic
// pick keeps PickableAt"). PickableAt is stricter than the pickFreeBinAt it
// retires: beyond claimed and pending-reserved it skips a locked bin, a status
// that blocks pickup and a synthetic node. A bin the destination's carrier rule
// refuses is skipped too, so the choice is one the destination will take.
func (p *Planner) ChooseMove(source, dest *nodes.Node) Plan {
	defer observe(p.db, "move")()
	if !dest.Enabled {
		return refused(Refusal{Rule: RefusedDestDisabled})
	}
	candidates, err := p.db.ListBinsByNode(source.ID)
	if err != nil {
		return refused(Refusal{Rule: RefusedUnreadable, Err: err})
	}
	admits, err := p.carrierRule(dest)
	if err != nil {
		return refused(Refusal{Rule: RefusedUnreadable, Err: err})
	}
	for _, b := range candidates {
		if binresolver.BinUnavailableReasonFor(b, "", domain.BinTypeRule{}, domain.Anyone) != "" {
			continue
		}
		ok, aerr := admits(b)
		if aerr != nil {
			return refused(Refusal{Rule: RefusedUnreadable, Err: aerr})
		}
		if !ok {
			continue
		}
		return p.moveDestination(b, source, dest, admits)
	}
	return refused(Refusal{Rule: RefusedNoFreeBin})
}

// moveDestination is the half both entries share: the destination is alive,
// has room, takes this carrier, and (a group) which child it is.
//
// Capacity is asked excluding no order, as the preview the door asked before B2
// did: there is no order yet to exclude. admits is the destination's carrier
// rule when the caller already read it (ChooseMove filtered by it), so it is not
// read twice; nil reads it here.
func (p *Planner) moveDestination(b *bins.Bin, source, dest *nodes.Node, admits func(*bins.Bin) (bool, error)) Plan {
	if !dest.Enabled {
		return refused(Refusal{Rule: RefusedDestDisabled})
	}
	if blocked, v := DropoffCapacityForType(p.db, dest.Name, 0, binresolver.KnownCarrier(b)); blocked {
		return refused(Refusal{Rule: RefusedDestFull, Capacity: v})
	}
	pl := Plan{Outcome: OutcomeFound, Bin: b, Node: source, Dest: dest}
	if dest.IsSynthetic {
		// A group: the child the carrier is put down on is chosen here, with the
		// carrier stated, so the resolver's own fence read is the carrier rule.
		res, err := p.StoreDestination(dest, b.PayloadCode, binresolver.KnownCarrier(b), domain.Anyone)
		if err != nil || res == nil || res.Node == nil {
			if err == nil {
				err = errors.New("the group resolver chose no child")
			}
			return refused(Refusal{Rule: RefusedCarrier, Err: err})
		}
		pl.Dest = res.Node
		return pl
	}
	if admits == nil {
		var err error
		if admits, err = p.carrierRule(dest); err != nil {
			return refused(Refusal{Rule: RefusedUnreadable, Err: err})
		}
	}
	ok, err := admits(b)
	if err != nil {
		return refused(Refusal{Rule: RefusedUnreadable, Err: err})
	}
	if !ok {
		return refused(Refusal{Rule: RefusedCarrier})
	}
	return pl
}

// carrierRule reads a concrete destination's Allowed Bin Types once and answers
// per bin, through the same bins.TypeAdmits the capacity gate uses (a bare
// marker fits where its carrier fits). An unfenced node admits everything and
// costs its one read. A group answers through its resolver instead, so this
// admits everything for one.
func (p *Planner) carrierRule(dest *nodes.Node) (func(*bins.Bin) (bool, error), error) {
	if dest.IsSynthetic {
		return func(*bins.Bin) (bool, error) { return true, nil }, nil
	}
	bts, err := p.db.GetEffectiveBinTypes(dest.ID)
	if err != nil {
		return nil, err
	}
	return func(b *bins.Bin) (bool, error) {
		if bins.TypeAdmits(bts, b.BinTypeID, nil) {
			return true, nil
		}
		bareOf, err := p.db.BinTypeBareOf(b.BinTypeID)
		if err != nil {
			return false, err
		}
		return bareOf != nil && bins.TypeAdmits(bts, b.BinTypeID, bareOf), nil
	}, nil
}

func refused(r Refusal) Plan {
	return Plan{Outcome: OutcomeStructural, TermCode: string(protocol.TermStructural), Refused: &r}
}
