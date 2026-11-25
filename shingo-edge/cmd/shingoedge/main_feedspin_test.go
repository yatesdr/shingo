package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/messaging"
)

// main_feedspin_test.go — source-shape pins of the inbound subject closures
// the versioned feeds change: which steps each one calls, in order, as main.go
// writes them. The closures are built by buildInboundRouter, which needs the
// engine, its order handler and a heartbeater but no Kafka client; the door
// walk dispatches every inbound key through it (door_walk_test.go). The walk
// asserts that a closure's writes reach the boards, not which calls it makes,
// so the call lists are pinned here as written. What main.go hands
// messaging.NewHeartbeater is the compiler's to check (its arity and types);
// what the heartbeat carries is pinned in messaging/heartbeat_feedspin_test.go.
// One closure is also run, not parsed: what the registered reply asks of the
// heartbeater (TestBootAsks_TheRegisteredReplyAsksOnlyPastThirtySeconds).

// subjectClosures parses main.go and returns, for each subject registered with
// router.RegisterSubject, the closure's body.
func subjectClosures(t *testing.T) map[string]*ast.BlockStmt {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	out := map[string]*ast.BlockStmt{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || types.ExprString(call.Fun) != "router.RegisterSubject" || len(call.Args) != 3 {
			return true
		}
		subj, ok := call.Args[1].(*ast.SelectorExpr)
		lit, ok2 := call.Args[2].(*ast.FuncLit)
		if ok && ok2 {
			out[subj.Sel.Name] = lit.Body
		}
		return true
	})
	return out
}

// mainFunc parses main.go and returns the body of its top-level function name.
func mainFunc(t *testing.T, name string) *ast.BlockStmt {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return fn.Body
		}
	}
	t.Fatalf("main.go has no function %s: this pin lost its input; name the function that replaced it", name)
	return nil
}

// TestBootWiring_TheHooksAWebHandlerCallsAreSetBeforeTheBurst: in
// setupKafkaSubscribers, the three hooks a web handler calls through (the
// Messaging save's reconnect, the two Sync buttons) are set before
// hb.Start(). Start sends the boot burst on the caller, five messages at three
// tries each: with the broker down, 70 s and more in which the Sync buttons,
// set after it, answered ok and asked nothing (docket D31, ruling R53: the
// two lines move up inside this function; nothing else in main's order).
func TestBootWiring_TheHooksAWebHandlerCallsAreSetBeforeTheBurst(t *testing.T) {
	t.Parallel()
	steps := topLevelCalls(mainFunc(t, "setupKafkaSubscribers"))
	at := map[string]int{}
	for i, s := range steps {
		if _, seen := at[s]; !seen {
			at[s] = i
		}
	}
	start, ok := at["hb.Start"]
	if !ok {
		t.Fatalf("setupKafkaSubscribers no longer calls hb.Start: steps %v", steps)
	}
	for _, hook := range []string{"eng.SetKafkaReconnectFunc", "eng.SetNodeSyncFunc", "eng.SetCatalogSyncFunc"} {
		i, ok := at[hook]
		switch {
		case !ok:
			t.Errorf("setupKafkaSubscribers no longer calls %s: steps %v", hook, steps)
		case i > start:
			t.Errorf("%s is step %d of setupKafkaSubscribers, after hb.Start (step %d): move it above hb.Start, "+
				"or its web handler finds it unset for the whole boot burst", hook, i, start)
		}
	}
}

// TestBootWiring_ABootThatMetNoBrokerOwesItsReconcile (docket B14): the boot's
// order reconcile runs once, after the burst, and the first ack after a boot
// is not a gap (engine TestOnCoreAck_GapReconciles), so a reconcile the broker
// refused was not run again. setupKafkaSubscribers hands it to the heartbeater
// before hb.Start, to run after the burst the loop sends once the broker
// answers (hb.AfterOwedBurst = eng.StartupReconcile), and a boot reconcile
// that fails marks the boot owed (hb.OweBoot in its error branch). The
// heartbeater's half is run in messaging
// TestHeartbeater_ABootBurstThatMetNoBrokerGoesOutWhenItAnswers; this half is
// main's, which no test runs.
func TestBootWiring_ABootThatMetNoBrokerOwesItsReconcile(t *testing.T) {
	t.Parallel()
	body := mainFunc(t, "setupKafkaSubscribers")
	handed, start, owed := -1, -1, false
	for i, st := range body.List {
		switch st := st.(type) {
		case *ast.AssignStmt:
			if len(st.Lhs) == 1 && len(st.Rhs) == 1 && types.ExprString(st.Lhs[0]) == "hb.AfterOwedBurst" &&
				types.ExprString(st.Rhs[0]) == "eng.StartupReconcile" {
				handed = i
			}
		case *ast.ExprStmt:
			if call, ok := st.X.(*ast.CallExpr); ok && types.ExprString(call.Fun) == "hb.Start" {
				start = i
			}
		case *ast.IfStmt:
			if as, ok := st.Init.(*ast.AssignStmt); ok && len(as.Rhs) == 1 {
				if call, ok := as.Rhs[0].(*ast.CallExpr); ok && types.ExprString(call.Fun) == "eng.StartupReconcile" {
					owed = allCalls(st.Body)["hb.OweBoot"]
				}
			}
		}
	}
	if start < 0 {
		t.Fatal("setupKafkaSubscribers no longer calls hb.Start: this pin lost its input")
	}
	if handed < 0 || handed > start {
		t.Errorf("hb.AfterOwedBurst = eng.StartupReconcile is statement %d, hb.Start %d: hand the heartbeater the "+
			"boot reconcile before Start, or a boot the broker refused never reconciles", handed, start)
	}
	if !owed {
		t.Error("the boot reconcile's error branch does not call hb.OweBoot: a reconcile the broker refused is not run again")
	}
}

// TestBootWiring_TheWarningPrintsOnlyWhenConnectFails: main's boot WARNING
// that the Edge runs deaf sits in the kafka-connect-retry goroutine's
// `if err := msgClient.Connect(); err != nil` branch and nowhere else, and the
// line before the wiring says the writer was built. Connect builds the writer
// and contacts no broker (messaging Client.Connect), so the old condition,
// IsConnected() before any Connect, held on every boot: the warning printed on
// every healthy one, and "kafka connect succeeded" printed with the broker
// down (docket D31). A source pin: main itself cannot be run here.
func TestBootWiring_TheWarningPrintsOnlyWhenConnectFails(t *testing.T) {
	t.Parallel()
	body := mainFunc(t, "main")
	var connectFailed *ast.BlockStmt
	ast.Inspect(body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Init == nil || types.ExprString(ifs.Cond) != "err != nil" {
			return true
		}
		if as, ok := ifs.Init.(*ast.AssignStmt); ok && len(as.Rhs) == 1 {
			if call, ok := as.Rhs[0].(*ast.CallExpr); ok && types.ExprString(call.Fun) == "msgClient.Connect" {
				connectFailed = ifs.Body
			}
		}
		return true
	})
	if connectFailed == nil {
		t.Fatal("main has no `if err := msgClient.Connect(); err != nil` branch: this pin lost its input")
	}
	// mentions counts the string literals under n that contain s.
	mentions := func(n ast.Node, s string) int {
		count := 0
		ast.Inspect(n, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(v, s) {
					count++
				}
			}
			return true
		})
		return count
	}
	const warning = "WARNING messaging not connected at boot"
	if all, there := mentions(body, warning), mentions(connectFailed, warning); all != 1 || there != 1 {
		t.Errorf("main logs %q %d times, %d of them in the Connect-failed branch: want once, there", warning, all, there)
	}
	if n := mentions(body, "kafka connect succeeded"); n != 0 {
		t.Errorf("main still logs \"kafka connect succeeded\" (%d): Connect builds a writer and contacts no broker; "+
			"say \"kafka writer built\"", n)
	}
	if n := mentions(body, "kafka writer built"); n != 1 {
		t.Errorf("main logs \"kafka writer built\" %d times, want once, before the wiring", n)
	}
}

// topLevelCalls is the callee of every expression statement directly in the
// block, in order — the closure's own steps, not its arguments' calls.
func topLevelCalls(body *ast.BlockStmt) []string {
	var out []string
	for _, st := range body.List {
		if es, ok := st.(*ast.ExprStmt); ok {
			if call, ok := es.X.(*ast.CallExpr); ok {
				out = append(out, types.ExprString(call.Fun))
			}
		}
	}
	return out
}

// allCalls is the callee of every call anywhere in the node, nested closures
// included.
func allCalls(n ast.Node) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			out[types.ExprString(call.Fun)] = true
		}
		return true
	})
	return out
}

func TestFeedsPin_SubjectClosureSteps(t *testing.T) {
	t.Parallel()
	closures := subjectClosures(t)
	cases := []struct {
		subject string
		want    []string
		after   string
		label   string
	}{
		{
			// The ack does only a log line and the zone adoption: no feed
			// confirm, no claims compare, no reconcile.
			subject: "SubjectEdgeHeartbeatAck",
			want:    []string{"log.Printf", "adoptPlantTimezone", "eng.OnCoreAck"}, // F1
			after:   "[log.Printf adoptPlantTimezone eng.OnCoreAck] (feeds confirm, F5 claims compare, X2 gap rule inside OnCoreAck)",
			label:   "F1, F5, X2",
		},
		{
			subject: "SubjectNodeListResponse",
			// F4: the five calls moved into eng.ApplyNodeList, which skips the
			// first three on a matching digest (pinned behaviourally in
			// engine/node_list_apply_feedspin_test.go).
			want: []string{"log.Printf", "eng.ApplyNodeList"},
			after: "SetCoreNodes/SetCoreLoaders/SetPayloadBinTypes skipped when resp.Digest equals the held digest; " +
				"SetSceneGraph and SetSceneGeometry on every reply; digest stored only after SetCoreLoaders returns nil",
			label: "F4",
		},
		{
			subject: "SubjectCatalogPayloadsResponse",
			want:    []string{"log.Printf", "eng.ApplyCatalog"}, // F4: HandlePayloadCatalog, then the digest on nil
			after:   "HandlePayloadCatalog returns an error; the catalog digest is stored only on nil",
			label:   "F1, F4",
		},
	}
	for _, tc := range cases {
		body, ok := closures[tc.subject]
		if !ok {
			t.Errorf("%s: no RegisterSubject closure found in main.go", tc.subject)
			continue
		}
		if got := topLevelCalls(body); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s steps = %v, want %v (after %s: %s)", tc.subject, got, tc.want, tc.label, tc.after)
		}
	}
}

// TestFeedsPin_AckDoesNotReconcile: nothing reachable from the ack closure
// calls StartupReconcile today. after (X2): an ack more than 4 minutes after
// the previous one reconciles, through eng.OnCoreAck.
func TestFeedsPin_AckDoesNotReconcile(t *testing.T) {
	t.Parallel()
	body := subjectClosures(t)["SubjectEdgeHeartbeatAck"]
	if body == nil {
		t.Fatal("no SubjectEdgeHeartbeatAck closure in main.go")
	}
	calls := allCalls(body)
	cases := []struct {
		callee      string
		want, after bool
		label       string
	}{
		{"eng.StartupReconcile", false, false, "same (X2 reconciles inside eng.OnCoreAck, not in the closure)"},
		{"eng.OnCoreAck", true, true, "F1"}, // F1: was false
	}
	for _, tc := range cases {
		if got := calls[tc.callee]; got != tc.want {
			t.Errorf("ack closure calls %s = %v, want %v (after %s: %v)", tc.callee, got, tc.want, tc.label, tc.after)
		}
	}
}

// TestFeedsPin_PublishOnRegister: the registered ack republishes the full
// claim set through the publisher loaded from plantClaimsPub. after: same (F5
// keeps publish-on-register).
func TestFeedsPin_PublishOnRegister(t *testing.T) {
	t.Parallel()
	body := subjectClosures(t)["SubjectEdgeRegistered"]
	if body == nil {
		t.Fatal("no SubjectEdgeRegistered closure in main.go")
	}
	calls := allCalls(body)
	for _, callee := range []string{"plantClaimsPub.Load", "pub.PublishAll"} {
		if !calls[callee] { // after: same
			t.Errorf("registered closure no longer calls %s — publish-on-register is kept by F5", callee)
		}
	}
}

// askCounter is the inbound handlers' heartbeater, counting what they ask of it.
type askCounter struct{ registers, nodeAsks, catalogAsks int }

func (a *askCounter) SendRegister()       { a.registers++ }
func (a *askCounter) RequestNodeSync()    { a.nodeAsks++ }
func (a *askCounter) RequestCatalogSync() { a.catalogAsks++ }

// TestBootAsks_TheRegisteredReplyAsksOnlyPastThirtySeconds runs the registered
// closure itself, through buildInboundRouter, before any ack, or after an ack
// from a Core that answers feeds or from an older one. Inside an Edge's first
// 30 s (a boot: Core's reply lands within a round trip of the burst, before
// the first ack) it asks for no node list and no catalog, whatever Core is, so
// one boot's asks are the burst's
// (TestScenario_OneEdgeBootAgainstAFeedsCore_NodeListAndCatalogCounts). Past
// 30 s (a re-register: Core restarted, asked for it, or marked the Edge stale)
// it asks for both only while the Edge does not know Core answers feeds: an
// older Core, or no ack yet. A Core that answers feeds sends them when the next
// heartbeat quotes a digest it does not hold. The past-30-s cases stand an
// engine that was never started, whose Uptime reads past 30 s.
func TestBootAsks_TheRegisteredReplyAsksOnlyPastThirtySeconds(t *testing.T) {
	t.Parallel()
	const (
		noAck    = ""
		feedsAck = "a Core that answers feeds"
		olderAck = "an older Core"
	)
	cases := []struct {
		name                  string
		start                 bool
		ack                   string
		wantNode, wantCatalog int
	}{
		{"at boot, before the first ack", true, noAck, 0, 0},
		{"at boot, after a feeds ack", true, feedsAck, 0, 0},
		{"a re-register past 30 s, a feeds Core", false, feedsAck, 0, 0},
		{"a re-register past 30 s, an older Core", false, olderAck, 1, 1},
		{"a re-register past 30 s, no ack yet", false, noAck, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Defaults()
			cfg.StationUID = "edge.asks"
			eng := engine.New(engine.Config{AppConfig: cfg, DB: testdb.Open(t), LogFunc: func(string, ...any) {}})
			if tc.start {
				eng.Start()
				t.Cleanup(eng.Stop)
			}
			if up := eng.Uptime(); tc.start != (up <= 30) {
				t.Fatalf("engine uptime %d s: the case needs it %s 30 s", up, map[bool]string{true: "at or under", false: "over"}[tc.start])
			}
			hb := &askCounter{}
			in := buildInboundRouter(eng, messaging.NewEdgeHandler(eng.OrderManager()), hb, func(string, ...any) {})
			dispatch := func(subject string, body any) {
				env, err := protocol.NewDataEnvelope(subject, protocol.Address{Role: protocol.RoleCore},
					protocol.Address{Role: protocol.RoleEdge, Station: cfg.StationUID}, body)
				if err != nil {
					t.Fatalf("envelope %s: %v", subject, err)
				}
				in.Dispatch(env, env.Type)
			}
			switch tc.ack {
			case feedsAck:
				dispatch(protocol.SubjectEdgeHeartbeatAck, protocol.EdgeHeartbeatAck{StationID: cfg.StationUID, Feeds: map[string]string{}})
			case olderAck:
				dispatch(protocol.SubjectEdgeHeartbeatAck, protocol.EdgeHeartbeatAck{StationID: cfg.StationUID})
			}
			if local, _ := eng.LastCoreAck(); local.IsZero() != (tc.ack == noAck) {
				t.Fatalf("ack %q: the Edge's last ack is %v; the ack did not reach OnCoreAck as the case needs", tc.ack, local)
			}
			if eng.CoreSpeaksFeeds() != (tc.ack == feedsAck) {
				t.Fatalf("ack %q: the Edge reads Core as answering feeds = %v", tc.ack, eng.CoreSpeaksFeeds())
			}
			dispatch(protocol.SubjectEdgeRegistered, protocol.EdgeRegistered{StationID: cfg.StationUID, Message: "registered"})
			if hb.nodeAsks != tc.wantNode || hb.catalogAsks != tc.wantCatalog || hb.registers != 0 {
				t.Errorf("the registered reply asked %d node lists, %d catalogs and %d registers; want %d, %d and 0",
					hb.nodeAsks, hb.catalogAsks, hb.registers, tc.wantNode, tc.wantCatalog)
			}
		})
	}
}
