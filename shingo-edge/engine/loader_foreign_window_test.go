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
// The U1 is Core's to order now, and an unloader with no window here is
// another Edge's to ask for (ownWindow): this Edge asks Core nothing for it.

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

// TestUnloaderSweep_ForeignWindowSkipsNotErrors: the U1 path offered an
// unloader none of whose windows this Edge owns asks Core nothing, creates
// nothing and logs no error — the skip is quiet at log level, and the "not
// ours" reasoning lands in the debug line.
func TestUnloaderSweep_ForeignWindowSkipsNotErrors(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	var logs []string
	eng.logFn = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	core := answerLoaderAsks(t, eng)
	seedCoreLoader(t, eng, foreignWindowLoaderInfo("FGN_FOREIGN", "PART-F"))

	l, err := eng.loaderStore.LoaderForPayload(domain.PayloadCode("PART-F"), domain.RoleConsume)
	if err != nil || l == nil {
		t.Fatalf("resolve consume loader: loader=%v err=%v", l, err)
	}
	eng.offerUnloaderFulls(l, []domain.PayloadCode{"PART-F"})
	awaitAsks(t, eng)
	if n := core.asks.Load(); n != 0 {
		t.Errorf("asks for another Edge's unloader = %d, want 0", n)
	}

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

// corruptProcessNodeRow stores a non-integer in process_nodes.sequence for one
// window, so GetProcessNodeByCoreNodeName fails at Scan with an error that is
// not sql.ErrNoRows while every other window still resolves.
func corruptProcessNodeRow(t *testing.T, eng *Engine, coreNode string) {
	t.Helper()
	res, err := eng.db.Exec(`UPDATE process_nodes SET sequence='not-a-number' WHERE core_node_name=?`, coreNode)
	if err != nil {
		t.Fatalf("corrupt %s: %v", coreNode, err)
	}
	if n, rerr := res.RowsAffected(); rerr != nil || n != 1 {
		t.Fatalf("corrupt %s: %d rows affected, want 1", coreNode, n)
	}
	if _, gerr := eng.db.GetProcessNodeByCoreNodeName(coreNode); gerr == nil {
		t.Fatalf("fixture premise: GetProcessNodeByCoreNodeName(%s) succeeded on the corrupted row", coreNode)
	}
}

// TestOfferUnloaderFulls_AProcessNodeReadErrorAsksNothing: a process-node read
// that fails with anything but "no such row" is not taken as this Edge's
// window: the U1 asks Core nothing and the failure is logged under
// side-cycle:.
func TestOfferUnloaderFulls_AProcessNodeReadErrorAsksNothing(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	core := answerLoaderAsks(t, eng)
	var logs []string
	eng.logFn = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	windows := []string{"UE-W1", "UE-W2"}
	seedWindowNodes(t, db, "UE-PROC", windows)
	corruptProcessNodeRow(t, eng, "UE-W1")
	info := foreignWindowLoaderInfo("UE", "PART-A")
	info.Positions = []protocol.LoaderPosition{{CoreNodeName: "UE-W1"}, {CoreNodeName: "UE-W2"}}
	seedCoreLoader(t, eng, info)

	eng.MaybeCreateUnloaderFullIn("PART-A")
	awaitAsks(t, eng)
	if n := core.asks.Load(); n != 0 {
		t.Errorf("asks = %d, want none", n)
	}
	want := "side-cycle: unloader loader:UE window UE-W1: read its process node: "
	found := false
	for _, l := range logs {
		if strings.HasPrefix(l, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("logs %v, want a line starting %q", logs, want)
	}
}
