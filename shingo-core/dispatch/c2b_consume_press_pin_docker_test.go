//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/payloads"
)

// c2b_consume_press_pin_docker_test.go — C2b A.6, Q4: a dropoff step with no
// payload, naming a group, resolved against the ORDER's payload
// (plan.StepPayload), not against what the carrier it puts down holds. It now
// asks with what the carrier holds (plan.DropPayload): nothing, for an empty.
//
// Source: DESIGN-C2b.md §7 A.6(b); RULINGS-C2b-P0-2026-10-06.md §3 Q4 (build the
// one leak only if a pin reproduces it; otherwise close the item with the pin's
// name and why).
//
// The shape: a swap stamped with payload X lifts an EMPTY carrier off the press,
// sets it down in a group, fetches a full and delivers it. The group's one lane
// declares payload Y (node_payloads). An empty may be stored anywhere
// (payloadAllowedAt passes an empty payload), but the drop asks with X, so the
// lane is refused.
//
// The pins round parked this draft on a nil-resolver dispatcher, which cannot
// resolve a group at all; it runs with the real resolver.
//
// MUTATION: make plan.DropPayload return the order's payload for an empty; the
// restricted case waits under ngrp-resolve.
func TestC2bPin_ANoPayloadDropOfAnEmptyIsNotFencedByTheOrdersPayload(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		restricted bool
	}{
		{"control: the lane declares nothing", false},
		{"the lane declares another payload", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			sd := testdb.SetupStandardData(t, db)
			d, _ := newTestDispatcherWithResolver(t, db)
			prefix := "C2BQ4A"
			if tc.restricted {
				prefix = "C2BQ4B"
			}
			_, lane, _, shuffle, bp := setupNodeGroupWithShuffle(t, db)
			grp := "GRP-TEST"
			if tc.restricted {
				other := &payloads.Payload{Code: prefix + "-OTHER"}
				testutil.MustNoErr(t, db.CreatePayload(other), "the other payload")
				for _, n := range append(shuffle, lane) {
					testutil.MustNoErr(t, db.SetNodePayloads(n.ID, []int64{other.ID}), "the group's children declare it")
				}
			}
			press, market := lineNode(t, db, prefix+"-PRESS"), lineNode(t, db, prefix+"-MARKET")
			empty := &bins.Bin{BinTypeID: sd.BinType.ID, Label: prefix + "-EMPTY", NodeID: &press.ID, Status: "staged"}
			testutil.MustNoErr(t, db.CreateBin(empty), "the empty carrier on the press")
			prResident(t, db, market, sd.BinType.ID, bp.Code, prefix+"-FULL")

			uuid := "c2bq4-" + map[bool]string{false: "a", true: "b"}[tc.restricted]
			d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
				OrderUUID: uuid, PayloadCode: bp.Code, Quantity: 1, ProcessNode: press.Name,
				Steps: []protocol.ComplexOrderStep{
					{Action: protocol.ActionPickup, Node: press.Name, Empty: true}, prDrop(grp),
					prPick(market.Name), prDrop(press.Name),
				},
			})
			prScanPass(t, d, db, uuid)

			o := prReloadUUID(t, db, uuid)
			var steps []resolvedStep
			testutil.MustNoErr(t, json.Unmarshal([]byte(o.StepsJSON), &steps), "read the plan")
			resolved := len(steps) > 1 && steps[1].Node != grp
			if !tc.restricted {
				if !resolved {
					t.Fatalf("fixture: with nothing declared the group drop did not resolve (%s, %q, %q) — "+
						"the pin no longer reaches the payload question", o.Status, o.QueueCause, o.ErrorDetail)
				}
				return
			}
			var red []string
			if !resolved {
				red = append(red, fmt.Sprintf("the empty's drop into %s was refused by a lane declaring another "+
					"payload: order %d is %s under %q", grp, o.ID, o.Status, o.QueueCause))
			}
			if len(red) != 0 {
				t.Fatalf("%v", red)
			}
		})
	}
}
