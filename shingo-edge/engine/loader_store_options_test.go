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
// inbound, outbound, funnel_windows, changeover_load_directive and the bare
// bin type; the dedicated_positions branch carries inbound, outbound and the
// bare bin type.
func TestProjectCoreLoader_OptionsByBranch(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	seedCoreLoader(t, eng,
		protocol.LoaderInfo{
			Name: "OPT-SW", LoaderKey: "loader:OPT-SW", Role: "consume", Layout: "shared_window",
			Replenishment: protocol.LoaderReplenishmentOperator, ConfigGen: 1,
			InboundSource: "FG-SUPER", OutboundDest: "EMPTY-TOTES",
			FunnelWindows: true, ChangeoverLoadDirective: true, BareBinTypeCode: "HALF-SW",
			Positions: []protocol.LoaderPosition{{CoreNodeName: "OPT-SW-W1", Kind: "window"}},
			Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
		},
		protocol.LoaderInfo{
			Name: "OPT-DP", LoaderKey: "loader:OPT-DP", Role: "consume", Layout: "dedicated_positions",
			Replenishment: protocol.LoaderReplenishmentOperator, ConfigGen: 1,
			InboundSource: "FG-SUPER", OutboundDest: "EMPTY-TOTES",
			FunnelWindows: true, ChangeoverLoadDirective: true, BareBinTypeCode: "HALF-DP",
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

	dp := projectedByKey(t, eng, "loader:OPT-DP", domain.RoleConsume)
	if dp.InboundSource() != "FG-SUPER" || dp.OutboundDest() != "EMPTY-TOTES" {
		t.Errorf("dedicated inbound/outbound = %q/%q, want FG-SUPER/EMPTY-TOTES", dp.InboundSource(), dp.OutboundDest())
	}
	if got := dp.BareBinTypeCode(); got != "HALF-DP" {
		t.Errorf("dedicated bare bin type = %q, want HALF-DP", got)
	}
	// Not passed by the dedicated branch: funnel_windows is meaningless there
	// (positions never share a budget), and changeover_load_directive is read
	// off the shared branch only.
	if dp.FunnelWindows() || dp.ChangeoverLoadDirective() {
		t.Errorf("dedicated funnel/directive = %v/%v, want false/false (the branch does not pass them)",
			dp.FunnelWindows(), dp.ChangeoverLoadDirective())
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
