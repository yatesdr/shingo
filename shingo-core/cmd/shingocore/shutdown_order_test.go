package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestShutdown_TheConsumerClosesBeforeTheEngineStops (docket B11): main()
// cannot run in a test, so this reads Core's shutdown off main.go. main's
// deferred calls run last-registered first; in that order the inbound
// consumer's Close must come before the engine's Stop, and both before the
// database's Close. The consumer's Close waits for the message in hand
// (messaging.Client.Close, pinned by
// TestCoreHandler_AnOrderCutInHalfIsCreatedOnceAfterRedelivery), so that
// message is handled with the engine and the database still up. Run after
// them, the stop cut the handler and the redelivery was dropped as a
// duplicate.
func TestShutdown_TheConsumerClosesBeforeTheEngineStops(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var body *ast.BlockStmt
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "main" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("main.go declares no func main")
	}
	// The calls each defer on main's own frame makes, in the order main
	// registers them. A function literal's own defers (a goroutine's, a
	// closure's) are not main's, so literals are not walked except as the
	// deferred call itself.
	var registered [][]string
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.DeferStmt:
			registered = append(registered, deferredCalls(n.Call))
			return false
		}
		return true
	})
	var run []string
	for i := len(registered) - 1; i >= 0; i-- {
		run = append(run, registered[i]...)
	}
	at := func(name string) int {
		for i, c := range run {
			if c == name {
				return i
			}
		}
		t.Fatalf("main's deferred calls never make %s; they run %v", name, run)
		return -1
	}
	consumer, engine, database := at("msgClient.Close"), at("eng.Stop"), at("db.Close")
	if consumer > engine || engine > database {
		t.Errorf("main's shutdown runs %v: msgClient.Close must run before eng.Stop, and eng.Stop before db.Close. "+
			"Close the consumer inside the defer that stops the engine, before eng.Stop()", run)
	}
}

// deferredCalls names the calls a deferred call makes: the call itself, or,
// for a function literal, every call in its body in source order.
func deferredCalls(call *ast.CallExpr) []string {
	lit, ok := call.Fun.(*ast.FuncLit)
	if !ok {
		return []string{callName(call)}
	}
	var names []string
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			names = append(names, callName(c))
		}
		return true
	})
	return names
}

// callName is a call's receiver and name, "x.F" or "F".
func callName(c *ast.CallExpr) string {
	switch fn := c.Fun.(type) {
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok {
			return x.Name + "." + fn.Sel.Name
		}
		return fn.Sel.Name
	case *ast.Ident:
		return fn.Name
	}
	return ""
}
