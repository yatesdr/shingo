package dispatch

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// read_vs_missing_test.go — the two facts that used to share a disposition.
//
// "That lane does not exist" is configuration a human fixes; "the database did
// not answer" is a hiccup. Both were terminal, so a DB blip killed demand, and
// the terminal that WAS correct said nothing an operator could act on.

func TestReadFailed_SeparatesAbsenceFromFailure(t *testing.T) {
	if readFailed(nil) {
		t.Error("a successful read is not a failure")
	}
	if readFailed(sql.ErrNoRows) {
		t.Error("sql.ErrNoRows is the ANSWER \"there is nothing there\", not a failed read — filing " +
			"it as transient would park an order forever on a lane that will never exist")
	}
	if readFailed(fmt.Errorf("wrapped: %w", sql.ErrNoRows)) {
		t.Error("a wrapped ErrNoRows is still an absence; the store wraps its errors")
	}
	if !readFailed(errors.New("connection reset by peer")) {
		t.Error("a transport error is a failed read — terminating demand for it is wait-not-fail " +
			"broken on I/O")
	}
}

// TestConfigFailure_SaysWhatToGoAndFix pins the wording, because the wording is
// the feature. An operator reading "reshuffle_error" learns nothing; the message
// has to name whose problem it is and which thing is missing.
func TestConfigFailure_SaysWhatToGoAndFix(t *testing.T) {
	got := configFailure("lane node", "SMN_008")
	for _, want := range []string{"config failure", "lane node", "SMN_008", "does not exist"} {
		if !strings.Contains(got, want) {
			t.Errorf("message %q is missing %q — it has to say whose problem it is, what sort of "+
				"thing is missing, and which one", got, want)
		}
	}
	if id := configFailureID("lane node", 47); !strings.Contains(id, "47") {
		t.Errorf("id form %q does not name the id; a message that cannot answer \"which lane?\" "+
			"sends someone to read logs instead of fixing the configuration", id)
	}
}

// TestReadFailure_IsTransientAndCarriesItsOwnCause is the disposition half.
//
// The cause must NOT be a lane-busy one. During an outage dozens of orders park
// at once, and an operator surface that renders that as congestion sends someone
// to look at lanes.
func TestReadFailure_IsTransientAndCarriesItsOwnCause(t *testing.T) {
	pe := &planningError{Code: codeReadFailed, Detail: "could not read lane 7"}
	if !pe.Transient() {
		t.Fatal("a failed read is terminal — one unanswered SELECT kills the order")
	}
	if got := reshuffleWaitCause(codeReadFailed); got != CauseReadFailed {
		t.Errorf("wait cause = %q, want %q", got, CauseReadFailed)
	}
	for _, laneBusy := range []QueueCause{CauseLaneOccupied, CauseLaneDigActive, CauseLaneLocked, CauseLaneTargetBuried} {
		if CauseReadFailed == laneBusy {
			t.Errorf("the read-failure cause is %q, which is a lane-busy cause — a database outage "+
				"would read as congestion", laneBusy)
		}
	}

	// The genuinely-missing arm stays terminal, and terminal it must be: no retry
	// invents a node that was never configured.
	missing := &planningError{Code: codeInvalidNode, Detail: configFailure("lane node", "NOPE")}
	if missing.Transient() {
		t.Error("a nonexistent node parks and retries forever — the config is wrong and nothing but " +
			"a human will change it")
	}
}

// TestFinder_UnreadableBinNode_Waits is the finder site, driven through the fake.
//
// MUTATION (verified): restore `if err != nil { return ...OutcomeStructural...
// codeNode }`. The outcome assertion fires — a failed node read terminates a
// perfectly good source.
func TestFinder_UnreadableBinNode_Waits(t *testing.T) {
	db := newFakeFinderDB()
	posID := int64(51)
	db.addNode(&nodes.Node{ID: posID, Name: "RVM-SRC"})
	db.addNode(&nodes.Node{ID: 99, Name: "RVM-DEST"})

	// A plant-wide FIFO hit whose node read fails: the bin is real, the row is
	// fine, and the database simply does not answer for its node.
	binNode := int64(60)
	db.fifoBin = &bins.Bin{ID: 201, PayloadCode: "X", NodeID: &binNode}
	db.nodeErr = map[int64]error{binNode: errors.New("connection reset by peer")}

	finder := NewSourceFinder(db, nil, nil)
	order := &orders.Order{ID: 1, OrderType: OrderTypeRetrieve, DeliveryNode: "RVM-DEST", PayloadCode: "X"}

	res := finder.FindSource(order, IntentFull)
	if res.Outcome != OutcomeWait {
		t.Fatalf("outcome = %v, want OutcomeWait — the node read failed, which says nothing about "+
			"whether the bin is usable", res.Outcome)
	}
	if res.QueueCause != string(CauseReadFailed) {
		t.Errorf("queue_cause = %q, want %q", res.QueueCause, CauseReadFailed)
	}
}

// TestFinder_MissingBinNode_IsAConfigFailure is the other arm at the same site:
// the row points at a node id that is not there, which is real and terminal.
func TestFinder_MissingBinNode_IsAConfigFailure(t *testing.T) {
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 99, Name: "RVM2-DEST"})

	binNode := int64(60) // never added, so GetNode reports it absent
	db.fifoBin = &bins.Bin{ID: 202, PayloadCode: "X", NodeID: &binNode}
	db.nodeErr = map[int64]error{binNode: sql.ErrNoRows}

	finder := NewSourceFinder(db, nil, nil)
	order := &orders.Order{ID: 2, OrderType: OrderTypeRetrieve, DeliveryNode: "RVM2-DEST", PayloadCode: "X"}

	res := finder.FindSource(order, IntentFull)
	if res.Outcome != OutcomeStructural {
		t.Fatalf("outcome = %v, want OutcomeStructural — the node genuinely is not there, and "+
			"retrying will not configure it", res.Outcome)
	}
	if res.TermCode != codeInvalidNode {
		t.Errorf("term code = %q, want %q", res.TermCode, codeInvalidNode)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "config failure") {
		t.Errorf("error = %v, want the operator-facing config-failure wording — fixing this is a "+
			"human's job and the message has to say so", res.Err)
	}
}
