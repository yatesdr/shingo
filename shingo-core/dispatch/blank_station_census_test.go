package dispatch

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// blank_station_census_test.go — every place Core branches on a blank station,
// or names one of its own actors, and what each does for an actor's order.
//
// WHY. Three Core actors write their name where a station's uid goes:
// core-operator (Core's Orders and Bins pages), core-direct (the test-orders
// page's direct tab) and core-test (its Kafka tab, which sends as an Edge).
// Splitting the actor out of the station column leaves an actor's order with a
// blank station, and Core reads a blank station as "Core-born, nobody to tell"
// at every site below. Each is a place the split can change what an actor's
// order does, so each is listed with what it does today and what a blank
// station would make of it, for the change to decide site by site. This list is
// that review surface; TestCensus_BlankStationSites holds it to the tree.
//
// THE ORDERS, by what the branches read (station, station_unaware, edge_uuid):
//   - Edge: a station's own order (the wire doors) or a Core ask or window move
//     that names an Edge's uid. Its station is that uid.
//   - aware actor: station_unaware false and edge_uuid set, station an actor's
//     name: core-operator's spot transports and staged orders
//     (Handlers.apiManualOrderSubmit, through HandleOrderRequest and
//     HandleComplexOrderRequest), core-direct's complex orders
//     (Handlers.apiDirectComplexOrderSubmit), every core-test order (the Kafka
//     tab, through the wire as an Edge), and a dig leg of any of them (a leg
//     takes its parent's station, Dispatcher.issueLegsTx).
//   - unaware actor: the bin moves, station_unaware true and projects_to blank
//     (Engine.CreateBinMove): core-operator's (Handlers.apiRequestBinMove,
//     Handlers.submitSpotRetrieveSpecific) and core-direct's
//     (Handlers.apiDirectOrderSubmit).
//   - no actor: Core-born with a blank station, all station-unaware: the
//     carried-bin recovery and the cancel's return
//     (Engine.issueAndDispatchRecovery), and a window move or surplus move whose
//     window names no one Edge (Dispatcher.windowStation). projects_to is the
//     landing's Edge or blank. No door was found that issues a station-aware
//     order with a blank station.
// No Edge's uid is an actor's name, so a message Core sends to core-* reaches
// no Edge: it is an outbox row nobody consumes.
//
// HOW. The module's non-test source (scanCoreSources) is parsed and each
// declaration is keyed "dir:Recv.Func" (a package-level value "dir:Name"),
// with the count of each form found in it (bsForm): a station-named Go
// expression against "" (either operand order, through parentheses and
// strings.TrimSpace/ToLower/ToUpper), a local assigned from one under another
// name (st := o.StationID; stationID, ok := materialStation(o)), len(...) == 0,
// a switch with case "", cmp.Or; two station-named expressions compared with
// each other; in SQL text a station column, or an alias the declaration defines
// from one (… station_id END AS st), against '', a station matched against a
// list (the board scope); and a Core actor's name, in Go or SQL, or a "core"
// prefix test. "Station-named" includes ProjectsTo, the station a
// station-unaware order's material notices go to.
//
// WHAT IT CANNOT SEE: a station handed to a function under a parameter whose
// name says nothing of a station (who, s, uid), and a station passed on as an
// attribution rather than branched on (By: Person(o.StationID),
// CancelOrder(o, o.StationID, …)): those are value flows, not branches.
// The shared module holds no order type and imports nothing of shingo-core,
// and a source-shape pin reads only its own module, so it is not walked here;
// at e06b29b0 it has no site of any form (read by hand).

// bsKind says whether a site is about an order's station.
type bsKind int

const (
	// bsOrder: the branch reads an order's station, or an order-derived one
	// (its material station, an event or row carrying it), or names an actor.
	bsOrder bsKind = iota + 1
	// bsNotOrder: the station is not an order's: an Edge's envelope or report,
	// a kanban, a registry row, an RDS map point, a filter's parameter.
	bsNotOrder
	// bsHistory: migration text, which runs against the schema, not against an
	// order. The retired chain's sites (v66, v72 and its verify, the kanban
	// migration's census, the baseline DDL's partial uid index) went with it at
	// the squash (v184); v185's are the ones now.
	bsHistory
)

// bsSite is one declaration's branches. An order site says what each kind of
// order does at the branch today and what a blank station would make an
// actor's order do there if nothing were decided (split); any other site says
// why in why alone.
type bsSite struct {
	forms                     string
	kind                      bsKind
	edge, actors, none, split string
	why                       string
}

func notOrder(forms, why string) bsSite { return bsSite{forms: forms, kind: bsNotOrder, why: why} }
func history(forms, why string) bsSite  { return bsSite{forms: forms, kind: bsHistory, why: why} }

// bsNobody is what a send to an actor's name reaches.
const bsNobody = "an outbox row to core-* that no Edge reads"

var blankStationSites = map[string]bsSite{
	// ── Who may act on an order ───────────────────────────────────────────
	"dispatch:Dispatcher.checkOwnership": {forms: `station == station ×1`, kind: bsOrder,
		edge:   "its own cancel, release, receipt or redirect passes when the envelope's station is the order's; a leg's never does",
		actors: "core-operator sends as RoleCore and passes before the compare; core-direct and core-test send as RoleEdge under their own name and pass because the order's station is that name",
		none:   "no envelope names a blank station: only a RoleCore sender passes",
		split:  "DECIDED (names lane): the envelope is compared with order.Requester(), the station or the actor, so each actor still owns exactly its own orders and a blank sender gains none (TestPinDispatch_OwnershipBySender)"},
	"dispatch:Dispatcher.ReleasePoints": {forms: `station == station ×1`, kind: bsOrder,
		edge:   "an order is found only for the asking station (Doors.AskReleasePoints)",
		actors: "never asked for: only an Edge's ask reaches it, and an Edge asking an actor's order reads not found",
		none:   "not found",
		split:  "DECIDED (names lane): compared with Requester(); unchanged, no actor asks"},
	"engine:buildRobotPositions": {forms: `station == station ×1`, kind: bsOrder,
		edge:   "a watched order's robot goes to the watching station only when the order is that station's",
		actors: "no Edge watches one; one named in a watch is skipped",
		none:   "skipped",
		split:  "DECIDED (names lane): unchanged; OrderRobot.StationUID is the station, and no actor watches"},

	// ── What Core sends the order's station ───────────────────────────────
	"dispatch:LifecycleService.projectOrder": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "an admitted order's row is projected (order.projected) to its station",
		actors: "aware: core-operator's spot transports and core-test's plain orders are admitted here and projected to core-*, " + bsNobody + "; core-direct's complex orders are not admitted here; unaware (bin moves): not admitted here",
		none:   "not admitted here",
		split:  "DECIDED (names lane): projects to Requester(), so the projections to core-* go on as before"},
	"dispatch:Dispatcher.projectClear": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "a home clear's parent and partial rows are projected to the clearing station",
		actors: "not reached: a home clear is an Edge's ask (Doors.clearHome refuses a blank station)",
		none:   "not reached",
		split:  "DECIDED (names lane): projects to Requester(); unchanged"},
	"engine:dispatchEmitter.ProjectOrder": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "enqueues order.projected to the station projectOrder and projectClear name",
		actors: "aware: to core-*, " + bsNobody,
		none:   "a blank station sends nothing",
		split:  "DECIDED (names lane): its callers (projectOrder, projectClear) pass Requester(), so the projections to core-* go on"},
	"fulfillment:Scanner.notifyEdgeDispatched": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "the ack and the waybill go to the station",
		actors: "aware: both go to core-*, " + bsNobody + "; unaware: none (station_unaware)",
		none:   "none",
		split:  "DECIDED (names lane): reads and sends to Requester(), so the ack and waybill to core-* go on"},
	"store:DB.queueTellTx": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "every status write queues the order's station tell (order.update)",
		actors: "aware: queued to core-*, " + bsNobody + "; unaware: none",
		none:   "none",
		split:  "DECIDED (names lane): reads Told.Requester() (requested_by rides ToldReturning), so the tells to core-* go on"},
	"engine:Engine.wireEventHandlers": {forms: `StationID == "" ×4`, kind: bsOrder,
		edge:   "order.error, order.skipped and order.cancelled go to the station; a fault-cleared email takes the event's station",
		actors: "aware: the three go to core-*, " + bsNobody + "; unaware: none of the three (station_unaware); the cleared email names the actor",
		none:   "none of the three; the cleared email falls back to the station stored with its fault, also blank",
		split:  "DECIDED (names lane): every emit fills the event's StationID with order.Requester() (lifecycle transitions, the dispatcher's Fail/Skip/Cancel callers, nodegroup), so the sends to core-* and the cleared email's Station line go on"},
	"engine:Engine.notifyBinReturn": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "the cancelled order's station is told what became of its bin (data.bin_return)",
		actors: "aware and unaware alike (it does not read station_unaware, and a bin move carries an edge_uuid): sent to core-*, " + bsNobody,
		none:   "none",
		split:  "DECIDED (names lane): reads and sends to Requester(); the notices to core-* go on"},
	"engine:Engine.handleOrderDelivered": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "order.delivered goes to the order's material station (its own, or a window move's projects_to)",
		actors: "aware: to core-*, " + bsNobody + "; unaware: materialStation says nobody and it returns before this branch",
		none:   "station-unaware: the projects_to Edge or nobody; a station-aware order with a blank station would be broadcast to every Edge ('*')",
		split:  "DECIDED (names lane): materialStation returns Requester() for an aware order, so an actor's order.delivered goes to core-* as before and never to '*' (the step-0 engine pins)"},
	"engine:materialStation": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "a station-aware order's own station, true; a station-unaware one's projects_to, true only when it names one",
		actors: "aware: the actor's name, true; unaware: projects_to is blank, so nobody (false)",
		none:   "projects_to, or nobody",
		split:  "DECIDED (names lane): an aware order returns Requester(), the actor's name for an actor's order, so nothing is broadcast and the lift's notice keeps its station"},
	"service:BinService.Lift": {forms: `station == "" ×2`, kind: bsOrder,
		edge:   "the lifted ledger row's station is the uid, a repeated pickup reads the generation, and bin_picked_up goes to the station (the notice's station is materialStation's)",
		actors: "aware: the lifted row's station is core-* (the rows v185 blanks) and bin_picked_up goes to core-*, " + bsNobody + "; unaware: blank station, no notice",
		none:   "the projects_to Edge, or blank and no notice",
		split:  "DECIDED (names lane): the notice's station is materialStation's, the requester, as before; the lifted ledger row splits it (station_uid for a station, actor = the Core actor for an actor's order: the move rules)"},
	"service:landingStations": {forms: `StationID == "" ×1, station == station ×1`, kind: bsOrder,
		edge:   "the one claiming station, or with none the order's material station",
		actors: "aware: on a node no station claims, the landing goes to core-*, " + bsNobody + "; unaware: its material station is blank, so on an unclaimed node the plant arm: every Edge",
		none:   "as an unaware actor's: the plant arm on an unclaimed node",
		split:  "DECIDED (names lane): unchanged; the order's material station comes from ResolveLandingAddress, which reads the requester"},
	"store/plantclaims:ResolveLandingAddress": {forms: `SQL station = '' ×1, SQL alias = '' ×2`, kind: bsOrder,
		edge:   "the order row carries its material station (st) and that Edge's schema version; the plant arm is read only when no station claims the node and st is blank",
		actors: "aware: st is core-* (schema version 0) and no plant arm is read; unaware: st is projects_to, blank, so the plant arm is read on an unclaimed node",
		none:   "as an unaware actor's",
		split:  "DECIDED (names lane): st is the requester (station_uid, else requested_by) for an aware order, so an actor's landing goes to core-* and not to the plant arm"},

	// ── What a person reads ───────────────────────────────────────────────
	"service:NodeService.StationName": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "the station's display name, or its uid when it has none",
		actors: "Core · Manual, Core · Direct, Core · Test (CoreSourceLabels)",
		none:   "\"\": nothing is printed (a recovery is labelled CoreRecoveryLabel where it is shown)",
		split:  "DECIDED (names lane): the readers pass the requester (templates .Requester; JS station_uid || requested_by), so the labels read as before"},
	"service:CoreSourceLabels": {forms: `Core actor name ×3`, kind: bsOrder,
		edge:   "not read for an Edge's uid",
		actors: "the label of each actor's name, found by StationName from the order's station",
		none:   "not read",
		split:  "DECIDED (names lane): kept; looked up with the requester"},
	"notify:FaultAlert": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "the fault email prints Station: <uid>",
		actors: "prints Station: core-*",
		none:   "no Station line",
		split:  "DECIDED (names lane): the event's station is the requester where one is filled; unchanged (MarkFaulted fills none, for any order)"},
	"notify:FailAlert": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "the failure email prints Station: <uid>",
		actors: "prints Station: core-*",
		none:   "no Station line",
		split:  "DECIDED (names lane): the event's station is the requester (Fail and FailWithRef callers pass Requester()), so the Station line reads as before"},
	"notify:FaultClearedAlert": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "the cleared email prints Station: <uid>",
		actors: "prints Station: core-*",
		none:   "no Station line",
		split:  "DECIDED (names lane): as FaultAlert; unchanged"},
	"dispatch:FutilityKey.incomplete": {forms: `StationID == "" ×1`, kind: bsOrder,
		edge:   "a terminal that never reached in_transit counts against (station, process node, payload) when all three are set",
		actors: "counted under the actor's name only when the order names a process node and a payload (a test-page swap's legs can; a spot order names no process node)",
		none:   "never counted",
		split:  "DECIDED (names lane): noteFutility keys by Requester(), so an actor's futile asks count under its name as before"},
	"engine:moveActor": {forms: `StationID == "" ×1, station == "" ×1`, kind: bsOrder,
		edge:   "not reached: every caller of Engine.CreateBinMove is a Core page",
		actors: "the history row that takes a stranger's hold names core-operator or core-direct",
		none:   "\"bin-move\"; no caller passes a blank station today",
		split:  "DECIDED (names lane): the door names RequestedBy, and moveActor falls back to it, so the history still names core-operator or core-direct"},
	"store/orders:ListDistinctStations": {forms: `SQL alias = '' ×1`, kind: bsOrder,
		edge:   "each uid seen on an order is listed, for the station pickers (Handlers.apiStations)",
		actors: "core-operator, core-direct and core-test are listed, so the board scope and the missions filter offer them",
		none:   "a blank station is not listed",
		split:  "DECIDED (names lane): the list is the union of station_uid and requested_by, so the pickers still offer the actors"},
	"store/orders:ListActiveBoardFiltered": {forms: `SQL station IN (…) ×2`, kind: bsOrder,
		edge:   "a board scoped (dashboards.stations_json) to a uid shows that station's active orders",
		actors: "a board scoped to an actor's name shows that actor's active orders",
		none:   "shown only on an unscoped board",
		split:  "DECIDED (names lane): each entry is matched in its own column (station_uid IN, requested_by IN), the set station_id IN matched before"},
	"www:Handlers.apiMissionsActive": {forms: `station == "" ×2, station == station ×1`, kind: bsOrder,
		edge:   "with a station filter, counts that station's active orders; with no filter, the fast unfiltered count",
		actors: "a filter naming an actor counts that actor's active orders",
		none:   "counted only unfiltered",
		split:  "DECIDED (names lane): compares the filter with Requester(), so an actor filter counts its orders as before"},

	// ── The actors' own doors ─────────────────────────────────────────────
	"www:Handlers.apiManualOrderSubmit": {forms: `Core actor name ×1`, kind: bsOrder,
		edge:   "not reached: Core's Orders page",
		actors: "core-operator's spot transports and staged orders are sent as RoleCore from core-operator, which intake writes as the order's station (aware)",
		none:   "not reached",
		split:  "DECIDED (names lane): the envelope still names core-operator (domain.RequesterOperator); intake splits it into requested_by with a blank station"},
	"www:Handlers.submitSpotRetrieveSpecific": {forms: `Core actor name ×1`, kind: bsOrder,
		edge:   "not reached: Core's Orders page",
		actors: "core-operator's named-bin move: Engine.CreateBinMove with station core-operator (unaware)",
		none:   "not reached",
		split:  "DECIDED (names lane): the door names RequestedBy core-operator and leaves the station blank"},
	"www:Handlers.apiRequestBinMove": {forms: `Core actor name ×1`, kind: bsOrder,
		edge:   "not reached: Core's Bins page",
		actors: "core-operator's bin move: Engine.CreateBinMove with station core-operator (unaware)",
		none:   "not reached",
		split:  "DECIDED (names lane): the door names RequestedBy core-operator and leaves the station blank"},
	"www:Handlers.apiDirectOrderSubmit": {forms: `Core actor name ×1`, kind: bsOrder,
		edge:   "not reached: the test-orders page's direct tab",
		actors: "core-direct's bin move: Engine.CreateBinMove with station core-direct (unaware)",
		none:   "not reached",
		split:  "DECIDED (names lane): the door names RequestedBy core-direct and leaves the station blank"},
	"www:Handlers.apiDirectComplexOrderSubmit": {forms: `Core actor name ×1`, kind: bsOrder,
		edge:   "not reached: the direct tab",
		actors: "core-direct's complex orders, sent in-process as RoleEdge from core-direct (aware)",
		none:   "not reached",
		split:  "DECIDED (names lane): complex intake splits the envelope's core-direct into requested_by (domain.SplitRequester)"},
	"www:Handlers.apiDirectOrderRelease": {forms: `Core actor name ×1`, kind: bsOrder,
		edge:   "not reached: the direct tab",
		actors: "releases as RoleEdge from core-direct, which passes Dispatcher.checkOwnership because the order's station is core-direct",
		none:   "not reached",
		split:  "DECIDED (names lane): ownership compares with Requester(), so core-direct's release passes as before"},
	"www:Handlers.apiDirectOrdersList": {forms: `Core actor name ×1`, kind: bsOrder,
		edge:   "not reached",
		actors: "lists core-direct's orders by station (ListOrdersByStation)",
		none:   "not listed",
		split:  "DECIDED (names lane): ListByStation matches a Core actor in requested_by (RequesterColumn), so the list is core-direct's as before"},
	"www:Handlers.apiTestOrderSubmit": {forms: `Core actor name ×1`, kind: bsOrder,
		edge:   "not reached: the test-orders page's Kafka tab",
		actors: "core-test's plain orders, published as RoleEdge from core-test through the wire (aware)",
		none:   "not reached",
		split:  "DECIDED (names lane): intake splits the envelope's core-test into requested_by (domain.SplitRequester)"},
	"www:Handlers.apiKafkaComplexOrderSubmit": {forms: `Core actor name ×1`, kind: bsOrder,
		edge:   "not reached: the Kafka tab",
		actors: "core-test's complex orders, published as RoleEdge from core-test (aware)",
		none:   "not reached",
		split:  "DECIDED (names lane): complex intake splits the envelope's core-test into requested_by"},
	"www:Handlers.apiTestOrderCancel": {forms: `Core actor name ×1`, kind: bsOrder,
		edge:   "not reached: the Kafka tab",
		actors: "cancels as RoleEdge from core-test, which passes Dispatcher.checkOwnership",
		none:   "not reached",
		split:  "DECIDED (names lane): ownership compares with Requester(), so core-test's cancel passes as before"},
	"www:Handlers.apiTestOrderReceipt": {forms: `Core actor name ×1`, kind: bsOrder,
		edge:   "not reached: the Kafka tab",
		actors: "confirms receipt as RoleEdge from core-test, which passes Dispatcher.checkOwnership",
		none:   "not reached",
		split:  "DECIDED (names lane): ownership compares with Requester(), so core-test's receipt passes as before"},
	"www:Handlers.apiTestOrdersList": {forms: `Core actor name ×1`, kind: bsOrder,
		edge:   "not reached",
		actors: "lists core-test's orders by station (ListOrdersByStation)",
		none:   "not listed",
		split:  "DECIDED (names lane): ListByStation matches a Core actor in requested_by, so the list is core-test's as before"},

	// ── The requester (names lane): one spelling of who asked ───────────────
	"domain:Order.Requester": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "the station's uid",
		actors: "the actor's name, from requested_by",
		none:   "\"\": a Core-born order with neither",
		split:  "DECIDED (names lane): the requester is the value station_id held before v185; every site above that addressed, labelled, filtered or owned by it reads this"},
	"store/orders:Told.Requester": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "the told order's station uid",
		actors: "the actor's name (requested_by rides ToldReturning)",
		none:   "\"\": nothing is told",
		split:  "DECIDED (names lane): Order.Requester for a status write's RETURNING row (DB.queueTellTx)"},
	"store/internal/helpers:RequesterSQL": {forms: `SQL station = '' ×1`, kind: bsOrder,
		edge:   "station_uid",
		actors: "requested_by",
		none:   "''",
		split:  "DECIDED (names lane): Order.Requester in SQL, for the readers that group or address by it (the orphan trend and findings, the ETA backfill)"},
	"engine:Engine.CreateBinMove": {forms: `station == "" ×1`, kind: bsOrder,
		edge:   "not reached with a station: every caller is a Core page or a test",
		actors: "a caller that names its actor as RequestedBy keeps it; one that names it as the station (as the doors did before v185) has it moved to RequestedBy with a blank station (SplitRequester)",
		none:   "a blank station stays blank: the bin-move history names \"bin-move\"",
		split:  "DECIDED (names lane): the order is born with requested_by = the actor and a blank station, whichever way the caller named it"},
	"domain:CoreActors":        notOrder(`Core actor name ×3`, "the Core actors' one Go list, which IsCoreActor reads: SplitRequester's and RequesterColumn's question, is this sender or requester a Core actor"),
	"domain:RequesterOperator": notOrder(`Core actor name ×2`, "core-operator, defined once"),
	"domain:RequesterDirect":   notOrder(`Core actor name ×2`, "core-direct, defined once"),
	"domain:RequesterTest":     notOrder(`Core actor name ×2`, "core-test, defined once"),

	// ── Not an order's station ────────────────────────────────────────────
	"cmd/seeddev:main":                                    notOrder(`station == "" ×1`, "the dev seeder's -station flag, defaulted from the plant spec"),
	"cmd/seeddev:seedEdgeDB":                              notOrder(`station == "" ×1`, "a plant-spec claim's operator station, seeding an Edge database"),
	"cmd/soakstat:claimsMirror":                           notOrder(`SQL station = '' ×2`, "process_styles and edge_registry stations, in the soak report"),
	"cmd/soakstat:readEdge":                               notOrder(`station == "" ×1`, "an Edge database's lineside sample station, in the soak report"),
	"domain:BuildCycleSeries":                             notOrder(`station == "" ×1, station == station ×1`, "a cycle event's station, read from bin_uop_ledger.actor on bin_uop_delta rows (store/audit cycle time): the reporting Edge's uid, which the uid-under-actor census moves"),
	"engine:Engine.HandleRobotWatch":                      notOrder(`station == "" ×1`, "the watching Edge's envelope station"),
	"engine:Engine.RequestEdgeReregister":                 notOrder(`station == "" ×1`, "blank asks every Edge to re-register (the dashboard's re-sync)"),
	"engine:KanbanKeeper.tickKanban":                      notOrder(`station == "" ×1`, "a kanban's station (kanbans.station_uid): no station, no keeper"),
	"engine:SourceabilityMonitor.snapshotFor":             notOrder(`station == station ×2`, "a process's station against the station a sourceability report is for"),
	"engine:ThresholdMonitor.Resync":                      notOrder(`station == station ×1`, "a demand threshold's station (demand_registry)"),
	"engine:lidarCandidate":                               notOrder(`station == "" ×1`, "an RDS map point (robot CurrentStation)"),
	"engine:lidarKeep":                                    notOrder(`station == "" ×1, station == station ×1`, "RDS map points (robot CurrentStation)"),
	"engine:strandedDetail":                               notOrder(`station == "" ×1, station == station ×1`, "RDS map points (robot LastStation, CurrentStation)"),
	"engine:strandedNote":                                 notOrder(`station == "" ×1, alias == "" ×1`, "RDS map points (robot CurrentStation, LastStation)"),
	"messaging:CoreDataService.HandleEdgeCells":           notOrder(`station == "" ×1`, "the sending Edge's envelope station: a cell catalog with none has no station to apply to and is dropped"),
	"messaging:CoreDataService.HandleLinesideLevelReport": notOrder(`station == "" ×2`, "the reporting Edge (the report's station, else its envelope's)"),
	"messaging:CoreDataService.HandlePlantClaims":         notOrder(`station == "" ×1, alias == "" ×1, station == station ×1`, "the Edge publishing a process's claims, against the one that published it before"),
	"messaging:CoreDataService.unlistedFor":               notOrder(`station == "" ×1`, "the asking Edge's envelope station; it lists that station's active orders, which no actor asks for"),
	"messaging:loadersNameAnEdge":                         notOrder(`station == "" ×1`, "a loader's Edge (bin_loaders.station_uid)"),
	"messaging:nameLoaderEdges":                           notOrder(`alias == "" ×1`, "a loader's Edge (bin_loaders.station_uid)"),
	"plantspec:Plant.ForEdge":                             notOrder(`station == "" ×1`, "a plant-spec claim's operator station"),
	"plantspec:Plant.Validate":                            notOrder(`station == "" ×3`, "plant-spec stations: the plant's own Edge (station_uid), a claim's operator station, a kanban's station"),
	"service:BinManifestService.syncOrClearForReleased":   notOrder(`station == "" ×1`, "a release's stream fence, the releasing Edge's own statement"),
	"service:CountResult.FenceFor":                        notOrder(`station == "" ×1`, "a count's fence for the asking Edge's stream (Doors.binCount); no station is a count made at Core's bins page, which asks about no stream"),
	"service:KanbanConfig.isKanban":                       notOrder(`station == "" ×1`, "a kanban's maintenance station"),
	"service:NodeService.ValidateKanban":                  notOrder(`station == "" ×2`, "a kanban's maintenance station"),
	"service:NodeService.stationNameMap":                  notOrder(`station == "" ×1`, "edge_registry rows with no uid or display name are not labels"),
	"store/dashboards:scanRow":                            notOrder(`station == "" ×1`, "blank stations_json text reads as no scope"),
	"store/orders:filterWhere":                            notOrder(`StationID == "" ×1`, "a filter: blank means unscoped; DECIDED (names lane): a picked actor name is matched in requested_by (RequesterColumn), so it still matches that actor's orders"),
	"store/plantclaims:LastArmEdgesSQL":                   notOrder(`SQL station = '' ×1`, "edge_registry.station_uid: the plant arm's registered Edges"),
	"store/telemetry:GetDailyConcurrency":                 notOrder(`station == "" ×1`, "a filter on mission_telemetry's requester: blank is unscoped; DECIDED (names lane): an actor name is matched in requested_by (RequesterColumn), so it still matches that actor's rows"),
	"store/telemetry:GetHourlyConcurrency":                notOrder(`station == "" ×1`, "a filter on mission_telemetry's requester, as GetDailyConcurrency"),
	"store/telemetry:GetRobotMissionAggs":                 notOrder(`StationID == "" ×1`, "a filter on mission_telemetry's requester, as GetDailyConcurrency"),
	"store/telemetry:buildLiveWhere":                      notOrder(`StationID == "" ×1`, "a filter on the orders' requester for the live missions, as GetDailyConcurrency"),
	"store/telemetry:buildWhere":                          notOrder(`StationID == "" ×1`, "a filter on mission_telemetry's requester, as GetDailyConcurrency"),
	"store/telemetry:orderOutcomeWhere":                   notOrder(`StationID == "" ×1`, "a filter on the orders' requester for order outcomes, as GetDailyConcurrency"),
	"store:DB.ListEdgeCells":                              notOrder(`station == "" ×1`, "an edge_cells filter: blank lists every station's cells"),
	"store:kanbanSave.applyScalars":                       notOrder(`station == station ×1`, "a kanban's station before and after a save"),
	"uop:InventoryDeltaService.ApplyBinUOPDelta":          notOrder(`station == "" ×1`, "the counting Edge's envelope station"),
	"uop:InventoryDeltaService.repairEpoch":               notOrder(`station == "" ×1`, "the counting Edge's station; blank broadcasts the epoch refresh"),
	"uop:InventoryDeltaService.validateBucketLevel":       notOrder(`station == "" ×1`, "the reporting Edge's envelope station"),
	"www:Doors.clearHome":                                 notOrder(`station == "" ×1`, "the asking Edge's station"),
	"www:Doors.releasePoints":                             notOrder(`station == "" ×1`, "the asking Edge's station"),
	"www:Handlers.apiCellProcesses":                       notOrder(`station == "" ×1`, "a station query parameter: the processes ticking there (cell_part_events)"),
	"www:Handlers.apiListLoaders":                         notOrder(`station == "" ×1`, "a loader's Edge (bin_loaders.station_uid)"),
	"www:Handlers.apiStations":                            notOrder(`station == "" ×1`, "edge_registry rows with no station_uid are skipped (v185's CHECK leaves none); the order stations come from ListDistinctStations"),
	"www:robotsAtNodes":                                   notOrder(`alias == "" ×1`, "an RDS map point (robot CurrentStation)"),

	// ── Migration text ─────────────────────────────────────────────────────
	"store:v185Actors":         history(`Core actor name ×3`, "v185's three actors, which it moves out of the station columns into requested_by"),
	"store:v185Ops":            history(`SQL station = '' ×5, SQL station IN (…) ×2`, "v185's CHECKs: every edge_registry row has its uid, no Core actor stands in orders' or mission_telemetry's station_uid, and an order or a mission has one requester, its station or a Core actor"),
	"store:v185Moves":          history(`SQL station = '' ×12, SQL station IN (…) ×3`, "v185's data moves and their inverses: the actors out of the station columns, the uid out of actor, and back"),
	"store:v185Down":           history(`SQL station = '' ×1`, "v185's down step: it puts back the partial uid index, which leaves out a blank uid"),
	"store:v185FoldRegistry":   history(`station == "" ×1, SQL station = '' ×3`, "v185's registry fold: a blank uid takes the row's station_id; a colliding or both-blank row is deleted"),
	"store:v185LogReaddressed": history(`SQL station = '' ×1`, "v185's registry log: a row whose uid is set and is not its station_id is named, since the moved readers address it by the uid; it changes no row"),
}

// TestCensus_BlankStationSites holds blankStationSites to the tree: every
// declaration with a branch on a blank station (or an actor's name) is named
// with exactly the forms it has, and every named one is still there.
//
// Red when: a branch is added anywhere (a new key, or a listed function's
// count of a form goes up), a listed branch goes or moves to another
// function, or an entry is left half-written.
func TestCensus_BlankStationSites(t *testing.T) {
	t.Parallel()
	got, where := blankStationScan(t)
	const decide = "Decide what an actor's order (core-operator, core-direct, core-test) and a Core-born order " +
		"with no station do at this branch, and name it in blankStationSites (dispatch/blank_station_census_test.go)"
	for _, key := range slices.Sorted(maps.Keys(got)) {
		site, ok := blankStationSites[key]
		switch {
		case !ok:
			t.Errorf("%s branches on a blank station or names an actor (%s) and is not in blankStationSites:\n    %s\n%s, "+
				"or tag it notOrder and say why.", key, got[key], strings.Join(where[key], "\n    "), decide)
		case site.forms != got[key]:
			t.Errorf("%s: the tree has %s; blankStationSites names %s:\n    %s\n%s: a new branch in a listed function "+
				"needs its own decision, and a removed one may change the entry's text.",
				key, got[key], site.forms, strings.Join(where[key], "\n    "), decide)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(blankStationSites)) {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is in blankStationSites but the tree has no such branch in it: the function was renamed or "+
				"the branch moved or went. Re-key the entry to where the branch went and check its decision still "+
				"holds there, or delete the entry.", key)
		}
		s := blankStationSites[key]
		order := s.edge != "" && s.actors != "" && s.none != "" && s.split != "" && s.why == ""
		other := s.why != "" && s.edge == "" && s.actors == "" && s.none == "" && s.split == ""
		if (s.kind == bsOrder && !order) || (s.kind != bsOrder && !other) {
			t.Errorf("%s: an order site says edge, actors, none and split; any other site says why alone", key)
		}
	}
}

// bsForm is one shape of branch the scan finds.
type bsForm string

const (
	bsStationID   bsForm = `StationID == ""`
	bsStationName bsForm = `station == ""`
	bsAlias       bsForm = `alias == ""`
	bsLen         bsForm = `len(station) == 0`
	bsSwitch      bsForm = `switch station { case "" }`
	bsCmpOr       bsForm = `cmp.Or(station, …)`
	bsPair        bsForm = `station == station`
	bsSQLBlank    bsForm = `SQL station = ''`
	bsSQLAlias    bsForm = `SQL alias = ''`
	bsActor       bsForm = `Core actor name`
	bsBoardScope  bsForm = `SQL station IN (…)`
)

var bsFormOrder = []bsForm{bsStationID, bsStationName, bsAlias, bsLen, bsSwitch, bsCmpOr, bsPair,
	bsSQLBlank, bsSQLAlias, bsActor, bsBoardScope}

var (
	// bsNameRe is a Go name that holds a station: StationID, station, the
	// material station's ProjectsTo, a Station field, and so on.
	bsNameRe = regexp.MustCompile(`(?i)station|projects_?to|requester|requested_?by`)
	// bsLenNameRe is the len() form's operand: one station string (station,
	// StationID, EdgeStationUID, ProjectsTo), not a list or map named for
	// stations (perStation, byStation), which len() also takes.
	bsLenNameRe   = regexp.MustCompile(`(?i)^(station|projects_?to|\w*station_?u?id)$`)
	bsSQLName     = `[\w.]*(?:station|projects_to|requested_by)\w*`
	bsSQLAliasDef = regexp.MustCompile(`(?i)(?:station|projects_to|requested_by)\w*(?:\s+END)?\s+AS\s+(\w+)`)
	bsSQLActorRe  = regexp.MustCompile(`(?i)'core(?:-|%|')`)
	bsBoardRe     = regexp.MustCompile(`(?i)\b` + bsSQLName + `\s+IN\s*\(`)
	bsActorNames  = map[string]bool{"core-operator": true, "core-direct": true, "core-test": true}
	// bsActorConsts are the actors' one Go spelling (domain/requester.go); a use
	// of one names the actor as its string literal did.
	bsActorConsts = map[string]bool{"RequesterOperator": true, "RequesterDirect": true, "RequesterTest": true}
	bsSQLBlankRe  = bsSQLBlankFor(bsSQLName)
)

// bsSQLBlankFor is every SQL comparison of a column named by name against ”.
func bsSQLBlankFor(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:COALESCE\s*\(\s*)?\b` + name + `\b(?:\s*,\s*''\s*\))?\s*(?:=|<>|!=)\s*''` +
		`|''\s*(?:=|<>|!=)\s*\b` + name + `\b` +
		`|NULLIF\s*\(\s*` + name + `\s*,\s*''\s*\)` +
		`|\b` + name + `\s+IS\s+(?:NOT\s+)?DISTINCT\s+FROM\s+''`)
}

// blankStationScan finds every site in the module's non-test source, by
// declaration, with the forms found in it ("form ×n", in bsFormOrder), and
// where each one is (file:line form), for the failure message.
func blankStationScan(t *testing.T) (map[string]string, map[string][]string) {
	t.Helper()
	sources := scanCoreSources(t)
	if len(sources) < 500 {
		t.Fatalf("read %d source files; a census that reads nothing reports nothing", len(sources))
	}
	counts := map[string]map[bsForm]int{}
	where := map[string][]string{}
	at := map[string][]bsWhere{}
	fset := token.NewFileSet()
	for rel, src := range sources {
		f, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		dir := path.Dir(rel)
		visit := func(key string, n ast.Node) {
			bsInspect(n, func(form bsForm, p token.Pos) {
				if counts[key] == nil {
					counts[key] = map[bsForm]int{}
				}
				counts[key][form]++
				pos := fset.Position(p)
				at[key] = append(at[key], bsWhere{rel, pos.Line, form})
			})
		}
		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				key := dir + ":" + decl.Name.Name
				if r := bsRecv(decl); r != "" {
					key = dir + ":" + r + "." + decl.Name.Name
				}
				visit(key, decl)
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						visit(dir+":"+vs.Names[0].Name, vs)
					}
				}
			}
		}
	}
	out := map[string]string{}
	for key, byForm := range counts {
		var parts []string
		for _, form := range bsFormOrder {
			if n := byForm[form]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s ×%d", form, n))
			}
		}
		out[key] = strings.Join(parts, ", ")
		slices.SortFunc(at[key], func(a, b bsWhere) int { return cmp.Or(strings.Compare(a.rel, b.rel), a.line-b.line) })
		for _, w := range at[key] {
			where[key] = append(where[key], fmt.Sprintf("%s:%d %s", w.rel, w.line, w.form))
		}
	}
	return out, where
}

func bsRecv(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return ""
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if ix, ok := t.(*ast.IndexExpr); ok {
		t = ix.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// bsAliases is every local the declaration assigns from a station-named
// expression or call (st := o.StationID; stationID, ok := materialStation(o))
// under a name that does not itself say station.
func bsAliases(n ast.Node) map[string]bool {
	out := map[string]bool{}
	note := func(lhs []ast.Expr, rhs []ast.Expr) {
		for i, l := range lhs {
			id, ok := l.(*ast.Ident)
			if !ok || id.Name == "_" || bsNameRe.MatchString(id.Name) {
				continue
			}
			var r ast.Expr
			switch {
			case len(rhs) == len(lhs):
				r = rhs[i]
			case len(rhs) == 1 && i == 0:
				r = rhs[0]
			default:
				continue
			}
			if bsNameRe.MatchString(bsName(r)) || bsNameRe.MatchString(bsCallName(r)) {
				out[id.Name] = true
			}
		}
	}
	ast.Inspect(n, func(x ast.Node) bool {
		switch s := x.(type) {
		case *ast.AssignStmt:
			note(s.Lhs, s.Rhs)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, len(s.Names))
			for i, id := range s.Names {
				lhs[i] = id
			}
			note(lhs, s.Values)
		}
		return true
	})
	return out
}

func bsInspect(n ast.Node, note func(bsForm, token.Pos)) {
	aliases := bsAliases(n)
	sqlAliases := bsSQLAliases(n)
	// stationy is the form a blank comparison of x takes, "" for none.
	stationy := func(x ast.Expr) bsForm {
		name := bsName(x)
		switch {
		case name == "StationID" && bsIsSelector(x):
			return bsStationID
		case bsNameRe.MatchString(name):
			return bsStationName
		case aliases[name] && !bsIsSelector(x):
			return bsAlias
		}
		return ""
	}
	ast.Inspect(n, func(x ast.Node) bool {
		switch e := x.(type) {
		case *ast.BinaryExpr:
			if e.Op == token.EQL || e.Op == token.NEQ {
				switch {
				case bsIsBlank(e.Y) && stationy(e.X) != "":
					note(stationy(e.X), e.Pos())
				case bsIsBlank(e.X) && stationy(e.Y) != "":
					note(stationy(e.Y), e.Pos())
				case !bsIsLit(e.X) && !bsIsLit(e.Y) && stationy(e.X) != "" && stationy(e.Y) != "":
					note(bsPair, e.Pos())
				}
			}
			for _, pair := range [][2]ast.Expr{{e.X, e.Y}, {e.Y, e.X}} {
				call, ok := bsUnparen(pair[0]).(*ast.CallExpr)
				if !ok || len(call.Args) != 1 || !bsIsZero(pair[1]) {
					continue
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "len" && bsLenNameRe.MatchString(bsName(call.Args[0])) {
					note(bsLen, e.Pos())
				}
			}
		case *ast.SwitchStmt:
			if e.Tag == nil || stationy(e.Tag) == "" {
				return true
			}
			for _, s := range e.Body.List {
				for _, v := range s.(*ast.CaseClause).List {
					if bsIsBlank(v) {
						note(bsSwitch, v.Pos())
					}
				}
			}
		case *ast.CallExpr:
			sel, ok := e.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, _ := sel.X.(*ast.Ident)
			switch {
			case pkg == nil:
			case pkg.Name == "cmp" && sel.Sel.Name == "Or" && len(e.Args) > 0 && stationy(e.Args[0]) != "":
				note(bsCmpOr, e.Pos())
			case pkg.Name == "strings" && (sel.Sel.Name == "HasPrefix" || sel.Sel.Name == "CutPrefix") && len(e.Args) == 2:
				if s, ok := bsString(e.Args[1]); ok && strings.HasPrefix(s, "core") {
					note(bsActor, e.Pos())
				}
			}
		case *ast.Ident:
			if bsActorConsts[e.Name] {
				note(bsActor, e.Pos())
			}
		case *ast.BasicLit:
			bsLiteral(e, sqlAliases, note)
		}
		return true
	})
}

// bsSQLAliases is the blank comparison of every alias a SQL literal in the
// declaration defines from a station column (… station_id END AS st), nil
// when none does. A statement is often spliced from several literals, so an
// alias one defines is looked for in all of them.
func bsSQLAliases(n ast.Node) *regexp.Regexp {
	var names []string
	ast.Inspect(n, func(x ast.Node) bool {
		if lit, ok := x.(*ast.BasicLit); ok {
			s, _ := bsString(lit)
			for _, def := range bsSQLAliasDef.FindAllStringSubmatch(s, -1) {
				names = append(names, regexp.QuoteMeta(def[1]))
			}
		}
		return true
	})
	if len(names) == 0 {
		return nil
	}
	return bsSQLBlankFor(`(?:\w+\.)?(?:` + strings.Join(names, "|") + `)`)
}

// bsLiteral notes the forms a string literal carries: a Core actor's name as
// the whole value, and in SQL text a station column against ”, an alias the
// declaration defines from a station column against ”, an actor name, and a
// station matched against a list.
func bsLiteral(lit *ast.BasicLit, sqlAliases *regexp.Regexp, note func(bsForm, token.Pos)) {
	s, ok := bsString(lit)
	if !ok {
		return
	}
	at := func(off int) token.Pos { return lit.Pos() + token.Pos(off) }
	if bsActorNames[s] {
		note(bsActor, lit.Pos())
	}
	if lit.Value[0] != '`' {
		// An interpreted literal's offsets do not map back onto the source;
		// the literal's own position stands for every match in it.
		at = func(int) token.Pos { return lit.Pos() }
	}
	for _, m := range bsSQLBlankRe.FindAllStringIndex(s, -1) {
		note(bsSQLBlank, at(m[0]+1))
	}
	if sqlAliases != nil {
		for _, m := range sqlAliases.FindAllStringIndex(s, -1) {
			note(bsSQLAlias, at(m[0]+1))
		}
	}
	for _, m := range bsSQLActorRe.FindAllStringIndex(s, -1) {
		note(bsActor, at(m[0]+1))
	}
	for _, m := range bsBoardRe.FindAllStringIndex(s, -1) {
		note(bsBoardScope, at(m[0]+1))
	}
}

func bsUnparen(x ast.Expr) ast.Expr {
	for {
		p, ok := x.(*ast.ParenExpr)
		if !ok {
			return x
		}
		x = p.X
	}
}

func bsIsSelector(x ast.Expr) bool {
	_, ok := bsUnparen(x).(*ast.SelectorExpr)
	return ok
}

// bsName is the last name of an identifier or field, seen through
// strings.TrimSpace / ToLower / ToUpper.
func bsName(x ast.Expr) string {
	switch e := bsUnparen(x).(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.CallExpr:
		// o.Requester(): the order's station, or its Core actor.
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && len(e.Args) == 0 && sel.Sel.Name == "Requester" {
			return sel.Sel.Name
		}
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && len(e.Args) == 1 {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "strings" {
				switch sel.Sel.Name {
				case "TrimSpace", "ToLower", "ToUpper":
					return bsName(e.Args[0])
				}
			}
		}
	}
	return ""
}

// bsCallName is the called function's last name, "" when x is not a call.
func bsCallName(x ast.Expr) string {
	call, ok := bsUnparen(x).(*ast.CallExpr)
	if !ok {
		return ""
	}
	return bsName(call.Fun)
}

func bsIsLit(x ast.Expr) bool {
	_, ok := bsUnparen(x).(*ast.BasicLit)
	return ok
}

func bsIsBlank(x ast.Expr) bool {
	s, ok := bsString(x)
	return ok && s == ""
}

func bsIsZero(x ast.Expr) bool {
	lit, ok := bsUnparen(x).(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "0"
}

func bsString(x ast.Expr) (string, bool) {
	lit, ok := bsUnparen(x).(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// bsWhere is one branch's place, for the failure message.
type bsWhere struct {
	rel  string
	line int
	form bsForm
}
