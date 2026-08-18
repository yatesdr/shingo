package service

import (
	"fmt"

	"shingocore/store"
	"shingocore/store/nodes"
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

// SetMaintainedGroupFlag writes one of the two boolean scalars through the
// audited property path.
//
// Takes the key rather than exposing two near-identical methods, and REFUSES an
// unknown one: the value of routing these through a named set is that a typo in
// a caller becomes an error here instead of a property nothing will ever read.
func (s *NodeService) SetMaintainedGroupFlag(groupNodeID int64, key string, on bool) error {
	switch key {
	case nodes.PropMaintainEnabled, nodes.PropStrictSourcing:
	default:
		return fmt.Errorf("not a maintained-group flag: %q", key)
	}
	v := propOff
	if on {
		v = propOn
	}
	return s.db.SetNodeProperty(groupNodeID, key, v)
}

// SetMaintainedGroupText writes one of the two string scalars through the same
// audited path. Blank is a legitimate value for both and means "none".
func (s *NodeService) SetMaintainedGroupText(groupNodeID int64, key, value string) error {
	switch key {
	case nodes.PropMaintenanceStation, nodes.PropOverflowDestination:
	default:
		return fmt.Errorf("not a maintained-group setting: %q", key)
	}
	return s.db.SetNodeProperty(groupNodeID, key, value)
}

// SetMaintainLevel declares how many empty carriers of one type a group holds.
func (s *NodeService) SetMaintainLevel(l store.MaintainLevel) error {
	return s.db.SetMaintainLevel(l)
}

// RemoveMaintainLevel stops declaring a carrier type for a group. Distinct from
// setting want=0, which keeps the line.
func (s *NodeService) RemoveMaintainLevel(groupNodeID, binTypeID int64) error {
	return s.db.RemoveMaintainLevel(groupNodeID, binTypeID)
}

// ListMaintainLevels returns a group's declared level.
func (s *NodeService) ListMaintainLevels(groupNodeID int64) ([]store.MaintainLevel, error) {
	return s.db.ListMaintainLevels(groupNodeID)
}

// SetMaintainSupports replaces the set of process nodes a group serves.
func (s *NodeService) SetMaintainSupports(groupNodeID int64, processNodeIDs []int64) error {
	return s.db.SetMaintainSupports(groupNodeID, processNodeIDs)
}

// ListMaintainSupports returns the process nodes a group serves.
func (s *NodeService) ListMaintainSupports(groupNodeID int64) ([]store.MaintainSupport, error) {
	return s.db.ListMaintainSupports(groupNodeID)
}
