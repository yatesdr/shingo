package www

import (
	"reflect"
	"testing"
)

// Interface-width tripwires for the Phase 6.5 split.
//
// WIDTH CHANGES REQUIRE AN OWNER CONVERSATION. The split exists so a CRUD
// handler cannot reach an orchestration verb: h.engine is ServiceAccess,
// h.orchestration is EngineOrchestration, and the compiler enforces the
// boundary. Adding a method to ServiceAccess widens what every handler in this
// package can reach, which is the drift these tests exist to make visible.
//
// This REPLACES the tripwire that used to be cited from implementation-plan.md.
// That document is not in this repo and never was — it lives at
// docs/plans/implementation-plan.md in the GitHub root — so the ratchet it
// described could not run, and did not: edge's ServiceAccess doc comment said 16
// methods while the interface carried 19, and engine.go called the wide one
// "35 verbs" where it declares 52 of its own.
//
// Adding a method here is not the same as approving it. If a genuine addition
// lands, update the want-list AND the count in the interface's own doc comment,
// which these tests keep honest. Per SYNTH-round1 D5 the shape of this split is
// itself reopened as a design question, so treat a growing want-list as evidence
// for that study rather than as routine maintenance.
//
// Pattern copied from shingo-core/messaging/dispatcher_test.go.

func assertInterfaceWidth(t *testing.T, name string, rt reflect.Type, want []string) {
	t.Helper()
	if rt.Kind() != reflect.Interface {
		t.Fatalf("%s: kind = %v, want Interface", name, rt.Kind())
	}
	got := make(map[string]bool, rt.NumMethod())
	for i := 0; i < rt.NumMethod(); i++ {
		got[rt.Method(i).Name] = true
	}
	wantSet := make(map[string]bool, len(want))
	for _, m := range want {
		wantSet[m] = true
		if !got[m] {
			t.Errorf("%s: missing method %q — if it was renamed or removed, update the want-list", name, m)
		}
	}
	for m := range got {
		if !wantSet[m] {
			t.Errorf("%s: undeclared method %q — widening this interface needs an owner conversation, not a want-list edit", name, m)
		}
	}
	if rt.NumMethod() != len(want) {
		t.Errorf("%s: method count = %d, want %d", name, rt.NumMethod(), len(want))
	}
}

// TestServiceAccessWidth pins Edge's narrow surface at 19 methods. The
// interface's own doc comment states the same number; keep them together.
func TestServiceAccessWidth(t *testing.T) {
	t.Parallel()
	want := []string{
		"AdminService",
		"AppConfig",
		"CatalogService",
		"ChangeoverService",
		"ConfigPath",
		"CoreAPI",
		"CoreNodes",
		"CoreSync",
		"CounterService",
		"OrderManager",
		"OrderService",
		"PLCManager",
		"PayloadBinTypes",
		"ProcessService",
		"Reconciliation",
		"ShiftService",
		"SourcingStateForProcess",
		"StationService",
		"StyleService",
	}
	var iface ServiceAccess
	assertInterfaceWidth(t, "ServiceAccess", reflect.TypeOf(&iface).Elem(), want)
}

// TestEngineOrchestrationWidth pins Edge's wide surface at 71 methods —
// ServiceAccess's 19 embedded, plus 52 orchestration verbs of its own.
func TestEngineOrchestrationWidth(t *testing.T) {
	t.Parallel()
	want := []string{
		"AbandonChangeoverSupply",
		"AckSupplyRefusal",
		"AdminAdjustLinesideBucket",
		"AdminService",
		"AppConfig",
		"ApplyWarLinkConfig",
		"BackfillBucketsForStation",
		"BucketBackfillNeeded",
		"CancelProcessChangeover",
		"CancelProcessChangeoverRedirect",
		"CatalogService",
		"ChangeoverGateStatus",
		"ChangeoverService",
		"CleanupReportingPointTag",
		"ClearBin",
		"ClearLoaderHome",
		"ClearPostCutoverFlag",
		"CompleteProcessProductionCutover",
		"ConfigPath",
		"CoreAPI",
		"CoreNodes",
		"CoreSync",
		"CounterService",
		"CreateRetrieveForAPI",
		"DeliverNewMaterialForChangeover",
		"EnrichHomeBufferPartials",
		"EnsureTagPublished",
		"EvacuateNode",
		"FetchMarketBins",
		"FlipABNode",
		"LoadBin",
		"ManageReportingPointTag",
		"OrderManager",
		"OrderService",
		"PLCManager",
		"PayloadBinTypes",
		"PostCutoverFlag",
		"PreviewChangeoverPlan",
		"ProcessService",
		"PullFromMarket",
		"PushEmptyOut",
		"Reconciliation",
		"ReconnectKafka",
		"RecordBinCount",
		"RefuseSupply",
		"ReleaseNodeEmpty",
		"ReleaseNodePartial",
		"ReleaseNodeWithRemainingUOP",
		"ReleaseOrderWithLineside",
		"ReleaseStagedOrders",
		"RequestCatalogSync",
		"RequestEmptyBin",
		"RequestFullBin",
		"RequestNodeMaterial",
		"RequestNodeSync",
		"RequestProduceSwap",
		"SendEnvelope",
		"SequentialChangeoverCutover",
		"ShiftService",
		"SourcingStateForProcess",
		"StageNodeChangeoverMaterial",
		"StartProcessChangeover",
		"StationService",
		"StyleService",
		"SwitchNodeToTarget",
		"SwitchOperatorStationToTarget",
		"SyncProcessCounter",
		"UndoSupplyRefusal",
		"UpdateCellReorder",
	}
	var iface EngineOrchestration
	assertInterfaceWidth(t, "EngineOrchestration", reflect.TypeOf(&iface).Elem(), want)
}
