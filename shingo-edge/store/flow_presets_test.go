package store

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol/testutil"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// flow_presets_test.go — named flows an engineer saves for a process, with
// the parts left blank.
//
// Store and validation only (no UI, no endpoints yet). Two refusals matter
// enough to pin: a preset carrying a payload — a preset is a SHAPE, the part
// is chosen when it is applied — and a preset naming a node the process may
// not route through, which is anything outside its positions and the enabled
// members of its routing set.

func seedPresetProcess(t *testing.T, db *DB) int64 {
	t.Helper()
	pid, err := db.CreateProcess("P400", "", "", "", "", false)
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	for _, n := range []string{"PLN_01", "PLN_04"} {
		if _, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: n, Name: n, Enabled: true}); err != nil {
			t.Fatalf("CreateProcessNode %s: %v", n, err)
		}
	}
	for _, in := range []domain.RoutingNodeInput{
		{ProcessID: pid, CoreNodeName: "Supermarket Empty Totes", Role: domain.RoutingRoleSource, Enabled: true},
		{ProcessID: pid, CoreNodeName: "Supermarket Area", Role: domain.RoutingRoleDestination, Enabled: true},
		{ProcessID: pid, CoreNodeName: "STG_UNREVIEWED", Role: domain.RoutingRoleStaging, Enabled: false, Origin: domain.RoutingOriginBackfill},
	} {
		if _, err := db.UpsertRoutingNode(in); err != nil {
			t.Fatalf("UpsertRoutingNode %s: %v", in.CoreNodeName, err)
		}
	}
	return pid
}

const pressIndexFlow = `{"cells":[{"core_node_name":"PLN_01","swap_mode":"two_robot_press_index","paired_core_node":"PLN_04",
	"inbound_source":"Supermarket Empty Totes","outbound_destination":"Supermarket Area","changeover_evac_nodes":["PLN_01","PLN_04"]}]}`

// TestFlowPreset_CreateListGetArchive: a preset is versioned by (process,
// name) — saving the same name again is a new version, never an edit in
// place — and archiving hides it from the list without breaking the id a
// claim's provenance points at.
func TestFlowPreset_CreateListGetArchive(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid := seedPresetProcess(t, db)

	v1, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: "Press index, two positions", FlowJSON: pressIndexFlow, CreatedBy: "alice"})
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	v2, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: "Press index, two positions", FlowJSON: pressIndexFlow, CreatedBy: "bob"})
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	p1, err := db.GetFlowPreset(v1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	p2, err := db.GetFlowPreset(v2)
	testutil.MustNoErr(t, err, "db.GetFlowPreset")
	if p1.Version != 1 || p2.Version != 2 || p1.Name != p2.Name || p2.CreatedBy != "bob" {
		t.Fatalf("versions = %+v / %+v, want v1 then v2 of the same name", p1, p2)
	}
	if p1.FlowJSON != pressIndexFlow || p1.ProcessID != pid || p1.CreatedAt.IsZero() || p1.ArchivedAt != nil {
		t.Fatalf("v1 did not round-trip: %+v", p1)
	}

	list, err := db.ListFlowPresets(pid, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Version != 2 || list[1].Version != 1 {
		t.Fatalf("list = %+v, want v2 then v1 (newest version first)", list)
	}

	if err := db.ArchiveFlowPreset(v1); err != nil {
		t.Fatalf("archive v1: %v", err)
	}
	list, err = db.ListFlowPresets(pid, false)
	testutil.MustNoErr(t, err, "db.ListFlowPresets")
	if len(list) != 1 || list[0].ID != v2 {
		t.Fatalf("list after archive = %+v, want only v2", list)
	}
	all, err := db.ListFlowPresets(pid, true)
	testutil.MustNoErr(t, err, "db.ListFlowPresets")
	if len(all) != 2 {
		t.Fatalf("list including archived = %d, want 2", len(all))
	}
	p1, err = db.GetFlowPreset(v1)
	if err != nil || p1.ArchivedAt == nil {
		t.Fatalf("archived v1 must still resolve by id (provenance points at it): %+v %v", p1, err)
	}
	// Another process cannot see it.
	other, err := db.CreateProcess("OTHER", "", "", "", "", false)
	testutil.MustNoErr(t, err, "db.CreateProcess")
	l, err := db.ListFlowPresets(other, true)
	testutil.MustNoErr(t, err, "db.ListFlowPresets")
	if len(l) != 0 {
		t.Fatalf("another process lists P400's presets: %+v", l)
	}
}

// TestFlowPreset_RefusesAPayload: a preset is a shape; the part is chosen
// when it is applied. Any cell with a payload is refused, with a message
// that says so and names the cell.
func TestFlowPreset_RefusesAPayload(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid := seedPresetProcess(t, db)
	for _, flow := range []string{
		`{"cells":[{"core_node_name":"PLN_01","payload":"SYN-A-P001"}]}`,
		`{"cells":[{"core_node_name":"PLN_01","payload_code":"SYN-A-P001"}]}`,
		`{"cells":[{"core_node_name":"PLN_01","payload":{"code":"X"}}]}`,
	} {
		_, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: "with part", FlowJSON: flow})
		if !errors.Is(err, domain.ErrFlowPresetHasPayload) {
			t.Fatalf("%s: err = %v, want ErrFlowPresetHasPayload", flow, err)
		}
		if !strings.Contains(err.Error(), "PLN_01") {
			t.Errorf("%s: refusal does not name the cell: %v", flow, err)
		}
	}
	// An EMPTY payload is not a payload.
	if _, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: "blank part",
		FlowJSON: `{"cells":[{"core_node_name":"PLN_01","payload":"","payload_code":null}]}`}); err != nil {
		t.Fatalf("empty payload refused: %v", err)
	}
	l, err := db.ListFlowPresets(pid, true)
	testutil.MustNoErr(t, err, "db.ListFlowPresets")
	if len(l) != 1 {
		t.Fatalf("refused presets were stored: %+v", l)
	}
}

// TestFlowPreset_RefusesANodeOutsidePositionsAndRoutingSet: every node a
// preset names must be a position of the process or a member of its routing
// set. A name Core may well have but this process does not route through is
// refused.
//
// ADOPTED OR NOT (owner ruling R4, 2026-09-12). `enabled` is the engineer
// filtering what OPERATORS are offered on the HMI, not a statement about what
// a flow may name — and `flow/save` never checked it, so a press whose routing
// set was backfilled and not yet adopted could RUN a flow it could not NAME.
// STG_UNREVIEWED moved from this list to the pin below it.
func TestFlowPreset_RefusesANodeOutsidePositionsAndRoutingSet(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid := seedPresetProcess(t, db)
	for _, tc := range []struct{ field, name string }{
		{"core_node_name", "PLN_99"},
		{"paired_core_node", "PLN_99"},
		{"second_paired_core_node", "PLN_99"},
		{"inbound_source", "SMN_BUF_100"},
		{"outbound_destination", "Somewhere Else"},
		{"changeover_evac_destination", "ULN_002"},
	} {
		flow := `{"cells":[{"core_node_name":"PLN_01","` + tc.field + `":"` + tc.name + `"}]}`
		_, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: "bad " + tc.field, FlowJSON: flow})
		if !errors.Is(err, domain.ErrFlowPresetUnknownNode) {
			t.Fatalf("%s=%s: err = %v, want ErrFlowPresetUnknownNode", tc.field, tc.name, err)
		}
		if !strings.Contains(err.Error(), tc.name) {
			t.Errorf("%s=%s: refusal does not name the node: %v", tc.field, tc.name, err)
		}
	}
	// A node list is checked element by element.
	_, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: "bad evac list",
		FlowJSON: `{"cells":[{"core_node_name":"PLN_01","changeover_evac_nodes":["PLN_01","PLN_77"]}]}`})
	if !errors.Is(err, domain.ErrFlowPresetUnknownNode) || !strings.Contains(err.Error(), "PLN_77") {
		t.Fatalf("evac list: err = %v, want ErrFlowPresetUnknownNode naming PLN_77", err)
	}
	// Positions and routing members are fine, in every field.
	if _, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: "good", FlowJSON: pressIndexFlow}); err != nil {
		t.Fatalf("a preset over positions and routing members was refused: %v", err)
	}
	// R4: A ROUTING ROW NOBODY HAS ADOPTED IS STILL NAMEABLE, before and
	// after adoption, and this is the pin for it. STG_UNREVIEWED is derived
	// and switched OFF here, which is the state a backfilled press arrives in
	// and the state in which `flow/save` has always accepted it.
	unadopted := `{"cells":[{"core_node_name":"PLN_01","inbound_staging":"STG_UNREVIEWED"}]}`
	if _, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: "not adopted yet",
		FlowJSON: unadopted}); err != nil {
		t.Fatalf("an unadopted routing member was refused, so a flow the press already runs cannot be named: %v", err)
	}
	rows, err := db.ListRoutingNodes(pid)
	testutil.MustNoErr(t, err, "db.ListRoutingNodes")
	for _, r := range rows {
		if r.CoreNodeName == "STG_UNREVIEWED" {
			if r.Enabled {
				t.Fatal("STG_UNREVIEWED is already adopted; this pin needs it switched off to mean anything")
			}
			if err := db.SetRoutingNodeEnabled(pid, r.ID, true, "eng"); err != nil {
				t.Fatalf("adopt: %v", err)
			}
		}
	}
	if _, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: "now adopted",
		FlowJSON: unadopted}); err != nil {
		t.Fatalf("adopted routing member refused: %v", err)
	}
}

// TestFlowPreset_RenameMovesEveryVersionAndNothingElse pins owner ruling R6:
// a rename is one column on every version of a name, the shape and the
// versions are untouched, and a name another lineage already holds is refused
// by name.
func TestFlowPreset_RenameMovesEveryVersionAndNothingElse(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid := seedPresetProcess(t, db)
	v1, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: "index", FlowJSON: pressIndexFlow})
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	v2, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: "index", FlowJSON: pressIndexFlow})
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	other, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: "swap", FlowJSON: pressIndexFlow})
	if err != nil {
		t.Fatalf("other: %v", err)
	}
	if err := db.ArchiveFlowPreset(v2); err != nil {
		t.Fatalf("archive v2: %v", err)
	}
	before2, err := db.GetFlowPreset(v2)
	testutil.MustNoErr(t, err, "db.GetFlowPreset")

	if err := db.RenameFlowPreset(v1, "2-robot index"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// EVERY VERSION, the archived one included: a lineage split across two
	// names is two presets that mean one shape, and the archived row is the
	// one a member's provenance is most likely to point at.
	for _, id := range []int64{v1, v2} {
		p, err := db.GetFlowPreset(id)
		if err != nil {
			t.Fatalf("reload %d: %v", id, err)
		}
		if p.Name != "2-robot index" {
			t.Errorf("preset %d is still called %q", id, p.Name)
		}
	}
	// AND NOTHING ELSE. The version, the shape and the archive stamp are what
	// provenance and the chooser read; a rename that moved one of them would
	// change what a member claim's source_preset_version means.
	after2, err := db.GetFlowPreset(v2)
	testutil.MustNoErr(t, err, "db.GetFlowPreset")
	if after2.Version != before2.Version || after2.FlowJSON != before2.FlowJSON {
		t.Errorf("rename moved more than the name: %+v -> %+v", before2, after2)
	}
	if (after2.ArchivedAt == nil) != (before2.ArchivedAt == nil) {
		t.Error("rename changed the archive stamp")
	}
	// A name another lineage holds is refused by name, not merged into it:
	// two lineages under one name would interleave versions.
	err = db.RenameFlowPreset(v1, "swap")
	if err == nil || !strings.Contains(err.Error(), "swap") {
		t.Fatalf("renaming onto a name in use: err = %v, want a refusal naming it", err)
	}
	p, err := db.GetFlowPreset(other)
	testutil.MustNoErr(t, err, "db.GetFlowPreset")
	if p.Name != "swap" {
		t.Errorf("the refused rename touched the other lineage: %+v", p)
	}
	p, err = db.GetFlowPreset(v1)
	testutil.MustNoErr(t, err, "db.GetFlowPreset")
	if p.Name != "2-robot index" {
		t.Errorf("the refused rename moved the renamed one: %+v", p)
	}
}

// TestFlowPreset_RefusesMalformedAndEmpty: flow_json must parse and must
// carry at least one cell, and a preset needs a name.
func TestFlowPreset_RefusesMalformedAndEmpty(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid := seedPresetProcess(t, db)
	for name, flow := range map[string]string{
		"not json":  `{"cells":[`,
		"no cells":  `{"cells":[]}`,
		"no object": `[]`,
	} {
		if _, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: name, FlowJSON: flow}); err == nil {
			t.Errorf("%s (%s) was accepted", name, flow)
		}
	}
	if _, err := db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: pid, Name: "  ", FlowJSON: pressIndexFlow}); err == nil {
		t.Error("a blank name was accepted")
	}
	l, err := db.ListFlowPresets(pid, true)
	testutil.MustNoErr(t, err, "db.ListFlowPresets")
	if len(l) != 0 {
		t.Fatalf("refused presets were stored: %+v", l)
	}
}
