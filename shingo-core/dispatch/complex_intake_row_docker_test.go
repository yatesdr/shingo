//go:build docker

package dispatch

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store/orders"
)

// complex_intake_row_docker_test.go — the row complex intake writes, column by
// column, held while the writing moves onto the issue (complexNeed, then K5's
// issue): the facts the request states, the coordinated stamp, the resolved
// steps, and nothing the intake does not write (no bin, no plan, no robot, no
// projection, no source intent).

func TestComplexIntake_TheRowAsWritten(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storage, line, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, nil)
	origin := uuid.New().String()
	steps := []protocol.ComplexOrderStep{
		{Action: protocol.ActionPickup, Node: storage.Name},
		{Action: protocol.ActionDropoff, Node: line.Name},
	}
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "cir-1", PayloadCode: bp.Code, PayloadDesc: "the row's desc", Quantity: 2, Priority: 3,
		ProcessNode: line.Name, SiblingOrderUUID: "cir-sibling", KeyRoute: []string{"R1", "R2"}, KeyTask: "T1",
		OriginID: origin, Steps: steps,
	})
	o, err := db.GetOrderByUUID("cir-1")
	testutil.MustNoErr(t, err, "the complex row")

	var gotSteps []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(o.StepsJSON), &gotSteps), "its steps")
	wantSteps := []resolvedStep{{Action: protocol.ActionPickup, Node: storage.Name}, {Action: protocol.ActionDropoff, Node: line.Name}}
	if !reflect.DeepEqual(gotSteps, wantSteps) {
		t.Errorf("steps = %+v, want %+v", gotSteps, wantSteps)
	}
	got := *o
	got.ID, got.StepsJSON, got.CreatedAt, got.UpdatedAt = 0, "", got.CreatedAt.Truncate(0), got.UpdatedAt.Truncate(0)
	want := orders.Order{
		EdgeUUID: "cir-1", StationID: testEnvelope().Src.Station, OrderType: OrderTypeComplex,
		Status: StatusPending, Quantity: 2, Priority: 3, PayloadCode: bp.Code, PayloadDesc: "the row's desc",
		SourceNode: storage.Name, DeliveryNode: line.Name, ProcessNode: line.Name, Coordinated: true,
		SiblingOrderUUID: "cir-sibling", KeyRoute: []string{"R1", "R2"}, KeyTask: "T1",
		OriginID: origin, OriginClass: protocol.OriginClassAttached,
		CreatedAt: got.CreatedAt, UpdatedAt: got.UpdatedAt,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the complex row\n got %+v\nwant %+v", got, want)
	}
}
