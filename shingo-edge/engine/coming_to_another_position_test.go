package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// A CARRIER COMING TO ANOTHER POSITION DOES NOT WORK THE CELL. On an index
// press a prime (a plain order bound for a position behind the front) brings
// the line no bin, so it neither refuses the line's REQUEST nor silences the
// level sweep; both read worksTheCell. Everything that lifts from the line,
// sets down on it or is a multi-step leg still does.
func TestWorksTheCell_APrimeToAPositionBehindTheFrontDoesNot(t *testing.T) {
	t.Parallel()
	press := &processes.NodeClaim{CoreNodeName: "CP-FRONT", SwapMode: protocol.SwapModeTwoRobotPressIndex,
		Role: protocol.ClaimRoleProduce, PairedCoreNode: "CP-BACK", SecondPairedCoreNode: "CP-THIRD"}
	row := func(typ protocol.OrderType, src, dst string) domain.Order {
		return domain.Order{ID: 1, OrderType: typ, SourceNode: src, DeliveryNode: dst, Status: protocol.StatusSubmitted}
	}
	for _, c := range []struct {
		name  string
		o     domain.Order
		works bool
	}{
		{"an empty to the back (a prime)", row(protocol.OrderTypeRetrieve, "CP-MKT", "CP-BACK"), false},
		{"an empty to the third position", row(protocol.OrderTypeRetrieveEmpty, "CP-MKT", "CP-THIRD"), false},
		{"a move onto the back from a buffer", row(protocol.OrderTypeMove, "CP-BUF", "CP-BACK"), false},
		{"a retrieve to the line", row(protocol.OrderTypeRetrieve, "CP-MKT", "CP-FRONT"), true},
		{"a move off the line to the back", row(protocol.OrderTypeMove, "CP-FRONT", "CP-BACK"), true},
		{"a clear off the back", row(protocol.OrderTypeMove, "CP-BACK", "CP-MKT"), true},
		{"a multi-step leg ending on the back", row(protocol.OrderTypeComplex, "", "CP-BACK"), true},
	} {
		if got := worksTheCell(&c.o, press); got != c.works {
			t.Errorf("%s: works the cell = %v, want %v", c.name, got, c.works)
		}
	}

	// The same rows on a mode with no positions behind its line: an order to
	// a node of that name is no prime and still works the cell.
	single := &processes.NodeClaim{CoreNodeName: "CP-FRONT", SwapMode: protocol.SwapModeSingleRobot,
		Role: protocol.ClaimRoleProduce, PairedCoreNode: "CP-BACK"}
	if o := row(protocol.OrderTypeRetrieve, "CP-MKT", "CP-BACK"); !worksTheCell(&o, single) {
		t.Error("a retrieve to a node a single-robot claim does not hold as a position stopped working its cell")
	}

	// The vacant-line gate over the same rows: a prime alone admits the
	// line's delivery; with a retrieve to the line live, the refusal names it.
	node := &processes.Node{Name: "CP-FRONT", CoreNodeName: "CP-FRONT"}
	prime := row(protocol.OrderTypeRetrieve, "CP-MKT", "CP-BACK")
	if err := positionWorkedBy(node, press, []domain.Order{prime}); err != nil {
		t.Errorf("a prime to the back refused the line's delivery: %v", err)
	}
	line := row(protocol.OrderTypeRetrieve, "CP-MKT", "CP-FRONT")
	line.ID = 2
	if err := positionWorkedBy(node, press, []domain.Order{prime, line}); err == nil || !strings.Contains(err.Error(), "order 2") {
		t.Errorf("with a retrieve to the line live, the gate = %v, want a refusal naming order 2", err)
	}
}
