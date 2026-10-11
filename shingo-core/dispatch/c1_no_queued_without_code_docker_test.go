//go:build docker

package dispatch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// c1_no_queued_without_code_docker_test.go — `queued` always carries a cause
// (BRIEF C1.1.5; RULINGS-C1-P0 F5: "no `queued` write without a code, anywhere").
//
// `queued` is the one place an order rests short of the fleet, and the row says
// why. A queued row with no code is a wait nobody can explain, which is the
// shape the ladder cutover exists to end. Three layers hold it: the transition
// refuses it here; testdb's teardown sweep checks every docker test's history for
// a row into `queued` with no code; and the census below keeps the transition the
// only writer.

// TestLadder_NoQueuedWriteWithoutACode: the two doors that wrote one.
//
// RED at 647b65af: the transition writes `queued` for a row with no code, and
// the fleet-refusal undo leaves `sourcing` with none.
func TestLadder_NoQueuedWriteWithoutACode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	t.Run("the transition refuses a row with no code", func(t *testing.T) {
		o := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "nq-transition"; o.Status = StatusPending })
		err := d.lifecycle.transition(o, StatusQueued, Event{Actor: "test", Reason: "no cause"})
		got := testdb.RequireOrder(t, db, "nq-transition")
		if err == nil || got.Status == StatusQueued {
			t.Fatalf("pending → queued with no code: err %v, status %s; want refused, still pending", err, got.Status)
		}
	})

	t.Run("a fleet-refusal undo with no code leaves no wait without a cause", func(t *testing.T) {
		o := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "nq-demote"; o.Status = StatusDispatched })
		d.DemoteAfterFleetRefusal(o, QueueWait{}, "", "", QueueParams{})
		got := testdb.RequireOrder(t, db, "nq-demote")
		if (got.Status == StatusQueued || got.Status == StatusSourcing) && got.QueueCode == "" {
			t.Fatalf("the undo left the order %s with no code: a wait nobody can explain", got.Status)
		}
	})
}

// TestLadder_TheTransitionIsTheOnlyStatusWriter keeps the refusal above the
// whole story: every non-terminal status write in Core goes through the store's
// compare-and-swap writer, and only the lifecycle calls it.
func TestLadder_TheTransitionIsTheOnlyStatusWriter(t *testing.T) {
	t.Parallel()
	writer := regexp.MustCompile(`UpdateOrderStatusFromWithReason(Tx)?\(|orders\.UpdateStatusFromWithReason(Tx)?\(|\bUpdateOrderStatus\(`)
	var callers []string
	root := ".."
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if n := info.Name(); n == "testdata" || n == "node_modules" || strings.HasPrefix(n, ".") && n != ".." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		slash := filepath.ToSlash(path)
		if strings.Contains(slash, "/store/") {
			return nil // the writers themselves and their delegates
		}
		b, rerr := os.ReadFile(path)
		testutil.MustNoErr(t, rerr, "read "+path)
		for i, line := range strings.Split(string(b), "\n") {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
				continue
			}
			if writer.MatchString(line) && !strings.HasSuffix(slash, "dispatch/lifecycle.go") {
				callers = append(callers, slash+":"+itoa(i+1))
			}
		}
		return nil
	})
	testutil.MustNoErr(t, err, "walk the module")
	if len(callers) != 0 {
		t.Fatalf("a status write outside the lifecycle's transition, which is where `queued` is refused "+
			"without a code: %v", callers)
	}
	if !protocol.IsValidTransition(StatusPending, StatusQueued) {
		t.Fatal("fixture: pending → queued is the park; it must stay an edge")
	}
}
