package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
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
		Positions:     []protocol.LoaderPosition{{CoreNodeName: coreNode}},
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

	l, err := eng.loaderStore.LoaderForPayload(domain.PayloadCode("PART-F"), domain.RoleConsume)
	if err != nil || l == nil {
		t.Fatalf("resolve consume loader: loader=%v err=%v", l, err)
	}
	eng.createUnloaderFullIns(l, []domain.PayloadCode{"PART-F"})

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

// TestForeignWindow_ReadsOccupiedOffTheTable: a window this Edge does not
// watch (another Edge's, its process node there) has no row in this Edge's
// bins table, and a name with no row reads occupied: the budget spends it and
// no U1 targets it, so the sweep neither orders against it nor errors. Its own
// Edge orders to it. The decision line says so (resident=1).
func TestForeignWindow_ReadsOccupiedOffTheTable(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	var lines []string
	eng.logFn = func(f string, a ...any) { lines = append(lines, strings.TrimSpace(fmt.Sprintf(f, a...))) }
	seedCoreLoader(t, eng, foreignWindowLoaderInfo("FGN_TRACE", "PART-FT"))
	holdSweep(t, eng, nil)

	l, err := eng.loaderStore.LoaderForPayload(domain.PayloadCode("PART-FT"), domain.RoleConsume)
	if err != nil || l == nil {
		t.Fatalf("resolve consume loader: loader=%v err=%v", l, err)
	}
	eng.createUnloaderFullIns(l, []domain.PayloadCode{"PART-FT"})

	found := false
	for _, line := range lines {
		if strings.HasPrefix(line, "loader_budget loader=loader:FGN_TRACE ") && strings.Contains(line, "resident=1") &&
			strings.Contains(line, "occupancy=ok") && strings.Contains(line, "to_fire=0") {
			found = true
		}
	}
	if !found {
		t.Errorf("decision line must read the unwatched window occupied (resident=1, occupancy=ok, to_fire=0); got %v", lines)
	}
	ords, oerr := db.ListActiveOrders()
	if oerr != nil {
		t.Fatalf("list orders: %v", oerr)
	}
	if len(ords) != 0 {
		t.Errorf("orders = %d, want none against a window this Edge does not watch", len(ords))
	}
}
