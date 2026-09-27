package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// order_census_sites_test.go — the census widened past "who writes an order row"
// to the three other questions the unification brief needs answered by the tree
// rather than by memory: who reaches a door because a PERSON pressed something,
// who asks the dropoff-capacity gate, and who calls the fleet's CreateOrder
// without writing an order row at all.
//
// Each is pinned as a set of (file, enclosing function) call sites, found by
// parsing, not by grep — a grep counts a comment that names the call, and a
// line number moves with every edit above it.

// callSites returns "file:Func" for every call in sources whose selector or
// function name is one of names. A call outside any function (a package-level
// var initialiser) is reported under "file:<package>".
func callSites(t *testing.T, sources map[string]string, names ...string) []string {
	t.Helper()
	return callSitesExcept(t, sources, "", names...)
}

// callSitesExcept is callSites skipping calls whose receiver's last name is
// skipRecv (e.g. "db", so d.db.CreateOrder is not counted as a fleet call).
func callSitesExcept(t *testing.T, sources map[string]string, skipRecv string, names ...string) []string {
	t.Helper()
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for path, src := range sources {
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		visit := func(owner string, n ast.Node) {
			ast.Inspect(n, func(x ast.Node) bool {
				call, ok := x.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					name = fn.Sel.Name
					if skipRecv != "" && lastName(fn.X) == skipRecv {
						return true
					}
				case *ast.Ident:
					name = fn.Name
				}
				if want[name] {
					seen[path+":"+owner] = true
				}
				return true
			})
		}
		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				visit(decl.Name.Name, decl)
			default:
				visit("<package>", decl)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// lastName is the final identifier of a receiver expression: db for d.db, e.db, db.
func lastName(x ast.Expr) string {
	switch r := x.(type) {
	case *ast.Ident:
		return r.Name
	case *ast.SelectorExpr:
		return r.Sel.Name
	}
	return ""
}

// only keeps the sources under one of the given path prefixes.
func only(sources map[string]string, prefixes ...string) map[string]string {
	out := map[string]string{}
	for p, src := range sources {
		for _, pre := range prefixes {
			if strings.HasPrefix(p, pre) {
				out[p] = src
			}
		}
	}
	return out
}

// sameSites fails with both lists when the tree's sites differ from the pinned ones.
func sameSites(t *testing.T, what string, got []string, want map[string]string, advice string) {
	t.Helper()
	var wantKeys []string
	for k := range want {
		wantKeys = append(wantKeys, k)
	}
	slices.Sort(wantKeys)
	if !slices.Equal(got, wantKeys) {
		var missing, extra []string
		for _, w := range wantKeys {
			if !slices.Contains(got, w) {
				missing = append(missing, w)
			}
		}
		for _, g := range got {
			if _, ok := want[g]; !ok {
				extra = append(extra, g)
			}
		}
		t.Errorf("%s moved.\n  in the tree, not named: %v\n  named, not in the tree: %v\n%s",
			what, extra, missing, advice)
	}
}

// TestCensus_PersonDoors pins every Core UI handler through which a PERSON
// brings an order into existence, and which door each one reaches.
//
// The door census above names the doors by what they are; this names who can
// open them from Core's own pages. Every one of these reaches a door the census
// already counts — none writes a row of its own — and that is the property
// pinned: a new page that creates orders has to arrive here and say which door
// it is, so the planner phase inherits the complete caller list for each door.
//
// The test-order pages that PUBLISH onto the bus reach the wire intake door
// through messaging.CoreHandler, exactly as an Edge would; they are listed by
// the helper that publishes.
func TestCensus_PersonDoors(t *testing.T) {
	t.Parallel()
	www := only(scanCoreSources(t), "www/")
	got := callSites(t, www, "HandleOrderRequest", "HandleComplexOrderRequest", "CreateBinMove",
		"RecoverCarriedBin", "Publish")
	sameSites(t, "the Core UI's person doors", got, map[string]string{
		"www/handlers_orders.go:apiManualOrderSubmit":             "Edge wire intake: the manual order form (retrieve, retrieve_empty, move)",
		"www/handlers_orders.go:submitSpotComplexOrder":           "complex intake: a spot complex order",
		"www/handlers_orders.go:submitSpotRetrieveSpecific":       "bin move: retrieve a named bin to a node",
		"www/handlers_test_orders_direct.go:apiDirectOrderSubmit": "bin move: the /test-orders direct form",
		"www/handlers_test_orders_direct.go:dispatchComplex":      "complex intake: the /test-orders direct swap forms",
		"www/handlers_test_orders_kafka.go:apiTestOrderSubmit":    "Edge wire intake, over the bus: the /test-orders Kafka form",
		"www/handlers_test_orders_kafka.go:publishComplex":        "complex intake, over the bus: the /test-orders Kafka swap forms",
		"www/handlers_diagnostics.go:apiRepairAnomaly":            "carried-bin recovery: the Recover button on an anomaly",
		"www/handlers_test_orders_kafka.go:apiTestOrderCancel":    "NOT a door: publishes a cancel for an order that exists",
		"www/handlers_test_orders_kafka.go:apiTestOrderReceipt":   "NOT a door: publishes a receipt for an order that exists",
	}, "A page that creates orders is a person door. Name it here with the door it reaches, "+
		"or route it through one that is already named.")
}

// TestCensus_DropoffCapacityGateSites pins every place that asks the
// dropoff-capacity gate, directly or through its in-flight count.
//
// The gate's count reads HOLDERS, not statuses (orders.InFlightForDropoffSQL:
// an order counts when it hard-claims a bin bound for the node). So every site
// below changes behaviour the moment what an order holds while it waits changes
// — which is exactly what the one-step acquire does. The list is the blast
// radius of that ruling, written down before it lands.
//
// The typed door (CheckDropoffCapacityForType) is the gate's body, in
// dispatch/plan since B2 (plan.DropoffCapacityForType); its one caller outside
// the gate is the cancel-return's loader chooser, which knows the carried bin's
// type.
func TestCensus_DropoffCapacityGateSites(t *testing.T) {
	t.Parallel()
	got := callSites(t, scanCoreSources(t), "CheckDropoffCapacity", "DropoffCapacity", "DropoffCapacityForType", "CheckDropoffCapacityForType",
		"CountInFlightOrdersByDeliveryNodeExcluding", "CountInFlightByDeliveryNode",
		"CountInFlightByDeliveryNodeExcluding", "InFlightForDropoffSQL")
	sameSites(t, "the dropoff-capacity gate's callers", got, map[string]string{
		"dispatch/plan/capacity.go:DropoffCapacity":                   "the gate itself, through its typed form (the planner's since B2; dispatch's CheckDropoffCapacity is its face)",
		"dispatch/capacity.go:CheckDropoffCapacityForType":            "the typed door's face, for the cancel-return chooser",
		"dispatch/plan/capacity.go:DropoffCapacityForType":            "the gate's in-flight arm (the cleanup phase folds the wrapper away)",
		"dispatch/plan/capacity.go:checkNGRPCapacity":                 "the NGRP arm: in-flight per child",
		"dispatch/plan/loader_window_choice.go:LoaderWindows":         "loader replenish: per window at decision time (the planner's since B3)",
		"dispatch/plan/move.go:moveDestination":                       "the bin move's destination verdict (B2), named when the census learned the planner's own name for the gate (B3)",
		"dispatch/capacity.go:CheckDropoffCapacity":                   "dispatch's face on the planner's gate",
		"dispatch/complex_dispatch.go:reserveComplexDestination":      "complex: the final dropoff and each intermediate one",
		"dispatch/loader_place.go:placeForContainment":                "quality containment: whether the containment node has room",
		"dispatch/loader_place.go:placeForDedicatedLoader":            "dedicated loader: the home it would place at",
		"dispatch/loader_place.go:tryPlaceFromHomeSource":             "dedicated loader: a home-sourced placement",
		"dispatch/loader_place.go:homeForPayload":                     "dedicated loader: choosing a home for a payload",
		"dispatch/loader_place.go:placeForLoader":                     "loader: choosing a buffer",
		"dispatch/planning_service.go:decideTransport":                "wire intake: the plain path's gate, asked before the row is born (B3)",
		"dispatch/preview.go:PreviewDropoffCapacity":                  "the read-only preview the UI shows",
		"dispatch/vacated_slot.go:noOtherInFlight":                    "the vacated-slot rule's in-flight half: another order delivering to the node, the asker excluded",
		"engine/cancel_return.go:storeIntoLoader":                     "cancel-return: whether a loader home has room for the carried bin's type",
		"fulfillment/scanner.go:fulfill":                              "the scanner's re-run before every dispatch (a pass's order, or AcquireOne's)",
		"store/orders.go:CountInFlightOrdersByDeliveryNodeExcluding":  "the store wrapper",
		"store/orders/orders.go:CountInFlightByDeliveryNode":          "the count",
		"store/orders/orders.go:CountInFlightByDeliveryNodeExcluding": "the count, excluding the asker",
	}, "A new reader of the dropoff gate is a new site whose answer moves when holds move. "+
		"Name it here, and say in the phase's PREDICTIONS.md whether its answer changes.")
}

// TestCensus_FleetTransportCreateIsNotADoor names every call to a fleet
// backend's CreateOrder, and says why none of them is an order-creation door.
//
// The name collides with the order writer (db.CreateOrder), and writerSymbols
// deliberately spells the store call with its receiver so the collision cannot
// count a vendor transport as an order row. This is the other half: the
// transport calls are named, so "not a door" is a stated judgement.
func TestCensus_FleetTransportCreateIsNotADoor(t *testing.T) {
	t.Parallel()
	sources := scanCoreSources(t)
	var fleetCallers = map[string]string{}
	for p, src := range sources {
		if strings.Contains(src, ".CreateOrder(") {
			fleetCallers[p] = src
		}
	}
	got := callSitesExcept(t, fleetCallers, "db", "CreateOrder")
	sameSites(t, "the fleet CreateOrder callers", got, map[string]string{
		"dispatch/fleet_handover.go:handoverToFleet":   "the handover: an order row that already exists goes to the fleet — the one sanctioned call",
		"fleet/seerrds/adapter.go:CreateOrder":         "the adapter's own implementation, calling the RDS client",
		"fleet/seerrds/adapter.go:executeOrderCommand": "the adapter's command path, calling the RDS client",
		"www/handlers_robots.go:apiRobotMoveTo":        "a person drives a robot to a point: a vendor transport with NO order row — not a door, and invisible to every order reader by design",
	}, "A new caller of a fleet's CreateOrder either hands over an order row (route it through "+
		"handoverToFleet) or moves a robot with no order, which has to be said here.")
}
