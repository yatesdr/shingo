package service

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"shingocore/store"
	"shingocore/store/audit"
	"shingocore/store/bins"
)

// uid_under_actor_census_test.go — every writer of bin_uop_ledger,
// bin_uop_exception and bin_uop_delta_daily, and what it puts in actor.
//
// WHY. A station's uid is written into actor on the rows a station's own
// counts and reports make, while actor elsewhere names a person, a mechanism
// ("system", "edge:<component>") or nobody. The clean column model moves the
// uid into a station_uid column and leaves actor for the who; the move is a
// data statement keyed by op (ledger) and kind (exception), and those keys are
// read from this census: UIDUnderActorLedgerOps, UIDUnderActorExceptionKinds
// with UIDUnderActorExceptionOps, UIDInExceptionDetail, and
// ActorInLedgerStationOps for the ledger rows whose station column can hold a
// Core actor's name. The lists are derived from uidUnderActorWriters below and
// held to it, and the writer table is held to the tree, so a migration that
// asserts its lists equal these cannot move the wrong rows.
//
// SINCE v185 the writers put the uid in station_uid and leave actor for the
// who ('' on a station's own rows). An entry marked moved says its op's or
// kind's rows held the uid in actor before v185, which is what v185 moves;
// detailKey and actorFromStation mark the hold drop's detail key and the
// lifted row's Core actor the same way. The exported lists are read from
// those marks, so they still name exactly the rows the migration moves.
//
// HOW. The module's non-test source (parseCoreSource; the migration chain and
// the baseline DDL are left out by skippedSource and counted separately below)
// is parsed. A writer site is every call to the four helpers in store/audit
// (AppendBinUOP, AppendBinUOPOverride, AppendBinUOPException,
// AppendHoldDropped) and every SQL literal that INSERTs into one of the three
// tables, keyed "dir:Recv.Func" with the call's op (kind/op for an exception)
// and actor arguments as written. Each site is named with what it writes. An
// actor written as a literal ("" or "system") and an op written as a constant
// are classified by the test itself; any other argument is classified by
// reading, and the entry says why. When the argument is the enclosing
// function's parameter, the function's production callers (found by name, an
// over-approximation that can only add callers) are named too, so a new caller
// that hands it something else fails here.
//
// WHAT IT CANNOT SEE: a caller two levels up handing a different value through
// an unchanged middle function (each reading names its chain).

// uaActor is what a writer puts in actor.
type uaActor string

const (
	// uaUID: the station uid of the Edge whose count or report the row is
	// about, from its envelope.
	uaUID uaActor = "station uid"
	// uaOperator: a person, or an Edge's called_by (an operator's name, the
	// HMI's "operator_station" default, "edge-operator").
	uaOperator uaActor = "operator"
	// uaComponent: a hold drop's called_by: "edge:<component>" or an operator.
	uaComponent uaActor = "edge component or operator"
	uaSystem    uaActor = `"system"`
	uaNone      uaActor = `""`
	// uaInherited: the boundary exception AppendBinUOP writes for a bump op
	// takes the ledger row's actor.
	uaInherited uaActor = "the ledger row's actor"
	// uaFromLedger: the daily roll-up copies bin_uop_delta rows' actor.
	uaFromLedger uaActor = "bin_uop_delta's actor"
	// uaHelper: a helper's own INSERT; its call sites are the census.
	uaHelper uaActor = "the helper's actor parameter"
	// uaFixture: internal/testdb, which no production path reaches.
	uaFixture uaActor = "test fixture"
	// uaRequesterActor: the lifting order's Core actor, else "system".
	uaRequesterActor uaActor = "the lifting order's Core actor, else \"system\""
)

// uaWriter is one writer site. ops is the ledger ops it writes, or for an
// exception "kind/op" pairs ("kind/*": any op). station is what the ledger's
// station column, or a hold drop's detail station, holds ("" for nothing).
// actorInStation says the station can hold a Core actor's name. boundary says
// the function flags its ledger row as a boundary (BinUOPContext.Boundary).
type uaWriter struct {
	n              int
	table          string
	ops            []string
	actor          uaActor
	station        string
	actorInStation bool
	boundary       bool
	reading        string
	callers        []string
	// moved: the writer puts the station uid in station_uid where it put it in
	// actor before v185, so v185 moves this op's or kind's old rows.
	moved bool
	// detailKey: before v185 the writer put the uid in this detail key.
	detailKey string
	// actorFromStation: before v185 the station column could hold a Core
	// actor's name, which the writer now puts in actor.
	actorFromStation bool
}

const (
	uaLedger    = "bin_uop_ledger"
	uaException = "bin_uop_exception"
	uaDaily     = "bin_uop_delta_daily"
)

// uaAppliedBy is the chain every applier row's station comes through.
const uaAppliedBy = "station is ApplyBinUOPDelta's, which CoreDataService.HandleBinUOPDelta passes from the envelope (env.Src.Station)"

var uidUnderActorWriters = map[string]uaWriter{
	// ── The counting Edge's rows: the uid in station_uid (v185 moved it out of actor) ──
	"uop:InventoryDeltaService.ApplyBinUOPDelta INSERT INTO bin_uop_ledger": {n: 1, table: uaLedger,
		ops: []string{"bin_uop_delta"}, actor: uaNone, moved: true,
		reading: "the op is the statement's literal and station_uid its $5, " + uaAppliedBy + "; actor is not written",
		callers: []string{"messaging:CoreDataService.HandleBinUOPDelta"}},
	"uop:InventoryDeltaService.ApplyBinUOPDelta AppendBinUOPOverride(audit.OpStaleEpochDropped, \"\")": {n: 1, table: uaLedger,
		ops: []string{"stale_epoch_dropped"}, actor: uaNone, station: "station", moved: true, reading: uaAppliedBy,
		callers: []string{"messaging:CoreDataService.HandleBinUOPDelta"}},
	"uop:InventoryDeltaService.ApplyBinUOPDelta AppendBinUOPOverride(audit.OpPayloadBoundFirstDelta, \"\")": {n: 1, table: uaLedger,
		ops: []string{"payload_bound_first_delta"}, actor: uaNone, station: "station", moved: true, reading: uaAppliedBy,
		callers: []string{"messaging:CoreDataService.HandleBinUOPDelta"}},
	"uop:InventoryDeltaService.ApplyBinUOPDelta AppendBinUOPOverride(audit.OpPayloadReboundWithInventory, \"\")": {n: 1, table: uaLedger,
		ops: []string{"payload_rebound_with_inventory"}, actor: uaNone, station: "station", moved: true, reading: uaAppliedBy,
		callers: []string{"messaging:CoreDataService.HandleBinUOPDelta"}},
	"uop:InventoryDeltaService.recordRejectedDelta AppendBinUOPOverride(audit.OpPayloadMismatchDropped, \"\")": {n: 1, table: uaLedger,
		ops: []string{"payload_mismatch_dropped"}, actor: uaNone, station: "station", moved: true,
		reading: "ApplyBinUOPDelta's station; " + uaAppliedBy,
		callers: []string{"uop:InventoryDeltaService.ApplyBinUOPDelta"}},
	"uop:InventoryDeltaService.refuseAfterLift AppendBinUOPOverride(audit.OpAfterLift, \"\")": {n: 1, table: uaLedger,
		ops: []string{"after_lift"}, actor: uaNone, station: "station", moved: true,
		reading: "ApplyBinUOPDelta's station; " + uaAppliedBy,
		callers: []string{"uop:InventoryDeltaService.ApplyBinUOPDelta"}},
	"service:BinManifestService.RebaseAfterEdgeRollbackTx AppendBinUOP(audit.OpEdgeRollback, \"\")": {n: 1, table: uaLedger,
		ops: []string{"edge_rollback"}, actor: uaNone, station: "station", moved: true,
		reading: "recordEdgeRollback's station, from atOrBelowHighWater, from ApplyBinUOPDelta; " + uaAppliedBy +
			"; before v185 the same uid went into actor as well as the station column",
		callers: []string{"uop:InventoryDeltaService.recordEdgeRollback"}},

	"uop:InventoryDeltaService.ApplyBinUOPDelta AppendBinUOPException(audit.ExcStaleEpoch/audit.OpStaleEpochDropped, \"\")": {n: 1, table: uaException,
		ops: []string{"stale_epoch/stale_epoch_dropped"}, actor: uaNone, station: "station", moved: true, reading: uaAppliedBy,
		callers: []string{"messaging:CoreDataService.HandleBinUOPDelta"}},
	"uop:InventoryDeltaService.ApplyBinUOPDelta AppendBinUOPException(audit.ExcNegativeCrossing/\"bin_uop_delta\", \"\")": {n: 1, table: uaException,
		ops: []string{"negative_crossing/bin_uop_delta"}, actor: uaNone, station: "station", moved: true, reading: uaAppliedBy,
		callers: []string{"messaging:CoreDataService.HandleBinUOPDelta"}},
	"uop:InventoryDeltaService.recordRejectedDelta AppendBinUOPException(audit.ExcPayloadMismatch/audit.OpPayloadMismatchDropped, \"\")": {n: 1, table: uaException,
		ops: []string{"payload_mismatch/payload_mismatch_dropped"}, actor: uaNone, station: "station", moved: true,
		reading: "ApplyBinUOPDelta's station; " + uaAppliedBy,
		callers: []string{"uop:InventoryDeltaService.ApplyBinUOPDelta"}},
	"uop:InventoryDeltaService.recordEdgeRollback AppendBinUOPException(audit.ExcEdgeRollback/audit.OpEdgeRollback, \"\")": {n: 1, table: uaException,
		ops: []string{"edge_rollback/edge_rollback"}, actor: uaNone, station: "station", moved: true,
		reading: "atOrBelowHighWater's station, from ApplyBinUOPDelta; " + uaAppliedBy,
		callers: []string{"uop:InventoryDeltaService.atOrBelowHighWater"}},
	"store:DB.RecordReportDivergences INSERT INTO bin_uop_exception": {n: 1, table: uaException,
		ops: []string{"report_divergence/*"}, actor: uaNone, moved: true,
		reading: "kind is ExcReportDivergence, op the divergence's class, actor '' and station_uid the station parameter: " +
			"LinesideDivergenceService.CheckReport's, which HandleLinesideLevelReport passes from the report (else its envelope)",
		callers: []string{"service:LinesideDivergenceService.CheckReport"}},
	"store:DB.RecordHoldDropped AppendHoldDropped(reason, actor)": {n: 1, table: uaException,
		ops: []string{"hold_dropped/*"}, actor: uaComponent, station: "station", detailKey: "station",
		reading: "op is the drop's reason and actor its called_by (edge:recount, edge:sweep, an operator), " +
			"from CoreDataService.HandleHoldDropped; the uid is the envelope's station, written to station_uid " +
			"(before v185, to detail->>'station')",
		callers: []string{"messaging:CoreDataService.HandleHoldDropped"}},

	// ── The boundary row: the ledger row's actor and station ────────────
	"store/audit:AppendBinUOP AppendBinUOPException(ExcBoundary/op, actor)": {n: 1, table: uaException,
		ops: []string{"boundary/*"}, actor: uaInherited, station: "ctx.StationUID",
		reading: "written for every EpochBumpOps op and a BinUOPContext.Boundary row, with that call's op, actor and " +
			"station_uid: a station uid only where the ledger call's is (edge_rollback)"},

	// ── The lifting order's requester ───────────────────────────────────
	"service:BinService.Lift AppendBinUOP(audit.OpLifted, liftActor)": {n: 1, table: uaLedger,
		ops: []string{"lifted"}, actor: uaRequesterActor, station: "liftStation", actorFromStation: true,
		reading: "the notice's station, materialStation(order) in Engine.handlePickupBlockCompletedFor (a " +
			"station-aware order's requester, else its projects_to), split by domain.SplitRequester: a station's " +
			"uid goes to station_uid; a Core actor's name (core-operator / core-direct / core-test) is the actor, " +
			"else \"system\". Before v185 the actor's name sat in the station column under actor \"system\""},

	// ── A person's or a mechanism's rows ─────────────────────────────────
	"service:BinManifestService.syncOrClearForReleased AppendBinUOP(op, actor)": {n: 2, table: uaLedger,
		ops: []string{"released_empty", "released_empty_fallback", "released_overpack", "released_partial",
			"released_partial_fallback", "released_underpack"},
		actor: uaOperator,
		reading: "op is chosen in the function; actor is the release's called_by (\"system\" when blank), " +
			"from the four SyncOrClearForReleased* wrappers, from Dispatcher.syncManifestForRelease (p.CalledBy)",
		callers: []string{"service:BinManifestService.SyncOrClearForReleased", "service:BinManifestService.SyncOrClearForReleasedFenced",
			"service:BinManifestService.SyncOrClearForReleasedNoOwner", "service:BinManifestService.SyncOrClearForReleasedNoOwnerFenced"}},
	"service:BinManifestService.AuditReleaseOverride AppendBinUOPOverride(audit.OpOperatorOverrideReleasePartial, actor)": {n: 1, table: uaLedger,
		ops: []string{"operator_override_release_partial"}, actor: uaOperator,
		reading: "the release's called_by (\"system\" when blank), from Dispatcher.syncManifestForRelease",
		callers: []string{"dispatch:Dispatcher.syncManifestForRelease"}},
	"service:BinManifestService.AuditReleaseOverride AppendBinUOPOverride(audit.OpOperatorOverridePullParts, actor)": {n: 1, table: uaLedger,
		ops: []string{"operator_override_pull_parts"}, actor: uaOperator,
		reading: "the release's called_by (\"system\" when blank), from Dispatcher.syncManifestForRelease",
		callers: []string{"dispatch:Dispatcher.syncManifestForRelease"}},
	"service:BinService.RecordCount AppendBinUOP(audit.OpCycleCount, actor)": {n: 1, table: uaLedger,
		ops: []string{"cycle_count"}, actor: uaOperator,
		reading: "the counter: Core's signed-in user (Handlers.binRecordCount) or the Edge's stated actor, " +
			"\"edge-operator\" when blank (Doors.binCount)",
		callers: []string{"www:Doors.binCount", "www:Handlers.binRecordCount"}},
	"service:BinService.RecordCount AppendBinUOPOverride(audit.OpCycleCountOverCapacity, actor)": {n: 1, table: uaLedger,
		ops: []string{"cycle_count_over_capacity"}, actor: uaOperator,
		reading: "the counter, as cycle_count's",
		callers: []string{"www:Doors.binCount", "www:Handlers.binRecordCount"}},
	"service:stampDeclaredFullTx AppendBinUOP(audit.OpReleaseDeclaredFull, full.CalledBy)": {n: 1, table: uaLedger,
		ops: []string{"release_declared_full"}, actor: uaOperator,
		reading: "who declared the bin full (the Edge's declaration's called_by), from BinManifestService.recordProduced",
		callers: []string{"service:BinManifestService.recordProduced"}},
	"service:BinManifestService.ClearForReuseTx AppendBinUOP(op, \"\")": {n: 1, table: uaLedger,
		ops: []string{"clear_for_reuse", "released_capture_empty", "released_overpack"}, actor: uaNone, boundary: true,
		reading: "op is the caller's (clear_for_reuse, or released_capture_empty from ApplyBinUOPDelta), " +
			"rewritten in the function to released_overpack, flagged a boundary, for a capture-empty below zero",
		callers: []string{"engine:Engine.ClearForReuseStampAndBookDeparture", "engine:Engine.issueClear",
			"service:BinManifestService.ClearForReuseResolved", "service:BinManifestService.ReplaceAtLine",
			"uop:InventoryDeltaService.ApplyBinUOPDelta"}},
	"store:DB.UpdateBin AppendBinUOP(op, \"\")": {n: 1, table: uaLedger,
		ops: []string{"bin_retyped", "totes_edited"}, actor: uaNone,
		reading: "op is chosen in the function from what the edit changed"},
	"service:BinManifestService.DeclareTotesTx AppendBinUOP(audit.OpTotesDeclared, \"\")": {n: 1, table: uaLedger,
		ops: []string{"totes_declared"}, actor: uaNone},
	"service:BinManifestService.writeProductionTx AppendBinUOP(audit.OpSetForProduction, \"\")": {n: 1, table: uaLedger,
		ops: []string{"set_for_production"}, actor: uaNone},
	"service:BinManifestService.confirmTx AppendBinUOP(audit.OpManifestConfirmed, \"\")": {n: 1, table: uaLedger,
		ops: []string{"manifest_confirmed"}, actor: uaNone},
	"service:BinManifestService.Unconfirm AppendBinUOP(audit.OpManifestUnconfirmed, \"\")": {n: 1, table: uaLedger,
		ops: []string{"manifest_unconfirmed"}, actor: uaNone},
	"service:BinManifestService.clearAndClaimTx AppendBinUOP(audit.OpClearAndClaim, \"\")": {n: 1, table: uaLedger,
		ops: []string{"clear_and_claim"}, actor: uaNone},
	"service:BinManifestService.syncUOPAndClaimTx AppendBinUOP(audit.OpSyncUOPAndClaim, \"\")": {n: 1, table: uaLedger,
		ops: []string{"sync_uop_and_claim"}, actor: uaNone},
	"service:BinManifestService.RecordIngestRefused AppendBinUOPOverride(audit.OpIngestStaleEpochRefused, \"\")": {n: 1, table: uaLedger,
		ops: []string{"ingest_stale_epoch_refused"}, actor: uaNone},
	"store:DB.DeriveTotesAtLift AppendBinUOP(bins.OpTotesDerived, \"system\")": {n: 1, table: uaLedger,
		ops: []string{"totes_derived"}, actor: uaSystem},

	// ── The daily roll-up ───────────────────────────────────────────────
	"store/audit:RollupBinUOPDeltaDay INSERT INTO bin_uop_delta_daily": {n: 1, table: uaDaily,
		ops: []string{"bin_uop_delta"}, actor: uaFromLedger,
		reading: "bin_uop_delta_daily.actor (in its key) is the ledger's actor over bin_uop_delta rows: the counting Edge's uid"},

	// ── The helpers' own statements, and the fixture ──────────────────────
	"store/audit:AppendBinUOP INSERT INTO bin_uop_ledger":             {n: 1, table: uaLedger, actor: uaHelper, reading: "AppendBinUOP's statement"},
	"store/audit:AppendBinUOPOverride INSERT INTO bin_uop_ledger":     {n: 1, table: uaLedger, actor: uaHelper, reading: "AppendBinUOPOverride's statement"},
	"store/audit:AppendBinUOPException INSERT INTO bin_uop_exception": {n: 1, table: uaException, actor: uaHelper, reading: "AppendBinUOPException's statement"},
	"store/audit:AppendHoldDropped INSERT INTO bin_uop_exception":     {n: 1, table: uaException, actor: uaHelper, reading: "AppendHoldDropped's statement"},
	"internal/testdb:SeedAudit INSERT INTO bin_uop_ledger":            {n: 1, table: uaLedger, actor: uaFixture, reading: "a test seed; no production path imports internal/testdb"},
}

// uaHistory is every INSERT into the three tables in the migrations above the
// retired chain, which parseCoreSource leaves out, by function: rows a
// migration writes on a plant's disk, which a move keyed by op and kind also
// moves. None writes one today. Until the squash at v184 the chain held four
// (read at e06b29b0): v93BinUOPException's backfill, three (negative_crossing,
// each crossing row's own op and actor; the two drop kinds; boundary), and
// v94BinUOPDeltaDaily's, one. Their rows are on the plants' disks still, and
// the lists below cover their ops and kinds.
var uaHistory = map[string]int{}

// The lists the clean model's move reads (v185 asserts its own equal these).

// UIDUnderActorLedgerOps are the bin_uop_ledger ops whose actor is a station uid.
var UIDUnderActorLedgerOps = []string{"after_lift", "bin_uop_delta", "edge_rollback", "payload_bound_first_delta",
	"payload_mismatch_dropped", "payload_rebound_with_inventory", "stale_epoch_dropped"}

// UIDUnderActorExceptionKinds are the bin_uop_exception kinds whose actor is a
// station uid on some or all rows: UIDUnderActorExceptionOps says which.
var UIDUnderActorExceptionKinds = []string{"boundary", "edge_rollback", "negative_crossing", "payload_mismatch",
	"report_divergence", "stale_epoch"}

// UIDUnderActorExceptionOps is, for each of those kinds, the op column values
// its uid rows are written with today; nil is any op. A boundary row's actor
// is a uid only on op edge_rollback: on the other bump ops it is "", "system"
// or an operator.
var UIDUnderActorExceptionOps = map[string][]string{
	"boundary":          {"edge_rollback"},
	"edge_rollback":     {"edge_rollback"},
	"negative_crossing": {"bin_uop_delta"},
	"payload_mismatch":  {"payload_mismatch_dropped"},
	"report_divergence": nil,
	"stale_epoch":       {"stale_epoch_dropped"},
}

// UIDInExceptionDetail is each exception kind that carries a station uid in a
// detail key rather than in actor, and that key.
var UIDInExceptionDetail = map[string]string{"hold_dropped": "station"}

// ActorInLedgerStationOps are the bin_uop_ledger ops whose station column can
// hold a Core actor's name (an aware actor order's material station).
var ActorInLedgerStationOps = []string{"lifted"}

// uaConsts resolves each op or kind constant a writer site names, by its
// spelling at the site, to its value: the compiler checks the right column.
var uaConsts = map[string]string{
	"audit.OpAfterLift":                      audit.OpAfterLift,
	"audit.OpClearAndClaim":                  audit.OpClearAndClaim,
	"audit.OpCycleCount":                     audit.OpCycleCount,
	"audit.OpCycleCountOverCapacity":         audit.OpCycleCountOverCapacity,
	"audit.OpEdgeRollback":                   audit.OpEdgeRollback,
	"audit.OpIngestStaleEpochRefused":        audit.OpIngestStaleEpochRefused,
	"audit.OpLifted":                         audit.OpLifted,
	"audit.OpManifestConfirmed":              audit.OpManifestConfirmed,
	"audit.OpManifestUnconfirmed":            audit.OpManifestUnconfirmed,
	"audit.OpOperatorOverridePullParts":      audit.OpOperatorOverridePullParts,
	"audit.OpOperatorOverrideReleasePartial": audit.OpOperatorOverrideReleasePartial,
	"audit.OpPayloadBoundFirstDelta":         audit.OpPayloadBoundFirstDelta,
	"audit.OpPayloadMismatchDropped":         audit.OpPayloadMismatchDropped,
	"audit.OpPayloadReboundWithInventory":    audit.OpPayloadReboundWithInventory,
	"audit.OpReleaseDeclaredFull":            audit.OpReleaseDeclaredFull,
	"audit.OpSetForProduction":               audit.OpSetForProduction,
	"audit.OpStaleEpochDropped":              audit.OpStaleEpochDropped,
	"audit.OpSyncUOPAndClaim":                audit.OpSyncUOPAndClaim,
	"audit.OpTotesDeclared":                  audit.OpTotesDeclared,
	"bins.OpTotesDerived":                    bins.OpTotesDerived,
	"audit.ExcEdgeRollback":                  audit.ExcEdgeRollback,
	"audit.ExcNegativeCrossing":              audit.ExcNegativeCrossing,
	"audit.ExcPayloadMismatch":               audit.ExcPayloadMismatch,
	"audit.ExcStaleEpoch":                    audit.ExcStaleEpoch,
	"ExcBoundary":                            audit.ExcBoundary,
}

// uaReadingConsts are the values entries name by reading, checked against
// the constants so a renamed op cannot leave a list naming the old one.
var uaReadingConsts = map[string]bool{
	audit.OpReleasedEmpty: true, audit.OpReleasedEmptyFallback: true, audit.OpReleasedOverpack: true,
	audit.OpReleasedPartial: true, audit.OpReleasedPartialFallback: true, audit.OpReleasedUnderpack: true,
	audit.OpClearForReuse: true, audit.OpReleasedCaptureEmpty: true, audit.OpBinRetyped: true, audit.OpTotesEdited: true,
	"bin_uop_delta": true, store.ExcReportDivergence: true, audit.ExcHoldDropped: true, audit.ExcBoundary: true,
}

// TestCensus_UIDUnderActorWriters holds uidUnderActorWriters to the tree and
// the exported move lists to uidUnderActorWriters.
//
// Red when: a writer site is added, removed, or its op, actor or station
// argument changes; a parameter-fed writer gains or loses a caller; a helper's
// signature moves its op or actor; an entry's classification disagrees with
// a literal or constant argument; the migration chain gains a writer; or an
// exported list disagrees with the table.
func TestCensus_UIDUnderActorWriters(t *testing.T) {
	t.Parallel()
	got := uaScan(t)
	uaCheckHelpers(t, got)
	const decide = "Say what it puts in actor (a station uid, an operator, a mechanism, nothing) and in the " +
		"station column, in uidUnderActorWriters (service/uid_under_actor_census_test.go); a station uid " +
		"under actor joins the move lists the clean model's migration reads"
	for _, key := range slices.Sorted(maps.Keys(got.sites)) {
		f := got.sites[key]
		w, ok := uidUnderActorWriters[key]
		if !ok {
			t.Errorf("%s writes %s and is not in uidUnderActorWriters (%d site(s)). %s.", key, f.table, f.n, decide)
			continue
		}
		if w.n != f.n || w.table != f.table || w.station != f.station || w.boundary != f.boundary {
			t.Errorf("%s: the tree has %d site(s) into %s, station %q, boundary %v; the census names %d into %s, "+
				"station %q, boundary %v. %s.", key, f.n, f.table, f.station, f.boundary, w.n, w.table, w.station, w.boundary, decide)
		}
		uaCheckClass(t, key, w, f)
		// The boundary row's callers are the ledger sites, each its own entry.
		if (f.param != "" || w.callers != nil) && w.actor != uaInherited {
			if c := got.callers[uaFuncName(key)]; !slices.Equal(c, w.callers) {
				t.Errorf("%s: its argument comes from its function's parameter; that function's production callers are %v, "+
					"the census names %v. Follow the new caller's value to its source and say what it is. %s.", key, c, w.callers, decide)
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(uidUnderActorWriters)) {
		if _, ok := got.sites[key]; !ok {
			t.Errorf("%s is in uidUnderActorWriters but no longer in the tree: its op or actor argument changed, or the "+
				"writer moved or went. Re-key the entry to it and recheck its classification, or delete it; if it carried "+
				"a station uid, the move lists change with it.", key)
		}
	}
	uaCheckLists(t)
	uaCheckHistory(t)
}

// uaCheckClass holds an entry's classification to the arguments the scan saw:
// a literal actor and constant ops classify themselves; anything else is
// classified by reading and says why.
func uaCheckClass(t *testing.T, key string, w uaWriter, f uaFound) {
	t.Helper()
	lit := map[string]uaActor{`""`: uaNone, `"system"`: uaSystem}
	actorClass, actorLit := lit[uaArg(key, 1)]
	ops, opsConst := uaResolve(key)
	switch {
	case strings.Contains(key, " INSERT INTO "):
		if w.reading == "" {
			t.Errorf("%s: a raw statement's arguments are positional: classify it by reading and say why", key)
		}
	case actorLit && opsConst && f.station == "":
		if w.actor != actorClass || !slices.Equal(w.ops, ops) || w.reading != "" {
			t.Errorf("%s: its arguments say actor %s and ops %v, classified by the test; the census says %s, %v, "+
				"reading %q", key, actorClass, ops, w.actor, w.ops, w.reading)
		}
	default:
		if w.reading == "" {
			t.Errorf("%s: an argument (actor, op or station) is not a literal or a constant: classify it by reading "+
				"and say why", key)
		}
		if actorLit && w.actor != actorClass {
			t.Errorf("%s: its actor argument is %s; the census says %s", key, actorClass, w.actor)
		}
		if opsConst && !slices.Equal(w.ops, ops) {
			t.Errorf("%s: its ops are %v; the census says %v", key, ops, w.ops)
		}
	}
	for _, op := range w.ops {
		for _, v := range strings.Split(op, "/") {
			if v != "*" && !uaReadingConsts[v] && !slices.Contains(slices.Collect(maps.Values(uaConsts)), v) {
				t.Errorf("%s: names op or kind %q, which no constant spells", key, v)
			}
		}
	}
}

// uaCheckLists derives the move lists from the table and holds the exported
// ones to them.
func uaCheckLists(t *testing.T) {
	t.Helper()
	ledger, inStation := map[string]bool{}, map[string]bool{}
	excOps := map[string][]string{}
	detail := map[string]string{}
	var bumpable []string // the ledger ops whose row gets a boundary exception
	bumpable = append(bumpable, audit.EpochBumpOps...)
	for _, w := range uidUnderActorWriters {
		if w.table == uaLedger && w.boundary {
			bumpable = append(bumpable, w.ops...)
		}
	}
	for _, w := range uidUnderActorWriters {
		switch {
		case w.table == uaLedger && (w.actor == uaUID || w.moved):
			for _, op := range w.ops {
				ledger[op] = true
				if slices.Contains(bumpable, op) {
					excOps["boundary"] = append(excOps["boundary"], op)
				}
			}
		case w.table == uaException && (w.actor == uaUID || w.moved):
			for _, p := range w.ops {
				kind, op, _ := strings.Cut(p, "/")
				if op == "*" {
					excOps[kind] = nil
					continue
				}
				excOps[kind] = append(excOps[kind], op)
			}
		case w.detailKey != "":
			kind, _, _ := strings.Cut(w.ops[0], "/")
			detail[kind] = w.detailKey
		}
		if w.actorInStation || w.actorFromStation {
			for _, op := range w.ops {
				inStation[op] = true
			}
		}
	}
	for k := range excOps {
		slices.Sort(excOps[k])
		excOps[k] = slices.Compact(excOps[k])
	}
	const fix = "the exported list must equal what uidUnderActorWriters says; correct whichever is wrong, " +
		"and the clean model's migration moves follow"
	if got := slices.Sorted(maps.Keys(ledger)); !slices.Equal(got, UIDUnderActorLedgerOps) {
		t.Errorf("UIDUnderActorLedgerOps = %v, the writers say %v: %s", UIDUnderActorLedgerOps, got, fix)
	}
	if got := slices.Sorted(maps.Keys(excOps)); !slices.Equal(got, UIDUnderActorExceptionKinds) {
		t.Errorf("UIDUnderActorExceptionKinds = %v, the writers say %v: %s", UIDUnderActorExceptionKinds, got, fix)
	}
	if !reflect.DeepEqual(excOps, UIDUnderActorExceptionOps) {
		t.Errorf("UIDUnderActorExceptionOps = %v, the writers say %v: %s", UIDUnderActorExceptionOps, excOps, fix)
	}
	if !maps.Equal(detail, UIDInExceptionDetail) {
		t.Errorf("UIDInExceptionDetail = %v, the writers say %v: %s", UIDInExceptionDetail, detail, fix)
	}
	if got := slices.Sorted(maps.Keys(inStation)); !slices.Equal(got, ActorInLedgerStationOps) {
		t.Errorf("ActorInLedgerStationOps = %v, the writers say %v: %s", ActorInLedgerStationOps, got, fix)
	}
}

// uaCheckHelpers holds uaHelpers' argument positions to the helpers'
// declared parameter names, and checks the scan read the module.
func uaCheckHelpers(t *testing.T, got uaScanResult) {
	t.Helper()
	if len(got.funcs) < 1000 {
		t.Fatalf("parsed %d functions; a census that reads nothing reports nothing", len(got.funcs))
	}
	for name, h := range uaHelpers {
		params, ok := got.params["store/audit:"+name]
		if !ok {
			t.Errorf("store/audit:%s is gone: repoint uaHelpers at the writer that replaced it", name)
			continue
		}
		want := map[int]string{h.op: "op", h.actor: "actor"}
		if h.kind >= 0 {
			want[h.kind] = "kind"
		}
		if h.ctx >= 0 {
			want[h.ctx] = "ctx"
		}
		if h.detailStation >= 0 {
			want[h.detailStation] = "station"
			want[h.op] = "reason"
		}
		if h.stationUID >= 0 {
			want[h.stationUID] = "stationUID"
		}
		if name == "AppendHoldDropped" {
			want[h.op] = "reason"
		}
		for i, p := range want {
			if i >= len(params) || params[i] != p {
				t.Errorf("store/audit:%s: parameter %d is not %q (%v): its signature moved, repoint uaHelpers", name, i, p, params)
			}
		}
	}
}

// uaCheckHistory counts the migration chain's INSERTs into the three tables,
// which the scan leaves out with the rest of the chain, against uaHistory.
func uaCheckHistory(t *testing.T) {
	t.Helper()
	got := map[string]int{}
	fset := token.NewFileSet()
	var matches []string
	for _, dir := range []string{"store", filepath.Join("store", "schema")} {
		m, err := filepath.Glob(filepath.Join("..", dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		matches = append(matches, m...)
	}
	read := 0
	for _, p := range matches {
		rel := filepath.ToSlash(strings.TrimPrefix(p, ".."+string(filepath.Separator)))
		if strings.HasSuffix(p, "_test.go") || !skippedSource(rel) {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		read++
		eachDecl(f, func(name string, node ast.Node) {
			ast.Inspect(node, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok {
					if s, ok := uaString(lit); ok {
						got[name] += len(uaInsertRe.FindAllString(s, -1))
					}
				}
				return true
			})
		})
	}
	maps.DeleteFunc(got, func(_ string, n int) bool { return n == 0 })
	if read == 0 {
		t.Fatalf("read no migration-chain file; repoint uaCheckHistory")
	}
	if !maps.Equal(got, uaHistory) {
		t.Errorf("the migration chain's INSERTs into the ledger tables are %v, uaHistory names %v: a migration that "+
			"writes rows names what it puts in actor, as a runtime writer does", got, uaHistory)
	}
}

// uaArg is the i-th argument (0 op, 1 actor) as the site key spells it.
func uaArg(key string, i int) string {
	open := strings.Index(key, "(")
	if open < 0 || !strings.HasSuffix(key, ")") {
		return ""
	}
	args := strings.SplitN(key[open+1:len(key)-1], ", ", 2)
	if i >= len(args) {
		return ""
	}
	return args[i]
}

// uaResolve is a site's ops from its op argument (kind/op for an exception),
// when every part is a constant or a literal.
func uaResolve(key string) ([]string, bool) {
	arg := uaArg(key, 0)
	if arg == "" {
		return nil, false
	}
	var out []string
	for _, part := range strings.Split(arg, "/") {
		if v, ok := uaConsts[part]; ok {
			out = append(out, v)
			continue
		}
		if s, err := strconv.Unquote(part); err == nil {
			out = append(out, s)
			continue
		}
		return nil, false
	}
	return []string{strings.Join(out, "/")}, true
}

// uaFuncName is the function a site key names, without its directory or
// receiver: what its callers call.
func uaFuncName(key string) string {
	fn, _, _ := strings.Cut(key, " ")
	fn = fn[strings.Index(fn, ":")+1:]
	return fn[strings.LastIndex(fn, ".")+1:]
}

// uaFound is what the scan finds at one writer site.
type uaFound struct {
	n        int
	table    string
	station  string // the ledger station column's expression, or a hold drop's detail station; "" for none
	param    string // the enclosing function's parameter an op or actor argument comes from, "" for none
	boundary bool   // the function flags a ledger row as a boundary
}

type uaScanResult struct {
	sites   map[string]uaFound
	callers map[string][]string // function name -> every production caller key
	params  map[string][]string // function key -> its parameter names, in order
	funcs   map[string]bool
}

// uaHelpers is each writer helper's table and the positions of its op (or
// kind), actor and, for the ledger, context arguments, as declared in
// store/audit (uaCheckHelpers holds them to the parameter names).
var uaHelpers = map[string]struct {
	table              string
	op, kind, actor    int
	ctx, detailStation int
	// stationUID is the parameter that writes the station_uid column (v185),
	// -1 where the helper takes it in ctx or has none.
	stationUID int
}{
	"AppendBinUOP":          {table: uaLedger, op: 4, kind: -1, actor: 8, ctx: 9, detailStation: -1, stationUID: -1},
	"AppendBinUOPOverride":  {table: uaLedger, op: 4, kind: -1, actor: 8, ctx: -1, detailStation: -1, stationUID: 9},
	"AppendBinUOPException": {table: uaException, op: 12, kind: 1, actor: 4, ctx: -1, detailStation: -1, stationUID: 5},
	"AppendHoldDropped":     {table: uaException, op: 6, kind: -1, actor: 7, ctx: -1, detailStation: -1, stationUID: 2},
}

var uaInsertRe = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+(bin_uop_ledger|bin_uop_exception|bin_uop_delta_daily)\b`)

func uaScan(t *testing.T) uaScanResult {
	t.Helper()
	res := uaScanResult{sites: map[string]uaFound{}, callers: map[string][]string{}, params: map[string][]string{}, funcs: map[string]bool{}}
	callers := map[string]map[string]bool{}
	for _, pf := range parseCoreSource(t) {
		dir := path.Dir(pf.rel)
		eachDecl(pf.file, func(name string, node ast.Node) {
			key := dir + ":" + name
			fd, _ := node.(*ast.FuncDecl)
			params := map[string]bool{}
			if fd != nil {
				if r := uaRecv(fd); r != "" {
					key = dir + ":" + r + "." + name
				}
				res.funcs[key] = true
				for _, f := range fd.Type.Params.List {
					for _, id := range f.Names {
						params[id.Name] = true
						res.params[key] = append(res.params[key], id.Name)
					}
				}
			}
			boundary := uaFlagsBoundary(node)
			ast.Inspect(node, func(n ast.Node) bool {
				switch e := n.(type) {
				case *ast.CallExpr:
					callee := uaLastName(e.Fun)
					if callee != "" && callee != name {
						if callers[callee] == nil {
							callers[callee] = map[string]bool{}
						}
						callers[callee][key] = true
					}
					if h, ok := uaHelpers[callee]; ok && len(e.Args) > h.actor {
						uaNoteCall(res.sites, key, callee, h.table, e, node, params, boundary)
					}
				case *ast.BasicLit:
					s, ok := uaString(e)
					if !ok {
						return true
					}
					for _, m := range uaInsertRe.FindAllStringSubmatch(s, -1) {
						site := fmt.Sprintf("%s INSERT INTO %s", key, strings.ToLower(m[1]))
						f := res.sites[site]
						f.n++
						f.table = strings.ToLower(m[1])
						res.sites[site] = f
					}
				}
				return true
			})
		})
	}
	for name, by := range callers {
		res.callers[name] = slices.Sorted(maps.Keys(by))
	}
	return res
}

// uaNoteCall records one helper call: its op (kind/op), actor, station and
// whether either argument is the enclosing function's parameter.
func uaNoteCall(sites map[string]uaFound, key, callee, table string, e *ast.CallExpr, fn ast.Node, params map[string]bool, boundary bool) {
	h := uaHelpers[callee]
	op := types.ExprString(e.Args[h.op])
	if h.kind >= 0 {
		op = types.ExprString(e.Args[h.kind]) + "/" + op
	}
	site := fmt.Sprintf("%s %s(%s, %s)", key, callee, op, types.ExprString(e.Args[h.actor]))
	f := sites[site]
	f.n++
	f.table = table
	f.boundary = boundary && table == uaLedger
	for _, i := range []int{h.op, h.kind, h.actor, h.stationUID} {
		if i < 0 {
			continue
		}
		if root := uaRoot(e.Args[i]); root != "" && params[root] {
			f.param = root
		}
	}
	switch {
	case h.ctx >= 0:
		f.station = uaCtxStation(fn, e.Args[h.ctx])
	case h.detailStation >= 0:
		f.station = types.ExprString(e.Args[h.detailStation])
	case h.stationUID >= 0:
		if s := types.ExprString(e.Args[h.stationUID]); s != `""` {
			f.station = s
		}
	}
	sites[site] = f
}

// uaFlagsBoundary reports whether a function sets BinUOPContext.Boundary.
func uaFlagsBoundary(fn ast.Node) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.AssignStmt:
			for _, l := range e.Lhs {
				if s, ok := l.(*ast.SelectorExpr); ok && s.Sel.Name == "Boundary" {
					found = true
				}
			}
		case *ast.KeyValueExpr:
			if id, ok := e.Key.(*ast.Ident); ok && id.Name == "Boundary" {
				found = true
			}
		}
		return !found
	})
	return found
}

// uaRoot is the identifier an argument is, or a selector chain starts from.
func uaRoot(x ast.Expr) string {
	for {
		switch e := x.(type) {
		case *ast.Ident:
			return e.Name
		case *ast.SelectorExpr:
			x = e.X
		default:
			return ""
		}
	}
}

// uaCtxStation is the expression a ledger call's context writes into the
// station column: a composite literal's Station field, or what the function
// assigns to <ctx>.Station; "" for none.
func uaCtxStation(fn ast.Node, ctx ast.Expr) string {
	switch c := ctx.(type) {
	case *ast.CompositeLit:
		for _, el := range c.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok && types.ExprString(kv.Key) == "StationUID" {
				return types.ExprString(kv.Value)
			}
		}
	case *ast.Ident:
		var out string
		ast.Inspect(fn, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			switch types.ExprString(as.Lhs[0]) {
			case c.Name + ".StationUID":
				out = types.ExprString(as.Rhs[0])
			case c.Name:
				if lit, ok := as.Rhs[0].(*ast.CompositeLit); ok && out == "" {
					out = uaCtxStation(fn, lit)
				}
			}
			return true
		})
		return out
	}
	return ""
}

func uaRecv(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return ""
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func uaLastName(x ast.Expr) string {
	switch e := x.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

func uaString(lit *ast.BasicLit) (string, bool) {
	if lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
