//go:build sim

package simulator

import (
	"testing"
	"time"

	"shingocore/config"
	"shingocore/domain"
	"shingocore/fleet"
)

// stepsDoneFrom counts the plan steps the blocks finish, as Core's block
// subscriber does (domain.StepsDoneAfter), reading the simulator's binTasks.
func stepsDoneFrom(o domain.Order, blocks []capturedBlock) int {
	action := map[string]string{"JackLoad": "pickup", "JackUnload": "dropoff", "Wait": "wait"}
	for _, b := range blocks {
		if n, ok := domain.StepsDoneAfter(&o, b.location, action[b.binTask]); ok {
			o.StepsDone = n
		}
	}
	return o.StepsDone
}

// TestDriverReportsTheLastBlockBeforeTheOrderFinishes: a plant's fleet reports
// every block FINISHED, the last one in the same poll as the order and ahead of
// it (rds.Poller.diffBlockStates). The simulator does the same, so Core's count
// of a delivered order's plan steps reaches the plan's length on the sim too.
func TestDriverReportsTheLastBlockBeforeTheOrderFinishes(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0, FailRate: 0}

	t.Run("a complete order: lift and drop", func(t *testing.T) {
		d, s, m, em := newTestDriver(t, cfg, 1)
		vid := mkTransport(t, s, "o1") // JackLoad@A, JackUnload@B
		runTicks(d, m, 20)
		if got := s.GetOrder(vid).State; got != "FINISHED" {
			t.Fatalf("want FINISHED, got %s", got)
		}
		want := []string{"block:o1_load", "block:o1_unload", "status:" + vid + ":FINISHED"}
		if got := tail(blockAndFinish(em.events, vid), len(want)); !equal(got, want) {
			t.Errorf("events %v, want the drop's block reported before FINISHED: %v", em.events, want)
		}
		if got := stepsDoneFrom(domain.Order{SourceNode: "A", DeliveryNode: "B"}, em.blocks); got != 2 {
			t.Errorf("the blocks reported count %d of the plan's 2 steps", got)
		}
	})

	t.Run("an order released past a bare wait", func(t *testing.T) {
		d, s, m, em := newTestDriver(t, cfg, 3)
		res, err := s.CreateOrder(fleet.CreateOrderRequest{ExternalID: "staged",
			Blocks: []fleet.OrderBlock{{BlockID: "b0", Location: "P", BinTask: "JackLoad"}}})
		if err != nil {
			t.Fatalf("CreateOrder: %v", err)
		}
		runTicks(d, m, 20)
		if err := s.ReleaseOrder(res.VendorOrderID, []fleet.OrderBlock{{BlockID: "b1", Location: "Q", BinTask: "JackUnload"}}, true); err != nil {
			t.Fatalf("ReleaseOrder: %v", err)
		}
		runTicks(d, m, 20)
		if got := s.GetOrder(res.VendorOrderID).State; got != "FINISHED" {
			t.Fatalf("want FINISHED, got %s", got)
		}
		plan := `[{"action":"pickup","node":"P"},{"action":"wait"},{"action":"dropoff","node":"Q"}]`
		if got := stepsDoneFrom(domain.Order{StepsJSON: plan}, em.blocks); got != 3 {
			t.Errorf("the blocks reported %v count %d of the plan's 3 steps", em.blocks, got)
		}
	})
}

// blockAndFinish is the events of one order's blocks and its FINISHED.
func blockAndFinish(events []string, vid string) []string {
	var out []string
	for _, e := range events {
		if e == "status:"+vid+":FINISHED" || len(e) > 6 && e[:6] == "block:" {
			out = append(out, e)
		}
	}
	return out
}

func tail(s []string, n int) []string {
	if len(s) < n {
		return s
	}
	return s[len(s)-n:]
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
