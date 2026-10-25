package engine

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingo/shared/comingvectors"
	"shingoedge/domain"
	"shingoedge/release"
	"shingoedge/store"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

var updateComingVectors = flag.Bool("update-coming-vectors", false,
	"rewrite shared/comingvectors/vectors.json from the step builders")

// comingVectorsPath is the shared file, from this package's directory.
var comingVectorsPath = filepath.Join("..", "..", "shared", "comingvectors", "vectors.json")

// buildComingVectors is every leg shape the builders write, steady state and
// changeover, for every swap mode, both roles, flipped and unflipped, two and
// three positions where the mode has them, plus the plain orders; each with
// protocol.OrderPlacesBinAt's answer at every node it names. Then the two
// stored shapes the type decides (a plain order carrying a plan, a complex row
// with no steps stored).
func buildComingVectors(t *testing.T) []comingvectors.Case {
	t.Helper()
	var out []comingvectors.Case
	add := func(name string, orderType protocol.OrderType, steps []protocol.ComplexOrderStep, delivery string) {
		if orderType == protocol.OrderTypeComplex && len(steps) == 0 {
			return
		}
		places := map[string]bool{}
		for _, s := range steps {
			if s.Node != "" {
				places[s.Node] = protocol.OrderPlacesBinAt(orderType, steps, delivery, s.Node)
			}
		}
		if delivery != "" {
			places[delivery] = protocol.OrderPlacesBinAt(orderType, steps, delivery, delivery)
		}
		var raw json.RawMessage
		if len(steps) > 0 {
			var err error
			if raw, err = json.Marshal(steps); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		out = append(out, comingvectors.Case{Name: name, OrderType: string(orderType), Steps: raw,
			DeliveryNode: delivery, Places: places})
	}
	node := &processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE"}
	for _, mode := range []protocol.SwapMode{protocol.SwapModeSingleRobot, protocol.SwapModeTwoRobot,
		protocol.SwapModeTwoRobotPressIndex, protocol.SwapModeSequential} {
		for _, role := range []protocol.ClaimRole{protocol.ClaimRoleProduce, protocol.ClaimRoleConsume} {
			for _, flipped := range []bool{false, true} {
				for _, second := range []string{"", "C"} {
					press := mode == protocol.SwapModeTwoRobotPressIndex
					if !press && (flipped || second != "") {
						continue
					}
					claim := &processes.NodeClaim{CoreNodeName: "LINE", SwapMode: mode, Role: role,
						IndexRobotSupplies: flipped, InboundSource: "IN", InboundStaging: "IN-STG",
						OutboundStaging: "OUT-STG", OutboundDestination: "OUT", PayloadCode: "PART"}
					if mode != protocol.SwapModeSingleRobot {
						claim.PairedCoreNode, claim.SecondPairedCoreNode = "B", second
					}
					to := *claim
					to.PayloadCode = "NEXT"
					name := string(mode) + "/" + string(role)
					if flipped {
						name += "/flipped"
					}
					if second != "" {
						name += "/3-position"
					}
					d, err := buildSwapDispatch(node, claim)
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if d != nil {
						add(name+"/steady/A", protocol.OrderTypeComplex, d.StepsA, "")
						add(name+"/steady/B", protocol.OrderTypeComplex, d.StepsB, "")
					}
					c := BuildSwapChangeoverSteps(claim, &to, "B", "LINE")
					if c.Roles != nil {
						add(name+"/changeover/evac", protocol.OrderTypeComplex, c.Roles.evac.steps, "")
						add(name+"/changeover/supply", protocol.OrderTypeComplex, c.Roles.supply.steps, "")
					} else {
						add(name+"/changeover/A", protocol.OrderTypeComplex, c.StepsA, "")
						add(name+"/changeover/B", protocol.OrderTypeComplex, c.StepsB, "")
					}
				}
			}
		}
	}
	add("plain/retrieve to the on-deck", protocol.OrderTypeRetrieve, nil, "B")
	add("plain/retrieve_empty to the on-deck", protocol.OrderTypeRetrieveEmpty, nil, "B")
	add("plain/move off the on-deck to the outbound", protocol.OrderTypeMove, nil, "OUT")
	add("plain/store to the outbound", protocol.OrderTypeStore, nil, "OUT")
	// The type decides, not whether steps are present. A plain order carrying
	// the transport plan Core persists at dispatch still places at its delivery
	// node alone; and a complex row whose steps were never stored (Core's
	// reconcile projects an order down without them) is a leg whose shape
	// cannot be established, so it reads as placing at every node asked, its
	// display delivery node and a node it never names alike (the caller's
	// rule, protocol/places_bin_at.go), not as a plain order at its delivery.
	add("plain/retrieve to the on-deck, carrying Core's plan", protocol.OrderTypeRetrieve,
		[]protocol.ComplexOrderStep{{Action: protocol.ActionPickup, Node: "IN"}, {Action: protocol.ActionDropoff, Node: "B"}}, "B")
	out = append(out, comingvectors.Case{Name: "complex/steps never stored (a row Core projected)",
		OrderType: string(protocol.OrderTypeComplex), DeliveryNode: "LINE", Places: map[string]bool{"LINE": true, "B": true}})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TestComingVectors_AreTheBuildersShapes is the Edge half of the "coming"
// agreement: the shared file is exactly what the builders write today. Core's
// docker test reads the same file over stored rows.
func TestComingVectors_AreTheBuildersShapes(t *testing.T) {
	t.Parallel()
	got, err := json.MarshalIndent(buildComingVectors(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *updateComingVectors {
		if err := os.WriteFile(comingVectorsPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(got, comingvectors.Raw()) {
		t.Fatalf("shared/comingvectors/vectors.json is stale against the step builders. Regenerate it:\n" +
			"  cd shingo-edge && go test ./engine/ -run TestComingVectors_AreTheBuildersShapes -update-coming-vectors\n" +
			"and run Core's TestComingVectors_CoreAgreesOverStoredRows (docker) on the new file.")
	}
}

// TestComingVectors_TheEdgesReadersAgree is the Edge's readers over the shared
// file: every case, stored as the Edge stores its row (storeComingVector), is
// answered at every node it names as the case says, by each of the three reads
// that ask "is a carrier coming to X" here: the live-placing SQL (store
// PlacingAt: REQUEST's dependent positions, the produce hold), the SEND BACK
// line's placing fact (domain, over rows in hand) and the changeover gates'
// orderPlacesBinAtAny (over the stored steps).
func TestComingVectors_TheEdgesReadersAgree(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := &Engine{db: db}
	cases, err := comingvectors.Cases()
	testutil.MustNoErr(t, err, "the vectors")
	for i, c := range cases {
		row := storeComingVector(t, db, i, c)
		for node, want := range c.Places {
			placing, err := db.LiveOrderPlacing(node)
			testutil.MustNoErr(t, err, c.Name+" at "+node)
			wantPlacing := orders.PlacingNone
			if want {
				wantPlacing = orders.PlacingCarrier
			}
			if placing[node] != wantPlacing {
				t.Errorf("%s: PlacingAt %s = %v, the vectors say placing %v", c.Name, node, placing[node], want)
			}
			line := domain.SendBackFacts{Paired: node}
			line.OrderFacts([]orders.Order{*row}, nil)
			if line.PlacingAtOutside != want {
				t.Errorf("%s: SEND BACK's placing at %s = %v, the vectors say %v", c.Name, node, line.PlacingAtOutside, want)
			}
			if got := e.orderPlacesBinAtAny(row, []string{node}); got != want {
				t.Errorf("%s: orderPlacesBinAtAny %s = %v, the vectors say %v", c.Name, node, got, want)
			}
		}
		testutil.MustNoErr(t, db.UpdateOrderStatus(row.ID, string(protocol.StatusCancelled)), "retire "+c.Name)
	}
}

// storeComingVector writes a case's row as the Edge writes one, live: a
// complex leg as its create does (its steps and the release facts computed
// from them, orders/manager_create.go); a complex row with no steps as Core's
// projection lands it (UpsertProjection stores none); a plain order as its
// create does, with the plan Core persists when the case carries one (no Edge
// door writes a plain row's steps; the store's writer does).
func storeComingVector(t *testing.T, db *store.DB, i int, c comingvectors.Case) *orders.Order {
	t.Helper()
	typ, uuid := protocol.OrderType(c.OrderType), fmt.Sprintf("cv-%d", i)
	if typ == protocol.OrderTypeComplex && len(c.Steps) == 0 {
		_, err := orders.UpsertProjection(db.DB, orders.ProjectionRow{UUID: uuid, OrderType: typ,
			Status: string(protocol.StatusDispatched), DeliveryNode: c.DeliveryNode})
		testutil.MustNoErr(t, err, c.Name)
		row, err := db.GetOrderByUUID(uuid)
		testutil.MustNoErr(t, err, c.Name)
		return row
	}
	id, err := db.CreateOrder(uuid, typ, nil, typ == protocol.OrderTypeRetrieveEmpty, 1, c.DeliveryNode,
		"", "", protocol.Height{}, false, "", "", "")
	testutil.MustNoErr(t, err, c.Name)
	switch {
	case typ == protocol.OrderTypeComplex:
		var steps []protocol.ComplexOrderStep
		testutil.MustNoErr(t, json.Unmarshal(c.Steps, &steps), c.Name)
		facts, err := release.FactsFromSteps(steps, "LINE").Encode()
		testutil.MustNoErr(t, err, c.Name)
		testutil.MustNoErr(t, db.UpdateOrderStepsAndFacts(id, string(c.Steps), facts), c.Name)
	case len(c.Steps) > 0:
		testutil.MustNoErr(t, db.UpdateOrderStepsJSON(id, string(c.Steps)), c.Name)
	}
	row, err := db.GetOrder(id)
	testutil.MustNoErr(t, err, c.Name)
	return row
}
