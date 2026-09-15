package domain

import "time"

// BinType is the lookup entity that classifies physical bins by their
// outer dimensions. Bins point at a BinType via Bin.BinTypeID; the
// BinTypeCode copy on Bin is the most common joined field, so most
// rendering paths don't need to follow the pointer.
//
// The three dimensions are DESCRIPTIVE. Nothing in the system decides whether a
// carrier fits a slot by comparing them — fit is type identity everywhere, and
// LengthIn arriving (v90) does not change that. They exist so a plant's carrier
// catalogue can be read off the schema instead of parsed out of codes like
// "45x58x32", which is where the third number lived until now.
type BinType struct {
	ID          int64   `json:"id"`
	Code        string  `json:"code"`
	Description string  `json:"description"`
	WidthIn     float64 `json:"width_in"`
	HeightIn    float64 `json:"height_in"`
	LengthIn    float64 `json:"length_in"`
	// RequiredRobotGroup is the SEER robot group required to move this carrier.
	// Empty = no restriction, which is every carrier until one is configured.
	//
	// IT IS NOT A TWIN OF Payload.RobotGroup. That one is the group for a
	// LOADED bin and is what a full bin dispatches to. This one is a REFUSAL:
	// it is read only where the bin would otherwise be relaxed (empty or near
	// empty), and it replaces the relaxed group there. So a carrier can only
	// ever make the decision heavier, never lighter — the empty FG rack that
	// must not go to a 600 regardless of what its payload permits.
	//
	// Applying it at every fill level instead would be wrong in the other
	// direction: a carrier misconfigured to a lighter group would then take a
	// full heavy load. See dispatch/robot_group.go.
	RequiredRobotGroup string    `json:"required_robot_group"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}
