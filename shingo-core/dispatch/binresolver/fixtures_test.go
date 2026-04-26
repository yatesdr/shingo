package binresolver

import (
	"time"

	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// Small constructors to keep test cases terse. Nothing magic — they
// just wire the handful of fields that isBinAvailableForRetrieve,
// classifyEmptyGroup, and the timestamp comparisons read.

// availBin returns a bin that passes isBinAvailableForRetrieve (status
// "available", manifest confirmed, unclaimed). CreatedAt controls
// FIFO/COST ordering; LoadedAt stays nil so the code paths that fall
// back to CreatedAt are exercised.
func availBin(id int64, payloadCode string, createdAt time.Time) *bins.Bin {
	return &bins.Bin{
		ID:                id,
		Status:            "available",
		ManifestConfirmed: true,
		PayloadCode:       payloadCode,
		CreatedAt:         createdAt,
	}
}

// unavailBin returns a bin that fails isBinAvailableForRetrieve — used
// to prove direct-child loops skip it. Here "unavailable" means
// manifest not yet confirmed.
func unavailBin(id int64, payloadCode string) *bins.Bin {
	return &bins.Bin{
		ID:                id,
		Status:            "available",
		ManifestConfirmed: false,
		PayloadCode:       payloadCode,
	}
}

// claimedBin returns a bin that fails isBinAvailableForRetrieve because
// it is already claimed by another order.
func claimedBin(id int64, payloadCode string, owner int64) *bins.Bin {
	return &bins.Bin{
		ID:                id,
		Status:            "available",
		ManifestConfirmed: true,
		ClaimedBy:         &owner,
		PayloadCode:       payloadCode,
	}
}

// laneChild returns a LANE-type enabled child. LANE nodes trigger the
// lane-aware branches in the group resolver (FindSourceBinInLane,
// FindStoreSlotInLane, etc.).
func laneChild(id int64, name string) *nodes.Node {
	return &nodes.Node{
		ID:           id,
		Name:         name,
		NodeTypeCode: "LANE",
		IsSynthetic:  true,
		Enabled:      true,
	}
}

// directChild returns a non-synthetic, non-LANE enabled child. The
// group resolver treats these as single-slot storage/retrieval targets.
func directChild(id int64, name string) *nodes.Node {
	return &nodes.Node{
		ID:          id,
		Name:        name,
		IsSynthetic: false,
		Enabled:     true,
	}
}

// disabledChild returns a child that should be skipped by every
// algorithm. Used by classifyEmptyGroup tests.
func disabledChild(id int64, name string) *nodes.Node {
	return &nodes.Node{
		ID:      id,
		Name:    name,
		Enabled: false,
	}
}

// ngrpNode returns a synthetic NGRP-type parent used as the argument
// to ResolveRetrieve / ResolveStore / DefaultResolver.Resolve for the
// NGRP-delegation path.
func ngrpNode(id int64, name string) *nodes.Node {
	return &nodes.Node{
		ID:           id,
		Name:         name,
		NodeTypeCode: "NGRP",
		IsSynthetic:  true,
		Enabled:      true,
	}
}

// slotInLane returns a slot node owned by a lane, with its NodeID set
// back to itself so *bin.NodeID -> GetNode -> slot works in the fake.
func slotInLane(id int64, name string) *nodes.Node {
	return &nodes.Node{
		ID:   id,
		Name: name,
	}
}

// attachSlot wires bin.NodeID -> slot.ID so the FIFO/COST resolvers can
// walk from a returned bin back to the slot it sits in.
func attachSlot(bin *bins.Bin, slot *nodes.Node) {
	id := slot.ID
	bin.NodeID = &id
}

// payload returns a minimal payloads.Payload with just Code set — that's
// the only field GetEffectivePayloads consumers check.
func payload(code string) *payloads.Payload {
	return &payloads.Payload{Code: code}
}

// binType returns a minimal *bins.BinType for restriction-set tests.
func binType(id int64) *bins.BinType {
	return &bins.BinType{ID: id}
}
