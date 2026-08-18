//go:build docker

package scenarios

import (
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"shingocore/dispatch"
	coreorders "shingocore/store/orders"
	edgeharness "shingoedge/testharness"
)

// order_projection_drift_test.go — a field Core puts on the wire must reach the
// Edge, or be named here as deliberately dropped.
//
// WHY THIS EXISTS. protocol.OrderProjection is written by
// dispatch.ProjectionFor on one side of a module boundary and read by
// engine.ApplyOrderProjection on the other. Nothing connected the two: a field
// added to the struct and copied by Core, but never added to the Edge's
// ProjectionRow or its INSERT column list, is dropped in silence. Both sides
// compile, every existing test passes, and the value simply is not there.
//
// That is not hypothetical. payload_desc has been on the wire and dropped on the
// floor for its whole life, and it took someone reading both files side by side
// to notice. This test is what that reading would have been.
//
// BEHAVIOURAL, NOT A DECLARED LIST. It would be easy — and worthless — to
// hand-maintain "these are the consumed fields" beside the struct: that is a
// comment with a test's costume on, and it goes stale exactly when the code
// does. So this projects a REAL Core order through the REAL ProjectionFor, hands
// it to the REAL Edge applier against a REAL SQLite database, reads the stored
// row back, and looks for each field's value. A field that arrives nowhere in
// the row was dropped, whatever anybody believes.
//
// It lives in integration/ because that is the only module importing both
// shingocore and shingoedge — neither module's own test package can compile the
// other's code. Same reason loader_parity_test.go lives here.

// knownDropped names every projection field the Edge deliberately does not
// store, with the reason. A field NOT in this map must survive the round trip.
//
// ADDING TO THIS MAP IS A DECISION, and that is the whole point of the map
// existing: dropping a field becomes something someone typed a reason for,
// rather than something that happened because two files were edited on different
// days.
// PayloadDesc, OriginID and OriginClass were all exempted here and all three
// entries were DELETED in MG2-8, which is the map working exactly as intended:
// each was a dropped field somebody had typed a reason for, and each reason
// named the fix. The Edge now has a column for all three, so an exemption would
// fail TestOrderProjection_KnownDroppedAreStillDropped rather than sit here
// describing a state of the world that ended.
var knownDropped = map[string]string{
	"StationID": "the Edge IS the station — it knows its own id, and storing Core's copy would create a second answer that can disagree",
}

// TestOrderProjection_NoFieldDropsSilently is the gate.
func TestOrderProjection_NoFieldDropsSilently(t *testing.T) {
	// A distinct sentinel per field, so "did this value arrive" cannot be
	// answered by accident from a neighbouring column.
	order := &coreorders.Order{
		EdgeUUID:     "mg1c-drift-uuid",
		OrderType:    dispatch.OrderTypeRetrieveEmpty,
		Status:       "queued",
		StationID:    "mg1c-drift-station",
		Quantity:     4242,
		SourceNode:   "MG1C-DRIFT-SOURCE",
		DeliveryNode: "MG1C-DRIFT-DELIVERY",
		PayloadCode:  "MG1C-DRIFT-PAYLOAD",
		PayloadDesc:  "MG1C-DRIFT-DESC",
		OriginID:     "11111111-2222-3333-4444-555555555555",
		OriginClass:  "MG1C-DRIFT-CLASS",
		QueueReason:  "MG1C-DRIFT-REASON",
		QueueCode:    "MG1C-DRIFT-CODE",
	}

	proj := dispatch.ProjectionFor(order)

	// FIXTURE CHECK FIRST. Every field on the wire type must be non-zero here,
	// or the round-trip check below is vacuous for it — a field left at its zero
	// value would "arrive" trivially or fail for the wrong reason. This is what
	// makes the test notice a NEW field: adding one to OrderProjection fails
	// here, at the fixture, with the field's name, before anybody gets to argue
	// about whether the Edge stores it.
	v := reflect.ValueOf(proj)
	typ := v.Type()
	var unset []string
	for i := 0; i < typ.NumField(); i++ {
		if v.Field(i).IsZero() {
			unset = append(unset, typ.Field(i).Name)
		}
	}
	if len(unset) > 0 {
		sort.Strings(unset)
		t.Fatalf(`projection fixture leaves %v at the zero value.

A new field was added to protocol.OrderProjection (or to dispatch.ProjectionFor)
and this fixture was not updated. Give it a distinctive non-zero value above,
then this test will tell you whether the Edge actually stores it.`, unset)
	}

	// The real applier, against a real Edge.
	edge := edgeharness.NewEdge(t, "mg1c-drift-station")
	if _, err := edge.Engine.ApplyOrderProjection(proj); err != nil {
		t.Fatalf("ApplyOrderProjection: %v", err)
	}

	stored := readOrderRow(t, edge.DB.DB, proj.OrderUUID)

	var lost []string
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if _, ok := knownDropped[name]; ok {
			continue
		}
		if !valueLandedInRow(v.Field(i), stored) {
			lost = append(lost, name)
		}
	}

	if len(lost) > 0 {
		sort.Strings(lost)
		t.Errorf(`projection field(s) %v never reached the Edge.

Core's dispatch.ProjectionFor puts these on the wire and nothing on the Edge
stores them. Fix one of:

  - shingo-edge/engine/order_projection.go   copy the field onto ProjectionRow
  - shingo-edge/store/orders/orders.go       add it to ProjectionRow, the INSERT
                                             column list, AND the DO UPDATE SET
  - shingo-edge/store/ migrations            add the column

or, if the drop is deliberate, add it to knownDropped in this file WITH THE
REASON. Silence is the one option this test removes.

stored row was: %v`, lost, stored)
	}
}

// TestOrderProjection_KnownDroppedAreStillDropped is the other direction, and it
// is what stops knownDropped from rotting into a list of things that used to be
// true.
//
// It has already done its job once. MG2-8 gave the Edge columns for
// payload_desc, origin_id and origin_class; this test failed on all three
// entries, and the fix was to DELETE them — which is exactly the moment somebody
// should be told an exemption is no longer needed.
func TestOrderProjection_KnownDroppedAreStillDropped(t *testing.T) {
	order := &coreorders.Order{
		EdgeUUID:     "mg1c-drift-known-uuid",
		OrderType:    dispatch.OrderTypeRetrieveEmpty,
		Status:       "queued",
		StationID:    "mg1c-drift-station",
		Quantity:     4243,
		SourceNode:   "MG1C-KNOWN-SOURCE",
		DeliveryNode: "MG1C-KNOWN-DELIVERY",
		PayloadCode:  "MG1C-KNOWN-PAYLOAD",
		PayloadDesc:  "MG1C-KNOWN-DESC",
		OriginID:     "99999999-8888-7777-6666-555555555555",
		OriginClass:  "MG1C-KNOWN-CLASS",
		QueueReason:  "MG1C-KNOWN-REASON",
		QueueCode:    "MG1C-KNOWN-CODE",
	}
	proj := dispatch.ProjectionFor(order)

	edge := edgeharness.NewEdge(t, "mg1c-drift-station")
	if _, err := edge.Engine.ApplyOrderProjection(proj); err != nil {
		t.Fatalf("ApplyOrderProjection: %v", err)
	}
	stored := readOrderRow(t, edge.DB.DB, proj.OrderUUID)

	v := reflect.ValueOf(proj)
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		reason, exempt := knownDropped[name]
		if !exempt {
			continue
		}
		if valueLandedInRow(v.Field(i), stored) {
			t.Errorf(`projection field %q IS stored on the Edge now, but knownDropped still exempts it:

    %s

Delete the entry — the exemption has been earned out.`, name, reason)
		}
	}
}

// readOrderRow reads the Edge's whole orders row for a uuid as column → text.
// SELECT *, deliberately: a check that named the columns would go stale in the
// same way the applier did.
func readOrderRow(t *testing.T, db *sql.DB, uuid string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM orders WHERE uuid = ?`, uuid)
	if err != nil {
		t.Fatalf("read stored order: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("no Edge orders row for uuid %s — the projection did not create one", uuid)
	}
	cells := make([]any, len(cols))
	for i := range cells {
		cells[i] = new(sql.NullString)
	}
	if err := rows.Scan(cells...); err != nil {
		t.Fatalf("scan stored order: %v", err)
	}
	out := make(map[string]string, len(cols))
	for i, c := range cols {
		if ns := cells[i].(*sql.NullString); ns.Valid {
			out[c] = ns.String
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// valueLandedInRow reports whether a projection field's value appears in any
// column of the stored row.
//
// BY VALUE, NOT BY COLUMN NAME, because the Edge is free to name its column
// whatever it likes (and does — RetrieveEmpty lives in retrieve_empty, the
// delivery node is ALSO resolved into process_node_id). Matching on the value
// asks the question that matters: did this information survive?
//
// The honest limitation: a bool has two possible values, so a dropped bool could
// match another boolean column by coincidence. Every string field here carries a
// unique sentinel, so this is exact for 11 of the 14; Quantity uses a value no
// other column holds; RetrieveEmpty is the one field where the match is by shape
// rather than by identity. Worth knowing before trusting a pass on a NEW bool.
func valueLandedInRow(field reflect.Value, row map[string]string) bool {
	var want []string
	switch field.Kind() {
	case reflect.Bool:
		if field.Bool() {
			want = []string{"1", "true"}
		} else {
			want = []string{"0", "false"}
		}
	case reflect.Int, reflect.Int64:
		want = []string{fmt.Sprintf("%d", field.Int())}
	default:
		want = []string{fmt.Sprintf("%v", field.Interface())}
	}
	for _, cell := range row {
		for _, w := range want {
			if strings.EqualFold(cell, w) {
				return true
			}
		}
	}
	return false
}
