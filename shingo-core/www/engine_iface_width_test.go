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

// TestServiceAccessWidth pins Core's narrow surface at 46 methods. The
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
		"DemandEpisodeService",
		"Dispatcher",
		"EdgeFeedFlags",
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
		"PayloadService",
		"Reconciliation",
		"Recovery",
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

// TestEngineOrchestrationWidth pins Core's wide surface at 62 methods —
// ServiceAccess's 46 embedded, plus 16 orchestration verbs of its own.
// (CleanNow and SaveCleaningSetup joined at v158: one sends a pinned vendor
// order, the other checks the cleaning points against the scene and the lanes
// before saving. The cleaning opt-out did not: it is a one-row write and went
// on NodeService, so ServiceAccess stayed at 50. ClearLoaderHome joined with
// the loader home's clear as a parent order.)
// (ReplenishmentHealth and DeltaIntegrityDaily left both on 2026-10-29: the
// /inventory page that read them became one table on InventoryService.Loop,
// and the daily drop chart went with it.)
// (PrepareDatabase left it in R27: Core's database settings apply after a
// restart, so the save door only pings the draft, through HealthService.)
//
// CMSFeedHealth is on the wide surface rather than reached through a service
// accessor because two of its three inputs are PROCESS state — whether a cms:
// block was configured, and whether the poster has muted itself. A service
// reading only the database would report a muted poster with a quiet queue as
// healthy and idle, which is the exact failure the health endpoint exists to
// make impossible.
//
// ClearForReuseStampAndBookDeparture is here for the other reason a verb
// belongs on the wide surface: it spans two aggregates in one transaction — the
// bin's manifest and the CMS ledger rows whose quantities the clear destroys.
// Reached through BinManifest() it could only be one or the other. (Renamed
// from ClearForReuseAndBookDeparture when it took the two-stage stamp; the
// width is unchanged.)
//
// ClearLoaderHome is the home clear (engine/clear_home.go): it births the
// clear's orders and zeroes the home's carrier in one transaction, under the
// scanner's lock — no service owns all three.
//
// SaveLoader is the loader update and its drain at the save (owner,
// 2026-10-25): a save that leaves a drain box on drains at once, and the drain
// is the dispatcher's, under the loader's lock, so the update door used to
// reach the dispatcher itself. Through the verb the loader doors reach it
// nowhere (TestLoaderDoors_ReachNoDispatcher). The create door saves through
// the service and drains nothing: a new loader has no windows until its homes
// are added.
func TestEngineOrchestrationWidth(t *testing.T) {
	t.Parallel()
	want := []string{
		"AdminService",
		"AppConfig",
		"AuditService",
		"BinManifest",
		"BinService",
		"CMSFeedHealth",
		"CMSTransactionService",
		"CalculatorService",
		"CarrierBindings",
		"CleanNow",
		"ClearForReuseStampAndBookDeparture",
		"ClearLoaderHome",
		"ConfigPath",
		"CreateBinMove",
		"DashboardService",
		"DeltaIntegrityByPayload",
		"DemandEpisodeService",
		"Dispatcher",
		"EdgeFeedFlags",
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
		"PayloadService",
		"Reconciliation",
		"ReconfigureFleet",
		"ReconfigureMessaging",
		"ReconfigureNotifications",
		"Recovery",
		"RequestEdgeReregister",
		"RobotGroups",
		"SaveCleaningSetup",
		"SaveLoader",
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
