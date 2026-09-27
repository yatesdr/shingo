package engine

import (
	"os"
	"strings"
	"testing"
)

// TestBinMove_BirthsTheRowAndTheHoldTogether pins where a bin move's creation
// and its soft hold on the bin happen.
//
// The two doors used to stamp creation and reserve the bin at opposite points,
// and the order mattered for the moves that lost the bin in the moment between:
// reserve-then-stamp gave them a failure with no beginning. The stamp is the
// INSERT (orders.Create writes the birth history row in the order row's own
// transaction), and since B2 the reserve is in that transaction too:
// Dispatcher.BirthMove runs Build, Q1's take and the move's own reservation, and
// commits them together. A move that loses the bin now writes nothing at all,
// so there is no history to read wrong.
//
// A SOURCE READ, because the fact is structural: the door must birth through
// BirthMove, and inside it the INSERT must come before the hold.
func TestBinMove_BirthsTheRowAndTheHoldTogether(t *testing.T) {
	t.Parallel()
	if !strings.Contains(sourceFunc(t, "bin_move.go", "func (e *Engine) CreateBinMove("), "BirthMove(") {
		t.Fatal("CreateBinMove no longer births through Dispatcher.BirthMove: the row and the bin's hold are no longer one transaction")
	}
	birth := sourceFunc(t, "../dispatch/builder.go", "func (d *Dispatcher) BirthMove(")
	build, hold := strings.Index(birth, "Build("), strings.Index(birth, "reservations.Acquire(")
	if build < 0 || hold < 0 {
		t.Fatalf("BirthMove no longer calls Build (%d) and reservations.Acquire (%d)", build, hold)
	}
	if build > hold {
		t.Error("BirthMove takes the bin before it writes the order: the reservation names an order id the INSERT has not made yet")
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
