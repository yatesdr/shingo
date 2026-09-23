package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingoedge/domain"
	"shingoedge/orders"
)

// Characterisation pins for stageOperatorEmpty (the operator-driven L1 path)
// and the window-resolve step both fire closures share. They pin the
// loader_push: log prefix, the entry guards, the origin pass-through, and that
// a process_node read error other than ErrNoRows aborts the fire loop.

// pushLoader builds an operator-replenished shared produce loader over windows,
// pulling empties from EMPTY-SUPER.
func pushLoader(t *testing.T, id string, windows []string, inbound string) *domain.Loader {
	t.Helper()
	ws := make([]domain.Window, len(windows))
	for i, w := range windows {
		ws[i] = domain.Window{Node: domain.NodeID(w)}
	}
	l, err := domain.NewSharedWindowLoader(domain.LoaderID(id), id, domain.RoleProduce, domain.ReplenishmentOperator,
		ws, []domain.PayloadCode{"P1"}, domain.WithInboundSource(inbound))
	if err != nil {
		t.Fatalf("build loader: %v", err)
	}
	return l
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
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("corrupt %s: %d rows affected, want 1", coreNode, n)
	}
	if _, gerr := eng.db.GetProcessNodeByCoreNodeName(coreNode); gerr == nil {
		t.Fatalf("fixture premise: GetProcessNodeByCoreNodeName(%s) succeeded on the corrupted row", coreNode)
	}
}

// TestStageOperatorEmpty_NilLoader: a nil loader creates nothing and is not an error.
func TestStageOperatorEmpty_NilLoader(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	if created, err := eng.stageOperatorEmpty(nil, "P1", 1, "", orders.NoDemand()); created != 0 || err != nil {
		t.Fatalf("nil loader: created=%d err=%v, want 0, nil", created, err)
	}
}

// TestStageOperatorEmpty_OperatorDrivenFiresWithPrefixedTrace: an
// operator-replenished loader is NOT suppressed on this path (it is the
// operator-driven supply path), the origin the caller passes lands on the order,
// and the per-order debug line carries the literal loader_push: prefix.
func TestStageOperatorEmpty_OperatorDrivenFiresWithPrefixedTrace(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	var debug []string
	eng.debugFn = func(f string, a ...any) { debug = append(debug, fmt.Sprintf(f, a...)) }
	seedWindowNodes(t, db, "LP-PROC", []string{"LP-W1"})
	l := pushLoader(t, "LP", []string{"LP-W1"}, "EMPTY-SUPER")
	if !l.IsOperatorDriven() {
		t.Fatal("fixture premise: loader must be operator-driven")
	}

	created, err := eng.stageOperatorEmpty(l, "P1", 1, "", orders.Attached("EP-LP-1"))
	if created != 1 || err != nil {
		t.Fatalf("operator-driven loader: created=%d err=%v, want 1, nil", created, err)
	}
	list, lerr := db.ListActiveOrdersByDeliveryNodeSet([]string{"LP-W1"})
	if lerr != nil || len(list) != 1 {
		t.Fatalf("orders at LP-W1: %d err=%v, want 1", len(list), lerr)
	}
	o := list[0]
	if !o.RetrieveEmpty || o.PayloadCode != "P1" || o.SourceNode != "EMPTY-SUPER" || o.OriginID != "EP-LP-1" {
		t.Errorf("L1 = retrieve_empty=%v payload=%q source=%q origin=%q, want true P1 EMPTY-SUPER EP-LP-1",
			o.RetrieveEmpty, o.PayloadCode, o.SourceNode, o.OriginID)
	}
	wantLine := fmt.Sprintf("loader_push: L1 order %d (1/1) loader=LP payload=P1 window=LP-W1", o.ID)
	found := false
	for _, d := range debug {
		if d == wantLine {
			found = true
		}
	}
	if !found {
		t.Errorf("debug trace %v, want the line %q", debug, wantLine)
	}
}

// TestStageOperatorEmpty_NoInboundTracePrefix: the no-inbound skip reads no
// Core occupancy, creates nothing, and says so under the loader_push: prefix.
func TestStageOperatorEmpty_NoInboundTracePrefix(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	var debug []string
	eng.debugFn = func(f string, a ...any) { debug = append(debug, fmt.Sprintf(f, a...)) }
	seedWindowNodes(t, db, "LN-PROC", []string{"LN-W1"})
	l := pushLoader(t, "LN", []string{"LN-W1"}, "")

	if created, err := eng.stageOperatorEmpty(l, "P1", 1, "", orders.NoDemand()); created != 0 || err != nil {
		t.Fatalf("no-inbound loader: created=%d err=%v, want 0, nil", created, err)
	}
	want := "loader_push: loader=LN payload=P1 skipped — no inbound source (fed directly)"
	if len(debug) != 1 || debug[0] != want {
		t.Errorf("debug trace %v, want exactly [%q]", debug, want)
	}
}

// TestStageOperatorEmpty_ResolveReadErrorAbortsLoop pins the resolve step's
// error arm (the ErrNoRows arm is TestLoaderL1_ForeignWindowSkipsNotErrors).
// A process_node read that fails for any other reason stops the fire loop at
// that window: later windows are not attempted, the count made so far is
// returned with a wrapped error, and the failure is logged under loader_push:.
func TestStageOperatorEmpty_ResolveReadErrorAbortsLoop(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		corrupt     string
		wantCreated int
		wantAt      map[string]int
	}{
		{name: "first_window_bad_stops_before_second", corrupt: "RE-W1", wantCreated: 0, wantAt: map[string]int{}},
		{name: "second_window_bad_keeps_first", corrupt: "RE-W2", wantCreated: 1, wantAt: map[string]int{"RE-W1": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			var logs []string
			eng.logFn = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
			windows := []string{"RE-W1", "RE-W2"}
			seedWindowNodes(t, db, "RE-PROC", windows)
			corruptProcessNodeRow(t, eng, tc.corrupt)
			l := pushLoader(t, "RE", windows, "EMPTY-SUPER")

			created, err := eng.stageOperatorEmpty(l, "P1", 2, "", orders.NoDemand())

			if created != tc.wantCreated {
				t.Errorf("created = %d, want %d", created, tc.wantCreated)
			}
			wantErr := "loader_push: no process_node for delivery target " + tc.corrupt + ": "
			if err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, wantErr)
			}
			list, _ := db.ListActiveOrdersByDeliveryNodeSet(windows)
			at := map[string]int{}
			for _, o := range list {
				at[o.DeliveryNode]++
			}
			if fmt.Sprint(at) != fmt.Sprint(tc.wantAt) {
				t.Errorf("orders by window = %v, want %v", at, tc.wantAt)
			}
			wantLog := fmt.Sprintf("loader_push: loader=RE payload=P1 reservation failed after %d created: ", tc.wantCreated)
			found := false
			for _, l := range logs {
				if strings.HasPrefix(l, wantLog) {
					found = true
				}
			}
			if !found {
				t.Errorf("logs %v, want a line starting %q", logs, wantLog)
			}
		})
	}
}

// TestCreateUnloaderFullIn_ResolveReadErrorAborts is the U1 side of the same
// arm: the read error aborts the fire, nothing is created, and the failure is
// logged under side-cycle:.
func TestCreateUnloaderFullIn_ResolveReadErrorAborts(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	var logs []string
	eng.logFn = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	windows := []string{"UE-W1", "UE-W2"}
	seedWindowNodes(t, db, "UE-PROC", windows)
	corruptProcessNodeRow(t, eng, "UE-W1")
	seedCoreLoader(t, eng, unloaderInfo("UE", windows, []string{"PART-A"}))

	eng.MaybeCreateUnloaderFullIn("PART-A")

	if got := fullsByWindow(t, eng, windows); len(got) != 0 {
		t.Fatalf("U1s = %v, want none", got)
	}
	want := "side-cycle: unloader loader:UE seam full-in for PART-A failed after 0 created: side-cycle: no process_node for unloader window UE-W1: "
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
