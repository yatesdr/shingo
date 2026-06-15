package loaders

import "testing"

func TestCheckHomeTripwire(t *testing.T) {
	t.Parallel()
	// Home node with 2 payloads → tripwire fires (the SLN_002 shape).
	bad := []MigrationClaim{{CoreNode: "SLN_002", Role: "produce", HomeLocation: true, Payloads: []string{"P1", "P2"}}}
	if err := CheckHomeTripwire(bad); err == nil {
		t.Error("expected tripwire error for a home node with 2 payloads")
	}
	// One payload per home → ok.
	if err := CheckHomeTripwire([]MigrationClaim{{CoreNode: "HOME-1", Role: "produce", HomeLocation: true, Payloads: []string{"P1"}}}); err != nil {
		t.Errorf("one payload per home should pass: %v", err)
	}
	// Non-home node with many payloads → NOT a tripwire (shared_window is legitimate).
	if err := CheckHomeTripwire([]MigrationClaim{{CoreNode: "SW-1", Role: "produce", HomeLocation: false, Payloads: []string{"P1", "P2", "P3"}}}); err != nil {
		t.Errorf("shared_window many-payload is not a tripwire: %v", err)
	}
	// Two separate home nodes, one payload each → ok (a real dedicated_positions loader).
	if err := CheckHomeTripwire([]MigrationClaim{
		{CoreNode: "POS-1", Role: "produce", HomeLocation: true, Payloads: []string{"P1"}},
		{CoreNode: "POS-2", Role: "produce", HomeLocation: true, Payloads: []string{"P2"}},
	}); err != nil {
		t.Errorf("two single-payload homes should pass: %v", err)
	}
}

func TestGroupIntoLoaders(t *testing.T) {
	t.Parallel()
	claims := []MigrationClaim{
		{CoreNode: "SW-A", Role: RoleProduce, Payload: "PA", Payloads: []string{"PA"}, ReorderPoint: 2, Thresholds: map[string]int{"PA": 100}, OutboundDest: "FG"},
		{CoreNode: "POS-1", Role: RoleProduce, Payload: "PH1", Payloads: []string{"PH1"}, ReorderPoint: 3, HomeLocation: true, OperatorStation: "HL-OPS"},
		{CoreNode: "POS-2", Role: RoleProduce, Payload: "PH2", Payloads: []string{"PH2"}, ReorderPoint: 1, HomeLocation: true, OperatorStation: "HL-OPS", Thresholds: map[string]int{"PH2": 50}},
		{CoreNode: "UNL", Role: RoleConsume, Payload: "PU", Payloads: []string{"PU"}, AutoPush: true},
	}
	derived, err := GroupIntoLoaders(claims)
	if err != nil {
		t.Fatalf("GroupIntoLoaders: %v", err)
	}
	if len(derived) != 3 {
		t.Fatalf("loaders = %d, want 3 (SW-A shared, HL-OPS dedicated, UNL)", len(derived))
	}
	byNode := map[string]DerivedLoader{}
	for _, d := range derived {
		byNode[d.Loader.Name] = d
	}

	sw := byNode["SW-A"]
	if sw.Loader.Layout != LayoutSharedWindow || len(sw.Payloads) != 1 || sw.Payloads[0].UOPThreshold != 100 {
		t.Errorf("SW-A = %+v / payloads %+v", sw.Loader, sw.Payloads)
	}
	// Step 1: a shared loader's anchor is materialised as its sole window member, so it
	// never resolves to zero members (the Edge empty-windows fallback becomes dead code).
	if len(sw.Homes) != 1 || sw.Homes[0].PositionNode != "SW-A" || sw.Homes[0].PayloadCode != "" {
		t.Errorf("SW-A homes = %+v, want 1 anchor window (SW-A, no per-position payload)", sw.Homes)
	}

	hl := byNode["HL-OPS"]
	if hl.Loader.Layout != LayoutDedicatedPositions || len(hl.Homes) != 2 {
		t.Fatalf("HL-OPS = %+v / homes %+v, want dedicated_positions + 2 homes (grouped by operator station)", hl.Loader, hl.Homes)
	}
	home := map[string]DerivedHome{}
	for _, h := range hl.Homes {
		home[h.PositionNode] = h
	}
	if home["POS-1"].PayloadCode != "PH1" || home["POS-1"].MinStock != 3 {
		t.Errorf("POS-1 home = %+v, want PH1/min3", home["POS-1"])
	}
	if home["POS-2"].UOPThreshold != 50 {
		t.Errorf("POS-2 threshold = %d, want 50", home["POS-2"].UOPThreshold)
	}

	if unl := byNode["UNL"]; unl.Loader.Replenishment != ReplenishmentAuto || unl.Loader.Role != RoleConsume || len(unl.Homes) != 1 || unl.Homes[0].PositionNode != "UNL" {
		t.Errorf("UNL = %+v / homes %+v, want consume/auto + 1 anchor window (unloader parity)", unl.Loader, unl.Homes)
	}
}

func TestGroupIntoLoaders_TripwireBlocks(t *testing.T) {
	t.Parallel()
	_, err := GroupIntoLoaders([]MigrationClaim{
		{CoreNode: "SLN_002", Role: RoleProduce, Payload: "P1", Payloads: []string{"P1", "P2"}, HomeLocation: true},
	})
	if err == nil {
		t.Error("expected GroupIntoLoaders to fail the tripwire for a home node with 2 payloads")
	}
}

func TestReplenishmentMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		c    MigrationClaim
		want string
	}{
		{"transitional→operator", MigrationClaim{Transitional: true, Role: RoleProduce}, ReplenishmentOperator},
		{"consume no-autopush→operator", MigrationClaim{Role: RoleConsume, AutoPush: false}, ReplenishmentOperator},
		{"consume autopush→auto", MigrationClaim{Role: RoleConsume, AutoPush: true}, ReplenishmentAuto},
		{"produce→auto", MigrationClaim{Role: RoleProduce}, ReplenishmentAuto},
	}
	for _, tc := range cases {
		if got := replenishment(tc.c); got != tc.want {
			t.Errorf("%s: replenishment = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLoaderFromClaimAndPayloadSet(t *testing.T) {
	t.Parallel()
	c := MigrationClaim{
		CoreNode: "LDR", Role: RoleProduce, Payloads: []string{"A", "B"}, ReorderPoint: 3,
		InboundSource: "IN", OutboundDest: "OUT", Thresholds: map[string]int{"A": 50},
	}
	l := LoaderFromClaim(c)
	if l.Name != "LDR" || l.Layout != LayoutSharedWindow || l.OutboundDest != "OUT" || l.InboundSource != "IN" {
		t.Errorf("loader = %+v", l)
	}
	if hl := LoaderFromClaim(MigrationClaim{CoreNode: "H", Role: RoleProduce, HomeLocation: true}); hl.Layout != LayoutDedicatedPositions {
		t.Errorf("home-location layout = %q, want dedicated_positions", hl.Layout)
	}
	ps := DerivePayloadSet(c)
	if len(ps) != 2 || ps[0].PayloadCode != "A" || ps[0].MinStock != 3 || ps[0].UOPThreshold != 50 || ps[1].UOPThreshold != 0 {
		t.Errorf("payload set = %+v, want A(min3,thr50) + B(min3,thr0)", ps)
	}
}
