package domain

import (
	"strconv"
	"time"
)

// BinType is the lookup entity that classifies physical bins by their
// outer dimensions. Bins point at a BinType via Bin.BinTypeID; the
// BinTypeCode copy on Bin is the most common joined field, so most
// rendering paths don't need to follow the pointer.
//
// The three dimensions are a plant's carrier catalogue, read off the schema instead
// of parsed out of codes like "45x58x32". Width and length make its Footprint, which
// the kanban save checks a family's members agree on and the card prints. Nothing
// decides on a footprint: what a cart may be stamped as is its family (FamilyID).
type BinType struct {
	ID          int64   `json:"id"`
	Code        string  `json:"code"`
	Description string  `json:"description"`
	WidthIn     float64 `json:"width_in"`
	HeightIn    float64 `json:"height_in"`
	LengthIn    float64 `json:"length_in"`
	// FamilyID is the physical cart this type is one setting of (carrier_families):
	// the one thing a CLEAR never changes. Every type has one, by default a family of
	// its own; a bare marker has its carrier's. FamilyCode is its name.
	FamilyID   int64  `json:"family_id"`
	FamilyCode string `json:"family_code"`
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
	RequiredRobotGroup string `json:"required_robot_group"`
	// Bare flags a carrier type that holds no container: the marker a stage-1
	// unloader's CLEAR stamps on the cart it leaves behind. Read-only — the
	// column is GENERATED from BareOf, so a bare type cannot be made by hand. A
	// bare cart is never handed out as an empty (bins.EmptyCarrierWhere), a
	// bare type is never in a payload rule, and stage 2's blank CLEAR (SEND ON)
	// stamps BareOf back.
	Bare bool `json:"bare"`
	// BareOf is the carrier a bare marker stands for: the same physical cart
	// with no bin on it. nil on every real type. Written only by
	// bins.EnsureBareMarkerTx; a marker is admitted wherever its carrier is.
	BareOf *int64 `json:"bare_of,omitempty"`
	// TotesPerLayer is how many totes one layer of this carrier holds. nil
	// (NULL): not a tote carrier, or not said; 1 or more when set. A declared
	// height on this carrier is layers × TotesPerLayer totes.
	TotesPerLayer *int      `json:"totes_per_layer,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Footprint is a carrier's width and length as entered, held in order so that a
// 45 × 48 and a 48 × 45 are one size: two are the same exactly when they are ==. Not
// the backstop Floor.
type Footprint struct{ short, long float64 }

// String is the footprint short side first: "45x48". Each side is its
// shortest exact spelling, so two strings are equal exactly when the sizes are ==.
func (f Footprint) String() string {
	return strconv.FormatFloat(f.short, 'f', -1, 64) + "x" + strconv.FormatFloat(f.long, 'f', -1, 64)
}

// Footprint is the type's footprint; false unless both its width and its
// length are entered (above 0). A type with none has no size to compare.
func (bt BinType) Footprint() (Footprint, bool) {
	if bt.WidthIn <= 0 || bt.LengthIn <= 0 {
		return Footprint{}, false
	}
	return Footprint{min(bt.WidthIn, bt.LengthIn), max(bt.WidthIn, bt.LengthIn)}, true
}

// BareStamp is what a CLEAR does to a cart's type in a two-stage unloader,
// decided by the caller from where the cart stands and resolved by the store
// inside the clear's transaction (bins.ResolveBareStampTx).
type BareStamp int

const (
	// StampNone: the clear writes the explicit type it was given, or none.
	StampNone BareStamp = iota
	// StampMarker: a stage-1 CLEAR — the cart takes its own type's bare marker,
	// created the first time that type comes through.
	StampMarker
	// StampCarrier: SEND ON — a bare cart takes its carrier back. A cart that is
	// no longer bare keeps its type.
	StampCarrier
)
