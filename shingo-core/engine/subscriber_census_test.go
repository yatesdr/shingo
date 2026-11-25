package engine

// subscriber_census_test.go — every subscription on Core's event bus, and the
// order they are made in.
//
// The bus calls its subscribers in the order they subscribed, on the emitter's
// goroutine (protocol/eventbus: "Subscribers are called in registration order
// on the emitting goroutine"). So where a subscribe call sits is behaviour: a
// subscriber finds done whatever the ones before it did, and nothing else says
// which those are. Three sets subscribe, each in its own package:
//
//   - the engine's own reactions: wireEventHandlers, called from Start, and the
//     two sets it calls (the cancel-return notices first, the lane gate after
//     every handler that releases a mouth row);
//   - main's, for messaging: the bins feed, after Start;
//   - the web layer's: SetupEngineListeners, from www.NewRouter, last.
//
// The census reads two things with go/ast from every non-test file of the
// module (build tags ignored) but the test harness's (testharness/: a rig's
// copy of main's composition, held to main by TestNewCore_MakesWhatMainMakes):
// each function's subscriptions in source order, and where each such function
// is called. Together they are the order the
// subscriptions are made in, so a subscription that is added, removed or moved,
// and a call that moves, fails here and says what to change.
//
// What it cannot see: a subscription made through a wrapper not named
// Subscribe, SubscribeTypes or SubscribeTyped (none exists); which of a
// function's subscriptions a given engine makes (16 sit behind three nil
// checks: the lane gate's 12 behind the dispatcher's, two behind the threshold
// monitor's, two behind the sourceability monitor's); what a handler does. The
// order the subscribers run in on a real engine is
// TestBusOrder_ADeliveryAndACancel (cmd/shingocore).

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// busSite is one function that subscribes to the bus or calls one that does,
// with its rows in source order. A row is one of:
//
//	"EventOrderFailed e.dispatcher.HandleChildOrderFailure"
//	    a subscription: its event, then one call its handler makes that names
//	    it (the function itself, when a function is passed). Two subscriptions
//	    of one event in one function must be named by calls the other does not
//	    make, so a swap of the two fails.
//	"-> e.wireLaneGateHandlers"
//	    a call of a function that subscribes, at the place it is made.
type busSite struct {
	site string // "<file>:<func>", the file from the module's root
	rows []string
}

// busRoot is where the order starts: the plant binary's composition root.
const busRoot = "cmd/shingocore/main.go:main"

// coreBusSites is the census, in the order the functions run.
var coreBusSites = []busSite{
	// The composition root makes the three sets in this order: the engine's at
	// Start, then the bins feed, then the web layer's when the router is built.
	{busRoot, []string{
		"-> eng.Start",
		"-> eng.SubscribeBinsFeed",
		"-> www.NewRouter",
	}},
	{"engine/engine_lifecycle.go:Start", []string{
		"-> e.wireEventHandlers",
	}},
	// The engine's own reactions.
	{"engine/wiring.go:wireEventHandlers", []string{
		"-> e.wireCancelReturnNotices",
		"EventOrderDispatched e.tracker.Track",
		"EventOrderStatusChanged e.handleVendorStatusChange",
		"EventOrderStatusChanged e.recordMissionEvent",
		"EventOrderRobotChanged e.handleVendorRobotChange",
		"EventOrderFailed e.dispatcher.HandleChildOrderFailure",
		"EventOrderSkipped e.dispatcher.HandleSwapPeerTerminal",
		"EventOrderCompleted e.handleOrderCompletedFor",
		"EventOrderCancelled e.dispatcher.HandleSwapPeerTerminal",
		"EventOrderReceived e.db.AppendAudit",
		"EventBinUpdated e.db.AppendAudit",
		"EventNodeUpdated e.db.AppendAudit",
		"EventBinUpdated e.RecordMovementTransactions",
		"EventCMSTransaction e.cmsPoster.Enqueue",
		"EventBinUpdated triggerFulfillment",
		"EventOrderCompleted triggerFulfillment",
		"EventOrderCancelled triggerFulfillment",
		"EventOrderFailed triggerFulfillment",
		"EventOrderSkipped triggerFulfillment",
		"EventBinEnteredTransit triggerFulfillment",
		// The scan a finished block triggers reads the mouth row the handler
		// before it released: these two stay in this order.
		"EventBlockCompleted e.handleBlockCompleted",
		"EventBlockCompleted triggerFulfillment",
		"EventBinEnteredTransit e.dispatcher.HandleTransitForLaneGate",
		"EventOrderResumed e.resumeAcquire",
		"EventBinUpdated e.thresholdMonitor.handleBinUpdated",
		"EventOrderReceived e.thresholdMonitor.NoteSwapRequestContradiction",
		"EventBinUpdated e.sourceabilityMonitor.onPayloadChanged",
		"EventOrderReceived e.sourceabilityMonitor.onPayloadChanged",
		"EventGraceExpired e.handleGraceExpired",
		"EventOrderFaulted notify.FaultAlert",
		"EventOrderFaultedRecovered notify.FaultClearedAlert",
		"EventOrderFailed notify.FailAlert",
		"EventGraceExpired notify.GraceExpiredAlert",
		// After every handler above that releases a mouth row; only the window
		// pulls, which release nothing, come after it.
		"-> e.wireLaneGateHandlers",
		"EventBinEnteredTransit e.onWindowLeft",
		"EventOrderCompleted e.onWindowPullOrderCompleted",
	}},
	{"engine/cancel_return.go:wireCancelReturnNotices", []string{
		"EventOrderCompleted e.returnOrderEnded",
		"EventOrderFailed e.returnOrderEnded",
		"EventOrderCancelled e.returnOrderEnded",
	}},
	{"engine/wiring_lane_gate.go:wireLaneGateHandlers", []string{
		"EventBlockCompleted gate",
		"EventBinEnteredTransit gate",
		"EventBinUpdated gate",
		"EventOrderCompleted terminal",
		"EventOrderCancelled terminal",
		"EventOrderFailed terminal",
		"EventOrderSkipped terminal",
		"EventBinEnteredTransit e.dispatcher.RedriveVacateOnLift",
		"EventOrderCompleted vacateTerminal",
		"EventOrderCancelled vacateTerminal",
		"EventOrderFailed vacateTerminal",
		"EventOrderSkipped vacateTerminal",
	}},
	// main's, for messaging: the bus events that can change what an Edge's
	// watched nodes hold, pointed at the bins feed's push. An engine method
	// main calls, so a harness can make the same eight.
	{"engine/bins_feed.go:SubscribeBinsFeed", []string{
		"EventBinUpdated h.Changed",
		"EventBinEnteredTransit h.Changed",
		"EventOrderReceived h.Started",
		"EventOrderDispatched h.Started",
		"EventOrderCompleted h.Ended",
		"EventOrderFailed h.Ended",
		"EventOrderCancelled h.Ended",
		"EventOrderSkipped h.Ended",
	}},
	// The web layer's: each rebroadcasts its event to the screens.
	{"www/router.go:NewRouter", []string{
		"-> hub.SetupEngineListeners",
	}},
	{"www/sse.go:SetupEngineListeners", []string{
		"EventOrderReceived h.Broadcast",
		"EventOrderDispatched h.Broadcast",
		"EventOrderStatusChanged eta.Stamp",                // the order-update frame
		"EventOrderStatusChanged eng.GetCachedRobotStatus", // the mission-event frame
		"EventOrderCompleted h.Broadcast",
		"EventOrderFailed h.Broadcast",
		"EventOrderCancelled h.Broadcast",
		"EventOrderFaulted h.Broadcast",
		"EventOrderFaultedRecovered h.Broadcast",
		"EventOrderSkipped h.Broadcast",
		"EventBinUpdated h.Broadcast",
		"EventBinEnteredTransit h.Broadcast",
		"EventBinRowChanged h.Broadcast",
		"EventNodeUpdated h.Broadcast",
		"EventSourcingUpdated h.Broadcast",
		"EventFleetConnected h.Broadcast",
		"EventFleetDisconnected h.Broadcast",
		"EventMessagingConnected h.Broadcast",
		"EventMessagingDisconnected h.Broadcast",
		"EventDBConnected h.Broadcast",
		"EventDBDisconnected h.Broadcast",
		"EventCMSTransaction h.Broadcast",
		"EventRobotsUpdated onsets.observe",
		"EventCellTick h.Broadcast",
	}},
}

// busCallsFollowedByText are the calls the walk follows by how they are
// written, to the function each runs: that function makes no subscription
// itself, and its name alone (Start, NewRouter) is not its own. A function
// that subscribes is followed by its name wherever it is called.
var busCallsFollowedByText = map[string]string{
	"eng.Start":     "engine/engine_lifecycle.go:Start",
	"www.NewRouter": "www/router.go:NewRouter",
}

// subscribeCallsNotOnTheBus are the calls named Subscribe that are the Kafka
// client's ("<site> <callee>" -> how many, and what each is): a topic and its
// handler, not an event. Any other call named Subscribe is read as the bus's.
var subscribeCallsNotOnTheBus = map[string]int{
	"cmd/shingocore/main.go:main msgClient.Subscribe":      1, // the ingestor on the orders topic
	"messaging/client.go:restoreSubscriptions c.Subscribe": 1, // each topic again, after a late connect
	"messaging/client.go:Reconfigure c.Subscribe":          1, // each topic again, on the new connection
}

// TestSubscriberCensus pins every subscription on Core's bus and the order
// they are made in: coreBusSites against the tree, both ways and in order.
func TestSubscriberCensus(t *testing.T) {
	t.Parallel()
	w := walkCoreBus(t)

	want := map[string][]string{}
	for _, s := range coreBusSites {
		if _, twice := want[s.site]; twice {
			t.Fatalf("the census lists %s twice", s.site)
		}
		want[s.site] = s.rows
	}
	var diffs []string
	for _, s := range coreBusSites {
		got, ok := w.sites[s.site]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s: in the census, no longer in the tree (%d rows). "+
				"If the function moved or was renamed, its rows go to the new site; if its subscriptions are gone, remove them.",
				s.site, len(s.rows)))
			continue
		}
		if d := compareBusSite(s, got); d != "" {
			diffs = append(diffs, d)
		}
	}
	for site, got := range w.sites {
		if _, ok := want[site]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s: in the tree, not in the census. A new place that subscribes to the bus, "+
				"or calls one that does: add it to coreBusSites with these rows, where it runs in the order, "+
				"and say in the commit why it is not one of the three sets.%s", site, listBusRows(got)))
		}
	}
	for key, n := range subscribeCallsNotOnTheBus {
		if w.notBus[key] != n {
			diffs = append(diffs, fmt.Sprintf("%s: the census names %d Kafka subscribe call(s) there, the tree has %d. "+
				"Core should read the orders topic on one reader: read TestCoreReadsTheOrdersTopicOnOneReader "+
				"before correcting subscribeCallsNotOnTheBus", key, n, w.notBus[key]))
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Fatalf("Core's bus subscriptions differ from the census. The bus runs subscribers in the order they "+
			"subscribed, so a subscription's place is behaviour.\n\n%s", strings.Join(diffs, "\n\n"))
	}

	// Every subscribing function is reached from the composition root once:
	// one the root never reaches is never made in a plant, and one reached
	// twice is made twice.
	order, visits := w.orderFrom(t, busRoot)
	for site := range w.sites {
		if visits[site] != 1 {
			t.Errorf("%s is reached %d times from %s, want once. A set nothing calls is never made; "+
				"a set called twice runs every handler twice.", site, visits[site], busRoot)
		}
	}

	// For a reader (-v): how many each function makes, and who hears each
	// event, in the order they hear it.
	perEvent := map[string][]string{}
	var events []string
	for _, s := range coreBusSites {
		n := 0
		for _, r := range w.sites[s.site] {
			if r.call == "" {
				n++
			}
		}
		t.Logf("%-50s %d subscriptions", s.site, n)
	}
	for _, o := range order {
		if _, seen := perEvent[o.row.events]; !seen {
			events = append(events, o.row.events)
		}
		perEvent[o.row.events] = append(perEvent[o.row.events], o.site[strings.LastIndex(o.site, ":")+1:])
	}
	sort.Strings(events)
	t.Logf("%d subscriptions; who hears each event, in order:", len(order))
	for _, ev := range events {
		t.Logf("  %-28s %s", ev, strings.Join(perEvent[ev], ", "))
	}
}

// coreKafkaReads is each way Core's source comes to read a Kafka topic, by
// "<site> <what>" (kafkaRead), with how many there. Together they are one
// reader of the orders topic: main subscribes it once, a subscribe starts one
// read loop, and the loop handles one message to its end before it fetches the
// next (messaging Client.readLoop).
var coreKafkaReads = map[string]int{
	"cmd/shingocore/main.go:main subscribe cfg.Messaging.OrdersTopic": 1, // the ingestor, at boot
	"messaging/client.go:restoreSubscriptions subscribe topic":        1, // each topic's handler, once, after a late connect (Subscribe made no reader before it)
	"messaging/client.go:Reconfigure subscribe topic":                 1, // each topic's handler, once, after Close stopped the old loops
	"messaging/client.go:Subscribe go readLoop":                       1, // one read loop a subscribe
	"messaging/client.go:Subscribe kafka.NewReader":                   1, // that loop's reader
	"messaging/client.go:readLoop kafka.NewReader":                    1, // the same loop's, in place of its own after a read error
	"messaging/ephemeral.go:newEphemeralReader kafka.NewReader":       1, // the ephemeral topic's (the robot feed), no group
}

// TestCoreReadsTheOrdersTopicOnOneReader pins that Core reads the orders topic
// on one reader: coreKafkaReads against the tree, both ways. It is a census of
// the source, so it cannot see a reader the running process starts twice from
// one site (Subscribe does not refuse a topic it already reads), nor a
// re-configure's new reader starting while a handler past Close's limit still
// runs (messaging Client.Close).
//
// The inbox dedup (middleware.NewInboxDedup, docket B11) and the delta apply
// (uop readBinScope, docket B18) both stand on it. Red by mutation, 2026-11-25:
// a second msgClient.Subscribe of the orders topic in main, and a second go
// c.readLoop in Subscribe, each fail here.
func TestCoreReadsTheOrdersTopicOnOneReader(t *testing.T) {
	t.Parallel()
	w := walkCoreBus(t)
	if len(w.kafkaReads) == 0 {
		t.Fatal("the walk found no Kafka read at all, not even main's subscribe: a census that reads nothing reports nothing")
	}
	var diffs []string
	for key, n := range coreKafkaReads {
		if got := w.kafkaReads[key]; got != n {
			diffs = append(diffs, fmt.Sprintf("%s: the census has %d, the tree %d", key, n, got))
		}
	}
	for key, got := range w.kafkaReads {
		if _, ok := coreKafkaReads[key]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s: in the tree (%d), not in the census", key, got))
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Fatalf("Core should read the orders topic on one reader, one message at a time, and the source now reads "+
			"Kafka another way:\n  %s\n\n"+
			"Two designs stand on one reader. The inbox dedup reads its row before the handler and writes it after "+
			"(middleware.NewInboxDedup, docket B11), so two readers given one envelope would both run its handler; "+
			"the delta apply reads its cursor before the bin lock (uop readBinScope, docket B18, the skipped case of "+
			"TestRelease_ADeltaWaitingBehindAFencedRelease). A second reader of the orders topic needs both changed "+
			"first: the dedup claims the envelope before its handler, and the apply reads its cursor after the lock. "+
			"Another topic's reader, or a reader moved, takes its line in coreKafkaReads with what it reads.",
			strings.Join(diffs, "\n  "))
	}
}

// kafkaRead names n when it is a way to read a Kafka topic: "subscribe
// <topic>" for a call named Subscribe with two arguments (the Kafka clients'
// Subscribe takes a topic and a handler, the bus's one handler), "go
// readLoop" for a go statement that starts a read loop, "kafka.NewReader" for
// a reader made. "" otherwise.
func kafkaRead(n ast.Node) string {
	switch n := n.(type) {
	case *ast.GoStmt:
		if busCallee(n.Call.Fun) == "readLoop" {
			return "go readLoop"
		}
	case *ast.CallExpr:
		if busCallee(n.Fun) == "Subscribe" && len(n.Args) == 2 {
			return "subscribe " + types.ExprString(n.Args[0])
		}
		if types.ExprString(n.Fun) == "kafka.NewReader" {
			return "kafka.NewReader"
		}
	}
	return ""
}

// compareBusSite compares one function's census rows with the tree's, in
// order, and says what differs and what to change; "" when they agree.
func compareBusSite(s busSite, got []busRow) string {
	first := -1
	for i := 0; i < len(s.rows) || i < len(got); i++ {
		if i >= len(s.rows) || i >= len(got) || !got[i].matches(s.rows[i]) {
			first = i
			break
		}
	}
	if first < 0 {
		// In order. Two subscriptions of one event must not share their name.
		for i, row := range s.rows {
			events, name, isSub := strings.Cut(row, " ")
			if !isSub || events == "->" {
				continue
			}
			for j, other := range got {
				if j != i && other.call == "" && other.events == events && slices.Contains(other.makes, name) {
					return fmt.Sprintf("%s: row %d (%s) names its handler by a call that row %d's handler makes too, "+
						"so a swap of the two would pass. Name it by a call only it makes.%s", s.site, i+1, row, j+1, listBusRows(got))
				}
			}
		}
		return ""
	}
	what := "the rows differ"
	switch {
	case len(got) == len(s.rows) && sameBusRows(s.rows, got):
		what = "a subscription moved (the same rows, in another order). Registration order is delivery order: " +
			"a move needs the owner's word first (AGENTS.md, Stop and ask before), and then its row moves here with it"
	case len(got) > len(s.rows):
		what = "the tree has more rows than the census. A new subscription takes a row here at the place it " +
			"is made, named by one call its handler makes, and the commit says why it subscribes there"
	case len(got) < len(s.rows):
		what = "the census has rows the tree no longer has. Remove the row of a subscription that is gone"
	}
	at := func(i int, n int, row func(int) string) string {
		if i >= n {
			return "(none)"
		}
		return row(i)
	}
	return fmt.Sprintf("%s: %s.\n  first difference at row %d:\n    census: %s\n    tree:   %s%s",
		s.site, what, first+1,
		at(first, len(s.rows), func(i int) string { return s.rows[i] }),
		at(first, len(got), func(i int) string { return got[i].String() }),
		listBusRows(got))
}

// sameBusRows reports whether the tree's rows are the census's in some order.
func sameBusRows(want []string, got []busRow) bool {
	used := make([]bool, len(got))
	for _, row := range want {
		found := false
		for j, g := range got {
			if !used[j] && g.matches(row) {
				used[j], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func listBusRows(rows []busRow) string {
	var b strings.Builder
	b.WriteString("\n  the tree's rows, in order:")
	for i, r := range rows {
		fmt.Fprintf(&b, "\n    %2d %s", i+1, r)
	}
	return b.String()
}

// busRow is one row the walk read from a function: a subscription, or a call
// of a function that subscribes.
type busRow struct {
	call   string   // "-> <callee>", "" for a subscription
	events string   // the subscription's events, comma-joined
	makes  []string // the calls its handler makes; a function passed is its own one call
}

func (r busRow) String() string {
	if r.call != "" {
		return r.call
	}
	return r.events + "  (its handler calls: " + strings.Join(r.makes, " ") + ")"
}

// matches reports whether a census row describes r.
func (r busRow) matches(row string) bool {
	if r.call != "" {
		return row == r.call
	}
	events, name, ok := strings.Cut(row, " ")
	return ok && events == r.events && slices.Contains(r.makes, name)
}

// busWalk is what the module's non-test source says of the bus.
type busWalk struct {
	sites  map[string][]busRow // "<file>:<func>" -> its rows in source order
	notBus map[string]int      // "<site> <callee>" -> the Kafka subscribe calls there
	// subscribers is each function that subscribes, by name -> its sites.
	subscribers map[string][]string
	// kafkaReads is each way the source comes to read a Kafka topic, by
	// "<site> <what>" (kafkaRead) -> how many there.
	kafkaReads map[string]int
}

// coreBusWalk is the walk the census tests share: each reads the same tree,
// and a walk parses every file of the module. Nothing writes to it once made.
var coreBusWalk struct {
	once sync.Once
	w    *busWalk
	err  error
}

// walkCoreBus is the module's walk (readCoreBus), made once a test binary.
func walkCoreBus(t *testing.T) *busWalk {
	t.Helper()
	coreBusWalk.once.Do(func() { coreBusWalk.w, coreBusWalk.err = readCoreBus() })
	if coreBusWalk.err != nil {
		t.Fatal(coreBusWalk.err)
	}
	return coreBusWalk.w
}

// readCoreBus reads every non-test Go file of the module.
func readCoreBus() (*busWalk, error) {
	const root = ".."
	type fn struct {
		site string
		decl *ast.FuncDecl
	}
	var fns []fn
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != root && (n == "node_modules" || n == "testdata" || n == "vendor" || n == "testharness" || strings.HasPrefix(n, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
				fns = append(fns, fn{filepath.ToSlash(rel) + ":" + fd.Name.Name, fd})
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read shingo-core's source: %w", err)
	}

	w := &busWalk{sites: map[string][]busRow{}, notBus: map[string]int{}, subscribers: map[string][]string{},
		kafkaReads: map[string]int{}}
	// First the functions that subscribe, so that the second pass knows a call
	// of one when it meets it.
	for _, f := range fns {
		subscribes := false
		ast.Inspect(f.decl.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if _, ok := w.subscription(f.site, call, false); ok {
					subscribes = true
				}
			}
			return !subscribes
		})
		if subscribes {
			w.subscribers[f.decl.Name.Name] = append(w.subscribers[f.decl.Name.Name], f.site)
		}
	}
	if len(w.subscribers) == 0 {
		return nil, fmt.Errorf("the walk of %d functions found no subscription: a census that reads nothing reports nothing", len(fns))
	}
	for _, f := range fns {
		var rows []busRow
		ast.Inspect(f.decl.Body, func(n ast.Node) bool {
			if what := kafkaRead(n); what != "" {
				w.kafkaReads[f.site+" "+what]++
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if row, ok := w.subscription(f.site, call, true); ok {
				rows = append(rows, row)
				return false // what the handler calls is not a row of this function
			}
			text := types.ExprString(call.Fun)
			if _, followed := busCallsFollowedByText[text]; followed || len(w.subscribers[busCallee(call.Fun)]) > 0 {
				rows = append(rows, busRow{call: "-> " + text})
			}
			return true
		})
		if len(rows) > 0 {
			w.sites[f.site] = append(w.sites[f.site], rows...)
		}
	}
	return w, nil
}

// subscription reads call as a subscription on the bus, which has three ways
// in: eventbus.SubscribeTyped(bus, handler, events...), and the bus's own
// SubscribeTypes(handler, events...) and Subscribe(handler), every event.
func (w *busWalk) subscription(site string, call *ast.CallExpr, count bool) (busRow, bool) {
	var handler ast.Expr
	var events []ast.Expr
	switch name := busCallee(call.Fun); {
	case name == "SubscribeTyped" && len(call.Args) >= 2:
		handler, events = call.Args[1], call.Args[2:]
	case name == "SubscribeTypes" && len(call.Args) >= 1:
		handler, events = call.Args[0], call.Args[1:]
	case name == "Subscribe" && len(call.Args) >= 1:
		key := site + " " + types.ExprString(call.Fun)
		if _, kafka := subscribeCallsNotOnTheBus[key]; kafka {
			if count {
				w.notBus[key]++
			}
			return busRow{}, false
		}
		handler = call.Args[0]
	default:
		return busRow{}, false
	}
	row := busRow{events: "every event"}
	if len(events) > 0 {
		names := make([]string, len(events))
		for i, e := range events {
			names[i] = types.ExprString(e)
			if sel, ok := e.(*ast.SelectorExpr); ok { // engine.EventBinUpdated is EventBinUpdated
				names[i] = sel.Sel.Name
			}
		}
		row.events = strings.Join(names, ",")
	}
	lit, ok := handler.(*ast.FuncLit)
	if !ok {
		row.makes = []string{types.ExprString(handler)}
		return row, true
	}
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if name := types.ExprString(c.Fun); !slices.Contains(row.makes, name) {
				row.makes = append(row.makes, name)
			}
		}
		return true
	})
	slices.Sort(row.makes)
	return row, true
}

// busCallee is a call's function name: the identifier, the selector's field,
// or either under a generic's type arguments.
func busCallee(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	case *ast.IndexExpr:
		return busCallee(f.X)
	case *ast.IndexListExpr:
		return busCallee(f.X)
	}
	return ""
}

// busOrdered is one subscription with the function that makes it.
type busOrdered struct {
	site string
	row  busRow
}

// orderFrom is every subscription in the order it is made when root runs,
// following each call of a subscribing function to that function's rows, and
// how many times each function is reached.
func (w *busWalk) orderFrom(t *testing.T, root string) ([]busOrdered, map[string]int) {
	t.Helper()
	visits := map[string]int{}
	var order []busOrdered
	var visit func(site string, depth int)
	visit = func(site string, depth int) {
		visits[site]++
		if depth > 16 {
			t.Fatalf("%s: the calls between subscribing functions do not end", site)
		}
		for _, r := range w.sites[site] {
			if r.call == "" {
				order = append(order, busOrdered{site, r})
				continue
			}
			text := strings.TrimPrefix(r.call, "-> ")
			next, ok := busCallsFollowedByText[text]
			if !ok {
				sites := w.subscribers[text[strings.LastIndex(text, ".")+1:]]
				if len(sites) != 1 {
					t.Fatalf("%s calls %s, which names %d subscribing functions (%v): the census cannot say which runs",
						site, text, len(sites), sites)
				}
				next = sites[0]
			}
			visit(next, depth+1)
		}
	}
	visit(root, 0)
	return order, visits
}
