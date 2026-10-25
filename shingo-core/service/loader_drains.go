package service

import (
	"errors"
	"fmt"

	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// loader_drains.go — where an unloader's drain boxes may be on (v165,
// loaders.DrainStates). The boxes have an unloader pull part-less carts that
// land in its inbound source, beside its fulls: "Point it at a press's
// outbound, not a stock group." Default off: nothing changes until a plant
// ticks one.
//
// ASKED ON EVERY SAVE THAT WOULD LEAVE A BOX ON, not only the save that ticks
// one. The page's update is a full-row write that carries the stored boxes
// back, so a box already on is refused the moment a save would strand it: the
// inbound source cleared later (or "fed directly" ticked), the outbound pointed
// back at the source. Pinned by TestLoaderService_DrainsRefusedWhereTheyCannotHold.
//
// A maintained inbound source is not refused here: every unloader with one is
// refused already, whatever its boxes (checkFlowEndpoints,
// ErrFullsFromAnEmptiesBank, whose sentence says the boxes drain nothing from
// one), and the pull re-asks the same test because a level can be declared
// later.

// ErrDrainProduce refuses a drain box on a produce loader, as Accept partials
// is refused there: a loader is fed empties, it clears no carts, and the box
// would be stored, shown and do nothing.
var ErrDrainProduce = errors.New("only an unloader drains carts from its inbound source: a loader is fed empties and clears none")

// ErrDrainNoSource refuses a drain box on an unloader with no inbound source:
// fed directly, or none named. The boxes drain what lands in the source, so
// there is nothing for them to drain.
var ErrDrainNoSource = errors.New("an unloader drains the carts that land in its inbound source, and this one has none (it is fed directly, or no source is named): name an inbound source or untick the drain boxes")

// ErrDrainDedicatedLayout refuses a drain box on an unloader with one spot per
// part: a drain moves carts into an unloader's shared windows, and the drain
// reads the layout off the row (dispatch.drainsAt), so a box stored here would
// be shown and pull nothing.
var ErrDrainDedicatedLayout = errors.New("an unloader drains carts into its shared windows, and this one has one spot per part: drains need the shared-window layout")

// ErrDrainTwoStage refuses a drain box on either half of a two-stage unloader:
// stage 2's windows are its own pull's (dispatch PullStage2), and stage 1's CLEAR
// stamps the cart bare, so neither has a window a drained cart could take.
var ErrDrainTwoStage = errors.New("a two-stage unloader drains nothing: stage 2's windows are its own pull's, and stage 1's CLEAR stamps the cart bare")

// ErrDrainIntoItsSource refuses a drain box on an unloader whose outbound
// destination, or that group's overflow destination, is its inbound source,
// contains it or lies in it: a drained cart's CLEAR sends it to the outbound,
// where it would land back in the source and be drained again, for ever. The
// refusal names both nodes. A longer cycle through other unloaders is not
// seen (named residual, W14).
var ErrDrainIntoItsSource = errors.New("a drained cart's CLEAR would send it back to be drained")

// ErrNotDrainable refuses a state no box drains. It is loaders.ErrNotDrainable
// under the name www can reach: www may not import a store package
// (www-no-direct-store), and its save maps this refusal to a 400.
var ErrNotDrainable = loaders.ErrNotDrainable

// checkDrains refuses l's drain boxes where they cannot hold, l being the row
// the save is about to store (its inbound source already blank when it is fed
// directly). No box on, nothing read.
func (s *LoaderService) checkDrains(l *loaders.Loader) error {
	if len(l.Drains) == 0 {
		return nil
	}
	if l.Role != loaders.RoleConsume {
		return ErrDrainProduce
	}
	if l.InboundSource == "" {
		return ErrDrainNoSource
	}
	if l.Layout != loaders.LayoutSharedWindow {
		return ErrDrainDedicatedLayout
	}
	if l.SecondStageLoaderID != nil {
		return ErrDrainTwoStage
	}
	if l.ID != 0 {
		one, err := s.firstStageOf(l.ID)
		if err != nil {
			return err
		}
		if one != nil {
			return ErrDrainTwoStage
		}
	}
	return s.checkDrainLoop(l.InboundSource, l.OutboundDest)
}

// DrainAllowed reports whether a drain box may be ticked on l as it is stored:
// the save's own test (checkDrains) asked with one box on. The page draws the
// boxes where this says yes, so the rule is spelled once, here. A loader, or an
// unloader with no inbound source, reads nothing; any other unloader reads what
// the save would.
func (s *LoaderService) DrainAllowed(l loaders.Loader) bool {
	l.Drains = loaders.DrainStates{loaders.Drainable[0]}
	return s.checkDrains(&l) == nil
}

// checkDrainLoop refuses an outbound destination, or its overflow destination
// (the group's overflow_destination property, as the group editor reads it),
// that is the inbound source, contains it or lies in it. A name that resolves
// to nothing is not this check's question (checkInboundSource; an outbound is
// not resolve-checked).
func (s *LoaderService) checkDrainLoop(inboundSource, outboundDest string) error {
	if outboundDest == "" {
		return nil
	}
	src, err := s.db.GetNodeByDotName(inboundSource)
	if err != nil || src == nil {
		return nil
	}
	out, err := s.db.GetNodeByDotName(outboundDest)
	if err != nil || out == nil {
		return nil
	}
	if err := s.drainLoop("the outbound destination", out, src); err != nil {
		return err
	}
	name := s.db.GetNodeProperty(out.ID, nodes.PropOverflowDestination)
	if name == "" {
		return nil
	}
	over, err := s.db.GetNodeByDotName(name)
	if err != nil || over == nil {
		return nil
	}
	return s.drainLoop("the outbound destination's overflow", over, src)
}

// drainLoop refuses dest when it is src, lies in it or contains it, in a
// sentence naming both. Two subtree reads at most (store.NodeIsUnderAny, each
// root inclusive), and none when the two are one node.
func (s *LoaderService) drainLoop(what string, dest, src *nodes.Node) error {
	rel := ""
	if dest.ID == src.ID {
		rel = "is"
	} else if in, err := s.db.NodeIsUnderAny(dest.ID, []int64{src.ID}); err != nil {
		return err
	} else if in {
		rel = "lies in"
	} else if holds, err := s.db.NodeIsUnderAny(src.ID, []int64{dest.ID}); err != nil {
		return err
	} else if holds {
		rel = "contains"
	}
	if rel == "" {
		return nil
	}
	return fmt.Errorf("%s %q %s the inbound source %q: %w", what, dest.Name, rel, src.Name, ErrDrainIntoItsSource)
}
