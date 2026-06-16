package engine

import (
	"sync"
	"testing"

	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// seedWindowNodes creates a process_node per window name so the fire closure's
// GetProcessNodeByCoreNodeName resolves each one. The seam keys on the names.
func seedWindowNodes(t *testing.T, db *store.DB, proc string, windows []string) {
	t.Helper()
	procID, err := db.CreateProcess(proc, "", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process %s: %v", proc, err)
	}
	for i, w := range windows {
		if _, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: procID, CoreNodeName: w, Code: w, Name: w, Sequence: i + 1, Enabled: true,
		}); err != nil {
			t.Fatalf("create window node %s: %v", w, err)
		}
	}
}

func mustMultiWindowLoader(t *testing.T, id string, windows []string, payload string) *domain.Loader {
	t.Helper()
	ws := make([]domain.Window, len(windows))
	for i, w := range windows {
		ws[i] = domain.Window{Node: domain.NodeID(w)}
	}
	l, err := domain.NewSharedWindowLoader(domain.LoaderID(id), id, domain.RoleProduce, domain.ReplenishmentThreshold,
		ws, []domain.PayloadCode{domain.PayloadCode(payload)},
		domain.WithInboundSource("EMPTY-SUPER"),
		domain.WithUOPThreshold(map[domain.PayloadCode]int{domain.PayloadCode(payload): 100}))
	if err != nil {
		t.Fatalf("build multi-window loader: %v", err)
	}
	return l
}

func windowCounts(t *testing.T, db *store.DB, windows []string) (total int, per map[string]int) {
	t.Helper()
	per = map[string]int{}
	list, err := db.ListActiveOrdersByDeliveryNodeSet(windows)
	if err != nil {
		t.Fatalf("ListActiveOrdersByDeliveryNodeSet: %v", err)
	}
	for _, o := range list {
		if o.RetrieveEmpty {
			total++
			per[o.DeliveryNode]++
		}
	}
	return total, per
}

// TestMultiWindow_DemandOfN_ExactlyNAcrossWindows is the C4 acceptance: with the
// multi-window flag on, a shared loader's N windows share one budget of N, and
// one demand of N fires EXACTLY N empties — spread one per window, never 2N and
// never two at the same window. A second demand at a full loader fires nothing.
func TestMultiWindow_DemandOfN_ExactlyNAcrossWindows(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	mw := true
	eng.cfg.LoadersMultiWindow = &mw

	windows := []string{"MW-W1", "MW-W2", "MW-W3"}
	seedWindowNodes(t, db, "MW-PROC", windows)
	loader := mustMultiWindowLoader(t, "MW-LDR", windows, "P1")

	created, err := eng.tryCreateL1(loader, "P1", L1LoopThreshold, 3, "")
	if err != nil || created != 3 {
		t.Fatalf("one demand of 3: created=%d err=%v, want 3", created, err)
	}
	total, per := windowCounts(t, db, windows)
	if total != 3 {
		t.Fatalf("in-flight total = %d, want exactly 3 (never 2N)", total)
	}
	for _, w := range windows {
		if per[w] != 1 {
			t.Errorf("window %s has %d empties, want exactly 1 (one bin per window)", w, per[w])
		}
	}

	// Loader full → a second demand fires nothing.
	created, err = eng.tryCreateL1(loader, "P1", L1LoopThreshold, 3, "")
	if err != nil || created != 0 {
		t.Errorf("full loader: created=%d err=%v, want 0", created, err)
	}
	if total, _ := windowCounts(t, db, windows); total != 3 {
		t.Errorf("after second demand: total = %d, want 3", total)
	}
}

// TestRace_MultiWindow_NeverExceedsWindowCount hammers a multi-window loader from
// many goroutines under -race: the cluster in-flight must never exceed the window
// count, and no window may hold more than one empty.
func TestRace_MultiWindow_NeverExceedsWindowCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	mw := true
	eng.cfg.LoadersMultiWindow = &mw

	windows := []string{"RMW-W1", "RMW-W2", "RMW-W3", "RMW-W4"}
	seedWindowNodes(t, db, "RMW-PROC", windows)
	loader := mustMultiWindowLoader(t, "RMW-LDR", windows, "P1")

	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			_, _ = eng.tryCreateL1(loader, "P1", L1LoopThreshold, len(windows), "")
		})
	}
	wg.Wait()

	total, per := windowCounts(t, db, windows)
	if total > len(windows) {
		t.Fatalf("cluster in-flight %d exceeds window count %d (budget violated)", total, len(windows))
	}
	for w, c := range per {
		if c > 1 {
			t.Errorf("window %s holds %d empties, want <= 1", w, c)
		}
	}
}
