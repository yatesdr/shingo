package service

import (
	"fmt"
	"strings"

	"shingocore/store/bins"
)

// The holds-bins guard.
//
// Four edits to a maintained group change what its residents MEAN, and all four
// are safe on an empty group and questionable on a full one:
//
//	disable          the level stops being held; the carriers standing there
//	                 become nobody's
//	reserve (strict) the carriers standing there stop being visible to every
//	                 process that is not on the list — including ones already
//	                 looking for them
//	narrow supports  the same, for exactly the processes just dropped
//	narrow types     a resident whose carrier type was just disallowed is now
//	                 standing somewhere it may not stand
//
// SEPARATE FROM THE SAVE-TIME RULES, and the difference is what they read. The
// rules in node_service_maintain_validate.go read CONFIGURATION and are true or
// false regardless of the floor. This reads the FLOOR — which carriers are
// standing there right now — and it is a delta check: it fires on the
// transition, not on the state, because a group that has been reserved for a
// month is not re-asking every time somebody saves the screen.
//
// REFUSE, NOT WARN, and overridable with force. The operator may well know the
// group is about to be emptied, or that the carriers standing there are exactly
// the ones the new configuration is for. What they must not do is find out
// afterwards. So: refuse, name the carriers, and let a confirm dialog carry
// force back.
//
// TOCTOU is accepted, in the same words the reparent guard uses: a carrier can
// arrive between this check and the write. These are rare operator-initiated
// actions on a screen a person is looking at, and the alternative — locking the
// group for the length of a modal — is worse than the thing it prevents.

// MaintainedGroupGuard is what the floor says about a pending change.
type MaintainedGroupGuard struct {
	// Blocked is the human sentence, empty when nothing is in the way.
	Blocked string `json:"blocked,omitempty"`
	// Drain names live orders that were already sourcing from this group when
	// it was reserved. Reported, never blocking — see checkStrictDrain.
	Drain []string `json:"drain,omitempty"`
}

// residents returns the carriers standing at a group's DIRECT children.
//
// Direct children only, which is exact rather than a shortcut: a maintained
// group is refused at save time unless it is flat, so its direct children are
// all of it. A recursive walk here would be code whose extra reach can never be
// exercised, and the first person to make groups nestable would find it already
// written and assume it had been thought about.
func (s *NodeService) residents(groupNodeID int64) ([]*bins.Bin, error) {
	children, err := s.db.ListChildNodes(groupNodeID)
	if err != nil {
		return nil, fmt.Errorf("read children of group %d: %w", groupNodeID, err)
	}
	if len(children) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(children))
	for i, c := range children {
		ids[i] = c.ID
	}
	return s.db.ListBinsByNodes(ids)
}

func binLabels(bs []*bins.Bin) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		if b.Label != "" {
			out = append(out, b.Label)
		} else {
			out = append(out, fmt.Sprintf("bin %d", b.ID))
		}
	}
	return out
}

// CheckMaintainedGroupSettingsChange runs the guard for the two settings
// transitions — turning maintenance off, and turning the reserve on.
//
// It compares against what is stored, so a save that leaves both switches where
// they were asks nothing: only the EDGE trips the guard.
func (s *NodeService) CheckMaintainedGroupSettingsChange(groupNodeID int64, set MaintainedGroupSettings, force bool) (MaintainedGroupGuard, error) {
	var g MaintainedGroupGuard
	before, err := s.GetMaintainedGroup(groupNodeID)
	if err != nil {
		return g, err
	}
	turningOff := before.Enabled && !set.MaintainEnabled
	turningStrictOn := !before.StrictSourcing && set.StrictSourcing
	if !turningOff && !turningStrictOn {
		return g, nil
	}

	// The drain report is computed even under force, and even when the carrier
	// check passes: it is the one thing here that is about orders rather than
	// carriers, and an operator reserving a group wants to know who is already
	// on their way to it regardless of what is standing there.
	if turningStrictOn {
		drain, err := s.checkStrictDrain(groupNodeID)
		if err != nil {
			return g, err
		}
		g.Drain = drain
	}
	if force {
		return g, nil
	}

	held, err := s.residents(groupNodeID)
	if err != nil {
		return g, err
	}
	if len(held) == 0 {
		return g, nil
	}
	group, err := s.db.GetNode(groupNodeID)
	if err != nil {
		return g, err
	}
	switch {
	case turningOff:
		g.Blocked = fmt.Sprintf("%s still holds %s — turning maintenance off leaves %s belonging to nothing",
			group.Name, strings.Join(binLabels(held), ", "), plural(len(held), "it", "them"))
	default:
		g.Blocked = fmt.Sprintf("%s still holds %s — reserving the group hides %s from every process not on the list",
			group.Name, strings.Join(binLabels(held), ", "), plural(len(held), "it", "them"))
	}
	return g, nil
}

// CheckMaintainedGroupSupportsChange runs the guard when the supported set
// NARROWS.
//
// Only on narrowing. Adding a process gives more people access to what is
// standing there, which strands nobody; removing one takes it away from a
// process that may be mid-order for exactly those carriers.
func (s *NodeService) CheckMaintainedGroupSupportsChange(groupNodeID int64, processNodeIDs []int64, force bool) (MaintainedGroupGuard, error) {
	var g MaintainedGroupGuard
	if force {
		return g, nil
	}
	before, err := s.GetMaintainedGroup(groupNodeID)
	if err != nil {
		return g, err
	}
	keeping := make(map[int64]bool, len(processNodeIDs))
	for _, id := range processNodeIDs {
		keeping[id] = true
	}
	var dropped []string
	for _, sup := range before.Supports {
		if !keeping[sup.ProcessNodeID] {
			dropped = append(dropped, sup.ProcessNodeName)
		}
	}
	if len(dropped) == 0 {
		return g, nil
	}
	held, err := s.residents(groupNodeID)
	if err != nil || len(held) == 0 {
		return g, err
	}
	group, err := s.db.GetNode(groupNodeID)
	if err != nil {
		return g, err
	}
	g.Blocked = fmt.Sprintf("%s still holds %s — dropping %s takes %s away from what is standing there",
		group.Name, strings.Join(binLabels(held), ", "), strings.Join(dropped, ", "),
		plural(len(dropped), "it", "them"))
	return g, nil
}

// CheckMaintainedGroupTypesChange runs the guard when a group's ALLOWED carrier
// types narrow.
//
// SCOPED TO THE CARRIERS ACTUALLY AFFECTED, unlike the other three. Dropping one
// type from a group holding four kinds strands the carriers of that type and
// nobody else, and a refusal that recited every resident would be a refusal an
// operator learns to force past without reading.
//
// keeping is the type-id set the save would leave; an EMPTY set means "no
// restriction" — the same reading the resolver's own predicate has — so it
// strands nothing and is not a narrowing at all.
func (s *NodeService) CheckMaintainedGroupTypesChange(groupNodeID int64, keeping []int64, force bool) (MaintainedGroupGuard, error) {
	var g MaintainedGroupGuard
	if force || len(keeping) == 0 {
		return g, nil
	}
	allowed := make(map[int64]bool, len(keeping))
	for _, id := range keeping {
		allowed[id] = true
	}
	held, err := s.residents(groupNodeID)
	if err != nil || len(held) == 0 {
		return g, err
	}
	var stranded []*bins.Bin
	for _, b := range held {
		if !allowed[b.BinTypeID] {
			stranded = append(stranded, b)
		}
	}
	if len(stranded) == 0 {
		return g, nil
	}
	group, err := s.db.GetNode(groupNodeID)
	if err != nil {
		return g, err
	}
	g.Blocked = fmt.Sprintf("%s holds %s, whose carrier type this change disallows — %s would be standing somewhere %s may not stand",
		group.Name, strings.Join(binLabels(stranded), ", "),
		plural(len(stranded), "it", "they"), plural(len(stranded), "it", "they"))
	return g, nil
}

// checkStrictDrain names the orders already sourcing from this group when the
// reserve goes up.
//
// REPORTED, NEVER BLOCKING. These orders have already been admitted and are
// looking for a carrier here; the fence does not reach back and cancel them, and
// nothing in this program cancels anything. What the operator gets is the list,
// so "the reserve is on but three orders still came and took carriers" is a
// sentence they were told in advance rather than one they discover.
//
// Pre-dispatch states only — the set ListActiveBySourceRef already answers, and
// the right one: an order past dispatch has its carrier and the fence changes
// nothing for it.
func (s *NodeService) checkStrictDrain(groupNodeID int64) ([]string, error) {
	group, err := s.db.GetNode(groupNodeID)
	if err != nil {
		return nil, err
	}
	names := []string{group.Name}
	children, err := s.db.ListChildNodes(groupNodeID)
	if err != nil {
		return nil, fmt.Errorf("read children of %s: %w", group.Name, err)
	}
	for _, c := range children {
		names = append(names, c.Name)
	}
	live, err := s.db.ListActiveOrdersBySourceRef(names)
	if err != nil {
		return nil, fmt.Errorf("read orders sourcing from %s: %w", group.Name, err)
	}
	out := make([]string, 0, len(live))
	for _, o := range live {
		out = append(out, fmt.Sprintf("order %d (%s, from %s)", o.ID, o.Status, o.SourceNode))
	}
	return out, nil
}
