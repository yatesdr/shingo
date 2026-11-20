package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// order_census_sites_test.go — the census widened past "who writes an order row"
// to the other questions that need answering by the tree rather than by memory:
// who reaches a door because a PERSON pressed something, who asks the
// dropoff-capacity gate, who calls the fleet's CreateOrder without writing an
// order row at all, who writes the block record and appends to the fleet, and
// who builds an order's blocks.
//
// Each is pinned as a set of (file, enclosing function) sites, found by
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
		"www/handlers_orders.go:fileBinMove":                      "bin move: a named bin to a node (the Orders page's Retrieve Specific Bin, the bins page's Request move)",
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
		"dispatch/plan/capacity.go:DropoffCapacityAt":                 "the gate's in-flight arm (the cleanup phase folds the wrapper away)",
		"dispatch/plan/capacity.go:checkNGRPCapacity":                 "the NGRP arm: in-flight per child",
		"dispatch/plan/loader_window_choice.go:LoaderWindows":         "loader replenish: per window at decision time (the planner's since B3)",
		"dispatch/plan/move.go:moveDestination":                       "the bin move's destination verdict (B2), named when the census learned the planner's own name for the gate (B3)",
		"dispatch/capacity.go:CheckDropoffCapacity":                   "dispatch's face on the planner's gate",
		"dispatch/complex_dispatch.go:reserveComplexDestination":      "complex: the final dropoff and each intermediate one",
		"dispatch/loader_place.go:placeForContainment":                "quality containment: whether the containment node has room",
		"dispatch/place_by_contents.go:placeByContentsMove":           "the by-contents choice: whether the drop a move's carrier goes to by what it holds (a partial home, a kanban) has room (blocked keeps the authored drop)",
		"dispatch/loader_place.go:placeForDedicatedLoader":            "dedicated loader: the home it would place at",
		"dispatch/loader_place.go:tryPlaceFromHomeSource":             "dedicated loader: a home-sourced placement",
		"dispatch/loader_place.go:homeForPayload":                     "dedicated loader: choosing a home for a payload",
		"dispatch/loader_place.go:placeForLoader":                     "loader: choosing a buffer",
		"dispatch/loader_place.go:returnLiftFrom":                     "a swap's supply, before its pick: whether a spot that takes the returning carrier stands free (asks placement's own gate, so it moves exactly when placement's answer does)",
		"dispatch/planning_service.go:decideTransport":                "wire intake: the plain path's gate, asked before the row is born (B3)",
		"dispatch/preview.go:PreviewDropoffCapacity":                  "the read-only preview the UI shows",
		"dispatch/vacated_slot.go:noOtherInFlight":                    "the vacated-slot rule's in-flight half: another order delivering to the node, the asker excluded",
		"engine/cancel_return.go:storeIntoLoader":                     "cancel-return: whether a loader home has room for the carried bin's type",
		"dispatch/clear_home.go:ClearSpot":                            "home clear: whether a buffer spot is open for the home's carrier (the spot arm)",
		"fulfillment/scanner.go:fulfill":                              "the scanner's re-run before every dispatch (a pass's order, or AcquireOne's)",
		"fulfillment/scanner.go:overflowsWhileParked":                 "a parked push's group's overflow: whether it has room before the reserve is asked (R.39); its answer moves as the gate's does",
		"store/orders.go:CountInFlightOrdersByDeliveryNodeExcluding":  "the store wrapper",
		"store/orders/orders.go:CountInFlightByDeliveryNode":          "the count",
		"store/orders/orders.go:CountInFlightByDeliveryNodeExcluding": "the count, excluding the asker",
		"store/orders/orders.go:ListInFlightByDeliveryNodes":          "the bins row's inbound orders: the same set as rows, read only (it gates nothing; its answer is the row's inbound and moves exactly when the gate's count does)",
	}, "A new reader of the dropoff gate is a new site whose answer moves when holds move. "+
		"Name it here, and say in the phase's PREDICTIONS.md whether its answer changes.")
}

// TestCensus_FleetTransportCreateIsNotADoor names every call to a fleet
// backend's CreateOrder, and says why none of them is an order-creation door.
//
// The name collides with the order writer (db.CreateOrder), and the issue census
// (issueWalk) spells the store call with its receiver, so the collision cannot
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
		"dispatch/fleet_handover.go:handoverToFleet":         "the handover: an order row that already exists goes to the fleet — the one sanctioned call",
		"engine/cleaning.go:dispatchCleaning":                "the air-knife cleaning order: a pinned vendor transport with NO order row, recorded only in cleaning_missions — not a door, and invisible to every order reader by design",
		"fleet/seerrds/adapter.go:CreateOrder":               "the adapter's own implementation, calling the RDS client",
		"fleet/seerrds/adapter.go:executeOrderCommand":       "the adapter's command path, calling the RDS client",
		"internal/testdb/create_window_fleet.go:CreateOrder": "the create-window test fleet: a test double that runs the test inside a create, then answers through the tracking mock it embeds; no robot, no order row",
		"internal/testdb/create_window_fleet.go:take":        "the same test fleet recording a job it took on a create whose answer Core never hears",
		"www/handlers_robots.go:apiRobotMoveTo":              "a person drives a robot to a point: a vendor transport with NO order row — not a door, and invisible to every order reader by design",
	}, "A new caller of a fleet's CreateOrder either hands over an order row (route it through "+
		"handoverToFleet) or moves a robot with no order, which has to be said here.")
}

// TestCensus_TheBlockRecordIsWrittenWhereBlocksAreSent names every statement
// that writes orders.block_actions, and every caller of the ones that do: the
// record is written by the fleet's yes to the create and by the append's claim,
// from the blocks those two hand the fleet, and nowhere else. A new writer could
// record a block the fleet was never sent, or forget one it was.
func TestCensus_TheBlockRecordIsWrittenWhereBlocksAreSent(t *testing.T) {
	t.Parallel()
	sources := scanCoreSources(t)
	write := regexp.MustCompile(`block_actions\s*=|INSERT\s+INTO\s+orders\b[^;]*block_actions`)
	var writers []string
	fset := token.NewFileSet()
	for path, src := range sources {
		locs := write.FindAllStringIndex(src, -1)
		if len(locs) == 0 {
			continue
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, loc := range locs {
			owner := "<package>"
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok &&
					fset.Position(fd.Pos()).Offset <= loc[0] && loc[0] < fset.Position(fd.End()).Offset {
					owner = fd.Name.Name
				}
			}
			writers = append(writers, path+":"+owner)
		}
	}
	slices.Sort(writers)
	sameSites(t, "the statements that write orders.block_actions", writers, map[string]string{
		"store/orders/ask.go:FleetYesTx":     "the fleet's yes to the create, the blocks the create sent",
		"store/orders/orders.go:ClaimAppend": "the append's compare-and-set, merging the segment's blocks",
	}, "A new writer of block_actions records blocks somewhere other than where they are sent. "+
		"Record them in the fleet's yes (FleetYesTx) or the append (ClaimAppend).")

	callers := only(sources, "dispatch/", "engine/", "store/", "fleet/", "www/", "service/", "fulfillment/")
	sameSites(t, "the callers of the block record's writers",
		callSites(t, callers, "FleetYesTx", "FleetYes", "ClaimAppend"),
		map[string]string{
			"dispatch/complex_release.go:appendSegment": "the one fleet append, with the segment it sends",
			"dispatch/lifecycle.go:Dispatch":            "the fleet's yes, with the blocks the create sent (handoverToFleet's)",
			"store/orders.go:ClaimAppend":               "the store delegate",
			"store/orders.go:FleetYes":                  "the yes on a transaction of its own",
		}, "The block record is written from the blocks the fleet is handed, at the create and the append.")
}

// TestCensus_EveryFleetAppendIsTheClaimedAppend names every call to a fleet
// backend's ReleaseOrder. The block record is written only by the create and the
// claimed append, so an order-backed block reaching the fleet any other way
// would finish unrecorded.
func TestCensus_EveryFleetAppendIsTheClaimedAppend(t *testing.T) {
	t.Parallel()
	sources := scanCoreSources(t)
	appenders := map[string]string{}
	for p, src := range sources {
		if strings.Contains(src, ".ReleaseOrder(") {
			appenders[p] = src
		}
	}
	sameSites(t, "the fleet ReleaseOrder callers", callSites(t, appenders, "ReleaseOrder"), map[string]string{
		"dispatch/complex_release.go:appendSegment":       "the claimed append: its blocks are recorded by ClaimAppend",
		"dispatch/complex_dispatch.go:afterComplexCreate": "the no-wait complex create's mark-complete: no blocks",
	}, "A new caller of a fleet's ReleaseOrder hands the fleet blocks no record names. "+
		"Route it through appendSegmentAndAdvance.")
}

// TestCensus_OneBlockBuilder names the one builder of an order's blocks, the
// four sites that call it, and every block literal in Core. The builder
// (orderBlocks) is where a cart's family names its lift and wait keys; a site
// that built an order's blocks any other way would send the default key where
// a family names one, silently, the way a load sequence was once dropped by a
// path that did not know it. A block literal outside the builder is an
// orderless door, each named with why it carries no carrier key, or the RDS
// adapter copying a block already built.
func TestCensus_OneBlockBuilder(t *testing.T) {
	t.Parallel()
	sources := scanCoreSources(t)
	sameSites(t, "the callers of stepsToBlocks", callSites(t, sources, "stepsToBlocks"), map[string]string{
		"dispatch/carrier_keys_blocks.go:orderBlocks": "the one builder: each step's carrier key named, then the blocks built",
	}, "stepsToBlocks is called by the one builder only. Build an order's blocks with d.orderBlocks.")
	sameSites(t, "the callers of the block builder", callSites(t, sources, "orderBlocks"), map[string]string{
		"dispatch/dispatcher.go:dispatchToFleetCore":         "the sealed create: a plain order's whole plan, one fleet call",
		"dispatch/lane_gate_dispatch.go:buildUnsealedCreate": "the unsealed create: the pre-wait run of a gated, a turned or a home clear trade's plan",
		"dispatch/complex_dispatch.go:buildComplexCommit":    "the ungated complex create: the plan up to its first wait",
		"dispatch/complex_release.go:appendSegment":          "the append: every release's segment, through the claimed append",
	}, "A new site that sends an order's blocks calls d.orderBlocks with its run (createRun at a create, "+
		"d.appendRun at an append), so a family's keys reach it. Name it here with what it sends.")
	sameSites(t, "the fleet block literals", blockLiterals(t, sources), map[string]string{
		"dispatch/complex_steps.go:stepsToBlocks":      "the builder's blocks, each with the name its step was given",
		"engine/cleaning.go:dispatchCleaning":          "an orderless door: the air-knife cleaning mission drives points with no order row and no cart, so no binTask at all",
		"www/handlers_robots.go:apiRobotMoveTo":        "an orderless door: a person sends a robot to a point, no order row and no cart, so no binTask at all",
		"fleet/seerrds/adapter.go:executeOrderCommand": "an orderless door: the adapter's own command order (move, jack, unjack, charge) for one robot, no order row and no cart, no binTask",
		"fleet/seerrds/adapter.go:CreateOrder":         "the adapter copying a create's blocks onto the wire as the builder named them",
		"fleet/seerrds/adapter.go:ReleaseOrder":        "the adapter copying an append's blocks onto the wire as the builder named them",
	}, "A block built outside stepsToBlocks sends no carrier key. An order's blocks come from d.orderBlocks; "+
		"an orderless door (no order row, no cart) is named here with why it carries no key.")
}

// blockLiterals is "file:Func" for every composite literal of a fleet block
// (fleet.OrderBlock) or a wire block (rds.Block) in sources, a slice or array
// of them included (its elements may elide the type).
func blockLiterals(t *testing.T, sources map[string]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for path, src := range sources {
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		isBlock := func(x ast.Expr) bool {
			switch ty := x.(type) {
			case *ast.SelectorExpr:
				pkg := lastName(ty.X)
				return (pkg == "fleet" && ty.Sel.Name == "OrderBlock") || (pkg == "rds" && ty.Sel.Name == "Block")
			case *ast.Ident:
				return (f.Name.Name == "fleet" && ty.Name == "OrderBlock") || (f.Name.Name == "rds" && ty.Name == "Block")
			}
			return false
		}
		for _, d := range f.Decls {
			owner := "<package>"
			if fd, ok := d.(*ast.FuncDecl); ok {
				owner = fd.Name.Name
			}
			ast.Inspect(d, func(x ast.Node) bool {
				lit, ok := x.(*ast.CompositeLit)
				if !ok || lit.Type == nil {
					return true
				}
				ty := lit.Type
				if arr, ok := ty.(*ast.ArrayType); ok {
					ty = arr.Elt
				}
				if isBlock(ty) {
					seen[path+":"+owner] = true
				}
				return true
			})
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}
