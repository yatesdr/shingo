package domain

import "testing"

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
			if l, err := NewSharedWindowLoader(tc.id, "n", tc.role, ReplenishmentThreshold, tc.windows, tc.payload); err == nil {
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
			if l, err := NewDedicatedPositionsLoader(tc.id, "n", tc.role, ReplenishmentThreshold, tc.positions); err == nil {
				t.Fatalf("expected error, got loader %+v", l)
			}
		})
	}
}

// TestNewSharedWindowLoader_Valid confirms a well-formed shared loader builds and
// that the budget (SlotCount) is DERIVED from the window count — it cannot be set
// to a mismatching value because it is never a parameter.
func TestNewSharedWindowLoader_Valid(t *testing.T) {
	t.Parallel()
	l, err := NewSharedWindowLoader("WELD-1", "Weld 1 loader", RoleProduce, ReplenishmentThreshold,
		[]Window{{Node: "WELD-1-W1"}, {Node: "WELD-1-W2"}, {Node: "WELD-1-W3"}},
		[]PayloadCode{"PART-A", "PART-B"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.ID() != "WELD-1" || !l.IsShared() || l.IsDedicated() {
		t.Errorf("identity/layout wrong: id=%s shared=%v", l.ID(), l.IsShared())
	}
	if l.SlotCount() != 3 {
		t.Errorf("SlotCount = %d, want 3 (= window count)", l.SlotCount())
	}
	if got := l.DeliveryNodes(); len(got) != 3 || got[0] != "WELD-1-W1" || got[2] != "WELD-1-W3" {
		t.Errorf("DeliveryNodes = %v, want the three window nodes", got)
	}
	if len(l.PayloadSet()) != 2 || len(l.Positions()) != 0 {
		t.Errorf("shared loader should carry a payload set and no positions: %+v", l)
	}
}

// TestNewDedicatedPositionsLoader_Valid confirms a well-formed dedicated loader
// builds, derives SlotCount from the position count, and accepts an unassigned
// (empty-payload) position — which is legal, distinct from a window.
func TestNewDedicatedPositionsLoader_Valid(t *testing.T) {
	t.Parallel()
	l, err := NewDedicatedPositionsLoader("SLN-2", "Home loader", RoleProduce, ReplenishmentOperator,
		[]Position{{Node: "HOME-1", Payload: "PART-A"}, {Node: "HOME-2", Payload: ""}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !l.IsDedicated() || l.IsShared() {
		t.Errorf("layout wrong: dedicated=%v", l.IsDedicated())
	}
	if l.SlotCount() != 2 {
		t.Errorf("SlotCount = %d, want 2 (= position count)", l.SlotCount())
	}
	if !l.IsOperatorDriven() {
		t.Errorf("replenishment=operator should report IsOperatorDriven")
	}
	if got := l.DeliveryNodes(); len(got) != 2 || got[0] != "HOME-1" || got[1] != "HOME-2" {
		t.Errorf("DeliveryNodes = %v, want the two position nodes", got)
	}
	if len(l.Windows()) != 0 || len(l.PayloadSet()) != 0 {
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
// MisconfiguredThreshold now carries the whole weight of surfacing that state,
// because nothing feeds such a loader at all.
func TestUsesOperatorStaging_TheSwitchMeansWhatItSays(t *testing.T) {
	t.Parallel()
	windows := []Window{{Node: "W1"}}
	payloads := []PayloadCode{"P1"}

	configured, err := NewSharedWindowLoader("CFG", "n", RoleProduce, ReplenishmentThreshold,
		windows, payloads, WithUOPThreshold(map[PayloadCode]int{"P1": 100}))
	if err != nil {
		t.Fatalf("build configured: %v", err)
	}
	if configured.UsesOperatorStaging() {
		t.Error("threshold loader WITH a threshold: UsesOperatorStaging = true, want false — the automatic path owns it")
	}
	if configured.MisconfiguredThreshold() {
		t.Error("threshold loader WITH a threshold: MisconfiguredThreshold = true, want false")
	}

	// Threshold switched ON with nothing configured: the threshold path still
	// owns it and simply has nothing to fire on. It does NOT become an
	// operator-staged loader behind the operator's back.
	bare, err := NewSharedWindowLoader("BARE", "n", RoleProduce, ReplenishmentThreshold, windows, payloads)
	if err != nil {
		t.Fatalf("build bare: %v", err)
	}
	if bare.UsesOperatorStaging() {
		t.Error("threshold loader with NO threshold: UsesOperatorStaging = true, want false — " +
			"switching threshold on must not silently put the loader in the other mode")
	}
	if !bare.MisconfiguredThreshold() {
		t.Error("threshold loader with NO threshold: MisconfiguredThreshold = false, want true — " +
			"with no fallback left, this flag is the only thing between that configuration " +
			"and a loader nobody feeds")
	}

	operator, err := NewSharedWindowLoader("OPS", "n", RoleProduce, ReplenishmentOperator, windows, payloads)
	if err != nil {
		t.Fatalf("build operator: %v", err)
	}
	if !operator.UsesOperatorStaging() {
		t.Error("operator loader: UsesOperatorStaging = false, want true")
	}
	if operator.MisconfiguredThreshold() {
		t.Error("operator loader: MisconfiguredThreshold = true, want false — operator mode is a choice, not a misconfiguration")
	}
}

// TestReservationTarget pins the per-layout reservation semantics across all three
// loader TYPES + the same-payload-two-positions member-aware routing:
//   - MULTI-WINDOW shared: funnels to the anchor (flag off) / spreads across windows
//     (flag on, budget = slot count); member is ignored (windows share one budget).
//   - SINGLE-WINDOW shared: its one window, budget 1.
//   - DEDICATED: a payload maps to its position; when member names one of two
//     same-payload positions, route THERE (not first-match); no member → first-match.
func TestReservationTarget(t *testing.T) {
	t.Parallel()

	// MULTI-WINDOW shared.
	shared, err := NewSharedWindowLoader("WELD-1", "n", RoleProduce, ReplenishmentThreshold,
		[]Window{{Node: "WELD-1-W1"}, {Node: "WELD-1-W2"}}, []PayloadCode{"P1"})
	if err != nil {
		t.Fatalf("build shared: %v", err)
	}
	if nodes, budget := shared.ReservationTarget("", "P1", false); len(nodes) != 1 || nodes[0] != "WELD-1-W1" || budget != 1 {
		t.Errorf("multi flag off = (%v, %d), want ([WELD-1-W1], 1) — funnel to the first WINDOW, not the loader identity", nodes, budget)
	}
	if nodes, budget := shared.ReservationTarget("", "P1", true); len(nodes) != 2 || budget != 2 {
		t.Errorf("multi flag on = (%v, %d), want (2 windows, 2) — spread across windows", nodes, budget)
	}
	if nodes, budget := shared.ReservationTarget("", "NOPE", true); nodes != nil || budget != 0 {
		t.Errorf("non-served payload = (%v, %d), want (nil, 0)", nodes, budget)
	}
	if nodes, _ := shared.ReservationTarget("", "", false); len(nodes) != 1 {
		t.Errorf("blank payload should still resolve a target, got %v", nodes)
	}
	// member is ignored for shared — the seam's free-window assignment picks the slot.
	if nodes, budget := shared.ReservationTarget("WELD-1-W2", "P1", true); len(nodes) != 2 || budget != 2 {
		t.Errorf("shared ignores member = (%v, %d), want (2 windows, 2)", nodes, budget)
	}

	// SINGLE-WINDOW shared.
	single, err := NewSharedWindowLoader("LDR-1", "n", RoleProduce, ReplenishmentThreshold,
		[]Window{{Node: "LDR-1"}}, []PayloadCode{"P1"})
	if err != nil {
		t.Fatalf("build single: %v", err)
	}
	if nodes, budget := single.ReservationTarget("", "P1", true); len(nodes) != 1 || nodes[0] != "LDR-1" || budget != 1 {
		t.Errorf("single-window flag on = (%v, %d), want ([LDR-1], 1)", nodes, budget)
	}

	// DEDICATED with TWO same-payload positions.
	ded, err := NewDedicatedPositionsLoader("DECK", "n", RoleProduce, ReplenishmentThreshold,
		[]Position{{Node: "POS-1", Payload: "PA"}, {Node: "POS-2", Payload: "PA"}, {Node: "POS-3", Payload: "PB"}})
	if err != nil {
		t.Fatalf("build dedicated: %v", err)
	}
	// member names POS-2 (the second same-payload position) → route THERE, not first-match POS-1.
	if nodes, budget := ded.ReservationTarget("POS-2", "PA", false); len(nodes) != 1 || nodes[0] != "POS-2" || budget != 1 {
		t.Errorf("dedicated member-aware PA@POS-2 = (%v, %d), want ([POS-2], 1) — must NOT first-match POS-1", nodes, budget)
	}
	if nodes, _ := ded.ReservationTarget("POS-1", "PA", false); len(nodes) != 1 || nodes[0] != "POS-1" {
		t.Errorf("dedicated member-aware PA@POS-1 = %v, want [POS-1]", nodes)
	}
	// No member named → first-match (legacy DemandSignal / operator request).
	if nodes, _ := ded.ReservationTarget("", "PA", false); len(nodes) != 1 || nodes[0] != "POS-1" {
		t.Errorf("dedicated no-member PA = %v, want first-match [POS-1]", nodes)
	}
	// Distinct payload maps to its sole position.
	if nodes, budget := ded.ReservationTarget("", "PB", false); len(nodes) != 1 || nodes[0] != "POS-3" || budget != 1 {
		t.Errorf("dedicated PB = (%v, %d), want ([POS-3], 1)", nodes, budget)
	}
}

// TestLoaderFlowOptions pins the config carried on the aggregate so the completion
// handlers read outbound off the Loader instead of the legacy claim. Unset fields
// are empty.
func TestLoaderFlowOptions(t *testing.T) {
	t.Parallel()
	l, err := NewDedicatedPositionsLoader("DECK", "n", RoleProduce, ReplenishmentThreshold,
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
	bare, _ := NewSharedWindowLoader("L", "n", RoleProduce, ReplenishmentThreshold,
		[]Window{{Node: "L"}}, []PayloadCode{"P1"})
	if bare.OutboundDest() != "" || bare.InboundSource() != "" {
		t.Errorf("unset outbound/inbound should be empty, got %q/%q", bare.OutboundDest(), bare.InboundSource())
	}
}

// TestLoader_AccessorsReturnCopies pins immutability: mutating a returned slice
// must not corrupt the aggregate's internal state.
func TestLoader_AccessorsReturnCopies(t *testing.T) {
	t.Parallel()
	l, err := NewSharedWindowLoader("L", "n", RoleProduce, ReplenishmentThreshold,
		[]Window{{Node: "W1"}}, []PayloadCode{"P1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ws := l.Windows()
	ws[0].Node = "TAMPERED"
	if l.Windows()[0].Node != "W1" {
		t.Errorf("Windows() leaked internal state: %v", l.Windows())
	}
	ps := l.PayloadSet()
	ps[0] = "TAMPERED"
	if l.PayloadSet()[0] != "P1" {
		t.Errorf("PayloadSet() leaked internal state: %v", l.PayloadSet())
	}
}

// TestPayloadsMissingThreshold_PartialCoverageIsTheOneThatHides.
//
// A loader with a threshold on SOME of its payloads passes every check that
// asks "is a threshold configured" — one is — while the rest are ordered by
// nobody. Nothing fires for them and nothing says so. It is the same silence as
// the mode fallback that used to sit above it, one level down: not a loader in
// the wrong mode, but parts inside a right one that no path covers.
//
// Owner's ask, 2026-08-02: say how many, and point at where to fix it.
func TestPayloadsMissingThreshold_PartialCoverageIsTheOneThatHides(t *testing.T) {
	t.Parallel()
	windows := []Window{{Node: "W1"}}
	set := []PayloadCode{"P1", "P2", "P3", "P4", "P5"}

	partial, err := NewSharedWindowLoader("PART", "n", RoleProduce, ReplenishmentThreshold,
		windows, set, WithUOPThreshold(map[PayloadCode]int{"P1": 100, "P3": 50, "P5": 25}))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	missing := partial.PayloadsMissingThreshold()
	if len(missing) != 2 || missing[0] != "P2" || missing[1] != "P4" {
		t.Errorf("missing = %v, want [P2 P4] — three of five are set, so this loader looks "+
			"configured to every check that only asks whether A threshold exists", missing)
	}
	// And it is NOT the severe case: something does fire here.
	if partial.MisconfiguredThreshold() {
		t.Error("MisconfiguredThreshold = true — that flag means NOTHING fires; " +
			"conflating it with partial coverage loses the difference between " +
			"a loader nobody feeds and two parts nobody orders")
	}

	full, err := NewSharedWindowLoader("FULL", "n", RoleProduce, ReplenishmentThreshold,
		windows, []PayloadCode{"P1"}, WithUOPThreshold(map[PayloadCode]int{"P1": 100}))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := full.PayloadsMissingThreshold(); len(got) != 0 {
		t.Errorf("fully configured loader reports %v missing, want none", got)
	}

	// An OPERATOR loader has no thresholds by design — reporting them missing
	// would put a permanent complaint on every correctly-configured loader.
	ops, err := NewSharedWindowLoader("OPS", "n", RoleProduce, ReplenishmentOperator, windows, set)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := ops.PayloadsMissingThreshold(); len(got) != 0 {
		t.Errorf("operator loader reports %v missing, want none — a threshold is meaningless there", got)
	}
}
