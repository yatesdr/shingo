package messaging

import (
	"fmt"
	"go/ast"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// feed_doors_census_test.go — every write of what the node list, the scene and
// the catalog are built from reaches a change door, or is named here as one
// that does not.
//
// WHY. The doors (service.FeedDoors: FeedsChangedPlantWide, FeedsChangedAt,
// wired to CoreDataService.FeedsChanged) forget the memos a write made stale
// and push each Edge its new copies a moment later. A write that reaches no
// door is caught by the heartbeat compare once the memo ages out (two minutes
// for a station's node list): that is the backstop, not the promise.
//
// HOW, on the walker TestBumpEpoch_EverySiteReachesABinsPushOrIsNamed uses
// (parseCoreCallGraph):
//   - a SITE is a function whose string literals insert into, update or delete
//     from a table a feed build reads (feedWriteSQL). Each is named in
//     feedSites: as a feed write, or with the reason no feed reads what it
//     writes;
//   - the store's API for a feed write is every function in the store packages
//     on a path from a feed site;
//   - a WRITER is a function outside the store packages that calls a method of
//     that API on a store-shaped receiver (db, x.db) or one of its package
//     functions. Each is named in feedWriters: a door (its body, or the helper
//     named, calls a change door), an entry with the reason it reaches none, a
//     helper whose production callers are listed (each its own entry), or one
//     with no production caller.
//
// What it cannot see: a store reached through a receiver of another name; the
// columns an UPDATE sets (a site that writes only columns no feed reads is
// named so, by reading); SQL keywords not in upper case; and whether the door
// is called after the write in the body.

// feedWriteSQL is a statement writing a table one of the three builds reads
// (buildNodeList, buildScene, buildCatalog and the reads they make).
var feedWriteSQL = regexp.MustCompile(`\b(INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+(nodes|node_stations|kanban_levels|` +
	`bin_loaders|bin_loader_homes|bin_loader_payloads|edge_registry|payload_bin_types|bin_types|carrier_families|` +
	`payloads|payload_manifest|parts|scene_points|scene_edges|style_claims|process_styles)\b`)

// noteFeedCall notes what the change doors census reads of one call: a door,
// and a method called on a store-shaped receiver.
func (cf *censusFunc) noteFeedCall(s *ast.SelectorExpr) {
	switch s.Sel.Name {
	case "FeedsChangedPlantWide", "FeedsChangedAt", "FeedsChanged": // the doors, and their body inside messaging
		cf.door = true
	}
	storeShaped := false
	switch x := s.X.(type) {
	case *ast.Ident:
		storeShaped = x.Name == "db"
	case *ast.SelectorExpr:
		storeShaped = x.Sel.Name == "db"
	}
	if storeShaped {
		if cf.dbCalls == nil {
			cf.dbCalls = map[string]bool{}
		}
		cf.dbCalls[s.Sel.Name] = true
	}
}

// pushDoor is a feedWriters entry's push: it reaches a change door, in its own
// body or, with via, in the helper named.
const pushDoor = "door"

// inStore says the function is in the store packages.
func inStore(key string) bool {
	return key == "store" || strings.HasPrefix(key, "store:") || strings.HasPrefix(key, "store/")
}

// feedSites is every function whose SQL writes a table a feed build reads: ""
// for a write a feed reads, followed to a door; otherwise why no feed reads
// what it writes.
var feedSites = map[string]string{
	// Carrier types and their families: the node lists' Family map and each
	// payload's carrier rows.
	"store/bins:CreateType":    "",
	"store/bins:DeleteType":    "",
	"store/bins:SetTypeFamily": "",
	"store/bins:UpdateType":    "",
	// Loaders, their windows and their parts: every node list's Loaders.
	"store/loaders:CreateLoader":  "",
	"store/loaders:DeleteLoader":  "",
	"store/loaders:RemoveHome":    "",
	"store/loaders:RemovePayload": "",
	"store/loaders:SetHomeOrder":  "",
	"store/loaders:UpdateLoader":  "",
	"store/loaders:UpsertHome":    "",
	"store/loaders:UpsertPayload": "",
	// Nodes, groups, a node's stations, kanban levels.
	"store/nodes:AddLane":                  "",
	"store/nodes:AssignStation":            "",
	"store/nodes:ClearParent":              "",
	"store/nodes:Create":                   "",
	"store/nodes:CreateGroup":              "",
	"store/nodes:Delete":                   "",
	"store/nodes:DeleteCarrierNodeIfEmpty": "",
	"store/nodes:DeleteGroup":              "",
	"store/nodes:DeleteLevel":              "",
	"store/nodes:Reparent":                 "",
	"store/nodes:RetireEmptyCarrierNodes":  "",
	"store/nodes:SetParent":                "",
	"store/nodes:SetStations":              "",
	"store/nodes:UnassignStation":          "",
	"store/nodes:Update":                   "",
	"store/nodes:UpsertLevel":              "",
	// Payloads, their carrier types, manifest lines and parts' cat ids: the
	// catalog, and the node lists' carrier rows.
	"store/payloads:Delete":             "",
	"store/payloads:DeleteItem":         "",
	"store/payloads:SetPartCATID":       "",
	"store/payloads:UpdateItem":         "",
	"store/payloads:UpsertPart":         "",
	"store/payloads:createRow":          "",
	"store/payloads:insertManifestLine": "",
	"store/payloads:replaceManifestTx":  "",
	"store/payloads:setBinTypesTx":      "",
	"store/payloads:updateRow":          "",
	// The claims mirror: the nodes other stations' lists mark as claimed.
	"store/plantclaims:ReplaceProcessFromStation": "",
	"store/plantclaims:deleteClaims":              "",
	"store/plantclaims:WipeAll":                   "",
	"store/plantclaims:deleteStyles":              "",
	"store/plantclaims:RemoveStation":             "",
	// An Edge's name: a loader that names the Edge, and a node it claims, carry it.
	"store/registry:Claim":          "",
	"store/registry:Enroll":         "",
	"store/registry:SetDisplayName": "",
	"store/registry:RemoveStale":    "",
	// The scene.
	"store/scene:DeleteByArea":      "",
	"store/scene:DeleteEdgesByArea": "",
	"store/scene:ReplaceArea":       "",
	"store/scene:Upsert":            "",
	"store/scene:UpsertEdge":        "",

	// A slot's claim (nodes.claimed_by): no feed reads it.
	"store/internal/helpers:PlaceBinTx":                 "a slot's claim (nodes.claimed_by), which no feed reads",
	"store/nodes:ClaimSlotTx":                           "a slot's claim (nodes.claimed_by), which no feed reads",
	"store/nodes:UnclaimOrderSlots":                     "a slot's claim (nodes.claimed_by), which no feed reads",
	"store/nodes:UnclaimSlot":                           "a slot's claim (nodes.claimed_by), which no feed reads",
	"store/reconciliation:ReleaseAcquiringOrphanClaims": "a slot's claim (nodes.claimed_by), which no feed reads",
	"store/reconciliation:ReleaseOrphanedClaims":        "a slot's claim (nodes.claimed_by), which no feed reads",
	"store:DB.ConfirmReleaseDrop":                       "a slot's claim (nodes.claimed_by), which no feed reads",
	"store:DB.GiveBackReleaseDrop":                      "a slot's claim (nodes.claimed_by), which no feed reads",
	"store:DB.ReleaseSlotClaim":                         "a slot's claim (nodes.claimed_by), which no feed reads",
	"store:demoteHoldsTx":                               "a slot's claim (nodes.claimed_by), which no feed reads",
	"store:releaseOrderHoldingsTx":                      "a slot's claim (nodes.claimed_by), which no feed reads",
	// Node columns the node list does not carry.
	"store/nodes:ReorderLaneSlots":      "a lane's slot order (nodes.depth), which the node list does not carry",
	"store/nodes:SetDisplayName":        "a node's display name, which the node list does not carry",
	"store/nodes:cascadeEnabledToSlots": "a group's slots' enabled flag, which the node list does not carry",
	// Carrier-type rows the node list leaves out.
	"store/bins:EnsureBareMarkerTx": "a bare marker type, which the node list's families leave out and no payload carries",
	"store/bins:EnsureFamily":       "a family with no type in it yet, which no type's family names",
	"store/bins:SetFamilyKeys":      "a family's lift and wait keys, which the node list does not carry",
	// A loader's config generation and stamp.
	"store/loaders:bumpGen": "a loader's config_gen and updated_at, which the node list does not carry",
	// The Edge registry's own columns: only station_uid and display_name are read, by a loader naming an Edge
	// and a node one claims.
	"store/registry:AddTickRejected": "an Edge's rejected-tick count, which no feed reads",
	"store/registry:Introduce":       "an Edge's row with no display name: a loader naming it carries no name and a node it claims the uid, as for no row",
	"store/registry:MarkStale":       "an Edge's status, which no feed reads",
	"store/registry:Rebind":          "an Edge's bound host, which no feed reads",
	"store/registry:Register":        "an Edge's host, version, zone and status, which no feed reads",
	"store/registry:UpdateHeartbeat": "an Edge's heartbeat stamps, which no feed reads",
	// Not Core serving.
	"internal/testdb:ClaimSlotForTest": "a test fixture: internal/testdb is linked only into tests",
	"internal/testdb:CreateBinAtNode":  "a test fixture: internal/testdb is linked only into tests",
	"store:v185Down":                   "a migration's down: Core is not serving while it runs",
	"store:v185FoldRegistry":           "a migration: it runs before Core serves",
}

// feedWriters is every function outside the store packages that writes what a
// feed reads through the store, and how it reaches a door.
var feedWriters = map[string]censusNode{
	// A claims report: the other stations' lists mark the nodes it claims.
	"messaging:CoreDataService.HandlePlantClaims": {push: pushDoor, via: "messaging:CoreDataService.claimedNodesMoved"},
	// Payloads: every write ends in payloadsWritten.
	"service:PayloadService.Create":             {push: pushDoor, via: "service:PayloadService.payloadsWritten"},
	"service:PayloadService.CreateFromFloor":    {push: pushDoor, via: "service:PayloadService.payloadsWritten"},
	"service:PayloadService.CreateManifestItem": {push: pushDoor, via: "service:PayloadService.payloadsWritten"},
	"service:PayloadService.CreateWithSets":     {push: pushDoor, via: "service:PayloadService.payloadsWritten"},
	"service:PayloadService.Delete":             {push: pushDoor, via: "service:PayloadService.payloadsWritten"},
	"service:PayloadService.DeleteManifestItem": {push: pushDoor, via: "service:PayloadService.payloadsWritten"},
	"service:PayloadService.ReplaceManifest":    {push: pushDoor, via: "service:PayloadService.payloadsWritten"},
	"service:PayloadService.SetBinTypes":        {push: pushDoor, via: "service:PayloadService.payloadsWritten"},
	"service:PayloadService.SetPartCATID":       {push: pushDoor, via: "service:PayloadService.payloadsWritten"},
	"service:PayloadService.Update":             {push: pushDoor, via: "service:PayloadService.payloadsWritten"},
	"service:PayloadService.UpdateManifestItem": {push: pushDoor, via: "service:PayloadService.payloadsWritten"},
	"service:PayloadService.UpdateWithSetsSeen": {push: pushDoor, via: "service:PayloadService.payloadsWritten"},

	// Nodes, groups, lanes and a node's stations.
	"service:NodeService.AddLane":          {push: pushDoor, via: "service:NodeService.nodesWritten"},
	"service:NodeService.CreateNode":       {push: pushDoor, via: "service:NodeService.nodesWritten"},
	"service:NodeService.CreateNodeGroup":  {push: pushDoor, via: "service:NodeService.nodesWritten"},
	"service:NodeService.DeleteNode":       {push: pushDoor, via: "service:NodeService.nodesWritten"},
	"service:NodeService.DeleteNodeGroup":  {push: pushDoor, via: "service:NodeService.nodesWritten"},
	"service:NodeService.ReparentNode":     {push: pushDoor, via: "service:NodeService.nodesWritten"},
	"service:NodeService.UpdateNode":       {push: pushDoor, via: "service:NodeService.nodesWritten"},
	"service:NodeService.ApplyAssignments": {push: pushDoor, via: "service:NodeService.stationsWritten"},
	"service:NodeService.SetNodeStations":  {push: pushDoor, via: "service:NodeService.stationsWritten"},
	// Kanban levels: the save goes through the door once its writes are done.
	"service:NodeService.SaveKanban": {push: pushDoor},
	// Pools only, inside the save, before its door.
	"service:NodeService.familyWarnings": {callers: []string{"service:NodeService.SaveKanban"}},
	// An Edge's name, which a loader naming the Edge carries.
	"service:NodeService.ClaimEdge":  {push: pushDoor},
	"service:NodeService.EnrollEdge": {push: pushDoor},
	"service:NodeService.RenameEdge": {push: pushDoor},
	// A removed station: the nodes it claimed are no longer marked.
	"service:NodeService.RemoveEdge": {push: pushDoor},

	// Loaders: every write ends in rederive, which goes through the door.
	"service:LoaderService.CreateLoader":   {push: pushDoor, via: "service:LoaderService.rederive"},
	"service:LoaderService.CreateTwoStage": {push: pushDoor, via: "service:LoaderService.rederive"},
	"service:LoaderService.Delete":         {push: pushDoor, via: "service:LoaderService.rederive"},
	"service:LoaderService.RemoveHome":     {push: pushDoor, via: "service:LoaderService.rederive"},
	"service:LoaderService.RemovePayload":  {push: pushDoor, via: "service:LoaderService.rederive"},
	"service:LoaderService.ReorderHomes":   {push: pushDoor, via: "service:LoaderService.rederive"},
	"service:LoaderService.SetHome":        {push: pushDoor, via: "service:LoaderService.rederive"},
	"service:LoaderService.SetPayload":     {push: pushDoor, via: "service:LoaderService.rederive"},
	"service:LoaderService.Update":         {push: pushDoor, via: "service:LoaderService.rederive"},
	"service:LoaderService.SyncPairs":      {push: pushDoor},
	// A two-stage pair's groups and its stage 1's outbound.
	"service:LoaderService.syncPair":   {callers: []string{"service:LoaderService.RemoveHome", "service:LoaderService.syncPairOf"}},
	"service:LoaderService.syncPairOf": {callers: []string{"service:LoaderService.SetHome", "service:LoaderService.SyncPairs", "service:LoaderService.Update"}},
	"service:LoaderService.deletePair": {callers: []string{"service:LoaderService.Delete"}},
	"service:LoaderService.dropGroup": {callers: []string{"service:LoaderService.deletePair", "service:LoaderService.dropPairGroup",
		"service:LoaderService.syncStage1Group"}},
	"service:LoaderService.dropPairGroup": {callers: []string{"service:LoaderService.deletePair", "service:LoaderService.syncPair",
		"service:LoaderService.syncPulledDirectlyGroup"}},
	"service:LoaderService.groupWindows":            {callers: []string{"service:LoaderService.groupStage2Windows", "service:LoaderService.syncStage1Group"}},
	"service:LoaderService.groupStage2Windows":      {callers: []string{"service:LoaderService.syncPair", "service:LoaderService.syncPulledDirectlyGroup"}},
	"service:LoaderService.syncStage1Group":         {callers: []string{"service:LoaderService.syncPair"}},
	"service:LoaderService.syncPulledDirectlyGroup": {callers: []string{"service:LoaderService.syncPair"}},

	// Bin types and their families.
	"service:BinService.CreateBinType":          {push: pushDoor, via: "service:BinService.typesWritten"},
	"service:BinService.DeleteBinType":          {push: pushDoor, via: "service:BinService.typesWritten"},
	"service:BinService.SetBinTypeFamilyByCode": {push: pushDoor, via: "service:BinService.typesWritten"},
	"service:BinService.UpdateBinType":          {push: pushDoor, via: "service:BinService.typesWritten"},

	// The scene sync, through the engine that runs it.
	"scenesync:SyncScenePoints":     {callers: []string{"engine:Engine.SyncScenePoints", "scenesync:Sync"}},
	"scenesync:SyncFleetNodes":      {callers: []string{"engine:Engine.SyncFleetNodes", "scenesync:Sync"}},
	"scenesync:Sync":                {callers: []string{"engine:Engine.SceneSync"}},
	"engine:Engine.SceneSync":       {push: pushDoor},
	"engine:Engine.SyncScenePoints": {push: pushDoor},
	"engine:Engine.SyncFleetNodes":  {push: pushDoor},
	"scenesync:UpdateNodeZones": {entry: "writes a node's zone and nothing else (UpdateNode with the row as read), " +
		"and the node list does not carry the zone"},

	// The robot carrier nodes: roots with no station, on the plant-wide list.
	"engine:Engine.carrierNode":              {push: pushDoor},
	"engine:Engine.placeCarriedBinIfSettled": {push: pushDoor},
	"engine:Engine.retireEmptyCarrierNodes":  {push: pushDoor},

	// Not the Core that serves the Edges.
	"cmd/seeddev:ensureBinType":          {entry: seedDev},
	"cmd/seeddev:ensureNode":             {entry: seedDev},
	"cmd/seeddev:ensurePayload":          {entry: seedDev},
	"cmd/seeddev:ensurePayloadTemplate":  {entry: seedDev},
	"cmd/seeddev:seedBinLoaders":         {entry: seedDev},
	"cmd/seeddev:seedCore":               {entry: seedDev},
	"cmd/seeddev:seedKanbans":            {entry: seedDev},
	"internal/testdb:KanbanKeeper":       {entry: testFixture},
	"internal/testdb:MarkedGroup":        {entry: testFixture},
	"internal/testdb:SeedSceneBinTasks":  {entry: testFixture},
	"internal/testdb:SeedScenePoint":     {entry: testFixture},
	"internal/testdb:SetupCompound":      {entry: testFixture},
	"internal/testdb:getOrCreateBinType": {entry: testFixture},
	"internal/testdb:getOrCreateNode":    {entry: testFixture},
	"internal/testdb:getOrCreatePayload": {entry: testFixture},
	"internal/testdb:saveKanban":         {entry: testFixture},
}

const (
	seedDev     = "cmd/seeddev, a tool that seeds a database from its own process: a Core serving it heals by the memo's lifetime"
	testFixture = "a test fixture: internal/testdb is linked only into tests"
)

func TestFeedWrites_EveryWriterReachesADoorOrIsNamed(t *testing.T) {
	t.Parallel()
	g := parseCoreCallGraph(t, "..")

	// The sites.
	sites := map[string]bool{}
	for key, f := range g.funcs {
		if f.feedSQL {
			sites[key] = true
		}
	}
	var unnamed []string
	for _, key := range slices.Sorted(maps.Keys(sites)) {
		if _, ok := feedSites[key]; !ok {
			unnamed = append(unnamed, key)
		}
	}
	if len(unnamed) > 0 {
		t.Errorf("functions writing a table a feed reads, not in feedSites: name each as a feed write (\"\") or "+
			"say why no feed reads what it writes:\n%s", quotedLines(unnamed))
	}
	for _, key := range slices.Sorted(maps.Keys(feedSites)) {
		if !sites[key] {
			t.Errorf("%s is in feedSites but writes no table a feed reads (or is gone)", key)
		}
	}

	// The store's API for a feed write: the store functions on a path from a
	// feed site.
	api := map[string]bool{}
	for key := range sites {
		if feedSites[key] == "" && inStore(key) {
			api[key] = true
		}
	}
	for grew := true; grew; {
		grew = false
		for key := range api {
			for caller := range g.callers[key] {
				if inStore(caller) && !api[caller] {
					api[caller] = true
					grew = true
				}
			}
		}
	}
	dbMethods := map[string]bool{}
	for key := range api {
		if name, ok := strings.CutPrefix(key, "store:DB."); ok {
			dbMethods[name] = true
		}
	}
	if len(dbMethods) < 10 {
		t.Fatalf("the store's API for a feed write is %d methods; a census that finds none checks nothing", len(dbMethods))
	}

	// The writers, and the walk from each to its door.
	writers := map[string]bool{}
	for key, f := range g.funcs {
		if inStore(key) {
			continue
		}
		for m := range f.dbCalls {
			if dbMethods[m] {
				writers[key] = true
			}
		}
		for _, target := range g.pkgTargets(f) {
			if api[target] {
				writers[key] = true
			}
		}
		// A site outside the store packages is a writer of its own.
		if sites[key] && feedSites[key] == "" {
			writers[key] = true
		}
	}
	reached := map[string]bool{}
	var missing []string
	var walk func(key string)
	walk = func(key string) {
		if reached[key] {
			return
		}
		reached[key] = true
		n, ok := feedWriters[key]
		if !ok {
			missing = append(missing, key)
			return
		}
		g.checkDoor(t, key, n)
		for _, c := range n.callers {
			walk(c)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(writers)) {
		walk(key)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("writers of what a feed reads, or their callers, not in feedWriters: wire each to a change door "+
			"(service.FeedDoors) or name why it reaches none:\n%s", quotedLines(missing))
	}
	for _, key := range slices.Sorted(maps.Keys(feedWriters)) {
		if !reached[key] {
			t.Errorf("%s is in feedWriters but on no path from a write a feed reads", key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(feedWriters)) {
		if n := feedWriters[key]; n.entry != "" {
			t.Logf("no change door: %s — %s", key, n.entry)
		}
	}
}

// pkgTargets is the module functions f's body calls by package name.
func (g *coreCallGraph) pkgTargets(f *censusFunc) []string {
	var out []string
	ast.Inspect(f.body, func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := s.X.(*ast.Ident); ok {
				if dir, ok := f.imports[id.Name]; ok {
					if _, ok := g.funcs[dir+":"+s.Sel.Name]; ok {
						out = append(out, dir+":"+s.Sel.Name)
					}
				}
			}
		}
		return true
	})
	return out
}

// checkDoor holds one writer to what the census says of it.
func (g *coreCallGraph) checkDoor(t *testing.T, key string, n censusNode) {
	t.Helper()
	f, ok := g.funcs[key]
	if !ok {
		t.Errorf("%s: no such function in the module", key)
		return
	}
	got := slices.Sorted(maps.Keys(g.callers[key]))
	switch {
	case n.push != "":
		holder := f
		if n.via != "" {
			holder = g.funcs[n.via]
			if holder == nil || !slices.Contains(g.targets(f), n.via) {
				t.Errorf("%s: no longer calls %s, the helper named as its door", key, n.via)
				return
			}
		}
		if !holder.door {
			t.Errorf("%s: %s calls no change door, the census says it does — a writer that stopped telling the Edges",
				key, holder.key)
		}
	case n.entry != "":
		if f.door {
			t.Errorf("%s now calls a change door: move it from the entries to the doors", key)
		}
	case n.unreach != "":
		if len(got) != 0 {
			t.Errorf("%s has production callers %v, census says none (%s)", key, got, n.unreach)
		}
	default:
		want := slices.Sorted(slices.Values(n.callers))
		if !slices.Equal(got, want) {
			t.Errorf("%s: production callers %v, census names %v. Follow the new one to its door.", key, got, want)
		}
	}
}

// quotedLines is keys as Go map-literal lines, one per key, to paste.
func quotedLines(keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "\t%q: ,\n", k)
	}
	return b.String()
}
