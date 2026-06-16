package domain

import (
	"testing"

	"shingo/protocol"
)

// TestLoader_SynthClaim pins the Core-owned-loader stand-in claim: a loader can
// project a manual_swap NodeClaim for one of its nodes so the operator board +
// runtime treat it as a loader without a per-style style_node_claim. The claim is
// non-persisted (ID==0), carries the loader's role/payloads/flow, and auto-confirms
// the robot drop (the operator still confirms the physical load/unload separately).
func TestLoader_SynthClaim(t *testing.T) {
	t.Parallel()
	l, err := NewSharedWindowLoader("loader:UNLD", "Supermarket Unloader", RoleConsume, ReplenishmentOperator,
		[]Window{{Node: "SMN_01"}, {Node: "SMN_02"}}, []PayloadCode{"P-A", "P-B"},
		WithInboundSource("Supermarket Area"), WithOutboundDest("Supermarket Empty Totes"))
	if err != nil {
		t.Fatalf("build loader: %v", err)
	}

	c := l.SynthClaim("SMN_01")
	if c == nil {
		t.Fatal("SynthClaim returned nil for a member node")
	}
	if c.ID != 0 {
		t.Errorf("synth claim ID = %d, want 0 (must never be persisted / used as an FK)", c.ID)
	}
	if c.CoreNodeName != "SMN_01" {
		t.Errorf("CoreNodeName = %q, want SMN_01", c.CoreNodeName)
	}
	if c.SwapMode != protocol.SwapModeManualSwap {
		t.Errorf("SwapMode = %q, want manual_swap", c.SwapMode)
	}
	if c.Role != protocol.ClaimRoleConsume {
		t.Errorf("Role = %q, want consume", c.Role)
	}
	if !c.AutoConfirm {
		t.Error("AutoConfirm = false; want true (robot-drop ack — the operator still confirms load/unload)")
	}
	if c.InboundSource != "Supermarket Area" || c.OutboundDestination != "Supermarket Empty Totes" {
		t.Errorf("flow = %q -> %q, want Supermarket Area -> Supermarket Empty Totes", c.InboundSource, c.OutboundDestination)
	}
	got := map[string]bool{}
	for _, p := range c.AllowedPayloadCodes {
		got[p] = true
	}
	if len(c.AllowedPayloadCodes) != 2 || !got["P-A"] || !got["P-B"] {
		t.Errorf("AllowedPayloadCodes = %v, want the loader's set [P-A P-B]", c.AllowedPayloadCodes)
	}
}
