package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"testing"
)

// main_feedspin_test.go — source-shape pins of the setupKafkaSubscribers
// closures the versioned feeds change. The closures are registered on a
// router inside a function that needs a live Kafka client and engine, so there
// is no seam to drive them; what each one calls is pinned as written.

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
			want: []string{"log.Printf", "eng.SetCoreNodes", "eng.SetCoreLoaders", "eng.SetPayloadBinTypes",
				"eng.SetSceneGraph", "eng.SetSceneGeometry"},
			after: "SetCoreNodes/SetCoreLoaders/SetPayloadBinTypes skipped when resp.Digest equals the held digest; " +
				"SetSceneGraph and SetSceneGeometry on every reply; digest stored only after SetCoreLoaders returns nil",
			label: "F4",
		},
		{
			subject: "SubjectCatalogPayloadsResponse",
			want:    []string{"log.Printf", "eng.HandlePayloadCatalog"},
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

// TestFeedsPin_HeartbeaterCountsActiveOrders: main.go hands NewHeartbeater a
// closure over db.CountActiveOrders, read on every heartbeat (once a minute).
// after (X5): NewHeartbeater takes no order-count func and nothing in main.go
// passes db.CountActiveOrders to it.
func TestFeedsPin_HeartbeaterCountsActiveOrders(t *testing.T) {
	t.Parallel()
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var args []ast.Expr
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && types.ExprString(call.Fun) == "messaging.NewHeartbeater" {
			args = call.Args
		}
		return true
	})
	cases := []struct {
		name        string
		got         func() any
		want, after any
		label       string
	}{
		{"NewHeartbeater argument count", func() any { return len(args) }, 6, 5, "X5"},
		{"last argument reads db.CountActiveOrders", func() any {
			if len(args) == 0 {
				return false
			}
			return allCalls(args[len(args)-1])["db.CountActiveOrders"]
		}, true, false, "X5"},
	}
	for _, tc := range cases {
		if got := tc.got(); got != tc.want {
			t.Errorf("%s = %v, want %v (after %s: %v)", tc.name, got, tc.want, tc.label, tc.after)
		}
	}
	if len(args) == 0 {
		t.Fatal("no messaging.NewHeartbeater call in main.go")
	}
}
