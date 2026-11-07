package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"testing"
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
