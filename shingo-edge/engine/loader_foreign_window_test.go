package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
)

// One plant, several Edges: Core broadcasts the loader config to every Edge,
// and each Edge holds process_node rows only for its own windows. On the
// two-Edge sim stack (2026-09-07) edge1's unloader sweep resolved edge2's
// FGN_L2_01 window 520 times an hour and each resolution came back
// sql.ErrNoRows — which the L1/U1 fire closures treated as a hard failure
// instead of the store's documented "we do not own this destination" answer.
// These tests pin the skip: a window with no process_node row here is another
// Edge's window; the sweep moves on without erroring and without ordering
// against it.

// foreignWindowLoaderInfo builds a shared_window consume loader whose ONE window
// has no process_node row in this Edge's DB — the shape a multi-Edge sync hands
// the Edges that don't own the window.
func foreignWindowLoaderInfo(coreNode, payload string) protocol.LoaderInfo {
	return protocol.LoaderInfo{
		Name:          coreNode,
		LoaderKey:     "loader:" + coreNode,
		Role:          "consume",
		Layout:        "shared_window",
		Replenishment: "auto",
		InboundSource: "FG-SUPER",
		ConfigGen:     1,
		Positions:     []protocol.LoaderPosition{{CoreNodeName: coreNode, Kind: "window"}},
		Payloads:      []protocol.LoaderPayloadInfo{{PayloadCode: payload}},
	}
}

// TestUnloaderSweep_ForeignWindowSkipsNotErrors: the U1 path offered a window
// this Edge doesn't own must create nothing and log no error — the skip is
// quiet at log level, and the "not ours" reasoning lands in the debug line.
// A regression to the hard-error behaviour shows up as the error log line
// ("seam full-in ... failed"), asserted absent below.
func TestUnloaderSweep_ForeignWindowSkipsNotErrors(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	var logs []string
	eng.logFn = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	seedCoreLoader(t, eng, foreignWindowLoaderInfo("FGN_FOREIGN", "PART-F"))

	l, err := eng.loaderStore.LoaderForPayload(domain.PayloadCode("PART-F"), domain.RoleConsume, true)
	if err != nil || l == nil {
		t.Fatalf("resolve consume loader: loader=%v err=%v", l, err)
	}
	eng.createUnloaderFullInViaSeam(l, "PART-F")

	for _, line := range logs {
		if strings.Contains(line, "failed") {
			t.Errorf("a foreign window must fail the sweep for no one; error log: %s", line)
		}
	}
	ords, oerr := db.ListActiveOrders()
	if oerr != nil {
		t.Fatalf("list orders: %v", oerr)
	}
	if len(ords) != 0 {
		t.Errorf("no order may be created against a window this Edge does not own; found %d", len(ords))
	}
}

// TestLoaderL1_ForeignWindowSkipsNotErrors: the symmetric L1 case — a produce
// loader's window with no process_node row here skips rather than failing the
// reservation, so a co-deployed loader on another Edge cannot wedge this one's
// empties.
func TestLoaderL1_ForeignWindowSkipsNotErrors(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	info := foreignWindowLoaderInfo("PLK_FOREIGN", "PART-FK")
	info.Role = "produce"
	info.Replenishment = "operator"
	seedCoreLoader(t, eng, info)

	l, err := eng.loaderStore.LoaderForPayload(domain.PayloadCode("PART-FK"), domain.RoleProduce, true)
	if err != nil || l == nil {
		t.Fatalf("resolve produce loader: loader=%v err=%v", l, err)
	}
	if created, cerr := eng.stageOperatorEmpty(l, "PART-FK", 1, "", orders.Origin{}); cerr != nil || created != 0 {
		t.Errorf("foreign-window loader L1: created=%d err=%v, want 0, nil", created, cerr)
	}
	ords, oerr := db.ListActiveOrders()
	if oerr != nil {
		t.Fatalf("list orders: %v", oerr)
	}
	if len(ords) != 0 {
		t.Errorf("no order may be created against a window this Edge does not own; found %d", len(ords))
	}
}

// TestForeignWindow_DebugLineNamesTheWindow: the skip must be traceable — a
// trace of the 520/hr spam on the sim stack should resolve to "another Edge's
// window", not silence.
func TestForeignWindow_DebugLineNamesTheWindow(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	var debugLines []string
	eng.debugFn = func(f string, a ...any) { debugLines = append(debugLines, strings.TrimSpace(fmt.Sprintf(f, a...))) }
	seedCoreLoader(t, eng, foreignWindowLoaderInfo("FGN_TRACE", "PART-FT"))

	l, err := eng.loaderStore.LoaderForPayload(domain.PayloadCode("PART-FT"), domain.RoleConsume, true)
	if err != nil || l == nil {
		t.Fatalf("resolve consume loader: loader=%v err=%v", l, err)
	}
	eng.createUnloaderFullInViaSeam(l, "PART-FT")

	found := false
	for _, line := range debugLines {
		if strings.Contains(line, "FGN_TRACE") && strings.Contains(line, "another Edge's window") {
			found = true
		}
	}
	if !found {
		t.Errorf("debug trace must name the skipped window and say why; got %v", debugLines)
	}
}
