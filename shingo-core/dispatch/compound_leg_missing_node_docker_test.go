//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// compound_leg_missing_node_docker_test.go — an ORDER that names a node which is
// not there, and what the demand behind it is told.
//
// The wording of configFailure is pinned in read_vs_missing_test.go and the
// finder's arm is driven there too. Neither is this: those are a formatter and a
// SOURCE SELECTION, and a finder that declines a source has not yet decided
// anything about the order. The two sites here are the order-level ones —
// compound.go's two node reads for a leg about to be dispatched — and they are
// where a name nobody configured actually ends a demand.
//
// It matters more here than anywhere else in the family for the reason the
// production comment gives: a failed leg fails the whole dig, and the dig is
// what a retrieve is waiting behind. So the arm must be exactly right in BOTH
// directions — terminal for a name that is not there (no retry invents a node),
// and never terminal for a read that merely did not answer.
//
// The existing coverage stops short of the disposition. dispatcher_test.go's
// TestHandleOrderRequest_Retrieve_InvalidDeliveryNode asserts only that no order
// was created; engine_concurrent_test.go's asserts only "not dispatched". Both
// are true of a parked order too.

// legWithNodeNames builds a reshuffling parent with ONE pending leg carrying the
// given endpoint names, which is the state AdvanceCompoundOrder is entered in
// after a dig's children are written.
func legWithNodeNames(t *testing.T, db *store.DB, prefix, sourceNode, deliveryNode string) (parent, child *orders.Order) {
	t.Helper()
	parent = &orders.Order{
		EdgeUUID:  prefix + "-parent",
		StationID: "line-1",
		OrderType: OrderTypeRetrieve,
		Status:    StatusReshuffling,
		Quantity:  1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create the dig parent")

	child = &orders.Order{
		EdgeUUID:      prefix + "-leg-1",
		StationID:     parent.StationID,
		OrderType:     OrderTypeMove,
		Status:        StatusPending,
		Quantity:      1,
		ParentOrderID: &parent.ID,
		Sequence:      1,
		SourceNode:    sourceNode,
		DeliveryNode:  deliveryNode,
	}
	testutil.MustNoErr(t, db.CreateOrder(child), "create the leg")
	return parent, child
}

// TestCompoundLeg_SourceNodeDoesNotExist_TerminalWithTheConfigMessage is
// compound.go's first node read.
//
// MUTATION (verified): replace configFailure("source node", next.SourceNode) in
// compound.go with a bare string — "invalid source node", say, or the
// reshuffle_error the sites used to carry. The status assertions stay green
// because the disposition is unchanged; the error_detail assertions fire. That
// is the point of the test: the disposition here was never the bug, the message
// was, and a message is invisible to every assertion about status.
func TestCompoundLeg_SourceNodeDoesNotExist_TerminalWithTheConfigMessage(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, child := legWithNodeNames(t, db, "LEGSRC", "SHUF-NOBODY-CONFIGURED", lineNode.Name)

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "advance the compound")

	after, err := db.GetOrder(child.ID)
	testutil.MustNoErr(t, err, "read the leg back")
	if after.Status != protocol.StatusFailed {
		t.Fatalf("leg status = %q, want %q — the node is genuinely not there and no retry configures "+
			"one, so holding this leg parks the dig forever", after.Status, protocol.StatusFailed)
	}
	if after.VendorOrderID != "" {
		t.Errorf("the leg carries vendor order %q — a robot was sent to a node that does not exist",
			after.VendorOrderID)
	}
	for _, want := range []string{"config failure", "source node", "SHUF-NOBODY-CONFIGURED", "does not exist"} {
		if !strings.Contains(after.ErrorDetail, want) {
			t.Errorf("error_detail %q is missing %q — this is the sentence an operator gets for a "+
				"stopped dig, and it has to say whose problem it is and which node to go and add",
				after.ErrorDetail, want)
		}
	}

	// AND THE DEMAND ENDS WITH IT, which is the correct outcome and worth pinning
	// alongside the wait-not-fail tests rather than apart from them: a real fault
	// MAY fail, and a name nobody configured is as real as faults get. A dig left
	// half-run in `reshuffling` with a dead leg is the failure mode this replaces.
	parentAfter, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "read the parent back")
	if !protocol.IsTerminal(parentAfter.Status) {
		t.Errorf("parent is %q with a failed leg under it — the dig cannot finish and nothing will "+
			"come back to notice", parentAfter.Status)
	}
}

// TestCompoundLeg_DeliveryNodeDoesNotExist_TerminalWithTheConfigMessage is the
// second read, and a separate test because the two arms are separate code that
// differs in the one word an engineer navigates by. A leg has two endpoints and
// "config failure: node X does not exist" would not say which end.
//
// MUTATION (verified): change compound.go's delivery arm to
// configFailure("source node", next.DeliveryNode) — a copy-paste of its
// neighbour, and the likeliest way this actually breaks. Only the "delivery
// node" assertion fires; every other assertion here, and every existing test
// over this path, stays green.
func TestCompoundLeg_DeliveryNodeDoesNotExist_TerminalWithTheConfigMessage(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, child := legWithNodeNames(t, db, "LEGDST", storageNode.Name, "LINE-NOBODY-CONFIGURED")

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "advance the compound")

	after, err := db.GetOrder(child.ID)
	testutil.MustNoErr(t, err, "read the leg back")
	if after.Status != protocol.StatusFailed {
		t.Fatalf("leg status = %q, want %q", after.Status, protocol.StatusFailed)
	}
	if after.VendorOrderID != "" {
		t.Errorf("the leg carries vendor order %q — a robot was sent to a node that does not exist",
			after.VendorOrderID)
	}
	for _, want := range []string{"config failure", "delivery node", "LINE-NOBODY-CONFIGURED", "does not exist"} {
		if !strings.Contains(after.ErrorDetail, want) {
			t.Errorf("error_detail %q is missing %q", after.ErrorDetail, want)
		}
	}
	if strings.Contains(after.ErrorDetail, "source node") {
		t.Errorf("error_detail %q blames the source end for a delivery-end fault; a leg has two "+
			"endpoints and the message is the only thing that says which one to look at",
			after.ErrorDetail)
	}

	parentAfter, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "read the parent back")
	if !protocol.IsTerminal(parentAfter.Status) {
		t.Errorf("parent is %q with a failed leg under it", parentAfter.Status)
	}
}

// TestCompoundLeg_UnreadableNode_HoldsTheLegInsteadOfFailingIt is the arm the
// two above are only half of.
//
// Both sites read a node and both used to fail the leg on ANY error, so the
// terminal that was right for an absent node was also being handed to a database
// that did not answer — and that terminal costs the whole dig and the retrieve
// behind it. The two dispositions are one `readFailed` call apart in the source
// and could not be further apart in what they do to demand, which is why this
// sits in the same file as the terminal arms rather than in the read-failure one.
//
// MUTATION (verified): delete the `if readFailed(err)` block above compound.go's
// source-node read. The leg falls into the arm below it and comes back `failed`
// — the status assertion stops the test there, and the log line the run emits
// says the rest of the cost out loud: "compound order 1 has failed/cancelled
// children — marking parent failed".
func TestCompoundLeg_UnreadableNode_HoldsTheLegInsteadOfFailingIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, child := legWithNodeNames(t, db, "LEGREAD", storageNode.Name, lineNode.Name)

	// Both endpoints are real; the database stops answering for nodes.
	heal := breakNodeReads(t, db)

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "advance the compound during the outage")

	after, err := db.GetOrder(child.ID)
	testutil.MustNoErr(t, err, "read the leg back")
	if after.Status != protocol.StatusPending {
		t.Fatalf("leg status = %q, want %q — GetNextChildOrder selects `pending`, and a leg moved out "+
			"of it for an unanswered SELECT is a leg nothing comes back for", after.Status, protocol.StatusPending)
	}
	if after.QueueCause != string(CauseReadFailed) {
		t.Errorf("queue_cause = %q, want %q", after.QueueCause, CauseReadFailed)
	}
	parentDuring, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "read the parent back")
	if protocol.IsTerminal(parentDuring.Status) {
		t.Fatalf("parent is %q — the dig and the retrieve behind it died because a node read did not "+
			"answer for a node that is right there", parentDuring.Status)
	}

	// The hold has a releaser, and it is the ordinary one: the leg is still
	// `pending`, so the next redrive dispatches it with nothing reset.
	heal()
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "advance the compound after the outage")
	recovered, err := db.GetOrder(child.ID)
	testutil.MustNoErr(t, err, "read the leg back after the outage")
	if recovered.Status == protocol.StatusPending {
		t.Error("the leg is still pending after the read recovered — the hold has no releaser, which " +
			"makes it a stall wearing a queue reason")
	}
}
