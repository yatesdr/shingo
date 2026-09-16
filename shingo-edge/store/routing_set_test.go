package store

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol/testutil"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// routing_set_test.go — the process ROUTING SET (process_routing_nodes) and the
// per-process flow-composer gate it is reviewed behind.
//
// A new test file, for a new aggregate: nothing else in this package is about
// which nodes a process may route material through. The fixture-driven pins
// against the plant A and B fixtures live in routing_set_backfill_test.go,
// because they exercise the same store surface against real rows.

// TestProcess_FlowComposerEnabled_ReadsBack pins the gate column both ways.
//
// A new column is not done until something reads it back and asserts on the
// value: the write path and the read path are different code, and only the one
// just written gets tested unless this exists. Off by default — the composer is
// gated until the engineer has reviewed the derived routing set.
func TestProcess_FlowComposerEnabled_ReadsBack(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, err := db.CreateProcess("P400", "", "", "", "", false)
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}

	p, err := db.GetProcess(pid)
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	if p.FlowComposerEnabled {
		t.Fatal("a fresh process reads flow_composer_enabled=true; the gate must start OFF")
	}

	if err := db.SetFlowComposerEnabled(pid, true); err != nil {
		t.Fatalf("SetFlowComposerEnabled(true): %v", err)
	}
	p, err = db.GetProcess(pid)
	if err != nil {
		t.Fatalf("GetProcess after enable: %v", err)
	}
	if !p.FlowComposerEnabled {
		t.Fatal("SetFlowComposerEnabled(true) did not read back — the column was written and never scanned")
	}

	// The LIST path scans through the same select; a column added to Get and
	// not to List is the closed_by defect one table over.
	list, err := db.ListProcesses()
	if err != nil {
		t.Fatalf("ListProcesses: %v", err)
	}
	var seen bool
	for _, lp := range list {
		if lp.ID == pid {
			seen = true
			if !lp.FlowComposerEnabled {
				t.Fatal("ListProcesses reads flow_composer_enabled=false after it was set true")
			}
		}
	}
	if !seen {
		t.Fatalf("process %d missing from ListProcesses", pid)
	}

	if err := db.SetFlowComposerEnabled(pid, false); err != nil {
		t.Fatalf("SetFlowComposerEnabled(false): %v", err)
	}
	p, err = db.GetProcess(pid)
	testutil.MustNoErr(t, err, "db.GetProcess")
	if p.FlowComposerEnabled {
		t.Fatal("SetFlowComposerEnabled(false) did not clear the gate")
	}
}

// seedRoutingProcess is a process with one live style and one position.
func seedRoutingProcess(t *testing.T, db *DB, name string) (processID, styleID int64) {
	t.Helper()
	pid, err := db.CreateProcess(name, "", "", "", "", false)
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	sid, err := db.CreateStyle("S-"+name, "", pid)
	if err != nil {
		t.Fatalf("CreateStyle: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, CoreNodeName: "PLN_01", Name: "PLN_01", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProcessNode: %v", err)
	}
	return pid, sid
}

// routingClaimSeed is one live style with one claim, named by the routing
// fields the derivation reads.
type routingClaimSeed struct {
	style, source, dest, staging string
}

func seedRoutingClaims(t *testing.T, db *DB, processID int64, node string, seeds []routingClaimSeed) {
	t.Helper()
	for _, sd := range seeds {
		sid, err := db.CreateStyle(sd.style, "", processID)
		if err != nil {
			t.Fatalf("CreateStyle(%s): %v", sd.style, err)
		}
		in := processes.NodeClaimInput{
			StyleID: sid, CoreNodeName: node, Role: "consume", SwapMode: "two_robot",
			PayloadCode: "RAW-" + sd.style, InboundSource: sd.source, OutboundDestination: sd.dest,
			InboundStaging: sd.staging,
		}
		if _, err := db.UpsertStyleNodeClaim(in); err != nil {
			t.Fatalf("seed claim for %s: %v", sd.style, err)
		}
	}
}

func findRoutingRow(t *testing.T, db *DB, processID, id int64) domain.RoutingNode {
	t.Helper()
	rows, err := db.ListRoutingNodes(processID)
	if err != nil {
		t.Fatalf("ListRoutingNodes: %v", err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("row %d not in process %d: %v", id, processID, routingKeys(rows))
	return domain.RoutingNode{}
}

func routingKeys(rows []domain.RoutingNode) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.CoreNodeName+":"+r.Role)
	}
	return out
}

// TestRoutingNodes_UpsertListAndEnable pins the plain CRUD: the list groups by
// role in source / staging / destination order, then by sequence; an upsert on
// the same (node, role) updates in place rather than minting a second row; the
// enabled flag reads back after a toggle.
func TestRoutingNodes_UpsertListAndEnable(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, _ := seedRoutingProcess(t, db, "P400")

	for _, in := range []domain.RoutingNodeInput{
		{ProcessID: pid, CoreNodeName: "Supermarket Area", Role: domain.RoutingRoleDestination, Enabled: true, CalledBy: "eng"},
		{ProcessID: pid, CoreNodeName: "STG_02", Role: domain.RoutingRoleStaging, Sequence: 2, Enabled: true, CalledBy: "eng"},
		{ProcessID: pid, CoreNodeName: "STG_01", Role: domain.RoutingRoleStaging, Sequence: 1, Enabled: true, CalledBy: "eng"},
		{ProcessID: pid, CoreNodeName: "Supermarket Empty Totes", Role: domain.RoutingRoleSource, Enabled: true, CalledBy: "eng"},
	} {
		if _, err := db.UpsertRoutingNode(in); err != nil {
			t.Fatalf("UpsertRoutingNode(%s %s): %v", in.CoreNodeName, in.Role, err)
		}
	}

	rows, err := db.ListRoutingNodes(pid)
	if err != nil {
		t.Fatalf("ListRoutingNodes: %v", err)
	}
	want := []string{"Supermarket Empty Totes:source", "STG_01:staging", "STG_02:staging", "Supermarket Area:destination"}
	if got := routingKeys(rows); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("list order = %v, want %v (role order source/staging/destination, then sequence)", got, want)
	}
	for _, r := range rows {
		if r.Origin != domain.RoutingOriginEngineer {
			t.Errorf("%s: origin = %q, want engineer (the default when the writer says nothing)", r.CoreNodeName, r.Origin)
		}
		if r.CalledBy != "eng" {
			t.Errorf("%s: called_by = %q, want eng — attribution was written and not read back", r.CoreNodeName, r.CalledBy)
		}
		if !r.Enabled {
			t.Errorf("%s: enabled = false, want true", r.CoreNodeName)
		}
	}

	// Same (node, role) again: an UPDATE, not a duplicate. A different role for
	// the same node is a second, legitimate row.
	id2, err := db.UpsertRoutingNode(domain.RoutingNodeInput{
		ProcessID: pid, CoreNodeName: "Supermarket Area", Role: domain.RoutingRoleDestination, Label: "Finished goods", Enabled: true, CalledBy: "eng2",
	})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if _, err := db.UpsertRoutingNode(domain.RoutingNodeInput{
		ProcessID: pid, CoreNodeName: "Supermarket Area", Role: domain.RoutingRoleSource, Enabled: true, CalledBy: "eng",
	}); err != nil {
		t.Fatalf("upsert second role: %v", err)
	}
	rows, err = db.ListRoutingNodes(pid)
	testutil.MustNoErr(t, err, "db.ListRoutingNodes")
	if len(rows) != 5 {
		t.Fatalf("after re-upsert + second role: %d rows, want 5: %v", len(rows), routingKeys(rows))
	}
	var dest *domain.RoutingNode
	for i := range rows {
		if rows[i].ID == id2 {
			dest = &rows[i]
		}
	}
	if dest == nil || dest.Label != "Finished goods" || dest.CalledBy != "eng2" {
		t.Fatalf("re-upsert did not update in place: %+v", dest)
	}

	// Enable toggle reads back.
	if err := db.SetRoutingNodeEnabled(pid, id2, false, "eng3"); err != nil {
		t.Fatalf("SetRoutingNodeEnabled: %v", err)
	}
	rows, err = db.ListRoutingNodes(pid)
	testutil.MustNoErr(t, err, "db.ListRoutingNodes")
	for _, r := range rows {
		if r.ID == id2 && (r.Enabled || r.CalledBy != "eng3") {
			t.Fatalf("SetRoutingNodeEnabled(false, eng3) did not read back: %+v", r)
		}
	}
	// A row id from another process is not reachable through this process.
	other, _ := seedRoutingProcess(t, db, "OTHER")
	if err := db.SetRoutingNodeEnabled(other, id2, true, "x"); !errors.Is(err, processes.ErrRoutingNodeNotFound) {
		t.Fatalf("toggling another process's row: err = %v, want ErrRoutingNodeNotFound", err)
	}
}

// TestRoutingNodes_EnablingABackfillIsTheApproval: the switch IS the approval
// (owner ruling 2026-09-10, Q5).
//
// A backfilled row is a name the derivation read out of a live claim and
// nobody has agreed to. It arrives off. Switching it on is an engineer saying
// "yes, operators may pick this", and that is a change of AUTHORSHIP, not just
// of a flag: the row stops being something the machine found and becomes
// something a person owns. So origin flips to engineer and called_by records
// who. It used to keep saying backfill, which meant the panel could not tell
// an approved name from one still waiting except by reading the flag it had
// just set — and the summary line counted it forever.
//
// Switching one OFF does not flip the origin back. Where a name came from is
// history, and a row an engineer approved and then withdrew is still a row an
// engineer touched. Nothing restores a backfill origin afterwards, and that
// is load-bearing: the derive runs at every boot, so rewriting origin there
// would un-approve every adopted name on every Edge restart.
func TestRoutingNodes_EnablingABackfillIsTheApproval(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, _ := seedRoutingProcess(t, db, "P400")

	id, err := db.UpsertRoutingNode(domain.RoutingNodeInput{
		ProcessID: pid, CoreNodeName: "Supermarket Empty Totes", Role: domain.RoutingRoleSource,
		Origin: domain.RoutingOriginBackfill, Enabled: false,
	})
	if err != nil {
		t.Fatalf("seed a backfill row: %v", err)
	}

	if err := db.SetRoutingNodeEnabled(pid, id, true, "eng"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	row := findRoutingRow(t, db, pid, id)
	if !row.Enabled {
		t.Error("enabled did not read back")
	}
	if row.Origin != domain.RoutingOriginEngineer {
		t.Errorf("origin = %q, want engineer: enabling a backfill row is the engineer adopting the name", row.Origin)
	}
	if row.CalledBy != "eng" {
		t.Errorf("called_by = %q, want eng", row.CalledBy)
	}

	// Off again: the flag moves, the authorship does not.
	if err := db.SetRoutingNodeEnabled(pid, id, false, "eng2"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	row = findRoutingRow(t, db, pid, id)
	if row.Enabled {
		t.Error("disable did not read back")
	}
	if row.Origin != domain.RoutingOriginEngineer {
		t.Errorf("origin = %q after switching off; where a name came from is history, and an engineer touched this one", row.Origin)
	}
	if row.CalledBy != "eng2" {
		t.Errorf("called_by = %q, want eng2", row.CalledBy)
	}
}

// TestRoutingNodes_ListCountsTheStylesBehindEachName: the row read carries the
// evidence the panel shows — how many of the process's live styles name this
// node in THIS role.
//
// It is the same predicate the delete guard matches on (routingFieldMatch), for
// the reason the guard uses it: a node that is a live source may be nobody's
// destination, and one count over all fields would put "on 10 styles" beside a
// destination row that no style sends anything to. The derive already reads
// these claims; this is that read exposed rather than a new one.
func TestRoutingNodes_ListCountsTheStylesBehindEachName(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, _ := seedRoutingProcess(t, db, "P400")
	seedRoutingClaims(t, db, pid, "PLN_01", []routingClaimSeed{
		{style: "A", source: "Supermarket Empty Totes", dest: "Supermarket Area", staging: "STG_01"},
		{style: "B", source: "Supermarket Empty Totes", dest: "Supermarket Area", staging: "STG_01"},
		{style: "C", source: "Line Buffer", dest: "Supermarket Area", staging: "STG_02"},
	})
	if _, err := db.DeriveRoutingNodesForProcess(pid, nil); err != nil {
		t.Fatalf("derive: %v", err)
	}
	rows, err := db.ListRoutingNodesWithCounts(pid)
	if err != nil {
		t.Fatalf("ListRoutingNodesWithCounts: %v", err)
	}
	want := map[string]int{
		"Supermarket Empty Totes:source": 2,
		"Line Buffer:source":             1,
		"Supermarket Area:destination":   3,
		"STG_01:staging":                 2,
		"STG_02:staging":                 1,
	}
	got := map[string]int{}
	for _, r := range rows {
		got[r.CoreNodeName+":"+r.Role] = r.StyleCount
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s: style_count = %d, want %d (rows: %v)", k, got[k], n, got)
		}
	}

	// AND THE PLAIN READ COUNTS NOTHING. The count is a correlated
	// COUNT(DISTINCT) with TRIM() on both sides, which no index can serve, and
	// it used to run on every station poll, every preview and twice per save
	// for a number only the Routing panel reads. This half of the test is what
	// stops it drifting back onto the hot read.
	plain, err := db.ListRoutingNodes(pid)
	if err != nil {
		t.Fatalf("ListRoutingNodes: %v", err)
	}
	if len(plain) != len(rows) {
		t.Fatalf("plain read returned %d rows, counting read %d", len(plain), len(rows))
	}
	for _, r := range plain {
		if r.StyleCount != 0 {
			t.Errorf("%s:%s carries style_count %d on the PLAIN read — the count belongs to the panel's read only",
				r.CoreNodeName, r.Role, r.StyleCount)
		}
	}
}

// TestRoutingNodes_UpsertRefusals: an unknown role and a name that is already
// one of the process's own positions are both refused. Positions stay in
// process_nodes — a routing row for one would put the same node in both halves
// of the union.
func TestRoutingNodes_UpsertRefusals(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, _ := seedRoutingProcess(t, db, "P400")

	if _, err := db.UpsertRoutingNode(domain.RoutingNodeInput{ProcessID: pid, CoreNodeName: "X", Role: "waypoint"}); !errors.Is(err, processes.ErrInvalidRoutingRole) {
		t.Fatalf("role=waypoint: err = %v, want ErrInvalidRoutingRole (waypoint is not a role)", err)
	}
	if _, err := db.UpsertRoutingNode(domain.RoutingNodeInput{ProcessID: pid, CoreNodeName: "  ", Role: domain.RoutingRoleSource}); err == nil {
		t.Fatal("blank name was accepted")
	}
	_, err := db.UpsertRoutingNode(domain.RoutingNodeInput{ProcessID: pid, CoreNodeName: "PLN_01", Role: domain.RoutingRoleStaging})
	if !errors.Is(err, processes.ErrRoutingNodeIsPosition) {
		t.Fatalf("position as routing node: err = %v, want ErrRoutingNodeIsPosition", err)
	}
	if err != nil && !strings.Contains(err.Error(), "PLN_01") {
		t.Errorf("refusal does not name the node: %v", err)
	}
}

// TestRoutingNodes_DeleteRefusesWhileALiveClaimReferencesTheName is the
// delete guard. A routing row cannot go while a live style's claim still names
// it in the matching field — the composer would then be offering a set the
// plant's own configuration contradicts — and the refusal names the style so
// the engineer knows where to look. Retire the style and the delete proceeds.
func TestRoutingNodes_DeleteRefusesWhileALiveClaimReferencesTheName(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, sid := seedRoutingProcess(t, db, "P400")
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "PLN_01", Role: "consume", SwapMode: "two_robot",
		PayloadCode: "RAW", InboundStaging: "STG_01", InboundSource: "SMN_BUF_100",
		OutboundDestination: "Supermarket Area",
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	ids := map[string]int64{}
	for _, in := range []domain.RoutingNodeInput{
		{ProcessID: pid, CoreNodeName: "SMN_BUF_100", Role: domain.RoutingRoleSource, Enabled: true},
		{ProcessID: pid, CoreNodeName: "SMN_BUF_100", Role: domain.RoutingRoleDestination, Enabled: true},
		{ProcessID: pid, CoreNodeName: "STG_01", Role: domain.RoutingRoleStaging, Enabled: true},
		{ProcessID: pid, CoreNodeName: "Supermarket Area", Role: domain.RoutingRoleDestination, Enabled: true},
	} {
		id, err := db.UpsertRoutingNode(in)
		if err != nil {
			t.Fatalf("upsert %s: %v", in.CoreNodeName, err)
		}
		ids[in.CoreNodeName+":"+in.Role] = id
	}

	for _, key := range []string{"SMN_BUF_100:source", "STG_01:staging", "Supermarket Area:destination"} {
		err := db.DeleteRoutingNode(pid, ids[key])
		if !errors.Is(err, processes.ErrRoutingNodeInUse) {
			t.Fatalf("delete %s while a live claim names it: err = %v, want ErrRoutingNodeInUse", key, err)
		}
		if !strings.Contains(err.Error(), "S-P400") {
			t.Errorf("delete %s: refusal does not name the style: %v", key, err)
		}
	}
	// The same name in a role no claim uses it for is free to go: the guard
	// matches the FIELD, not the name.
	if err := db.DeleteRoutingNode(pid, ids["SMN_BUF_100:destination"]); err != nil {
		t.Fatalf("delete SMN_BUF_100:destination (no claim uses it as a destination): %v", err)
	}

	// Retired style ⇒ its claims no longer hold the row.
	if err := db.DeleteStyle(sid); err != nil {
		t.Fatalf("DeleteStyle: %v", err)
	}
	if err := db.DeleteRoutingNode(pid, ids["SMN_BUF_100:source"]); err != nil {
		t.Fatalf("delete after the style retired: %v", err)
	}
	rows, err := db.ListRoutingNodes(pid)
	testutil.MustNoErr(t, err, "db.ListRoutingNodes")
	if len(rows) != 2 {
		t.Fatalf("after deletes: %v, want STG_01:staging and Supermarket Area:destination", routingKeys(rows))
	}
}

// TestRoutingNodes_DeriveFromClaims pins the backfill on a synthetic process:
// five fields → three roles, backfill origin, DISABLED; a name that is a
// position is derived and then dropped; running it again is a no-op; and once
// the flow composer is enabled the derivation does not run at all — a reviewed
// set is never re-seeded from claims.
func TestRoutingNodes_DeriveFromClaims(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, sid := seedRoutingProcess(t, db, "P400")
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "PLN_01", Role: "consume", SwapMode: "two_robot_press_index",
		PayloadCode: "RAW", PairedCoreNode: "PLN_04",
		InboundStaging: "PLN_02", OutboundStaging: "PLN_05",
		InboundSource: "Supermarket Empty Totes", OutboundDestination: "Supermarket Area",
		ChangeoverEvacDestination: domain.Ptr("Supermarket Area"),
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	// PLN_02 is a POSITION of this process; PLN_05 is not.
	if _, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: "PLN_02", Name: "PLN_02", Enabled: true}); err != nil {
		t.Fatalf("CreateProcessNode PLN_02: %v", err)
	}
	// A retired style's claim contributes nothing.
	dead, err := db.CreateStyle("DEAD", "", pid)
	testutil.MustNoErr(t, err, "db.CreateStyle")
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: dead, CoreNodeName: "PLN_01", Role: "consume", SwapMode: "two_robot",
		PayloadCode: "OLD", InboundStaging: "OLD_STG", InboundSource: "OLD_SRC", OutboundDestination: "OLD_DST",
	}); err != nil {
		t.Fatalf("seed dead claim: %v", err)
	}
	if err := db.DeleteStyle(dead); err != nil {
		t.Fatalf("retire dead style: %v", err)
	}

	reports, err := db.DeriveRoutingNodes(nil)
	if err != nil {
		t.Fatalf("DeriveRoutingNodes: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("reports = %+v, want one per process", reports)
	}
	rep := reports[0]
	rows, err := db.ListRoutingNodes(pid)
	testutil.MustNoErr(t, err, "db.ListRoutingNodes")
	want := []string{"Supermarket Empty Totes:source", "PLN_05:staging", "Supermarket Area:destination"}
	if got := routingKeys(rows); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("derived set = %v, want %v (PLN_02 is a position and must drop; OLD_* belong to a retired style)", got, want)
	}
	// BACKFILLED AND ENABLED (owner ruling 2026-09-16, reversing Q5).
	//
	// These names come off LIVE claims — the press is already routing material
	// through every one of them. Landing them switched off never stopped that;
	// it only stopped the flow composer OFFERING them, so a press drawing from
	// a node could not offer that node to the operator working it. The switch
	// is about what operators are shown, not about whether the engineer has
	// finished: "the engineer should be able to set up as is; it is kind of
	// stupid he has to switch it on, that's for the HMI."
	//
	// The ORIGIN still says backfill, because who put the name there is a fact
	// and is what the picker annotates with.
	for _, r := range rows {
		if r.Origin != domain.RoutingOriginBackfill || !r.Enabled {
			t.Errorf("%s: origin=%q enabled=%v, want backfill and IN the set: a name read off a "+
				"live claim is one the press already routes through", r.CoreNodeName, r.Origin, r.Enabled)
		}
	}
	if rep.FlowComposerEnabled || rep.Nodes != 3 || rep.Claims != 1 || rep.NeedDecision != 0 {
		t.Fatalf("report = %+v, want nodes=3 claims=1 need_decision=0 not skipped", rep)
	}
	if rep.Line() != "routing set: P400 — derived 3 nodes from 1 claims; 0 need a decision" {
		t.Fatalf("log line = %q", rep.Line())
	}

	// Idempotent: a second run changes nothing, and an adopted row stays adopted.
	if err := db.SetRoutingNodeEnabled(pid, rows[0].ID, true, "eng"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if _, err := db.DeriveRoutingNodes(nil); err != nil {
		t.Fatalf("second derive: %v", err)
	}
	again, err := db.ListRoutingNodes(pid)
	testutil.MustNoErr(t, err, "db.ListRoutingNodes")
	if got := routingKeys(again); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("second derive changed the set: %v", got)
	}
	if !again[0].Enabled {
		t.Fatal("re-derive reset an adopted row to disabled — INSERT OR IGNORE must leave existing rows alone")
	}

	// Unknown-name count: only when the caller could check.
	rep, err = db.DeriveRoutingNodesForProcess(pid, func(name string) bool { return name == "PLN_05" })
	if err != nil {
		t.Fatalf("derive with checker: %v", err)
	}
	if rep.NeedDecision != 1 || len(rep.Unknown) != 1 || rep.Unknown[0] != "PLN_05" {
		t.Fatalf("report with checker = %+v, want PLN_05 as the one name needing a decision", rep)
	}

	// Gate on ⇒ nothing derived, even for a brand-new claim field.
	if err := db.SetFlowComposerEnabled(pid, true); err != nil {
		t.Fatalf("enable gate: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "PLN_01", Role: "consume", SwapMode: "two_robot",
		PayloadCode: "RAW", InboundStaging: "NEW_STG", InboundSource: "NEW_SRC", OutboundDestination: "NEW_DST",
	}); err != nil {
		t.Fatalf("edit claim: %v", err)
	}
	rep, err = db.DeriveRoutingNodesForProcess(pid, nil)
	if err != nil {
		t.Fatalf("derive with gate on: %v", err)
	}
	if !rep.FlowComposerEnabled {
		t.Fatalf("derive ran with the gate on: %+v", rep)
	}
	after, err := db.ListRoutingNodes(pid)
	testutil.MustNoErr(t, err, "db.ListRoutingNodes")
	if got := routingKeys(after); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("gate on, yet the set changed: %v", got)
	}
	if !strings.Contains(rep.Line(), "not re-derived") {
		t.Fatalf("skipped line does not say so: %q", rep.Line())
	}
}
