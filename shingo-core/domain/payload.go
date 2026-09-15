package domain

import "time"

// Payload is a template describing a kind of content a bin can hold:
// a unique Code (matched against Bin.PayloadCode on retrieve), a
// human-readable Description, and a UOPCapacity — the number of
// unit-of-production "slots" a fresh bin holds. The actual line items
// that make up a full bin are on PayloadManifestItem, keyed back to
// the Payload via PayloadID.
type Payload struct {
	ID          int64  `json:"id"`
	Code        string `json:"code"`
	Description string `json:"description"`
	UOPCapacity int    `json:"uop_capacity"`
	// RobotGroup is the SEER robot-dispatch group that should execute transport
	// orders for this payload (e.g. a "1500kg" group for heavy bins). It maps to
	// rds.SetOrderRequest.Group at dispatch. Empty = unset = SEER's default
	// assignment. Distinct from a destination node group (NGRP).
	RobotGroup string `json:"robot_group"`
	// AdvancedLoadSequence names a configured load-sequence in the load_sequences
	// registry. Empty = today's single default load block (byte-identical, no
	// behavior change). A name set makes dispatch expand this payload's LOAD leg
	// into one same-location RDS block per named binTask in the sequence (the
	// quarter-child-cart interlock). The name IS the switch — there is no separate
	// enable flag. Validated at config-save against the RDS binTask keys of the
	// payload's assigned node locations (see engine.ValidateAdvancedLoadSequence).
	AdvancedLoadSequence string `json:"advanced_load_sequence"`
	// NearEmptyEnabled turns on the relaxation below: a bin at or under
	// NearEmptyThresholdPct percent of capacity dispatches to
	// NearEmptyRobotGroup instead of RobotGroup. A full bin is unaffected.
	//
	// IT IS A REAL COLUMN AND NOT NearEmptyRobotGroup != "", which is the
	// AdvancedLoadSequence precedent above ("the name IS the switch"). That
	// precedent does not hold here: a BLANK NearEmptyRobotGroup is a meaningful
	// value — it means "relax to the vendor-default pool", i.e. any robot,
	// which is frequently what a plant wants — and name-is-the-switch cannot
	// tell that apart from "unconfigured". This flag is what separates them.
	// Do not delete it in a simplification pass.
	NearEmptyEnabled bool `json:"near_empty_enabled"`
	// NearEmptyRobotGroup is the group that carries this payload's bins once
	// they are empty or near empty. Empty string is legitimate and means the
	// vendor default (any robot) — see NearEmptyEnabled.
	NearEmptyRobotGroup string `json:"near_empty_robot_group"`
	// NearEmptyThresholdPct is the fill percentage at or below which the bin
	// counts as near empty. 0 is not "off" — the off switch is
	// NearEmptyEnabled. 0 relaxes only an exactly-empty bin, which the rule
	// table already decides before the threshold is consulted.
	NearEmptyThresholdPct int       `json:"near_empty_threshold_pct"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}
