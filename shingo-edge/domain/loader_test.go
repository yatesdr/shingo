package domain

import (
	"slices"
	"testing"

	"shingo/protocol"
)

// TestNewLoader_RejectsInvalidStates is the constructor gate. It pins that the
// two constructors reject every representable invalid state, so an invalid
// Loader never reaches the reservation seam. The strongest invariant — a shared
// layout with per-position payloads — is enforced by the type signatures (the
// shared constructor takes a payload SET, not positions) and so cannot even be
// written; the cases below cover the runtime-checked remainder.
func TestNewLoader_RejectsInvalidStates(t *testing.T) {
	t.Parallel()

	okWindows := []Window{{Node: "WELD-1-W1"}, {Node: "WELD-1-W2"}}
	okPayloads := []PayloadCode{"PART-A", "PART-B"}
	okPositions := []Position{{Node: "HOME-1", Payload: "PART-A"}, {Node: "HOME-2", Payload: "PART-B"}}

	shared := []struct {
		name    string
		id      LoaderID
		role    LoaderRole
		windows []Window
		payload []PayloadCode
	}{
		{"shared: empty id", "", RoleProduce, okWindows, okPayloads},
		{"shared: invalid role", "L", "bin_loader", okWindows, okPayloads},
		{"shared: zero windows", "L", RoleProduce, nil, okPayloads},
		{"shared: zero payloads", "L", RoleProduce, okWindows, nil},
		{"shared: empty window node", "L", RoleProduce, []Window{{Node: ""}}, okPayloads},
		{"shared: empty payload in set", "L", RoleProduce, okWindows, []PayloadCode{"PART-A", ""}},
	}
	for _, tc := range shared {
		t.Run(tc.name, func(t *testing.T) {
			if l, err := NewSharedWindowLoader(tc.id, tc.role, ReplenishmentThreshold, tc.windows, tc.payload); err == nil {
				t.Fatalf("expected error, got loader %+v", l)
			}
		})
	}

	dedicated := []struct {
		name      string
		id        LoaderID
		role      LoaderRole
		positions []Position
	}{
		{"dedicated: empty id", "", RoleProduce, okPositions},
		{"dedicated: invalid role", "L", "", okPositions},
		{"dedicated: zero positions", "L", RoleProduce, nil},
		{"dedicated: empty position node", "L", RoleProduce, []Position{{Node: "", Payload: "PART-A"}}},
	}
	for _, tc := range dedicated {
		t.Run(tc.name, func(t *testing.T) {
			if l, err := NewDedicatedPositionsLoader(tc.id, tc.role, ReplenishmentThreshold, tc.positions); err == nil {
				t.Fatalf("expected error, got loader %+v", l)
			}
		})
	}
}

// TestNewSharedWindowLoader_Valid confirms a well-formed shared loader builds and
// that the budget (the slot count) is DERIVED from the window count — it cannot be set
// to a mismatching value because it is never a parameter.
func TestNewSharedWindowLoader_Valid(t *testing.T) {
	t.Parallel()
	l, err := NewSharedWindowLoader("WELD-1", RoleProduce, ReplenishmentThreshold,
		[]Window{{Node: "WELD-1-W1"}, {Node: "WELD-1-W2"}, {Node: "WELD-1-W3"}},
		[]PayloadCode{"PART-A", "PART-B"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.ID() != "WELD-1" || !l.IsShared() || l.IsDedicated() {
		t.Errorf("identity/layout wrong: id=%s shared=%v", l.ID(), l.IsShared())
	}
	if l.slotCount != 3 {
		t.Errorf("slot count = %d, want 3 (= window count)", l.slotCount)
	}
	if got := l.DeliveryNodes(); len(got) != 3 || got[0] != "WELD-1-W1" || got[2] != "WELD-1-W3" {
		t.Errorf("DeliveryNodes = %v, want the three window nodes", got)
	}
	if len(l.PayloadSet()) != 2 || len(l.Positions()) != 0 {
		t.Errorf("shared loader should carry a payload set and no positions: %+v", l)
	}
}

// TestNewDedicatedPositionsLoader_Valid confirms a well-formed dedicated loader
// builds, derives the slot count from the position count, and accepts an unassigned
// (empty-payload) position — which is legal, distinct from a window.
func TestNewDedicatedPositionsLoader_Valid(t *testing.T) {
	t.Parallel()
	l, err := NewDedicatedPositionsLoader("SLN-2", RoleProduce, ReplenishmentOperator,
		[]Position{{Node: "HOME-1", Payload: "PART-A"}, {Node: "HOME-2", Payload: ""}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !l.IsDedicated() || l.IsShared() {
		t.Errorf("layout wrong: dedicated=%v", l.IsDedicated())
	}
	if l.slotCount != 2 {
		t.Errorf("slot count = %d, want 2 (= position count)", l.slotCount)
	}
	if !l.UsesOperatorStaging() {
		t.Errorf("replenishment=operator should report UsesOperatorStaging")
	}
	if got := l.DeliveryNodes(); len(got) != 2 || got[0] != "HOME-1" || got[1] != "HOME-2" {
		t.Errorf("DeliveryNodes = %v, want the two position nodes", got)
	}
	if len(l.windows) != 0 || len(l.PayloadSet()) != 0 {
		t.Errorf("dedicated loader should carry positions, no windows/shared set: %+v", l)
	}
}

// TestUsesOperatorStaging_TheSwitchMeansWhatItSays.
//
// THIS TEST PINNED THE OPPOSITE UNTIL THE DECISION WAS MADE, and it said so: it
// was a characterization test recording that a threshold loader with no
// threshold quietly re-routed to operator staging, with its own comment noting
// that "whether this fallback survives... is a decision that needs the current
// behavior nailed down first". The decision is made (owner, 2026-08-02):
// threshold is a thing you switch ON, not a thing you end up in.
//
// So the fallback is gone. Threshold on means the threshold path runs, for
// whatever thresholds are configured — none configured, nothing fires. That is a
// visible "not set up yet" rather than a different mode wearing this one's name,
// which is what the old behaviour was: every screen said "threshold" while the
// carriers came from the operator push.
//
// Whether its thresholds are set is Core's to say (its loader and inventory
// pages: "no threshold set"); the Edge holds no copy of the threshold.
func TestUsesOperatorStaging_TheSwitchMeansWhatItSays(t *testing.T) {
	t.Parallel()
	windows := []Window{{Node: "W1"}}
	payloads := []PayloadCode{"P1"}

	// Threshold switched ON: Core's threshold path owns it, whatever Core has
	// configured. It does NOT become an operator-staged loader behind the
	// operator's back.
	threshold, err := NewSharedWindowLoader("THR", RoleProduce, ReplenishmentThreshold, windows, payloads)
	if err != nil {
		t.Fatalf("build threshold: %v", err)
	}
	if threshold.UsesOperatorStaging() {
		t.Error("threshold loader: UsesOperatorStaging = true, want false — " +
			"switching threshold on must not silently put the loader in the other mode")
	}

	operator, err := NewSharedWindowLoader("OPS", RoleProduce, ReplenishmentOperator, windows, payloads)
	if err != nil {
		t.Fatalf("build operator: %v", err)
	}
	if !operator.UsesOperatorStaging() {
		t.Error("operator loader: UsesOperatorStaging = false, want true")
	}
}

// TestPositionIsBuffer pins the classification, which is the whole
// point of carrying home_kind: an unpinned HOME and a BUFFER slot both have an
// empty payload, and before Core said which was which the Edge called both of
// them buffers. Core keeps them apart — InSourcePool leaves an unassigned home
// out of the loader's source pool — so a carrier moved onto one is parked where
// nothing will source it.
func TestPositionIsBuffer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pos  Position
		want bool
		why  string
	}{
		{"core says buffer", Position{HomeKind: HomeKindBuffer}, true,
			"an explicit buffer is a buffer"},
		{"core says buffer, payload somehow set", Position{HomeKind: HomeKindBuffer, Payload: "PA"}, true,
			"Core's answer wins over the payload; the payload is not the discriminator any more"},
		{"core says home, pinned", Position{HomeKind: HomeKindHome, Payload: "PA"}, false,
			"a pinned home is not a buffer"},
		{"core says home, UNPINNED", Position{HomeKind: HomeKindHome}, false,
			"THE BUG: an unassigned home has no payload but is not a buffer, and Core excludes it from the source pool"},
	} {
		if got := tc.pos.IsBuffer(); got != tc.want {
			t.Errorf("%s: IsBuffer() = %v, want %v — %s", tc.name, got, tc.want, tc.why)
		}
	}
}

// TestLoaderFlowOptions pins the config carried on the aggregate so the completion
// handlers read outbound off the Loader instead of the legacy claim. Unset fields
// are empty.
func TestLoaderFlowOptions(t *testing.T) {
	t.Parallel()
	l, err := NewDedicatedPositionsLoader("DECK", RoleProduce, ReplenishmentThreshold,
		[]Position{{Node: "POS-1", Payload: "PA"}},
		WithInboundSource("SYN_BUF_Deck"), WithOutboundDest("SYN_SM_Comp"))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if l.OutboundDest() != "SYN_SM_Comp" {
		t.Errorf("OutboundDest = %q, want SYN_SM_Comp", l.OutboundDest())
	}
	if l.InboundSource() != "SYN_BUF_Deck" {
		t.Errorf("InboundSource = %q, want SYN_BUF_Deck", l.InboundSource())
	}
	bare, err := NewSharedWindowLoader("L", RoleProduce, ReplenishmentThreshold,
		[]Window{{Node: "L"}}, []PayloadCode{"P1"})
	if err != nil {
		t.Fatalf("NewSharedWindowLoader: %v", err)
	}
	if bare.OutboundDest() != "" || bare.InboundSource() != "" {
		t.Errorf("unset outbound/inbound should be empty, got %q/%q", bare.OutboundDest(), bare.InboundSource())
	}
}

// TestLoader_AccessorsReturnCopies pins immutability: mutating a returned slice
// must not corrupt the aggregate's internal state.
func TestLoader_AccessorsReturnCopies(t *testing.T) {
	t.Parallel()
	l, err := NewSharedWindowLoader("L", RoleProduce, ReplenishmentThreshold,
		[]Window{{Node: "W1"}}, []PayloadCode{"P1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ps := l.PayloadSet()
	ps[0] = "TAMPERED"
	if l.PayloadSet()[0] != "P1" {
		t.Errorf("PayloadSet() leaked internal state: %v", l.PayloadSet())
	}
}

// TestPartialParts_TheLinesThatSourceFromStageTwo: what PARTIAL ON offers at a
// stage 2 is the parts of the produce claims whose inbound source is one of
// its windows or a group holding one, in the claims' order, each once. A
// consume claim, a loader claim and a line sourcing elsewhere add nothing, and
// a loader that takes no bare carts offers nothing.
func TestPartialParts_TheLinesThatSourceFromStageTwo(t *testing.T) {
	t.Parallel()
	windows := []Window{{Node: "S2-W1"}, {Node: "S2-W2"}}
	s2, err := NewSharedWindowLoader("loader:S2", RoleProduce, ReplenishmentOperator, windows, nil, WithTakesBare(true))
	if err != nil {
		t.Fatalf("build stage 2: %v", err)
	}
	groups := map[string][]string{"S2G": {"S2-W1", "S2-W2"}, "OTHER": {"X1"}}
	line := func(role protocol.ClaimRole, mode protocol.SwapMode, in string, parts ...string) NodeClaim {
		return NodeClaim{Role: role, SwapMode: mode, InboundSource: in, PayloadCode: parts[0], AllowedPayloadCodes: parts}
	}
	claims := []NodeClaim{
		line(protocol.ClaimRoleProduce, protocol.SwapModeSequential, "S2G", "PART-1"),
		line(protocol.ClaimRoleProduce, protocol.SwapModeSequential, "S2-W2", "PART-2", "PART-1"),
		line(protocol.ClaimRoleProduce, protocol.SwapModeSequential, "OTHER", "PART-3"),
		line(protocol.ClaimRoleConsume, protocol.SwapModeSequential, "S2G", "PART-4"),
		line(protocol.ClaimRoleProduce, protocol.SwapModeManualSwap, "S2G", "PART-5"),
	}
	if got := s2.PartialParts(claims, groups); !slices.Equal(got, []string{"PART-1", "PART-2"}) {
		t.Errorf("PartialParts = %v, want [PART-1 PART-2]", got)
	}
	plain, err := NewSharedWindowLoader("loader:U", RoleConsume, ReplenishmentOperator, windows, nil)
	if err != nil {
		t.Fatalf("build unloader: %v", err)
	}
	if got := plain.PartialParts(claims, groups); got != nil {
		t.Errorf("PartialParts at a loader that takes no bare carts = %v, want nil", got)
	}
}
