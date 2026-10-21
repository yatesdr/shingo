package engine

import (
	"testing"

	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// seedWindowNodes creates a process_node per window name so the fire closure's
// GetProcessNodeByCoreNodeName resolves each one. The seam keys on the names.
func seedWindowNodes(t *testing.T, db *store.DB, proc string, windows []string) {
	t.Helper()
	procID, err := db.CreateProcess(proc, "", "", "", false)
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

// mustMultiWindowLoader builds a shared loader over the given windows. Variadic
// in payload so the occupancy-live property test can exercise the per-payload
// dedup against the shared window budget; single-payload calls are unchanged.
func mustMultiWindowLoader(t *testing.T, id string, windows []string, payloads ...string) *domain.Loader {
	t.Helper()
	ws := make([]domain.Window, len(windows))
	for i, w := range windows {
		ws[i] = domain.Window{Node: domain.NodeID(w)}
	}
	ps := make([]domain.PayloadCode, len(payloads))
	thresholds := make(map[domain.PayloadCode]int, len(payloads))
	for i, p := range payloads {
		ps[i] = domain.PayloadCode(p)
		thresholds[domain.PayloadCode(p)] = 100
	}
	l, err := domain.NewSharedWindowLoader(domain.LoaderID(id), domain.RoleProduce, domain.ReplenishmentThreshold,
		ws, ps,
		domain.WithInboundSource("EMPTY-SUPER"),
		domain.WithUOPThreshold(thresholds))
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
