package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
)

// loader_store_options_test.go — which LoaderInfo options survive the whole
// hop (wire -> core_loaders cache -> projectCoreLoader -> domain.Loader) in each
// of the two projection branches. SetCoreLoaders is the live path: it writes the
// cache and swaps the snapshot, exactly as a node-list sync does.

func projectedByKey(t *testing.T, eng *Engine, key string, role domain.LoaderRole) *domain.Loader {
	t.Helper()
	ls, err := eng.loaderStore.Loaders(role)
	if err != nil {
		t.Fatalf("resolve %s: %v", key, err)
	}
	for _, l := range ls {
		if l.ID() == domain.LoaderID(key) {
			return l
		}
	}
	t.Fatalf("resolve %s: no %s loader with that key", key, role)
	return nil
}

// TestProjectCoreLoader_OptionsByBranch: the shared_window branch carries
// inbound, outbound, funnel_windows, changeover_load_directive, leaves_bare,
// takes_bare, auto_push and the two board settings; the dedicated_positions branch
// carries all of those except funnel_windows.
func TestProjectCoreLoader_OptionsByBranch(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	seedCoreLoader(t, eng,
		protocol.LoaderInfo{
			Name: "OPT-SW", LoaderKey: "loader:OPT-SW", Role: "consume", Layout: "shared_window",
			Replenishment: protocol.LoaderReplenishmentOperator,
			InboundSource: "FG-SUPER", OutboundDest: "EMPTY-TOTES",
			FunnelWindows: true, ChangeoverLoadDirective: true, LeavesBare: true, TakesBare: true, AutoPush: true,
			BoardHidesIdle: true, BoardShowsAllParts: true,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "OPT-SW-W1"}},
			Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
		},
		protocol.LoaderInfo{
			Name: "OPT-DP", LoaderKey: "loader:OPT-DP", Role: "consume", Layout: "dedicated_positions",
			Replenishment: protocol.LoaderReplenishmentOperator,
			InboundSource: "FG-SUPER", OutboundDest: "EMPTY-TOTES",
			FunnelWindows: true, ChangeoverLoadDirective: true, LeavesBare: true, TakesBare: true, AutoPush: true,
			BoardHidesIdle: true, BoardShowsAllParts: true,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "OPT-DP-P1", PayloadCode: "PART-A"}},
		},
	)

	sw := projectedByKey(t, eng, "loader:OPT-SW", domain.RoleConsume)
	if sw.InboundSource() != "FG-SUPER" || sw.OutboundDest() != "EMPTY-TOTES" {
		t.Errorf("shared inbound/outbound = %q/%q, want FG-SUPER/EMPTY-TOTES", sw.InboundSource(), sw.OutboundDest())
	}
	if !sw.FunnelWindows() || !sw.ChangeoverLoadDirective() {
		t.Errorf("shared funnel/directive = %v/%v, want true/true", sw.FunnelWindows(), sw.ChangeoverLoadDirective())
	}
	if !sw.LeavesBare() {
		t.Error("shared LeavesBare = false, want true")
	}
	if !sw.TakesBare() || !sw.SynthClaim(sw.DeliveryNodes()[0]).TakesBare {
		t.Error("shared TakesBare (loader or synthesized claim) = false, want true")
	}
	if !sw.SynthClaim(sw.DeliveryNodes()[0]).AutoPush {
		t.Error("shared synthesized claim AutoPush = false, want true")
	}
	if !sw.BoardHidesIdle() || !sw.BoardShowsAllParts() {
		t.Errorf("shared board hides/all = %v/%v, want true/true", sw.BoardHidesIdle(), sw.BoardShowsAllParts())
	}

	dp := projectedByKey(t, eng, "loader:OPT-DP", domain.RoleConsume)
	if dp.InboundSource() != "FG-SUPER" || dp.OutboundDest() != "EMPTY-TOTES" {
		t.Errorf("dedicated inbound/outbound = %q/%q, want FG-SUPER/EMPTY-TOTES", dp.InboundSource(), dp.OutboundDest())
	}
	if !dp.LeavesBare() {
		t.Error("dedicated LeavesBare = false, want true")
	}
	if !dp.TakesBare() || !dp.SynthClaim(dp.DeliveryNodes()[0]).TakesBare {
		t.Error("dedicated TakesBare (loader or synthesized claim) = false, want true")
	}
	if !dp.SynthClaim(dp.DeliveryNodes()[0]).AutoPush {
		t.Error("dedicated synthesized claim AutoPush = false, want true")
	}
	if !dp.BoardHidesIdle() || !dp.BoardShowsAllParts() {
		t.Errorf("dedicated board hides/all = %v/%v, want true/true", dp.BoardHidesIdle(), dp.BoardShowsAllParts())
	}
	// Not passed by the dedicated branch: funnel_windows is meaningless there
	// (positions never share a budget). The directive used to be dropped too.
	if dp.FunnelWindows() {
		t.Error("dedicated funnel = true, want false (the branch does not pass it)")
	}
	if !dp.ChangeoverLoadDirective() {
		t.Error("dedicated directive = false, want true")
	}
}

// TestProjectCoreLoader_ZeroPayloadSharedConsumeProjects: a shared_window
// unloader Core sends with no payloads projects and resolves at its window,
// but no payload resolves to it.
func TestProjectCoreLoader_ZeroPayloadSharedConsumeProjects(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	seedCoreLoader(t, eng, protocol.LoaderInfo{
		Name: "ZP-SW", LoaderKey: "loader:ZP-SW", Role: "consume", Layout: "shared_window",
		Replenishment: protocol.LoaderReplenishmentOperator, OutboundDest: "EMPTY-TOTES",
		Positions: []protocol.LoaderPosition{{CoreNodeName: "ZP-SW-W1"}},
	})
	if l, err := eng.loaderStore.LoaderForNode("ZP-SW-W1"); err != nil || l == nil {
		t.Errorf("zero-payload shared unloader at its window: loader=%v err=%v; want it resolved", l, err)
	}
	for _, p := range []domain.PayloadCode{"", "PART-A"} {
		if l, err := eng.loaderStore.LoaderForPayload(p, domain.RoleConsume); l != nil {
			t.Errorf("LoaderForPayload(%q) = %v (err %v); want no loader", p, l.ID(), err)
		}
	}
}

// TestSynthClaim_AutoPushFromTheLoader: the claim a Core-owned loader window
// acts on carries the loader's AutoPush, in each projection branch. It used to
// be false whatever Core sent: the stored claim was the only carrier of the
// flag and SynthClaim never set it. A loader Core sends without it stays false.
func TestSynthClaim_AutoPushFromTheLoader(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	seedCoreLoader(t, eng,
		protocol.LoaderInfo{
			Name: "AP-SW", LoaderKey: "loader:AP-SW", Role: "consume", Layout: "shared_window",
			Replenishment: protocol.LoaderReplenishmentOperator, AutoPush: true,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "AP-SW-W1"}},
			Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
		},
		protocol.LoaderInfo{
			Name: "AP-DP", LoaderKey: "loader:AP-DP", Role: "consume", Layout: "dedicated_positions",
			Replenishment: protocol.LoaderReplenishmentOperator, AutoPush: true,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "AP-DP-P1", PayloadCode: "PART-A"}},
		},
		protocol.LoaderInfo{
			Name: "AP-OFF", LoaderKey: "loader:AP-OFF", Role: "consume", Layout: "shared_window",
			Replenishment: protocol.LoaderReplenishmentOperator,
			Positions:     []protocol.LoaderPosition{{CoreNodeName: "AP-OFF-W1"}},
			Payloads:      []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
		},
	)
	for node, want := range map[string]bool{"AP-SW-W1": true, "AP-DP-P1": true, "AP-OFF-W1": false} {
		c := eng.synthLoaderClaim(node)
		if c == nil {
			t.Fatalf("%s: no synthesized claim", node)
		}
		if c.AutoPush != want {
			t.Errorf("%s: SynthClaim.AutoPush = %v, want %v", node, c.AutoPush, want)
		}
	}
}
