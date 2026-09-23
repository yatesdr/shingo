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
	l, err := eng.loaderStore.LoaderByKey(domain.LoaderID(key), role)
	if err != nil || l == nil {
		t.Fatalf("resolve %s: loader=%v err=%v", key, l, err)
	}
	return l
}

// TestProjectCoreLoader_OptionsByBranch: the shared_window branch carries
// inbound, outbound, funnel_windows, changeover_load_directive, the bare bin
// type and auto_push; the dedicated_positions branch carries all of those
// except funnel_windows.
func TestProjectCoreLoader_OptionsByBranch(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	seedCoreLoader(t, eng,
		protocol.LoaderInfo{
			Name: "OPT-SW", LoaderKey: "loader:OPT-SW", Role: "consume", Layout: "shared_window",
			Replenishment: protocol.LoaderReplenishmentOperator, ConfigGen: 1,
			InboundSource: "FG-SUPER", OutboundDest: "EMPTY-TOTES",
			FunnelWindows: true, ChangeoverLoadDirective: true, BareBinTypeCode: "HALF-SW", AutoPush: true,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "OPT-SW-W1", Kind: "window"}},
			Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
		},
		protocol.LoaderInfo{
			Name: "OPT-DP", LoaderKey: "loader:OPT-DP", Role: "consume", Layout: "dedicated_positions",
			Replenishment: protocol.LoaderReplenishmentOperator, ConfigGen: 1,
			InboundSource: "FG-SUPER", OutboundDest: "EMPTY-TOTES",
			FunnelWindows: true, ChangeoverLoadDirective: true, BareBinTypeCode: "HALF-DP", AutoPush: true,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "OPT-DP-P1", PayloadCode: "PART-A", Kind: "position"}},
		},
	)

	sw := projectedByKey(t, eng, "loader:OPT-SW", domain.RoleConsume)
	if sw.InboundSource() != "FG-SUPER" || sw.OutboundDest() != "EMPTY-TOTES" {
		t.Errorf("shared inbound/outbound = %q/%q, want FG-SUPER/EMPTY-TOTES", sw.InboundSource(), sw.OutboundDest())
	}
	if !sw.FunnelWindows() || !sw.ChangeoverLoadDirective() {
		t.Errorf("shared funnel/directive = %v/%v, want true/true", sw.FunnelWindows(), sw.ChangeoverLoadDirective())
	}
	if got := sw.BareBinTypeCode(); got != "HALF-SW" {
		t.Errorf("shared bare bin type = %q, want HALF-SW", got)
	}
	if !sw.AutoPush() {
		t.Error("shared AutoPush = false, want true")
	}

	dp := projectedByKey(t, eng, "loader:OPT-DP", domain.RoleConsume)
	if dp.InboundSource() != "FG-SUPER" || dp.OutboundDest() != "EMPTY-TOTES" {
		t.Errorf("dedicated inbound/outbound = %q/%q, want FG-SUPER/EMPTY-TOTES", dp.InboundSource(), dp.OutboundDest())
	}
	if got := dp.BareBinTypeCode(); got != "HALF-DP" {
		t.Errorf("dedicated bare bin type = %q, want HALF-DP", got)
	}
	if !dp.AutoPush() {
		t.Error("dedicated AutoPush = false, want true")
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
		Replenishment: protocol.LoaderReplenishmentOperator, ConfigGen: 1, OutboundDest: "EMPTY-TOTES",
		Positions: []protocol.LoaderPosition{{CoreNodeName: "ZP-SW-W1", Kind: "window"}},
	})
	if l, err := eng.loaderStore.LoaderForNode("ZP-SW-W1"); err != nil || l == nil {
		t.Errorf("zero-payload shared unloader at its window: loader=%v err=%v; want it resolved", l, err)
	}
	for _, p := range []domain.PayloadCode{"", "PART-A"} {
		if l, err := eng.loaderStore.LoaderForPayload(p, domain.RoleConsume, false); l != nil {
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
			Replenishment: protocol.LoaderReplenishmentOperator, ConfigGen: 1, AutoPush: true,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "AP-SW-W1", Kind: "window"}},
			Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
		},
		protocol.LoaderInfo{
			Name: "AP-DP", LoaderKey: "loader:AP-DP", Role: "consume", Layout: "dedicated_positions",
			Replenishment: protocol.LoaderReplenishmentOperator, ConfigGen: 1, AutoPush: true,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "AP-DP-P1", PayloadCode: "PART-A", Kind: "position"}},
		},
		protocol.LoaderInfo{
			Name: "AP-OFF", LoaderKey: "loader:AP-OFF", Role: "consume", Layout: "shared_window",
			Replenishment: protocol.LoaderReplenishmentOperator, ConfigGen: 1,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "AP-OFF-W1", Kind: "window"}},
			Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
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
