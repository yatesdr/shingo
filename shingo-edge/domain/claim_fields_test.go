package domain

import (
	"testing"

	"shingoedge/domain/flowspec"
)

// populateClaimField gives field f a value on a stored claim. The value is
// distinct per field so a test populating two of them never trips a
// "positions must differ" rule by accident.
func populateClaimField(c *NodeClaim, f flowspec.Field) {
	v := "X-" + string(f)
	switch f {
	case flowspec.InboundStaging:
		c.InboundStaging = v
	case flowspec.OutboundStaging:
		c.OutboundStaging = v
	case flowspec.PairedCoreNode:
		c.PairedCoreNode = v
	case flowspec.OutboundDestination:
		c.OutboundDestination = v
	case flowspec.InboundSource:
		c.InboundSource = v
	case flowspec.SecondPairedCoreNode:
		c.SecondPairedCoreNode = v
	case flowspec.PayloadCode:
		c.PayloadCode = v
	case flowspec.AllowedPayloadCodes:
		c.AllowedPayloadCodes = []string{v}
	case flowspec.UOPCapacity:
		c.UOPCapacity = 7
	case flowspec.ReorderPoint:
		c.ReorderPoint = 7
	case flowspec.ReorderPointSource:
		c.ReorderPointSource = v
	case flowspec.AutoReorder:
		c.AutoReorder = true
	case flowspec.LinesideSoftThreshold:
		c.LinesideSoftThreshold = 7
	case flowspec.Sequence:
		c.Sequence = 7
	case flowspec.KeepStaged:
		c.KeepStaged = true
	case flowspec.EvacuateOnChangeover:
		c.EvacuateOnChangeover = true
	case flowspec.ChangeoverEvacNodes:
		c.ChangeoverEvacNodes = []string{v}
	case flowspec.ChangeoverEvacDestination:
		c.ChangeoverEvacDestination = v
	case flowspec.ChangeoverCarryoverDisposition:
		c.ChangeoverCarryoverDisposition = CarryoverKeepLineside
	case flowspec.ReuseCompatibleBins:
		c.ReuseCompatibleBins = true
	case flowspec.IndexRobotSupplies:
		c.IndexRobotSupplies = true
	case flowspec.AutoConfirm:
		c.AutoConfirm = true
	case flowspec.AutoRequestPayload:
		c.AutoRequestPayload = v
	case flowspec.AutoPush:
		c.AutoPush = true
	case flowspec.KeyRoute:
		c.KeyRoute = []string{v}
	case flowspec.KeyTask:
		c.KeyTask = "load"
	}
}

// populateClaimInputField is populateClaimField for the write shape.
func populateClaimInputField(in *NodeClaimInput, f flowspec.Field) {
	v := "X-" + string(f)
	t := true
	seven := 7
	switch f {
	case flowspec.InboundStaging:
		in.InboundStaging = v
	case flowspec.OutboundStaging:
		in.OutboundStaging = v
	case flowspec.PairedCoreNode:
		in.PairedCoreNode = v
	case flowspec.OutboundDestination:
		in.OutboundDestination = v
	case flowspec.InboundSource:
		in.InboundSource = v
	case flowspec.SecondPairedCoreNode:
		in.SecondPairedCoreNode = v
	case flowspec.PayloadCode:
		in.PayloadCode = v
	case flowspec.AllowedPayloadCodes:
		in.AllowedPayloadCodes = []string{v}
	case flowspec.UOPCapacity:
		in.UOPCapacity = 7
	case flowspec.ReorderPoint:
		in.ReorderPoint = 7
	case flowspec.ReorderPointSource:
		in.ReorderPointSource = &v
	case flowspec.AutoReorder:
		in.AutoReorder = &t
	case flowspec.LinesideSoftThreshold:
		in.LinesideSoftThreshold = 7
	case flowspec.Sequence:
		in.Sequence = &seven
	case flowspec.KeepStaged:
		in.KeepStaged = &t
	case flowspec.EvacuateOnChangeover:
		in.EvacuateOnChangeover = true
	case flowspec.ChangeoverEvacNodes:
		nodes := []string{v}
		in.ChangeoverEvacNodes = &nodes
	case flowspec.ChangeoverEvacDestination:
		in.ChangeoverEvacDestination = &v
	case flowspec.ChangeoverCarryoverDisposition:
		d := CarryoverKeepLineside
		in.ChangeoverCarryoverDisposition = &d
	case flowspec.ReuseCompatibleBins:
		in.ReuseCompatibleBins = true
	case flowspec.IndexRobotSupplies:
		in.IndexRobotSupplies = &t
	case flowspec.AutoConfirm:
		in.AutoConfirm = true
	case flowspec.AutoRequestPayload:
		in.AutoRequestPayload = v
	case flowspec.AutoPush:
		in.AutoPush = true
	case flowspec.KeyRoute:
		route := []string{v}
		in.KeyRoute = &route
	case flowspec.KeyTask:
		task := "load"
		in.KeyTask = &task
	}
}

// TestClaimAccessorsAreTotal: every field flowspec knows can be read off both
// claim shapes. A blank claim has none of them; a claim with the field
// populated has exactly that one. A field added to flowspec without an
// accessor case fails here rather than reading as "never set" in a validator.
func TestClaimAccessorsAreTotal(t *testing.T) {
	t.Parallel()
	for _, f := range flowspec.Fields() {
		var c NodeClaim
		var in NodeClaimInput
		if ClaimHas(&c, f) {
			t.Errorf("ClaimHas(blank, %s) = true", f)
		}
		if ClaimInputHas(in, f) {
			t.Errorf("ClaimInputHas(blank, %s) = true", f)
		}
		populateClaimField(&c, f)
		populateClaimInputField(&in, f)
		if !ClaimHas(&c, f) {
			t.Errorf("ClaimHas(populated, %s) = false — no accessor case for this field", f)
		}
		if !ClaimInputHas(in, f) {
			t.Errorf("ClaimInputHas(populated, %s) = false — no accessor case for this field", f)
		}
		for _, other := range flowspec.Fields() {
			if other == f {
				continue
			}
			if ClaimHas(&c, other) {
				t.Errorf("populating %s also populated %s on NodeClaim", f, other)
			}
			if ClaimInputHas(in, other) {
				t.Errorf("populating %s also populated %s on NodeClaimInput", f, other)
			}
		}
	}
	if ClaimHas(nil, flowspec.PayloadCode) {
		t.Error("ClaimHas(nil, ...) = true")
	}
	// The carry-over default is the absence of an opinion, on both shapes.
	c := NodeClaim{ChangeoverCarryoverDisposition: CarryoverReplace}
	d := CarryoverReplace
	in := NodeClaimInput{ChangeoverCarryoverDisposition: &d}
	if ClaimHas(&c, flowspec.ChangeoverCarryoverDisposition) || ClaimInputHas(in, flowspec.ChangeoverCarryoverDisposition) {
		t.Error("replace counts as a carry-over value; blank reads as replace, so it must not")
	}
}
