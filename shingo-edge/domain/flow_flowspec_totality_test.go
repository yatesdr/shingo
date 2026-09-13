package domain

import (
	"reflect"
	"testing"

	"shingoedge/domain/flowspec"
)

// flow_flowspec_totality_test.go — flowspec has a row for EVERY column the
// composer can write.
//
// flowspec is the authority on what a choreography uses, requires and
// forbids: the model clears a forbidden field when the operator changes mode,
// the desktop draws a chip only for a field that is Used or Required, and the
// validator refuses on it. A column the composer can write and flowspec has
// never heard of is a column with no authority at all — its value domain is
// enforced in the browser and nowhere else, and the server accepts whatever
// arrives.
//
// reorder_point_source was exactly that, and it was the LAST one. This is the
// test that says so, so the next field added to FlowAdvanced cannot repeat it.
func TestEveryComposerWritableFieldHasAFlowspecRow(t *testing.T) {
	t.Parallel()

	known := map[string]bool{}
	for _, f := range flowspec.Fields() {
		known[string(f)] = true
	}
	// The two discriminants the per-mode tables are keyed BY. They are not in
	// Fields() — a mode does not "use" the mode — but they are claim field
	// names and they do have labels, which is what the surfaces look up.
	known[string(flowspec.SwapMode)] = true
	known[string(flowspec.Role)] = true

	// A cell's own identity. core_node_name is which position this is, not a
	// setting on it; a flowspec row saying a mode "uses" the position's name
	// would be answering a question nobody asks.
	exempt := map[string]bool{"core_node_name": true, "advanced": true, "locked": true}

	for _, tc := range []struct {
		what string
		typ  reflect.Type
	}{
		{"FlowCell", reflect.TypeOf(FlowCell{})},
		{"FlowAdvanced", reflect.TypeOf(FlowAdvanced{})},
	} {
		for i := 0; i < tc.typ.NumField(); i++ {
			name := jsonName(tc.typ.Field(i))
			if name == "" || exempt[name] {
				continue
			}
			if !known[name] {
				t.Errorf("%s writes %q and flowspec has no field for it. The model cannot know "+
					"whether a choreography forbids it, the desktop cannot decide whether to draw "+
					"a chip for it, and the validator has nothing to refuse on — so its value "+
					"domain is whatever the browser happens to enforce.", tc.what, name)
			}
		}
	}
}

// TestFlowspecHasNoFieldTheComposerCannotWrite is the other direction, and it
// is a WARNING SHAPE rather than a ban: flowspec also covers columns the
// composer never shows (the ones Expand carries through from the prior claim),
// and that is correct. What it must not have is a field that is not a claim
// column at all — a typo, or a name that drifted when a column was renamed.
func TestFlowspecFieldsAreClaimColumns(t *testing.T) {
	t.Parallel()

	columns := map[string]bool{}
	for _, typ := range []reflect.Type{reflect.TypeOf(NodeClaim{}), reflect.TypeOf(NodeClaimInput{})} {
		for i := 0; i < typ.NumField(); i++ {
			if n := jsonName(typ.Field(i)); n != "" {
				columns[n] = true
			}
		}
	}
	for _, f := range flowspec.Fields() {
		if !columns[string(f)] {
			t.Errorf("flowspec names %q and neither NodeClaim nor NodeClaimInput has such a field — "+
				"a per-mode table keyed on a column that does not exist answers about nothing", f)
		}
	}
}

// jsonName is a struct field's wire name, or "" when it carries none.
func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			tag = tag[:i]
			break
		}
	}
	if tag == "-" {
		return ""
	}
	return tag
}
