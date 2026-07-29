package protocol_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"shingo/protocol"
)

// process_identity_test.go — ONE identity for an Edge process, across every wire
// that names one.
//
// ── THE BUG THIS PINS ────────────────────────────────────────────────────────
//
// DemandOriginState.ProcessID was int64, carrying an Edge SQLite row id, while
// PlantClaimsReport.ProcessID and SourcingState.ProcessID were string, carrying
// the Edge process NAME ("SNF2"). Both landed in Core, in columns typed to
// match: demand_origins.process_id BIGINT against process_styles.process_id
// TEXT. Postgres will not compare the two at all — "operator does not exist:
// text = bigint" — and there was no mapping table to route through, because the
// absence of that mapping was the whole problem. Two Phase 6 designs died on it.
//
// ── WHY A REFLECTION TEST AND NOT JUST THE DOCKER JOIN TEST ──────────────────
//
// shingo-core/store/demand_origin_process_join_docker_test.go proves the join
// returns rows, which is the capability. But it needs a Postgres container, so
// it runs in the docker suite and not in the ninety-second gate — and the way
// this regresses is somebody changing a struct field, which is a compile-time
// event that ought to fail at compile-time speed. This costs microseconds and
// fails in `go test ./...`.
//
// It asserts SAMENESS rather than string-ness, deliberately. If a future round
// decides all three should be a named type, or a struct, this test wants them to
// move together rather than to forbid the move.
func TestProcessIdentityHasOneTypeAcrossEveryWire(t *testing.T) {
	fields := []struct {
		what  string
		typ   reflect.Type
		field string
	}{
		{"DemandOriginState", reflect.TypeOf(protocol.DemandOriginState{}), "ProcessID"},
		{"PlantClaimsReport", reflect.TypeOf(protocol.PlantClaimsReport{}), "ProcessID"},
		{"SourcingState", reflect.TypeOf(protocol.SourcingState{}), "ProcessID"},
	}

	var want reflect.Type
	for _, f := range fields {
		sf, ok := f.typ.FieldByName(f.field)
		if !ok {
			t.Fatalf("%s has no %s field. If it was renamed, rename it here too — this test "+
				"is the only thing holding the three wires to one description of a process.",
				f.what, f.field)
		}
		if want == nil {
			want = sf.Type
			continue
		}
		if sf.Type != want {
			t.Errorf("%s.%s is %s but %s.%s is %s.\n\n"+
				"These name the SAME Edge process and they land in Core columns typed to "+
				"match. When the types diverge, so do the columns, and Postgres cannot join "+
				"them: demand_origins.process_id BIGINT against process_styles.process_id "+
				"TEXT gives \"operator does not exist: text = bigint\", with no mapping "+
				"table to route through because the missing mapping IS the problem. The "+
				"agreed value is the Edge process NAME (\"SNF2\") — it is meaningful to "+
				"Core and it survives an Edge reinstall, which a row id does not.",
				f.what, f.field, sf.Type, fields[0].what, fields[0].field, want)
		}
	}
}

// TestCellEpisodeKeyCarriesTheProcessNameVerbatim holds the other half: the key
// and the column must agree, not merely be the same Go type.
//
// A key is a string either way, so the reflection test above cannot see it. What
// it CAN be is a string built from the wrong value — and that failure is silent
// in the worst way: a mismatched key does not error, it just fails to find the
// open episode and mints a second one for a place that already has one, breaking
// the single invariant the whole surface rests on.
func TestCellEpisodeKeyCarriesTheProcessNameVerbatim(t *testing.T) {
	const name = "PLN-01/L"
	key := protocol.CellEpisodeKey(name, "PANEL-B", protocol.EpisodeDirectionSupply)

	if !strings.Contains(key, name) {
		t.Fatalf("CellEpisodeKey(%q) = %q — the process name is not in it verbatim. If it "+
			"is being formatted, hashed or coerced, the key stops agreeing with "+
			"demand_origins.process_id and an episode becomes unfindable by the value "+
			"that identifies it.", name, key)
	}

	parsed, err := protocol.ParseEpisodeKey(key)
	if err != nil {
		t.Fatalf("ParseEpisodeKey(%q): %v", key, err)
	}
	if parsed.ProcessID != name {
		t.Errorf("round trip gave process %q, want %q", parsed.ProcessID, name)
	}

	// And the wire field round-trips the same value through JSON. The column is
	// written from the decoded message, so a tag or an omitempty surprise here
	// would put an empty process on a real episode.
	raw, err := json.Marshal(protocol.DemandOriginState{ProcessID: name})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back protocol.DemandOriginState
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ProcessID != name {
		t.Errorf("DemandOriginState.ProcessID round-tripped as %q, want %q", back.ProcessID, name)
	}
}
