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
	AdvancedLoadSequence string    `json:"advanced_load_sequence"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}
