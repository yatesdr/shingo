package domain

import (
	"testing"

	"shingo/protocol"
)

// TestLoader_LoadablePayloadCodesAt_Dedicated pins per-position payload scoping for
// a dedicated home loader. Each home is bound to ONE part; the operator board and
// the load/request gate must offer only that part at that node — never the loader's
// other positions' parts. This is the fix for the "ton of payloads / duplicated
// cards" board bug (every position was being fed the whole loader's payload set).
func TestLoader_LoadablePayloadCodesAt_Dedicated(t *testing.T) {
	t.Parallel()
	l, err := NewDedicatedPositionsLoader("loader:7", "Supermarket Dedicated Locations",
		RoleProduce, ReplenishmentOperator, []Position{
			{Node: "SMN_014", Payload: "76683-6TA0A.06"},
			{Node: "SMN_015", Payload: "76682-6TA0A.06"},
			{Node: "SMN_018"}, // buffer / unpinned home — no payload
		})
	if err != nil {
		t.Fatalf("build loader: %v", err)
	}

	// Whole-loader set is unchanged (used by non-node-scoped callers).
	if got := l.LoadablePayloadCodes(); len(got) != 2 {
		t.Errorf("LoadablePayloadCodes() = %v, want the 2 pinned payloads", got)
	}

	// Per-position scoping: each home offers ONLY its pinned payload.
	cases := map[NodeID][]string{
		"SMN_014": {"76683-6TA0A.06"},
		"SMN_015": {"76682-6TA0A.06"},
		"SMN_018": nil, // buffer slot pins no payload → nothing loadable here
	}
	for node, want := range cases {
		got := l.LoadablePayloadCodesAt(node)
		if len(got) != len(want) {
			t.Errorf("LoadablePayloadCodesAt(%s) = %v, want %v", node, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("LoadablePayloadCodesAt(%s)[%d] = %q, want %q", node, i, got[i], want[i])
			}
		}
	}

	// A node that isn't a position of this loader → nothing.
	if got := l.LoadablePayloadCodesAt("SMN_999"); got != nil {
		t.Errorf("LoadablePayloadCodesAt(non-member) = %v, want nil", got)
	}

	// PayloadAt mirrors the scoping.
	if p, ok := l.PayloadAt("SMN_014"); !ok || p != "76683-6TA0A.06" {
		t.Errorf("PayloadAt(SMN_014) = %q,%v, want 76683-6TA0A.06,true", p, ok)
	}
	if _, ok := l.PayloadAt("SMN_018"); ok {
		t.Error("PayloadAt(buffer) ok = true, want false (no payload pinned)")
	}
}

// TestLoader_SynthClaim_DedicatedScopesPayload pins that the synthesized claim a
// Core-owned dedicated loader projects for a home node carries ONLY that home's
// payload — so the operator board (which renders one card per allowed payload per
// position) shows one card per home, not the whole catalog at every home.
func TestLoader_SynthClaim_DedicatedScopesPayload(t *testing.T) {
	t.Parallel()
	l, err := NewDedicatedPositionsLoader("loader:7", "Supermarket Dedicated Locations",
		RoleProduce, ReplenishmentOperator, []Position{
			{Node: "SMN_014", Payload: "76683-6TA0A.06"},
			{Node: "SMN_015", Payload: "76682-6TA0A.06"},
		})
	if err != nil {
		t.Fatalf("build loader: %v", err)
	}
	c := l.SynthClaim("SMN_014")
	if c == nil {
		t.Fatal("SynthClaim returned nil for a member node")
	}
	if c.SwapMode != protocol.SwapModeManualSwap {
		t.Errorf("SwapMode = %q, want manual_swap", c.SwapMode)
	}
	if len(c.AllowedPayloadCodes) != 1 || c.AllowedPayloadCodes[0] != "76683-6TA0A.06" {
		t.Errorf("AllowedPayloadCodes = %v, want only the home's pinned payload [76683-6TA0A.06]",
			c.AllowedPayloadCodes)
	}
}

// TestLoader_LoadablePayloadCodesAt_SharedUnchanged pins that the node-scoped helper
// leaves shared_window loaders byte-for-byte unchanged: every window offers the whole
// shared set (the operator picks at load time), so the dedicated-only fix can't
// regress markets/drains.
func TestLoader_LoadablePayloadCodesAt_SharedUnchanged(t *testing.T) {
	t.Parallel()
	l, err := NewSharedWindowLoader("loader:6", "SMN_001", RoleProduce, ReplenishmentOperator,
		[]Window{{Node: "SMN_001"}}, []PayloadCode{"P-A", "P-B", "P-C"})
	if err != nil {
		t.Fatalf("build loader: %v", err)
	}
	if got := l.LoadablePayloadCodesAt("SMN_001"); len(got) != 3 {
		t.Errorf("LoadablePayloadCodesAt(window) = %v, want the whole shared set (3)", got)
	}
}
