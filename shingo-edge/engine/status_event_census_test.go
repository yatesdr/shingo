package engine

// status_event_census_test.go — who emits, and who hears, the four
// status-derived events: EventOrderStatusChanged, EventOrderDelivered,
// EventOrderCompleted, EventOrderFailed.
//
// They fire whenever an order row's status moves, whoever moved it: a Core
// message, the reconcile, the RELEASE tap, a rollback, the mirror catching up
// (status_mirror_pin_test.go). Work hung on one of them runs on every such
// move, including a reconcile moving a confirmed row back and a rollback to
// staged. So every reference to them in the Edge's tree is listed here, read
// with go/ast from every non-test file in the module (build tags ignored:
// sim_operator.go is -tags sim), attributed to the function it sits in. A new
// one fails until it is argued; a listed one that went away fails until its row
// is removed.
//
// Each reference reads as how it hears the event:
//   - subscribe(handlers): an argument of a Subscribe* call; the handlers are
//     the function passed, or the calls a closure passed makes;
//   - case: a case of a switch on the event's type (a subscriber of every
//     event, filtering);
//   - payload: a type assertion or type-switch case on the event's payload
//     type, the other way a subscriber of every event picks one out;
//   - read: any other use of the constant.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

var statusDerivedEvents = map[string]bool{
	"EventOrderStatusChanged": true, "EventOrderDelivered": true, "EventOrderCompleted": true, "EventOrderFailed": true,
}

// statusDerivedPayloads maps each event's payload type to the event.
var statusDerivedPayloads = map[string]string{
	"OrderStatusChangedEvent": "EventOrderStatusChanged", "OrderDeliveredEvent": "EventOrderDelivered",
	"OrderCompletedEvent": "EventOrderCompleted", "OrderFailedEvent": "EventOrderFailed",
}

// statusDerivedEmitterMethods are orders.EventEmitter's methods that put one of
// the four on the bus (orderEmitter, adapters.go).
var statusDerivedEmitterMethods = map[string]bool{
	"EmitOrderStatusChanged": true, "EmitOrderDelivered": true, "EmitOrderDeliveredFallback": true,
	"EmitOrderCompleted": true, "EmitOrderFailed": true,
}

// statusEventSubscribers is every site that hears a status-derived event, at
// this tree: "<file>:<func> <event>" -> how.
var statusEventSubscribers = map[string]string{
	// the engine's handlers (wiring.go)
	// The terminal wake rides Completed: it fires once, at the edge where the
	// order ended, for a versioned order (the applier) and an older Core's
	// (the lifecycle's transition) alike.
	"engine/wiring.go:wireEventHandlers EventOrderCompleted": "subscribe(e.handleNodeOrderCompleted e.onTerminalForIntents)",
	"engine/wiring.go:wireEventHandlers EventOrderDelivered": "subscribe(e.handleNodeOrderDelivered)",
	"engine/wiring.go:wireEventHandlers EventOrderFailed":    "subscribe(e.handleNodeOrderFailed)",
	// An older Core's order only: each declines a versioned one, whose
	// follow-ons hang on order.delivered and order.cancelled
	// (engine/order_follow_ons.go).
	"engine/wiring.go:wireEventHandlers EventOrderStatusChanged": "subscribe(e.createHeldPullingLeg e.returnCarrierLeftWaiting)",
	// the sim operator (-tags sim)
	"engine/sim_operator.go:StartSimOperator EventOrderDelivered":     "subscribe(op.onDelivered)",
	"engine/sim_operator.go:StartSimOperator EventOrderStatusChanged": "subscribe(op.onStatusChanged)",
	"engine/sim_operator.go:onDelivered EventOrderDelivered":          "payload",
	"engine/sim_operator.go:onStatusChanged EventOrderStatusChanged":  "payload",
	// the robot panel's stream
	"www/robot_stream.go:start EventOrderStatusChanged":      "subscribe(rs.orderEvent)",
	"www/robot_stream.go:orderEvent EventOrderStatusChanged": "payload",
	// the boards' SSE broadcast, a subscriber of every event
	"www/sse.go:SetupEngineListeners EventOrderCompleted":     "case; payload",
	"www/sse.go:SetupEngineListeners EventOrderFailed":        "case; payload",
	"www/sse.go:SetupEngineListeners EventOrderStatusChanged": "case; payload",
}

// statusEventEmitters is every site that emits a status-derived event: the bus's
// Event literals, all in orderEmitter, and the order manager's calls of them.
// Two funnels, one per kind of order: the lifecycle's transition for an order
// whose status is not Core's versioned word (an older Core's, the Edge's own
// abort and confirm), and the versioned applier for one whose is
// (orders/core_word.go: a status Core told, applied once, newer only; and the
// delivery's follow-ons, once both halves of it are in). Plus the delivered
// fallback (an order.delivered for an order with no Edge row).
var statusEventEmitters = map[string]string{
	"engine/adapters.go:EmitOrderCompleted EventOrderCompleted":               "Event literal",
	"engine/adapters.go:EmitOrderDelivered EventOrderDelivered":               "Event literal",
	"engine/adapters.go:EmitOrderDeliveredFallback EventOrderDelivered":       "Event literal",
	"engine/adapters.go:EmitOrderFailed EventOrderFailed":                     "Event literal",
	"engine/adapters.go:EmitOrderStatusChanged EventOrderStatusChanged":       "Event literal",
	"orders/lifecycle_service.go:applyTransition EmitOrderCompleted":          "order emitter call",
	"orders/lifecycle_service.go:applyTransition EmitOrderDelivered":          "order emitter call",
	"orders/lifecycle_service.go:applyTransition EmitOrderFailed":             "order emitter call",
	"orders/lifecycle_service.go:applyTransition EmitOrderStatusChanged":      "order emitter call",
	"orders/core_word.go:coreMoved EmitOrderCompleted":                        "order emitter call",
	"orders/core_word.go:coreMoved EmitOrderFailed":                           "order emitter call",
	"orders/core_word.go:coreMoved EmitOrderStatusChanged":                    "order emitter call",
	"orders/core_word.go:deliveredFollowOns EmitOrderDelivered":               "order emitter call",
	"orders/manager_status.go:handleDeliveredWire EmitOrderDeliveredFallback": "order emitter call",
}

// TestStatusEventCensus_Subscribers pins every subscriber of a status-derived
// event. Expected to change only by argument: the build that makes one
// versioned applier the emitter adds no subscriber.
func TestStatusEventCensus_Subscribers(t *testing.T) {
	t.Parallel()
	subs, _ := statusEventSites(t)
	compareStatusEventSites(t, subs, statusEventSubscribers,
		"A new subscriber of a status-derived event must be argued: durable work hangs on the message that "+
			"carries its cause, not on a status change. The row's status also moves on a reconcile (which can "+
			"move a confirmed row back), a rollback to staged, a mirror catch-up and the RELEASE tap, and the "+
			"event fires for each. Argue it at its row here, or hang the work on the message.")
}

// TestStatusEventCensus_Emitters pins where the four are emitted: one funnel,
// orders.LifecycleService applyTransition, through orderEmitter, plus the
// delivered fallback. Expected to flip when the versioned applier becomes the
// emitter: its site replaces (or joins) applyTransition's rows.
func TestStatusEventCensus_Emitters(t *testing.T) {
	t.Parallel()
	_, emits := statusEventSites(t)
	compareStatusEventSites(t, emits, statusEventEmitters,
		"The status-derived events have one funnel. A new emitter is a second one: every subscriber above "+
			"runs on it, and the order's history and status no longer say why.")
}

// statusEventSites reads the Edge module's non-test source for references to
// the four events, split into what hears them and what emits them.
func statusEventSites(t *testing.T) (subs, emits map[string][]string) {
	t.Helper()
	var files []string
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != ".." && (n == "node_modules" || n == "testdata" || strings.HasPrefix(n, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk shingo-edge: %v", err)
	}
	sort.Strings(files)
	subs, emits = map[string][]string{}, map[string][]string{}
	add := func(m map[string][]string, key, how string) {
		if !slices.Contains(m[key], how) {
			m[key] = append(m[key], how)
			slices.Sort(m[key])
		}
	}
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rel, err := filepath.Rel("..", path)
		if err != nil {
			t.Fatalf("relative path of %s: %v", path, err)
		}
		file := filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			site := file + ":" + fn.Name.Name
			var stack []ast.Node
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				parents := stack
				stack = append(stack, n)
				switch x := n.(type) {
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && statusDerivedEmitterMethods[sel.Sel.Name] {
						add(emits, site+" "+sel.Sel.Name, "order emitter call")
					}
				case *ast.Ident:
					// A qualified name (engine.EventOrderFailed) is its selector.
					var operand ast.Node = x
					if p, ok := last(parents).(*ast.SelectorExpr); ok && p.Sel == x {
						operand, parents = p, parents[:len(parents)-1]
					}
					if statusDerivedEvents[x.Name] {
						if how, emit := constantUse(operand, parents); emit {
							add(emits, site+" "+x.Name, how)
						} else {
							add(subs, site+" "+x.Name, how)
						}
					} else if ev, ok := statusDerivedPayloads[x.Name]; ok && payloadPick(operand, parents) {
						add(subs, site+" "+ev, "payload")
					}
				}
				return true
			})
		}
	}
	return subs, emits
}

// constantUse says how a reference to an event constant uses it, and whether
// that is an emit (the Type of an Event literal).
func constantUse(operand ast.Node, parents []ast.Node) (how string, emit bool) {
	switch p := last(parents).(type) {
	case *ast.CallExpr:
		if strings.HasPrefix(calleeName(p.Fun), "Subscribe") {
			return "subscribe(" + subscribedHandler(p) + ")", false
		}
	case *ast.CaseClause:
		return "case", false
	case *ast.KeyValueExpr:
		if k, ok := p.Key.(*ast.Ident); ok && k.Name == "Type" && p.Value == operand {
			return "Event literal", true
		}
	}
	return "read", false
}

// subscribedHandler renders the function a Subscribe* call registers: its
// name, or, for a closure, the calls it makes. eventbus.SubscribeTyped takes the
// bus first; the bus's own methods take the handler first.
func subscribedHandler(call *ast.CallExpr) string {
	i := 0
	if calleeName(call.Fun) == "SubscribeTyped" {
		i = 1
	}
	if i >= len(call.Args) {
		return "?"
	}
	lit, ok := call.Args[i].(*ast.FuncLit)
	if !ok {
		return types.ExprString(call.Args[i])
	}
	var calls []string
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if name := types.ExprString(c.Fun); !slices.Contains(calls, name) {
				calls = append(calls, name)
			}
		}
		return true
	})
	slices.Sort(calls)
	return strings.Join(calls, " ")
}

// payloadPick reports whether a payload type is being picked out of an event: a
// type assertion to it, or a type-switch case on it. Its other uses (a
// parameter, a literal, a generic's argument) hear nothing.
func payloadPick(operand ast.Node, parents []ast.Node) bool {
	switch p := last(parents).(type) {
	case *ast.TypeAssertExpr:
		return p.Type == operand
	case *ast.CaseClause:
		// CaseClause -> BlockStmt -> the switch.
		if len(parents) >= 3 {
			_, ok := parents[len(parents)-3].(*ast.TypeSwitchStmt)
			return ok
		}
	}
	return false
}

func last(nodes []ast.Node) ast.Node {
	if len(nodes) == 0 {
		return nil
	}
	return nodes[len(nodes)-1]
}

// compareStatusEventSites compares the sites found against the table, both
// ways, and says why the table is there when they differ.
func compareStatusEventSites(t *testing.T, found map[string][]string, table map[string]string, why string) {
	t.Helper()
	var diffs []string
	for site, hows := range found {
		got := strings.Join(hows, "; ")
		switch want, ok := table[site]; {
		case !ok:
			diffs = append(diffs, "in the tree, not in the census: "+site+" -> "+got)
		case want != got:
			diffs = append(diffs, "changed: "+site+" -> "+got+" (census: "+want+")")
		}
	}
	for site, want := range table {
		if _, ok := found[site]; !ok {
			diffs = append(diffs, "in the census, no longer in the tree (remove the row): "+site+" -> "+want)
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Errorf("%s\n  %s", why, strings.Join(diffs, "\n  "))
	}
}
