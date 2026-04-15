package countgroup

import (
	"sync"
	"time"

	"shingocore/config"
)

// Poller is the RDS client surface the loop needs. In production this
// is *rds.Client; in tests it's a fake.
type Poller interface {
	GetRobotsInCountGroup(group string) ([]string, error)
}

// Runner owns the per-group goroutines plus the shared never-occupied
// watchdog. Start wires one goroutine per enabled group. Stop waits
// for all goroutines to exit.
type Runner struct {
	cfg     config.CountGroupsConfig
	poller  Poller
	emitter Emitter
	logFn   func(string, ...any)

	stopChan chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	// Per-group last-non-empty timestamps, protected by mu.
	// Used by the never-occupied watchdog to flag typo'd group names.
	mu             sync.Mutex
	lastNonEmptyAt map[string]time.Time
	warnedNever    map[string]bool // true once we've logged WARN
	erroredNever   map[string]bool // true once we've logged ERROR
	enabledAt      time.Time
}

// NewRunner creates a Runner. Call Start to launch goroutines.
func NewRunner(cfg config.CountGroupsConfig, poller Poller, emitter Emitter, logFn func(string, ...any)) *Runner {
	if logFn == nil {
		logFn = func(string, ...any) {}
	}
	return &Runner{
		cfg:            cfg,
		poller:         poller,
		emitter:        emitter,
		logFn:          logFn,
		stopChan:       make(chan struct{}),
		lastNonEmptyAt: make(map[string]time.Time),
		warnedNever:    make(map[string]bool),
		erroredNever:   make(map[string]bool),
	}
}

// Start launches one goroutine per enabled group plus the watchdog.
// Safe to call on a Runner with no configured groups — it is a no-op.
func (r *Runner) Start() {
	r.enabledAt = time.Now()

	for _, g := range r.cfg.Groups {
		if !g.Enabled {
			r.logFn("countgroup: group %q disabled; skipping", g.Name)
			continue
		}
		r.wg.Add(1)
		go r.groupLoop(g)
	}

	// Watchdog only needs to run if at least one group is enabled.
	for _, g := range r.cfg.Groups {
		if g.Enabled {
			r.wg.Add(1)
			go r.neverOccupiedWatchdog()
			return
		}
	}
}

// Stop signals all goroutines and waits for them to exit.
func (r *Runner) Stop() {
	r.stopOnce.Do(func() { close(r.stopChan) })
	r.wg.Wait()
}

// groupLoop is one ticker per group. Runs the startup probe once,
// then loops polling RDS, feeding the debouncer, and emitting
// transitions. Manages the fail-safe timer on RDS failure.
func (r *Runner) groupLoop(g config.CountGroupConfig) {
	defer r.wg.Done()

	r.startupProbe(g.Name)

	interval := r.cfg.PollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	r.logFn("countgroup: group %s starting debounce fill (%d/%d samples needed for on/off)",
		g.Name, r.cfg.OnThreshold, r.cfg.OffThreshold)

	deb := newDebouncer(r.cfg.OnThreshold, r.cfg.OffThreshold)
	var lastSuccess time.Time = time.Now()
	failSafeActive := false

	for {
		select {
		case <-r.stopChan:
			return
		case <-ticker.C:
			robots, err := r.poller.GetRobotsInCountGroup(g.Name)
			if err != nil {
				// Failure path: hold state; advance fail-safe timer.
				r.logFn("countgroup: group %s poll error: %v", g.Name, err)
				if !failSafeActive && time.Since(lastSuccess) > r.cfg.FailSafeTimeout {
					if deb.forceOn() {
						failSafeActive = true
						r.emitter.Emit(Transition{
							Group:             g.Name,
							Desired:           "on",
							Robots:            nil,
							FailSafeTriggered: true,
							Timestamp:         time.Now(),
						})
						r.logFn("countgroup: group %s FAIL-SAFE ON after %s of RDS failure",
							g.Name, r.cfg.FailSafeTimeout)
					}
				}
				continue
			}

			// Success path: reset fail-safe timer; update last-non-empty tracker.
			lastSuccess = time.Now()
			failSafeActive = false
			if len(robots) > 0 {
				r.recordNonEmpty(g.Name)
			}

			// Feed every successful poll into the debouncer. Do NOT dedup
			// by hash here — the off transition requires `off_threshold`
			// consecutive empty samples, and every empty response has the
			// same hash. Deduping before the debouncer starves the
			// counter and the off transition becomes unreachable. The
			// debouncer itself already returns changed=false for steady
			// state, which is the correct place to suppress emits.
			_, changed := deb.feed(len(robots) > 0)
			if !changed {
				continue
			}
			desired := "off"
			if len(robots) > 0 {
				desired = "on"
			}
			r.emitter.Emit(Transition{
				Group:             g.Name,
				Desired:           desired,
				Robots:            robots,
				FailSafeTriggered: false,
				Timestamp:         time.Now(),
			})
		}
	}
}

// startupProbe posts each configured group once before the tick loop
// begins. A 4xx tells us the config is wrong (likely group name typo);
// a 200 tells us at least the endpoint is reachable and the request
// shape is valid. Does NOT block the loop — always proceeds.
func (r *Runner) startupProbe(group string) {
	robots, err := r.poller.GetRobotsInCountGroup(group)
	if err != nil {
		r.logFn("countgroup: startup probe for group %q FAILED: %v "+
			"— check the group name in shingocore.yaml and Roboshop (case-sensitive)", group, err)
		return
	}
	r.logFn("countgroup: startup probe for group %q ok: %d robot(s) currently in zone",
		group, len(robots))
	if len(robots) > 0 {
		r.recordNonEmpty(group)
	}
}

// recordNonEmpty stamps lastNonEmptyAt[group] = now. Called on any
// successful poll that returned a non-empty robot list.
func (r *Runner) recordNonEmpty(group string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastNonEmptyAt[group] = time.Now()
}

// neverOccupiedWatchdog catches silently-failing configs (typo'd group
// names that always return []). Runs once per minute. At warnThreshold
// since process start without ever seeing the group occupied, logs
// WARN once. At errorThreshold, logs ERROR once. Does not affect light
// state — the deadman + fail-safe handle actual safety; this is purely
// a telemetry aid for operators chasing "why isn't my light working?"
func (r *Runner) neverOccupiedWatchdog() {
	defer r.wg.Done()

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopChan:
			return
		case <-ticker.C:
			r.checkNeverOccupied()
		}
	}
}

func (r *Runner) checkNeverOccupied() {
	r.mu.Lock()
	defer r.mu.Unlock()

	elapsed := time.Since(r.enabledAt)
	for _, g := range r.cfg.Groups {
		if !g.Enabled {
			continue
		}
		if _, seen := r.lastNonEmptyAt[g.Name]; seen {
			continue
		}
		if elapsed >= r.cfg.NeverOccupiedError && !r.erroredNever[g.Name] {
			r.logFn("countgroup: ERROR group %q has been enabled for %s "+
				"and has never returned an occupied response — verify the "+
				"group exists in RDSCore Roboshop and the name matches "+
				"shingocore.yaml exactly (case-sensitive)", g.Name, elapsed.Round(time.Second))
			r.erroredNever[g.Name] = true
		} else if elapsed >= r.cfg.NeverOccupiedWarn && !r.warnedNever[g.Name] {
			r.logFn("countgroup: WARN group %q has been enabled for %s "+
				"and has never returned an occupied response — verify the "+
				"group name matches Roboshop (case-sensitive)",
				g.Name, elapsed.Round(time.Second))
			r.warnedNever[g.Name] = true
		}
	}
}
