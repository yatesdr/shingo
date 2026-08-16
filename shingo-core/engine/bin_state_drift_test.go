package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"shingocore/store/orders"
)

// driftPlan is the swap: pick a full bin at the press, drop it in the lane, pick
// an empty out, take the empty back to the press. Two bins, two destinations,
// which is what makes "the row for ONE of them is stale" expressible at all.
const driftPlan = `[{"action":"pickup","node":"D-PRESS"},` +
	`{"action":"dropoff","node":"D-LANE-S2"},` +
	`{"action":"pickup","node":"D-EMPTIES","empty":true},` +
	`{"action":"dropoff","node":"D-PRESS"}]`

// driftEngine is an Engine with nothing but a capturing logger. noteDestNodeDrift
// reads no database — it compares a record it is handed against a plan it is
// handed — so a real engine would only add a container to run in.
func driftEngine() (*Engine, func() []string) {
	var mu sync.Mutex
	var lines []string
	e := &Engine{logFn: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}}
	return e, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(lines))
		copy(out, lines)
		return out
	}
}

// TestDestNodeDrift_CountsTheStaleRowOnly pins the first bin-state tripwire.
//
// One bin-state fact recorded twice: the plan, and order_bins.dest_node. This
// counts the times they disagree at the moment it costs something — settle time,
// when the row is what places the bin.
//
// The assertion this instrument exists to make is FALSE IN PRODUCTION as of this
// batch, which is the tidiest possible proof of the disease and the reason the
// counter lands before A rather than after it.
func TestDestNodeDrift_CountsTheStaleRowOnly(t *testing.T) {
	resetDestNodeDriftTally()
	t.Cleanup(resetDestNodeDriftTally)

	eng, logs := driftEngine()
	order := &orders.Order{ID: 42, StepsJSON: driftPlan}
	rows := []*orders.OrderBin{
		// The full bin: the row agrees with the plan. Healthy, must not count.
		{BinID: 7, NodeName: "D-PRESS", DestNode: "D-LANE-S2"},
		// The empty: the row still names the slot allocation picked, while the plan
		// sends it back to the press. This is D2's shape — a re-bind moved the plan
		// and left the projection behind.
		{BinID: 9, NodeName: "D-EMPTIES", DestNode: "D-LANE-S0"},
	}

	eng.noteDestNodeDrift(order, rows, driftSiteDelivery)

	tally := DestNodeDriftTally()
	if tally[driftSiteDelivery] != 1 {
		t.Fatalf("%s = %d, want exactly 1 — the agreeing row must not count, or the tripwire fires "+
			"on every healthy swap and stops being readable", driftSiteDelivery, tally[driftSiteDelivery])
	}
	if tally[driftSiteCompleted] != 0 {
		t.Errorf("%s = %d, want 0 — a site that never ran must not appear",
			driftSiteCompleted, tally[driftSiteCompleted])
	}

	// The line has to name the MECHANISM and both nodes, or the count buys a number
	// and no diagnosis — the mistake this branch paid for at four arrival sites.
	joined := strings.Join(logs(), "\n")
	for _, want := range []string{"bin 9", "D-LANE-S0", "D-PRESS", "re-bind", "Expected count is ZERO"} {
		if !strings.Contains(joined, want) {
			t.Errorf("drift line does not carry %q — %s", want, joined)
		}
	}
	if strings.Contains(joined, "bin 7") {
		t.Errorf("the agreeing row was reported: %s", joined)
	}
}

// TestDestNodeDrift_QuietWhereItHasNoQuestion covers the arms that must stay free
// and silent, because this runs on every settle of every multi-bin order.
func TestDestNodeDrift_QuietWhereItHasNoQuestion(t *testing.T) {
	resetDestNodeDriftTally()
	t.Cleanup(resetDestNodeDriftTally)

	eng, logs := driftEngine()
	rows := []*orders.OrderBin{{BinID: 7, NodeName: "D-PRESS", DestNode: "D-LANE-S0"}}

	cases := []struct {
		name  string
		order *orders.Order
		rows  []*orders.OrderBin
	}{
		{"no plan to compare against", &orders.Order{ID: 1}, rows},
		{"single-bin order — no junction rows", &orders.Order{ID: 2, StepsJSON: driftPlan}, nil},
		{"unparseable plan is the settle's finding, not this one",
			&orders.Order{ID: 3, StepsJSON: "{not json"}, rows},
		{"nil order", nil, rows},
		// A row whose bin the plan does not move has no planned destination. That is
		// a different question (which bin is this order even for) and answering it
		// here would make every relay pickup a drift.
		{"bin the plan never carries", &orders.Order{ID: 4, StepsJSON: driftPlan},
			[]*orders.OrderBin{{BinID: 99, NodeName: "D-ELSEWHERE", DestNode: "D-LANE-S0"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng.noteDestNodeDrift(tc.order, tc.rows, driftSiteDelivery)
		})
	}

	if n := DestNodeDriftTally()[driftSiteDelivery]; n != 0 {
		t.Errorf("%s = %d, want 0 — none of these is a disagreement between a record and a plan",
			driftSiteDelivery, n)
	}
	if len(logs()) != 0 {
		t.Errorf("silent arms logged: %v", logs())
	}
}
