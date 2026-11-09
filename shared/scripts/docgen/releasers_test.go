package main

import (
	"strings"
	"testing"
)

var releasersFixture = map[string]string{
	"go.work":            "go 1.25\n\nuse ./shingo-core\n",
	"shingo-core/go.mod": "module shingocore\n",
	"shingo-core/fulfillment/scanner.go": `package fulfillment

type Scanner struct{}

// RunOnce executes a single scan pass.
func (s *Scanner) RunOnce() int { return 0 }
`,
	"shingo-core/dispatch/queue_releasers.go": `package dispatch

type WaitPopulation string

const (
	// PopAcquiring is the waiting orders.
	PopAcquiring WaitPopulation = "acquiring"
	// PopStationWait is an order the station owns.
	PopStationWait WaitPopulation = "station-wait"
)

type QueueCause string

const (
	CauseSlow QueueCause = "slow"
	CauseOdd  QueueCause = "odd"
)

type Dispatcher struct{}

// HandleOrderRelease processes a release.
func (d *Dispatcher) HandleOrderRelease() {}

var waitPopulations = []populationReleaser{
	{
		population: PopAcquiring,
		owner:      "core",
		redriver:   "fulfillment.Scanner.RunOnce",
		events:     []string{"EventBinUpdated", "EventOrderFailed"},
		floor:      "fulfillment.Scanner.StartPeriodicSweep (60s)",
	},
	{
		population:     PopStationWait,
		owner:          "station",
		redriver:       "Dispatcher.HandleOrderRelease (station-initiated)",
		events:         []string{"protocol.TypeOrderRelease (from the station)"},
		noFloorBecause: "only the station can tell. " + "The rest is detail.",
	},
}

var causeReleasers = []causeReleaser{
	{cause: CauseSlow, populations: []WaitPopulation{PopAcquiring}, what: "the " + "slot frees | soon"},
	{cause: CauseOdd, populations: []WaitPopulation{PopAcquiring, PopStationWait}, finding: "NO EVENT. Nothing will."},
}
`,
}

func TestReleasers_RendersBothTables(t *testing.T) {
	root := gosrcWriteTree(t, releasersFixture)
	pops, err := releasersPopulations(root)
	if err != nil {
		t.Fatal(err)
	}
	causes, err := releasersCauses(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"| `PopAcquiring` (`acquiring`) | PopAcquiring is the waiting orders. | core | `fulfillment.Scanner.RunOnce` — RunOnce executes a single scan pass. | `EventBinUpdated`, `EventOrderFailed` | `fulfillment.Scanner.StartPeriodicSweep` (60s) |",
		"| `PopStationWait` (`station-wait`) | PopStationWait is an order the station owns. | station | `Dispatcher.HandleOrderRelease` (station-initiated) — HandleOrderRelease processes a release. | `protocol.TypeOrderRelease` (from the station) | none: only the station can tell. |",
	} {
		if !strings.Contains(pops, w+"\n") {
			t.Errorf("populations: no row\n%s\nin\n%s", w, pops)
		}
	}
	for _, w := range []string{
		"| `CauseSlow` | `slow` | `acquiring` | the slot frees \\| soon |  |",
		"| `CauseOdd` | `odd` | `acquiring`, `station-wait` | — | NO EVENT. |",
	} {
		if !strings.Contains(causes, w+"\n") {
			t.Errorf("causes: no row\n%s\nin\n%s", w, causes)
		}
	}
}

// TestReleasers_Refuses: a re-driver that names nothing, and a cause that says
// neither what ends it nor why nothing does, are errors, never rows.
func TestReleasers_Refuses(t *testing.T) {
	for name, c := range map[string]struct {
		old, new string
		emit     func(string) (string, error)
	}{
		"dead re-driver": {`"fulfillment.Scanner.RunOnce"`, `"fulfillment.Scanner.Gone"`, releasersPopulations},
		"silent cause":   {`finding: "NO EVENT. Nothing will."`, `bridgeNote: "x"`, releasersCauses},
	} {
		files := map[string]string{}
		for k, v := range releasersFixture {
			files[k] = v
		}
		p := "shingo-core/dispatch/queue_releasers.go"
		files[p] = strings.Replace(files[p], c.old, c.new, 1)
		if _, err := c.emit(gosrcWriteTree(t, files)); err == nil {
			t.Errorf("%s: emitted without error", name)
		}
	}
}

// TestReleasers_TheRealTables pins two rows a reader leans on.
func TestReleasers_TheRealTables(t *testing.T) {
	pops, err := releasersPopulations(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	causes, err := releasersCauses(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pops, "| `PopStationWait` (`station-wait`) |") || !strings.Contains(pops, "| none: ") {
		t.Errorf("the station-owned population is missing or floored:\n%s", pops)
	}
	if !strings.Contains(causes, "| `CauseFleetRefusedCreate` | `fleet-error` |") {
		t.Errorf("no CauseFleetRefusedCreate row:\n%s", causes)
	}
}
