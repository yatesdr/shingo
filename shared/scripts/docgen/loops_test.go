package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gosrcWriteTree writes files (slash path → content) under a fresh root.
func gosrcWriteTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// loopsFixture is an engine with one of every launch shape the generator
// reads, and the shapes it must not count.
var loopsFixture = map[string]string{
	"go.work":  "go 1.25\n\nuse (\n\t./m\n)\n",
	"m/go.mod": "module m\n\ngo 1.25\n",
	"m/config/config.go": `package config

import "time"

const DefaultSweep = 5 * time.Minute

type Config struct {
	Staging StagingConfig ` + "`yaml:\"staging\"`" + `
}

type StagingConfig struct {
	SweepInterval time.Duration ` + "`yaml:\"sweep_interval\"`" + `
}
`,
	"m/engine/engine.go": `package engine

import (
	"context"
	"time"

	"m/config"
)

const tickInterval = 2 * time.Second

type Tracker interface {
	// Start begins tracking.
	Start()
}

type Engine struct {
	cfg     *config.Config
	keeper  *Keeper
	tracker Tracker
	stop    chan struct{}
}

func (e *Engine) Start() {
	go e.tickLoop()
	go func() { <-e.stop }()
	go e.oneShot()
	e.startHelpers()
	e.keeper.Run(context.Background())
	e.tracker.Start()
	e.subscribe(func() { go e.perEvent() })
	go e.sweepLoop(e.cfg.Staging.SweepInterval)
}

// tickLoop ticks on a constant.
func (e *Engine) tickLoop() {
	t := time.NewTicker(tickInterval)
	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
		}
	}
}

// oneShot walks a list once: not a loop.
func (e *Engine) oneShot() {
	for _, x := range []int{1, 2} {
		_ = x
	}
}

// perEvent is spawned by a handler: not a background loop.
func (e *Engine) perEvent() {
	for {
	}
}

func (e *Engine) subscribe(fn func()) {}

// startHelpers starts the config sweep.
func (e *Engine) startHelpers() {
	go e.configLoop()
}

// configLoop sweeps on the configured interval.
func (e *Engine) configLoop() {
	interval := e.cfg.Staging.SweepInterval
	if interval <= 0 {
		interval = config.DefaultSweep
	}
	t := time.NewTicker(interval)
	for range t.C {
	}
}

// sweepLoop sweeps on its argument.
func (e *Engine) sweepLoop(d time.Duration) {
	for {
		time.Sleep(d)
	}
}

// Keeper keeps a level.
type Keeper struct {
	interval time.Duration
}

// NewKeeper builds the keeper.
func NewKeeper() *Keeper { return &Keeper{interval: 30 * time.Second} }

// Run ticks until ctx is cancelled.
func (k *Keeper) Run(ctx context.Context) {
	go func() {
		t := time.NewTicker(k.interval)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}
`,
}

// TestLoops_ReadsEveryLaunchShape pins each shape: what must be a row, with
// what cadence, and what must not be one.
func TestLoops_ReadsEveryLaunchShape(t *testing.T) {
	rows, err := loopsInventory(gosrcWriteTree(t, loopsFixture), "m/engine")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]loopsRow{}
	var order []string
	for _, r := range rows {
		got[r.name] = r
		order = append(order, r.name)
	}
	want := []struct{ name, startedBy, cadence string }{
		{"tickLoop", "Engine.Start", "2s (`tickInterval`)"},
		{"configLoop", "startHelpers", "config `staging.sweep_interval`, else 5m (`config.DefaultSweep`)"},
		{"Keeper.Run", "Engine.Start", "30s"},
		{"Tracker.Start", "Engine.Start", "set by the implementation"},
		{"sweepLoop", "Engine.Start", "config `staging.sweep_interval`"},
	}
	for _, w := range want {
		r, ok := got[w.name]
		if !ok {
			t.Errorf("%s: not in the inventory (got %v)", w.name, order)
			continue
		}
		if r.startedBy != w.startedBy || r.cadence != w.cadence {
			t.Errorf("%s: started by %q at %q, want %q at %q", w.name, r.startedBy, r.cadence, w.startedBy, w.cadence)
		}
		if r.doc == "" {
			t.Errorf("%s: no doc sentence", w.name)
		}
	}
	for _, not := range []string{"oneShot", "perEvent", "Engine.Start"} {
		if _, ok := got[not]; ok {
			t.Errorf("%s is in the inventory; it is not a background loop", not)
		}
	}
	if len(rows) != len(want) {
		t.Errorf("rows %v, want exactly %d", order, len(want))
	}
}

// TestLoops_AnUndocumentedLoopIsAnError: a row that says nothing is a defect
// the generator reports rather than renders.
func TestLoops_AnUndocumentedLoopIsAnError(t *testing.T) {
	files := map[string]string{}
	for k, v := range loopsFixture {
		files[k] = v
	}
	files["m/engine/engine.go"] = strings.Replace(files["m/engine/engine.go"], "// tickLoop ticks on a constant.\n", "", 1)
	_, err := loopsInventory(gosrcWriteTree(t, files), "m/engine")
	if err == nil || !strings.Contains(err.Error(), "tickLoop") {
		t.Fatalf("err = %v, want one naming tickLoop", err)
	}
}

// TestLoops_TheRealInventory pins the tree's own shapes: the loops the hand
// list missed are rows, the ones it kept after they were deleted are not.
func TestLoops_TheRealInventory(t *testing.T) {
	for _, c := range []struct {
		dir     string
		want    map[string]string // loop → cadence
		missing []string
	}{
		{"shingo-core/engine", map[string]string{
			"KanbanKeeper.Run":           "30s (`keeperTickInterval`)",
			"claimSweepLoop":             "config `staging.sweep_interval`, else 5m (`config.DefaultStagingSweepInterval`)",
			"terminateChaseLoop":         "60s (`terminateChaseInterval`)",
			"robotRefreshLoop":           "2s (`robotPollInterval`)",
			"laneLivenessFloorLoop":      "60s (`laneLivenessFloorInterval`)",
			"Scanner.StartPeriodicSweep": "60s",
			"ReconciliationService.Loop": "config `staging.sweep_interval`, else 5m (`config.DefaultStagingSweepInterval`)",
		}, []string{"stagedBinSweepLoop"}},
		{"shingo-edge/engine", map[string]string{
			"catidMonitor.run": "500ms (`plcPollInterval`)",
			"Manager.pollLoop": "config `poll_rate`, else 1s (`config.DefaultPollRate`)",
		}, []string{"recordL1Burst"}},
	} {
		rows, err := loopsInventory(repoRoot, c.dir)
		if err != nil {
			t.Fatalf("%s: %v", c.dir, err)
		}
		got := map[string]string{}
		for _, r := range rows {
			got[r.name] = r.cadence
		}
		for name, cadence := range c.want {
			if g, ok := got[name]; !ok {
				t.Errorf("%s: %s is not in the inventory", c.dir, name)
			} else if g != cadence {
				t.Errorf("%s: %s cadence %q, want %q", c.dir, name, g, cadence)
			}
		}
		for _, name := range c.missing {
			if _, ok := got[name]; ok {
				t.Errorf("%s: %s is in the inventory", c.dir, name)
			}
		}
	}
}
