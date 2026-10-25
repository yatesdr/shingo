package engine

import (
	"os"
	"strings"
	"testing"
)

// TestBinMove_IssuesTheRowAndTheHoldTogether pins where a bin move's creation
// and its soft hold on the bin happen.
//
// The two doors used to stamp creation and reserve the bin at opposite points,
// and the order mattered for the moves that lost the bin in the moment between:
// reserve-then-stamp gave them a failure with no beginning. The stamp is the
// INSERT (orders.Create writes the creation history row in the order row's own
// transaction), and the reserve is in that transaction too: the move is issued
// for its bin (dispatch.Need.Cart), and the issue writes the row, the take and
// the move's own reservation and commits them together. A move that loses the
// bin now writes nothing at all, so there is no history to read wrong.
//
// A SOURCE READ, because the fact is structural: the door must issue for its
// cart, and inside the issue the INSERT must come before the hold.
func TestBinMove_IssuesTheRowAndTheHoldTogether(t *testing.T) {
	t.Parallel()
	door := sourceFunc(t, "bin_move.go", "func (e *Engine) CreateBinMove(")
	if !strings.Contains(door, "IssueOrder(") || !strings.Contains(door, "Cart:") {
		t.Fatal("CreateBinMove no longer issues the move for its bin: the row and the bin's hold are no longer one transaction")
	}
	issue := sourceFunc(t, "../dispatch/builder.go", "func issue(")
	insert, hold := strings.Index(issue, "orders.Create("), strings.Index(issue, "reservations.Acquire(")
	if insert < 0 || hold < 0 {
		t.Fatalf("the issue no longer calls orders.Create (%d) and reservations.Acquire (%d)", insert, hold)
	}
	if insert > hold {
		t.Error("the issue takes the bin before it writes the order: the reservation names an order id the INSERT has not made yet")
	}
}

// sourceFunc returns the text of the function whose declaration starts sig.
func sourceFunc(t *testing.T, path, sig string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(b)
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("no %q in %s — did it move, or get renamed?", sig, path)
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("could not find the end of %q in %s", sig, path)
	}
	return rest[:end]
}
