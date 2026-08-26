package service

import (
	"fmt"

	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/plantclaims"
)

// Maintained-group configuration, service side.
//
// A maintained group is an NGRP whose EMPTY-CARRIER level Core holds — so many
// unclaimed carriers of each declared type, at all times, near the equipment
// that consumes them. This file is the read/write surface the group settings
// modal talks to.
//
// INERT. Nothing consumes any of it yet; the level keeper is a later phase. What
// exists here is storage and its shape.
//
// TWO STORAGE HOMES, ON PURPOSE. The sets (levels, supports) are node-keyed
// tables; the four scalars are node_properties rows written through
// SetNodeProperty — the path the modal already uses, and the reason is the audit
// trail: the property endpoint records old→new for every key, unconditionally,
// with no list of interesting keys to go stale. A parallel write path for these
// four would be a fifth way to change configuration with no trail behind it,
// which is the exact finding (SPR Finding 2/3) this feature is not allowed to
// reproduce.

// on / off are the property spellings for the two boolean scalars, matching
// asrs_enabled and resolve_around on the same modal.
//
// ABSENT READS AS OFF, and that polarity is the point: a group nobody has
// configured must never be one Core has started steering. There is no third
// state and no default-on.
const (
	propOn  = "on"
	propOff = "off"
)

// MaintainedGroupConfig is one group's whole maintained-group configuration, as
// the settings modal reads it: the four scalars plus both sets, in one answer.
//
// One read rather than six, because a half-loaded configuration screen is a
// screen an operator can save from — and saving what a failed fetch left blank
// is how a level silently becomes zero.
type MaintainedGroupConfig struct {
	GroupNodeID         int64                   `json:"group_node_id"`
	Enabled             bool                    `json:"maintain_enabled"`
	StrictSourcing      bool                    `json:"strict_sourcing"`
	MaintenanceStation  string                  `json:"maintenance_station"`
	OverflowDestination string                  `json:"overflow_destination"`
	Levels              []store.MaintainLevel   `json:"levels"`
	Supports            []store.MaintainSupport `json:"supports"`
}

// GetMaintainedGroup returns everything the modal needs for one group.
//
// A group with nothing configured is not an error and not a nil: it is a config
// with Enabled false and two empty sets, which is what every NGRP in the plant
// looks like today and what the modal must be able to render.
func (s *NodeService) GetMaintainedGroup(groupNodeID int64) (*MaintainedGroupConfig, error) {
	levels, err := s.db.ListMaintainLevels(groupNodeID)
	if err != nil {
		return nil, err
	}
	supports, err := s.db.ListMaintainSupports(groupNodeID)
	if err != nil {
		return nil, err
	}
	return &MaintainedGroupConfig{
		GroupNodeID:         groupNodeID,
		Enabled:             s.db.GetNodeProperty(groupNodeID, nodes.PropMaintainEnabled) == propOn,
		StrictSourcing:      s.db.GetNodeProperty(groupNodeID, nodes.PropStrictSourcing) == propOn,
		MaintenanceStation:  s.db.GetNodeProperty(groupNodeID, nodes.PropMaintenanceStation),
		OverflowDestination: s.db.GetNodeProperty(groupNodeID, nodes.PropOverflowDestination),
		Levels:              levels,
		Supports:            supports,
	}, nil
}

// MaintainedGroupSettings is the four scalars as one save.
type MaintainedGroupSettings struct {
	MaintainEnabled     bool
	StrictSourcing      bool
	MaintenanceStation  string
	OverflowDestination string
}

// NodePropertyWrite is one key/value a caller is to write through whatever
// audited property path it owns.
type NodePropertyWrite struct {
	Key   string
	Value string
}

// MaintainedGroupPropertyWrites turns a settings save into the ordered property
// writes it means.
//
// IT RETURNS THE WRITES RATHER THAN PERFORMING THEM, and the split is the audit
// trail's. The old→new row is appended by the HTTP layer, which is the only
// place that knows the actor; a service method that wrote these four itself
// would be a configuration path with no trail behind it, which is the SPR
// Finding 2/3 shape exactly. So the vocabulary — which keys, and the on/off
// spelling shared with asrs_enabled and resolve_around — stays here with the
// aggregate that owns it, and the writing stays where the auditing already is.
func MaintainedGroupPropertyWrites(s MaintainedGroupSettings) []NodePropertyWrite {
	flag := func(b bool) string {
		if b {
			return propOn
		}
		return propOff
	}
	return []NodePropertyWrite{
		{nodes.PropMaintainEnabled, flag(s.MaintainEnabled)},
		{nodes.PropStrictSourcing, flag(s.StrictSourcing)},
		{nodes.PropMaintenanceStation, s.MaintenanceStation},
		{nodes.PropOverflowDestination, s.OverflowDestination},
	}
}

// SetMaintainLevel declares how many empty carriers of one type a group holds.
//
// Validated against the configuration this write WOULD leave behind, not against
// the row on its own: most of the save-time rules are about how the parts agree,
// and a row in isolation has nothing to disagree with.
func (s *NodeService) SetMaintainLevel(groupNodeID, binTypeID int64, want int) (MaintainedGroupCheck, error) {
	if want < 0 {
		return MaintainedGroupCheck{
			Refusals: []string{fmt.Sprintf("a maintained level cannot be negative (got %d)", want)},
		}, nil
	}
	bt, err := s.db.GetBinType(binTypeID)
	if err != nil {
		return MaintainedGroupCheck{}, fmt.Errorf("read carrier type %d: %w", binTypeID, err)
	}
	post, err := s.GetMaintainedGroup(groupNodeID)
	if err != nil {
		return MaintainedGroupCheck{}, err
	}
	replaced := false
	for i := range post.Levels {
		if post.Levels[i].BinTypeID == binTypeID {
			post.Levels[i].Want = want
			replaced = true
		}
	}
	if !replaced {
		post.Levels = append(post.Levels, store.MaintainLevel{
			GroupNodeID: groupNodeID, BinTypeID: binTypeID, BinTypeCode: bt.Code, Want: want,
		})
	}
	chk, err := s.ValidateMaintainedGroup(*post)
	if err != nil || chk.Err() != nil {
		return chk, err
	}
	// The mode is written FIRST and only when absent: a group that is about to
	// carry a declared level must not still be resolving its allowed types off
	// an ancestor.
	if err := s.ensureExplicitBinTypeMode(groupNodeID); err != nil {
		return chk, err
	}
	return chk, s.db.SetMaintainLevel(store.MaintainLevel{
		GroupNodeID: groupNodeID, BinTypeID: binTypeID, Want: want,
	})
}

// RemoveMaintainLevel stops declaring a carrier type for a group. Distinct from
// setting want=0, which keeps the line.
func (s *NodeService) RemoveMaintainLevel(groupNodeID, binTypeID int64) (MaintainedGroupCheck, error) {
	post, err := s.GetMaintainedGroup(groupNodeID)
	if err != nil {
		return MaintainedGroupCheck{}, err
	}
	kept := post.Levels[:0]
	for _, l := range post.Levels {
		if l.BinTypeID != binTypeID {
			kept = append(kept, l)
		}
	}
	post.Levels = kept
	chk, err := s.ValidateMaintainedGroup(*post)
	if err != nil || chk.Err() != nil {
		return chk, err
	}
	return chk, s.db.RemoveMaintainLevel(groupNodeID, binTypeID)
}

// ListMaintainLevels returns a group's declared level.
func (s *NodeService) ListMaintainLevels(groupNodeID int64) ([]store.MaintainLevel, error) {
	return s.db.ListMaintainLevels(groupNodeID)
}

// SetMaintainSupports replaces the set of process nodes a group serves.
func (s *NodeService) SetMaintainSupports(groupNodeID int64, processNodeIDs []int64) (MaintainedGroupCheck, error) {
	post, err := s.GetMaintainedGroup(groupNodeID)
	if err != nil {
		return MaintainedGroupCheck{}, err
	}
	post.Supports = post.Supports[:0]
	for _, id := range processNodeIDs {
		n, err := s.db.GetNode(id)
		if err != nil {
			return MaintainedGroupCheck{}, fmt.Errorf("read supported position %d: %w", id, err)
		}
		post.Supports = append(post.Supports, store.MaintainSupport{
			GroupNodeID: groupNodeID, ProcessNodeID: id, ProcessNodeName: n.Name,
		})
	}
	chk, err := s.ValidateMaintainedGroup(*post)
	if err != nil || chk.Err() != nil {
		return chk, err
	}
	return chk, s.db.SetMaintainSupports(groupNodeID, processNodeIDs)
}

// CheckMaintainedGroupSettings runs the save-time rules against a scalar change
// WITHOUT writing it.
//
// The four scalars are written by the HTTP layer, through the audited property
// path, so this half of the save splits: the rules live here with every other
// rule, the write stays where the audit row can name an actor. The caller runs
// this first and writes nothing if it refuses.
func (s *NodeService) CheckMaintainedGroupSettings(groupNodeID int64, set MaintainedGroupSettings) (MaintainedGroupCheck, error) {
	post, err := s.GetMaintainedGroup(groupNodeID)
	if err != nil {
		return MaintainedGroupCheck{}, err
	}
	post.Enabled = set.MaintainEnabled
	post.StrictSourcing = set.StrictSourcing
	post.MaintenanceStation = set.MaintenanceStation
	post.OverflowDestination = set.OverflowDestination
	return s.ValidateMaintainedGroup(*post)
}

// ListMaintainSupports returns the process nodes a group serves.
func (s *NodeService) ListMaintainSupports(groupNodeID int64) ([]store.MaintainSupport, error) {
	return s.db.ListMaintainSupports(groupNodeID)
}

// ListProcessNodeOptions returns each process with the Core nodes its claims
// resolve to — the picker's contents.
//
// The editor OFFERS processes and STORES nodes, and the resolution happens here,
// once, at config time. It has to: a claim lives on the Edge, so a rule stored as
// "process P" would have nothing Core could evaluate it against later. What the
// operator sees is a process; what lands in node_maintain_supports is the node
// set that process's claims name at the moment they save.
func (s *NodeService) ListProcessNodeOptions() ([]plantclaims.ProcessNodeOption, error) {
	return s.db.ListProcessNodeOptions()
}
