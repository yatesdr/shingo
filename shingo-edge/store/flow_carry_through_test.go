package store

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// flow_carry_through_test.go — a save through the composer leaves every column
// the composer does not speak exactly as it was.
//
// WHY THIS IS THE LOAD-BEARING TEST. SaveFlow used to refuse to write a cell
// whose claim carried anything the composer does not show, so an engineer's
// reorder point or keep-staged flag could not be flattened by an operator
// saving a flow. That refusal was belt-and-braces over a guarantee Expand
// already provides: of the columns UpsertClaim writes unconditionally it copies
// the unspoken ones from the prior claim, and of the pointer-gated ones it
// speaks only the three the cell carries and leaves the rest nil, so the update
// does not touch them.
//
// The refusal is gone (2026-09-09). This is the guarantee it was standing in
// front of, on the real plant rows, on the path every save now takes.

// composerSpeaks is every column a FlowCell carries, so a save is expected to
// write it. Everything else in style_node_claims must survive untouched.
var composerSpeaks = map[string]bool{
	"core_node_name": true, "role": true, "swap_mode": true, "payload_code": true,
	"paired_core_node": true, "second_paired_core_node": true,
	"inbound_source": true, "inbound_staging": true, "outbound_staging": true,
	"outbound_destination": true, "changeover_evac_destination": true,
	"changeover_evac_nodes": true, "key_route": true,
	// Quality containment: the claim's divert destination when containment is
	// active for its payload. Composer-authored (the Quality Containment
	// section of the claim editor) exactly like its sibling
	// outbound_destination; carried through a flow copy for the same reason.
	"containment_destination": true,
}

// composerRestamps is what a save is expected to change without being asked:
// who wrote the row and when. Not the flow, and not carried through.
var composerRestamps = map[string]bool{
	"source": true, "called_by": true, "updated_at": true, "retired_at": true,
}

// notAColumnOfItsOwn are row identity and the resolved-not-stored capacity.
var notAColumnOfItsOwn = map[string]bool{
	"id": true, "style_id": true, "created_at": true,
	// Resolved from payload_catalog on read; the column is dead and its value
	// follows payload_code, which the composer does speak. See
	// claim_capacity_test.go.
	"uop_capacity": true,
}

// vestigial columns are carried by the style_node_claims rebuild
// (migrations_style_claims.go) so an upgrade does not drop what a plant row
// happens to hold, and are named by nothing else: not claimSelect, not the
// INSERT, not the UPDATE, not cloneClaimColumns. They cannot be flattened by a
// composer save because no writer mentions them at all — which is a weaker
// guarantee than carry-through and a different one, so they are listed apart
// rather than folded in.
//
// They are listed BY NAME and not matched by a pattern: if one of them ever
// acquires a reader, whoever adds it has to move it into unspokenColumns and
// say what carries it.
var vestigialColumns = map[string]bool{
	"mode": true, "staging_node": true, "release_node": true,
	"outbound_source":            true,
	"inbound_source_node":        true,
	"inbound_source_node_group":  true,
	"outbound_source_node":       true,
	"outbound_source_node_group": true,
}

// unspokenColumn is one column the composer does not author, named, with the
// way to read it off a claim. NAMING THEM ONE BY ONE IS THE POINT: a column
// added to style_node_claims and not classified here fails
// TestFlowCarryThrough_EveryUnspokenColumnIsAccountedFor, so a new column
// cannot quietly join the set the composer is trusted not to flatten.
var unspokenColumns = []struct {
	column string
	of     func(processes.NodeClaim) any
}{
	{"reorder_point", func(c processes.NodeClaim) any { return c.ReorderPoint }},
	{"reorder_point_source", func(c processes.NodeClaim) any { return c.ReorderPointSource }},
	{"auto_reorder", func(c processes.NodeClaim) any { return c.AutoReorder }},
	{"below_reorder_since", func(c processes.NodeClaim) any { return c.BelowReorderSince }},
	{"allowed_payload_codes", func(c processes.NodeClaim) any { return c.AllowedPayloadCodes }},
	{"auto_request_payload", func(c processes.NodeClaim) any { return c.AutoRequestPayload }},
	{"keep_staged", func(c processes.NodeClaim) any { return c.KeepStaged }},
	{"evacuate_on_changeover", func(c processes.NodeClaim) any { return c.EvacuateOnChangeover }},
	{"auto_confirm", func(c processes.NodeClaim) any { return c.AutoConfirm }},
	{"sequence", func(c processes.NodeClaim) any { return c.Sequence }},
	{"lineside_soft_threshold", func(c processes.NodeClaim) any { return c.LinesideSoftThreshold }},
	{"reuse_compatible_bins", func(c processes.NodeClaim) any { return c.ReuseCompatibleBins }},
	{"auto_push", func(c processes.NodeClaim) any { return c.AutoPush }},
	{"index_robot_supplies", func(c processes.NodeClaim) any { return c.IndexRobotSupplies }},
	{"key_task", func(c processes.NodeClaim) any { return c.KeyTask }},
	{"changeover_carryover_disposition", func(c processes.NodeClaim) any { return c.ChangeoverCarryoverDisposition }},
	{"source_preset_id", func(c processes.NodeClaim) any { return c.SourcePresetID }},
	{"source_preset_version", func(c processes.NodeClaim) any { return c.SourcePresetVersion }},
}

// TestFlowCarryThrough_EveryUnspokenColumnIsAccountedFor holds the table above
// against the real table. Every column of style_node_claims is either one the
// composer speaks, one a save restamps, row identity, or named in
// unspokenColumns — and a column that is none of those is a column nobody has
// decided about, which is the failure.
func TestFlowCarryThrough_EveryUnspokenColumnIsAccountedFor(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	rows, err := db.Query(`SELECT name FROM pragma_table_info('style_node_claims')`)
	if err != nil {
		t.Fatalf("read table info: %v", err)
	}
	defer rows.Close()

	named := map[string]bool{}
	for _, u := range unspokenColumns {
		named[u.column] = true
	}
	var unclassified, missing []string
	actual := map[string]bool{}
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		actual[col] = true
		if composerSpeaks[col] || composerRestamps[col] || notAColumnOfItsOwn[col] ||
			vestigialColumns[col] || named[col] {
			continue
		}
		unclassified = append(unclassified, col)
	}
	for col := range named {
		if !actual[col] {
			missing = append(missing, col)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(missing)
	if len(unclassified) > 0 {
		t.Errorf("style_node_claims columns nobody has classified: %v\n"+
			"Add each to composerSpeaks (the composer authors it), to unspokenColumns "+
			"(the composer must carry it through), to vestigialColumns (nothing reads "+
			"or writes it), or to notAColumnOfItsOwn.", unclassified)
	}
	if len(missing) > 0 {
		t.Errorf("unspokenColumns names columns the table does not have: %v", missing)
	}
}

// TestFlowCarryThrough_SaveLeavesEveryUnspokenColumnAlone is the proof itself,
// on every live claim at both plants.
//
// It checks each unspoken column BY NAME, so a failure says which column the
// composer flattened rather than printing two structs — and so the set of
// columns being trusted is written down somewhere a reviewer can read.
func TestFlowCarryThrough_SaveLeavesEveryUnspokenColumnAlone(t *testing.T) {
	t.Parallel()
	for _, plant := range []string{"a", "b"} {
		t.Run(plant, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			f := readPlantFixture(t, plant)
			loadPlantFixture(t, db, f)
			live := liveFixtureClaims(t, db)
			if len(live) == 0 {
				t.Fatal("no live claims loaded")
			}
			carried := map[string]int{}
			for _, c := range live {
				label := plant + " claim " + itoa64(c.ID) + " " + c.CoreNodeName

				cell := domain.Collapse(c)
				in := domain.Expand(cell, &c, domain.ClaimSourceHMI, "Press 400")
				id, err := processes.UpsertClaim(db.DB, in)
				if err != nil {
					t.Errorf("%s: the store refused the claim's own cell: %v", label, err)
					continue
				}
				stored, err := db.GetStyleNodeClaim(id)
				if err != nil {
					t.Fatalf("%s: read back: %v", label, err)
				}
				for _, u := range unspokenColumns {
					want, got := u.of(c), u.of(*stored)
					if !reflect.DeepEqual(want, got) {
						t.Errorf("%s: a composer save changed %s: %v → %v", label, u.column, want, got)
						continue
					}
					// Count only the columns that had something to lose: a
					// column that was at its default on every row proves
					// nothing about carry-through.
					if !isZeroish(want) {
						carried[u.column]++
					}
				}
			}
			// The fixtures must actually exercise the guarantee. If no live
			// claim carries anything unspoken, this test is green on nothing.
			if len(carried) == 0 {
				t.Error("no live claim carried a non-default unspoken column — the test has no input")
			}
			cols := make([]string, 0, len(carried))
			for col, n := range carried {
				cols = append(cols, col+"="+itoa(n))
			}
			sort.Strings(cols)
			t.Logf("%s: unspoken columns carried through a composer save: %s", plant, strings.Join(cols, ", "))
		})
	}
}

func isZeroish(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map, reflect.Ptr, reflect.Interface:
		return rv.IsNil() || (rv.Kind() == reflect.Slice && rv.Len() == 0)
	default:
		return rv.IsZero()
	}
}
