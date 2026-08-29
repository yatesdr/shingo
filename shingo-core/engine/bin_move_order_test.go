package engine

import (
	"os"
	"strings"
	"testing"
)

// TestBinMove_StampsCreationBeforeTakingTheBin pins the one ordering decision
// the two-doors-into-one merge had to make for itself.
//
// The doors did this at opposite points and neither said why. The status column
// already says pending — the INSERT sets it — so what the hand-rolled MarkPending
// call was really for is the HISTORY row, which transitions write and inserts did
// not. Without it an order created directly at pending has no entry saying it
// ever started.
//
// THE STAMP IS THE INSERT NOW. orders.Create writes the birth row in the same
// transaction as the order row, for every door — so this test asks the same
// question of the call that actually creates the order. The claim it defends is
// unchanged; only the statement that satisfies it moved, and it moved somewhere
// no call site can reorder it away from.
//
// So the order matters for exactly one class of order: the ones that fail at
// the reservation, having lost the bin to another person in the moment between
// the availability check and the acquire. Stamp-then-reserve gives those a
// history that reads created-then-failed. Reserve-then-stamp gives them a
// failure with no beginning — and those are the orders somebody is most likely
// to go and read, because they are the ones where something went wrong.
//
// WHY THIS IS A SOURCE READ AND NOT A BEHAVIOUR TEST. The discriminating case
// cannot be reached from outside. Both selections check the bin's availability
// before creating anything — claimed and reserved alike — so a bin that is
// already spoken for is refused with no order row at all, and there is nothing
// whose history to inspect. Reaching the reservation failure needs two requests
// colliding inside the few microseconds between that check and the acquire.
// The codebase already reads its own source where the fact is structural (see
// the order-writer census), and this is that: the claim is about the shape of
// the function, so the test reads the shape.
func TestBinMove_StampsCreationBeforeTakingTheBin(t *testing.T) {
	t.Parallel()
	body := binMoveBody(t)

	stamp := strings.Index(body, "CreateOrder(")
	if stamp < 0 {
		t.Fatal("CreateBinMove no longer calls CreateOrder. The birth history row rides that INSERT (orders.Create); without it an order created straight at pending has no entry saying it was ever created, so its timeline starts at whatever happened next.")
	}
	reserve := strings.Index(body, "ReserveForDispatch(")
	if reserve < 0 {
		t.Fatal("CreateBinMove no longer calls ReserveForDispatch — the bin is not being soft-acquired before dispatch")
	}

	if stamp > reserve {
		t.Errorf("CreateOrder now runs AFTER ReserveForDispatch.\n" +
			"That was one of the two doors' orderings and it was not the one chosen: an order that loses the bin race is then failed without ever having been recorded as created, so its history reads as a failure with no beginning.\n" +
			"If the order is being changed on purpose, change this test and say why in the commit.")
	}
}

// binMoveBody returns the text of CreateBinMove.
func binMoveBody(t *testing.T) string {
	t.Helper()
	const path = "bin_move.go"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(b)
	const sig = "func (e *Engine) CreateBinMove("
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("no CreateBinMove in %s — did it move, or get renamed?", path)
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("could not find the end of CreateBinMove in %s", path)
	}
	return rest[:end]
}
