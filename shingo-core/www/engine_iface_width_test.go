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
// described could not run, and did not: core's ServiceAccess doc comment said
// 25 methods while the interface carried 49, and router.go said the wide one
// added 12 verbs where it adds 14.
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

// TestServiceAccessWidth pins Core's narrow surface at 49 methods. The
// interface's own doc comment states the same number; keep them together.
func TestServiceAccessWidth(t *testing.T) {
	t.Parallel()
	want := []string{
		"AdminService",
		"AppConfig",
		"AuditService",
		"BinManifest",
		"BinService",
		"CMSTransactionService",
		"CalculatorService",
		"CarrierBindings",
		"ConfigPath",
		"DashboardService",
		"DeltaIntegrityByPayload",
		"DeltaIntegrityDaily",
		"DemandEpisodeService",
		"DemandService",
		"Dispatcher",
		"EventBus",
		"Fleet",
		"FootprintService",
		"GetActiveOrderWithRobotLocation",
		"GetActiveOrdersWithRobotLocation",
		"GetActiveOrdersWithRobotLocationFiltered",
		"GetAllCachedRobots",
		"GetCachedRobotStatus",
		"GetNodeOccupancy",
		"HealthService",
		"HeartbeatService",
		"InventoryDeltaService",
		"InventoryService",
		"LoaderService",
		"MaintainedGroupStates",
		"MissionService",
		"MsgClient",
		"NegativeLedgerExcursions",
		"NegativeLedgerPayloads",
		"NodeService",
		"OpenNegativeBins",
		"OrderService",
		"PartsService",
		"PayloadService",
		"Reconciliation",
		"Recovery",
		"ReplenishmentHealth",
		"RequestEdgeReregister",
		"RobotGroups",
		"SourceabilityEvents",
		"SourceabilityPage",
		"TestCommandService",
		"Tracker",
		"ValidateAdvancedLoadSequence",
	}
	var iface ServiceAccess
	assertInterfaceWidth(t, "ServiceAccess", reflect.TypeOf(&iface).Elem(), want)
}

// TestEngineOrchestrationWidth pins Core's wide surface at 63 methods —
// ServiceAccess's 49 embedded, plus 14 orchestration verbs of its own.
func TestEngineOrchestrationWidth(t *testing.T) {
	t.Parallel()
	want := []string{
		"AdminService",
		"AppConfig",
		"ApplyBatchCorrection",
		"ApplyCorrection",
		"AuditService",
		"BinManifest",
		"BinService",
		"CMSTransactionService",
		"CalculatorService",
		"CarrierBindings",
		"ConfigPath",
		"CreateBinMove",
		"DashboardService",
		"DeltaIntegrityByPayload",
		"DeltaIntegrityDaily",
		"DemandEpisodeService",
		"DemandService",
		"Dispatcher",
		"EventBus",
		"Fleet",
		"FootprintService",
		"GetActiveOrderWithRobotLocation",
		"GetActiveOrdersWithRobotLocation",
		"GetActiveOrdersWithRobotLocationFiltered",
		"GetAllCachedRobots",
		"GetCachedRobotStatus",
		"GetNodeOccupancy",
		"HardReleaseOrder",
		"HealthService",
		"HeartbeatService",
		"InventoryDeltaService",
		"InventoryService",
		"LoaderService",
		"MaintainedGroupStates",
		"MissionService",
		"MsgClient",
		"NegativeLedgerExcursions",
		"NegativeLedgerPayloads",
		"NodeService",
		"OpenNegativeBins",
		"OrderService",
		"PartsService",
		"PayloadService",
		"Reconciliation",
		"ReconfigureCountGroups",
		"ReconfigureDatabase",
		"ReconfigureFleet",
		"ReconfigureMessaging",
		"ReconfigureNotifications",
		"Recovery",
		"ReplenishmentHealth",
		"RequestEdgeReregister",
		"RobotGroups",
		"SceneSync",
		"SendDataToEdge",
		"SourceabilityEvents",
		"SourceabilityPage",
		"SyncScenePoints",
		"TerminateOrder",
		"TestCommandService",
		"Tracker",
		"UpdateNodeZones",
		"ValidateAdvancedLoadSequence",
	}
	var iface EngineOrchestration
	assertInterfaceWidth(t, "EngineOrchestration", reflect.TypeOf(&iface).Elem(), want)
}
