package dispatch

import "testing"

// Pure unit tests for the tiered depth-ordered entry classifier — the decision
// heart, with all liveness already resolved into the inputs (no DB).

func TestClassifyLaneEntry_Tier1_SameOriginNeverGates(t *testing.T) {
	t.Parallel()
	// Two press pushes: same origin, one deeper. The shallow one must NOT be parked
	// by the deeper same-origin partner — Tier 1 co-dispatch.
	self := laneEntryOrder{id: 1, depth: 0, origin: "style:P|S"}
	others := []laneEntryOrder{{id: 2, depth: 2, origin: "style:P|S", grouped: true}}
	if c := classifyLaneEntry(self, others); c != "" {
		t.Errorf("same-origin partner (even deeper/grouped) must not park; got cause %q", c)
	}
}

func TestClassifyLaneEntry_Tier2_DeeperCrossOriginParks(t *testing.T) {
	t.Parallel()
	// A shallow store with a DEEPER cross-origin store still pending → park.
	self := laneEntryOrder{id: 1, depth: 0, origin: "style:P|A"}
	others := []laneEntryOrder{{id: 2, depth: 2, origin: "style:P|B"}}
	if c := classifyLaneEntry(self, others); c != causeLaneDeeperPending {
		t.Errorf("deeper cross-origin store must park the shallower; got %q, want %q", c, causeLaneDeeperPending)
	}
}

func TestClassifyLaneEntry_Tier2_ShallowerCrossOriginDoesNotPark(t *testing.T) {
	t.Parallel()
	// We are the DEEPER store; a shallower cross-origin store is present but not a
	// group. We go first (the shallower one waits for us, symmetrically).
	self := laneEntryOrder{id: 1, depth: 2, origin: "style:P|A"}
	others := []laneEntryOrder{{id: 2, depth: 0, origin: "style:P|B", grouped: false}}
	if c := classifyLaneEntry(self, others); c != "" {
		t.Errorf("a shallower cross-origin non-group store must not park us; got %q", c)
	}
}

func TestClassifyLaneEntry_Tier3_ActiveCrossOriginGroupParks(t *testing.T) {
	t.Parallel()
	// We are the DEEPER store, but a SHALLOWER cross-origin GROUP is active in the
	// lane (co-dispatched partners whose shallow member bypassed the depth gate).
	// We must wait for the group to complete — Tier 3.
	self := laneEntryOrder{id: 1, depth: 3, origin: "style:P|A"}
	others := []laneEntryOrder{{id: 2, depth: 0, origin: "style:P|B", grouped: true}}
	if c := classifyLaneEntry(self, others); c != causeLaneGroupActive {
		t.Errorf("an active cross-origin group must park a newcomer; got %q, want %q", c, causeLaneGroupActive)
	}
}

func TestClassifyLaneEntry_NoOthersAdmits(t *testing.T) {
	t.Parallel()
	if c := classifyLaneEntry(laneEntryOrder{id: 1, depth: 1, origin: "style:P|A"}, nil); c != "" {
		t.Errorf("empty lane must admit; got %q", c)
	}
}

func TestClassifyLaneEntry_UnclassifiedTreatedAsCrossOrigin(t *testing.T) {
	t.Parallel()
	// Two unclassified orders (empty origin — e.g. loaders absent from the mirror)
	// are NOT treated as same-origin: the shallower still parks behind the deeper.
	self := laneEntryOrder{id: 1, depth: 0, origin: ""}
	others := []laneEntryOrder{{id: 2, depth: 2, origin: ""}}
	if c := classifyLaneEntry(self, others); c != causeLaneDeeperPending {
		t.Errorf("unclassified orders must depth-order (not co-dispatch); got %q", c)
	}
}

func TestClassifyLaneEntry_IgnoresSelf(t *testing.T) {
	t.Parallel()
	// The order's own row in the active set must not gate it.
	self := laneEntryOrder{id: 7, depth: 0, origin: "style:P|A"}
	others := []laneEntryOrder{{id: 7, depth: 2, origin: "style:P|A", grouped: true}}
	if c := classifyLaneEntry(self, others); c != "" {
		t.Errorf("self row must be ignored; got %q", c)
	}
}

func TestSameOrigin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want bool
	}{
		{"style:P|S", "style:P|S", true},
		{"compound:9", "compound:9", true},
		{"style:P|S", "style:P|T", false},
		{"", "", false}, // unclassified never matches
		{"style:P|S", "", false},
	}
	for _, c := range cases {
		if got := sameOrigin(c.a, c.b); got != c.want {
			t.Errorf("sameOrigin(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestOriginKeyForStyles_CanonicalAndOrderIndependent(t *testing.T) {
	t.Parallel()
	// The same set in any order yields the same key; empty → "".
	k1 := originKeyForStyles([]string{"P|B", "P|A"})
	k2 := originKeyForStyles([]string{"P|A", "P|B"})
	if k1 != k2 || k1 == "" {
		t.Errorf("origin key must be canonical + order-independent; k1=%q k2=%q", k1, k2)
	}
	if got := originKeyForStyles(nil); got != "" {
		t.Errorf("empty style set must yield empty key; got %q", got)
	}
}
