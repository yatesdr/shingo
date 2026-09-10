//go:build docker

package orders_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// ownedByTransition is the subset of deliberatelyNotWritten whose columns a
// CALLER is likely to set on the struct and expect to persist, because they are
// ordinary-looking data fields rather than fleet bookkeeping.
//
// They are the reason this test exists. Complex intake assigned all three at
// creation and wrote them again through SetOrderQueueDetail; only the second
// write ever landed, and nothing said so. The rule is: queue detail belongs to
// the transition that queues an order, never to creation. Naming them here puts
// the creation-versus-transition boundary in one place a reader can find.
var ownedByTransition = map[string]string{
	"queue_reason": "SetOrderQueueDetail",
	"queue_code":   "SetOrderQueueDetail",
	"queue_cause":  "SetOrderQueueDetail",
}

// writtenByTheWriterItself are columns Create binds from its own clock rather
// than from the struct, so a caller's value is expected NOT to survive.
var writtenByTheWriterItself = map[string]string{
	"created_at": "clock.Now() inside Create",
	"updated_at": "clock.Now() inside Create",
}

// TestWriter_RoundTripsEveryFieldItWrites is the property behind the column
// census: a value set on an Order before Create comes back off the table
// unchanged.
//
// TestWriter_CoversEveryOrdersColumn compares two strings and can only prove the
// INSERT mentions a column. This proves the mention does something — that the
// column is bound to the field a caller would expect, in the right position.
// The five columns dropped from compound children were all mentioned somewhere;
// what was missing was any test that read one back.
//
// Coverage is enforced by reflection: every field on domain.Order must have a
// probe value here, or be excluded by name with a reason. Adding a field to the
// struct without doing one or the other fails this test, which is the point —
// the alternative is a hand-maintained list that quietly stops being total.
func TestWriter_RoundTripsEveryFieldItWrites(t *testing.T) {
	// The probe row stamps bin_id because bin_id is a COLUMN, and a status
	// because status is a column, and the pair happens to spell an acquiring
	// order pointing at a bin it does not hold. Nothing here is sourcing
	// anything; the end-of-test wedge sweep would be reading a census as a
	// scenario.
	testdb.DisableWedgeSweep(t, "probe values for a column census, not a plant state")
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "RT-BIN-1")
	parent := testdb.CreateOrder(t, db)

	// Every value is distinguishable from every other field's value and from
	// the DDL default, so a column bound to the wrong parameter shows up as a
	// swap rather than passing by coincidence.
	completedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	remainingUOP := 37
	probe := map[string]any{
		"EdgeUUID":         "roundtrip-edge-uuid",
		"StationID":        "roundtrip-station",
		"OrderType":        protocol.OrderType("move"),
		"Status":           protocol.Status("queued"),
		"Quantity":         int64(9),
		"SourceNode":       "ROUNDTRIP-SOURCE",
		"DeliveryNode":     "ROUNDTRIP-DELIVERY",
		"ProcessNode":      "ROUNDTRIP-PROCESS",
		"Priority":         5,
		"PayloadDesc":      "roundtrip payload description",
		"ParentOrderID":    &parent.ID,
		"Sequence":         3,
		"StepsJSON":        `{"steps":["roundtrip"]}`,
		"BinID":            &bin.ID,
		"PayloadCode":      std.Payload.Code,
		"SkipAutoConfirm":  true,
		"SiblingOrderUUID": "roundtrip-sibling-uuid",
		// TWO POINTS, NOT ONE, AND IN AN UNSORTED ORDER. key_route is a list in
		// one TEXT column: a writer that round-tripped it as a set would return
		// the same two strings and pass a one-element or sorted probe. SEER
		// walks the points in the order given, so the order IS the route.
		"KeyRoute":     []string{"ROUNDTRIP-AISLE-B", "ROUNDTRIP-AISLE-A"},
		"KeyTask":      "unload",
		"SourceIntent": "empty",
		"Coordinated":  true,
		"OriginID":     "6f1c8b2e-4a9d-4c3f-8e5b-7d2a1f0c9b34",
		"OriginClass":  "demand",
		// A birth fact, so unlike OpenForChildren it round-trips through Create.
		// That is the property worth pinning: if this ever stops surviving the
		// INSERT, a service dig's lane releases on the last blocker and the bin
		// it uncovered sits in an open lane with nothing but its claim.
	}

	// Fields the writer does not take from the struct. Keyed by struct field,
	// valued by the column so the reasons above stay reachable from here.
	notFromTheStruct := map[string]string{
		"ID":            "id",
		"CreatedAt":     "created_at",
		"UpdatedAt":     "updated_at",
		"VendorOrderID": "vendor_order_id",
		"VendorState":   "vendor_state",
		"RobotID":       "robot_id",
		"ErrorDetail":   "error_detail",
		"CompletedAt":   "completed_at",
		"WaitIndex":     "wait_index",
		"QueueReason":   "queue_reason",
		"QueueCode":     "queue_code",
		"QueueCause":    "queue_cause",
		"RemainingUOP":  "remaining_uop",
		// Create does not carry it and must not: an order is born sealed by the
		// column's DEFAULT, and SetCompoundOpen is the only thing that changes
		// it. Round-tripping a probe value through Create would assert the
		// opposite -- that a caller can hand openness in at creation.
		"OpenForChildren": "open_for_children",
	}

	// Every excluded field must be excluded for a reason that is written down,
	// and the two reason maps must name real columns. Otherwise "excluded"
	// degrades into "we gave up on this one".
	for field, col := range notFromTheStruct {
		_, skipped := deliberatelyNotWritten[col]
		_, byClock := writtenByTheWriterItself[col]
		if !skipped && !byClock {
			t.Errorf("field %s is excluded from the round trip but column %q is neither in deliberatelyNotWritten nor written by the writer's own clock — say which", field, col)
		}
	}
	for col := range ownedByTransition {
		if _, ok := deliberatelyNotWritten[col]; !ok {
			t.Errorf("ownedByTransition names %q, which is not in deliberatelyNotWritten — the two lists have drifted", col)
		}
	}

	o := &orders.Order{}
	rv := reflect.ValueOf(o).Elem()
	rt := rv.Type()

	// Set the probe values, and fail on any field that has neither a probe nor
	// an exclusion.
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		if _, ok := notFromTheStruct[name]; ok {
			continue
		}
		want, ok := probe[name]
		if !ok {
			t.Fatalf("Order has field %s with no probe value and no exclusion.\n"+
				"Add a distinctive value to probe (if Create should write it), or add the field to notFromTheStruct naming the column and who writes it instead.\n"+
				"A field in neither place is one nothing checks.", name)
		}
		wv := reflect.ValueOf(want)
		if !wv.Type().AssignableTo(rt.Field(i).Type) {
			t.Fatalf("probe value for %s is %s, field is %s", name, wv.Type(), rt.Field(i).Type)
		}
		rv.Field(i).Set(wv)
	}

	// Set an excluded field to a non-zero value too. Not to assert it survives —
	// it must not — but so a future writer that starts binding one of these is
	// caught by the read-back below instead of shipping silently.
	o.QueueReason = "set at creation, and it should not stick"
	o.VendorOrderID = "set at creation, and it should not stick"
	o.CompletedAt = &completedAt
	o.RemainingUOP = &remainingUOP

	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if o.ID == 0 {
		t.Fatal("Create did not write the new id back onto the struct")
	}

	got, err := db.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("read order %d back: %v", o.ID, err)
	}

	gv := reflect.ValueOf(got).Elem()
	var lost []string
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		if _, ok := notFromTheStruct[name]; ok {
			continue
		}
		want := probe[name]
		have := gv.Field(i).Interface()
		if !equalMaybePointer(want, have) {
			lost = append(lost, describeMismatch(name, want, have))
		}
	}
	if len(lost) > 0 {
		t.Errorf("values set before Create did not survive the round trip:\n  %s\n"+
			"Each of these is a column the INSERT names but does not carry to the right field.",
			strings.Join(lost, "\n  "))
	}

	// The exclusions, verified rather than assumed.
	if got.QueueReason != "" {
		t.Errorf("queue_reason = %q after create, want empty: it is written by the queueing transition (%s), not at creation. If that changed on purpose, move it out of deliberatelyNotWritten and give it a probe value.",
			got.QueueReason, ownedByTransition["queue_reason"])
	}
	if got.VendorOrderID != "" {
		t.Errorf("vendor_order_id = %q after create, want empty: it is minted at dispatch", got.VendorOrderID)
	}
	if got.CompletedAt != nil {
		t.Errorf("completed_at = %v after create, want NULL: it is written at the terminal transition", got.CompletedAt)
	}
	if got.RemainingUOP != nil {
		t.Errorf("remaining_uop = %v after create, want NULL: it is carried to the bin claim, not written here", *got.RemainingUOP)
	}
}

// equalMaybePointer compares probe values, following one level of pointer so a
// *int64 probe compares by value rather than by address.
func equalMaybePointer(want, have any) bool {
	wv, hv := reflect.ValueOf(want), reflect.ValueOf(have)
	if wv.Kind() == reflect.Pointer && hv.Kind() == reflect.Pointer {
		if wv.IsNil() || hv.IsNil() {
			return wv.IsNil() == hv.IsNil()
		}
		return reflect.DeepEqual(wv.Elem().Interface(), hv.Elem().Interface())
	}
	return reflect.DeepEqual(want, have)
}

func describeMismatch(name string, want, have any) string {
	wv, hv := reflect.ValueOf(want), reflect.ValueOf(have)
	if wv.Kind() == reflect.Pointer && hv.Kind() == reflect.Pointer && !wv.IsNil() && !hv.IsNil() {
		return fmt.Sprintf("%s: got %v, want %v", name, hv.Elem().Interface(), wv.Elem().Interface())
	}
	return fmt.Sprintf("%s: got %v, want %v", name, have, want)
}
