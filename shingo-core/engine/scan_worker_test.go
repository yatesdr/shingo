package engine

// scan_worker_test.go: who runs the fulfillment scanner's passes, how many,
// and who notices when nobody does.
//
// The engine here is bare: the bus, the real wiring and a real Scanner over a
// store that holds nothing. The event used is EventBinEnteredTransit with an
// empty payload: of its subscribers only the scanner's trigger acts on it.

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol/clock"
	"shingo/protocol/liveness"
	"shingocore/fulfillment"
	"shingocore/store/orders"
)

// The alarm's two sentences, by what each must contain.
var (
	scanStoppedSays = regexp.MustCompile(`^the scan worker has stopped: `)
	scanLongSays    = regexp.MustCompile(`^a scan pass is running long: `)
	scanLongNamed   = regexp.MustCompile(`^a scan pass is running long: [1-9][0-9]*s so far$`)
)

// passStore is a scanner store with no order in it, which counts the passes
// (each reads the acquiring set once) and lets a test act inside one.
type passStore struct {
	fulfillment.Store
	passes atomic.Int64
	inPass atomic.Pointer[func(n int64)]
	inGet  atomic.Pointer[func()]
}

func (s *passStore) ListAcquiringOrders() ([]*orders.Order, error) {
	n := s.passes.Add(1)
	if fn := s.inPass.Load(); fn != nil {
		(*fn)(n)
	}
	return nil, nil
}

func (s *passStore) GetOrder(int64) (*orders.Order, error) {
	if fn := s.inGet.Load(); fn != nil {
		(*fn)()
	}
	return nil, errors.New("no such order")
}

type scanRig struct {
	t        *testing.T
	e        *Engine
	store    *passStore
	stopOnce sync.Once
}

// newScanRig is a wired bare engine with its scanner started: interval is the
// safety timer's. The boot scan has finished unless booting is true.
func newScanRig(t *testing.T, interval time.Duration, booting bool) *scanRig {
	t.Helper()
	quiet := func(string, ...any) {}
	r := &scanRig{t: t, store: &passStore{}}
	r.e = &Engine{
		Events:      NewEventBus(),
		logFn:       quiet,
		stopChan:    make(chan struct{}),
		after:       newAfterQueue(quiet),
		bootScanned: make(chan struct{}),
		fulfillment: fulfillment.NewScanner(r.store, nil, nil, nil, nil, nil, nil, quiet, nil),
	}
	if !booting {
		close(r.e.bootScanned)
	}
	r.e.wireEventHandlers()
	r.e.fulfillment.StartPeriodicSweep(interval)
	t.Cleanup(r.stop)
	return r
}

func (r *scanRig) stop() { r.stopOnce.Do(r.e.Stop) }

// trigger is one of the seven events that ask for a pass.
func (r *scanRig) trigger() {
	r.e.Events.Emit(Event{Type: EventBinEnteredTransit, Payload: BinEnteredTransitEvent{}})
}

// duringPass sets what the store does inside the n-th pass.
func (r *scanRig) duringPass(fn func(n int64)) { r.store.inPass.Store(&fn) }

// holdFirstPass makes the first pass wait for release, which also runs at
// cleanup, before the rig stops. Every pass then takes passTime.
func (r *scanRig) holdFirstPass(passTime time.Duration) (held <-chan struct{}, release func()) {
	in, out := make(chan struct{}), make(chan struct{})
	r.duringPass(func(n int64) {
		if n == 1 {
			close(in)
			<-out
		}
		time.Sleep(passTime)
	})
	var once sync.Once
	release = func() { once.Do(func() { close(out) }) }
	r.t.Cleanup(release)
	return in, release
}

func (r *scanRig) await(ch <-chan struct{}, what string) {
	r.t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		r.t.Fatalf("no %s in 10 s", what)
	}
}

// quietPasses waits until no pass has started for settle, and returns how many
// ran.
func (r *scanRig) quietPasses(settle time.Duration) int64 {
	last := r.store.passes.Load()
	for {
		time.Sleep(settle)
		n := r.store.passes.Load()
		if n == last {
			return n
		}
		last = n
	}
}

// watched is the freeze alarm as main wires it over the scan worker. It asks
// through an optional method so this file compiles on an engine that has none,
// and then reads as a watch that raises nothing.
func (r *scanRig) watched() *liveness.Watch {
	w := liveness.New(r.t.Logf)
	if e, ok := any(r.e).(interface{ WatchScanWorker(*liveness.Watch) }); ok {
		e.WatchScanWorker(w)
	}
	return w
}

// awaitAlarm waits for a sentence that says want, and returns every sentence
// the alarm was raising with it.
func (r *scanRig) awaitAlarm(w *liveness.Watch, want *regexp.Regexp, within time.Duration) ([]string, bool) {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		got := liveness.Sentences(w.Frozen())
		for _, s := range got {
			if want.MatchString(s) {
				return got, true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, false
}

// neverAlarms fails if the alarm says anything in the window.
func (r *scanRig) neverAlarms(w *liveness.Watch, window time.Duration, when string) {
	r.t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if got := liveness.Sentences(w.Frozen()); len(got) > 0 {
			r.t.Fatalf("%s: the alarm said %q; want nothing", when, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// N triggers during a pass make one more pass. RED at 6bd55616, where each
// trigger spawned a pass of its own beside the rerun inside that pass: N+2.
func TestScanWorker_NTriggersDuringAPassMakeOneMorePass(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 10, 100} {
		t.Run("n="+strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			r := newScanRig(t, time.Hour, false)
			held, release := r.holdFirstPass(time.Millisecond)
			r.trigger()
			r.await(held, "first pass")
			for range n {
				r.trigger()
			}
			release()
			if got := r.quietPasses(200 * time.Millisecond); got != 2 {
				t.Errorf("%d triggers during a pass ran %d passes, want 2: that pass and one more", n, got)
			}
		})
	}
}

// What waits on the scan lock from the inbound reader waits at most two
// passes, whatever was triggered before it: an order door and the clear-home
// ask (Door), and the refill of a loader-retrieve ask (AcquireOne). RED at
// 6bd55616, where each waited behind every triggered pass (102).
func TestScanWorker_AReaderWaitsAtMostTwoPasses(t *testing.T) {
	t.Parallel()
	const n, passTime = 100, 20 * time.Millisecond
	for _, c := range []struct {
		name string
		take func(r *scanRig, got func())
	}{
		{"door", func(r *scanRig, got func()) { r.e.fulfillment.Door(got) }},
		{"loader-retrieve ask", func(r *scanRig, got func()) {
			r.store.inGet.Store(&got)
			r.e.fulfillment.AcquireOne(1)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newScanRig(t, time.Hour, false)
			held, release := r.holdFirstPass(passTime)
			r.trigger()
			r.await(held, "first pass")
			for range n {
				r.trigger()
			}
			time.Sleep(20 * time.Millisecond) // whatever the triggers queue is at the lock

			var before int64
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.take(r, func() { before = r.store.passes.Load() })
			}()
			time.Sleep(5 * time.Millisecond) // and the reader behind it
			asked := time.Now()
			release()
			r.await(done, "turn for the reader")
			wait := time.Since(asked).Round(time.Millisecond)
			t.Logf("MEASURED: %s waited %v behind %d pass(es) of %v, %d triggered", c.name, wait, before, passTime, n)
			if before > 2 {
				t.Errorf("%s waited for %d passes (%v), want at most 2: the one running and one more", c.name, before, wait)
			}
		})
	}
}

// A trigger follows its emitter's committed write: a pass that starts after the
// write reads it, also when the emitter holds the scan lock or a running pass
// read the waiting set before the write.
func TestScanWorker_ATriggersPassSeesItsEmittersWrite(t *testing.T) {
	t.Parallel()
	t.Run("written after the running pass read the waiting set", func(t *testing.T) {
		t.Parallel()
		r := newScanRig(t, time.Hour, false)
		var written atomic.Bool
		held, out, saw := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var sawOnce sync.Once
		r.duringPass(func(n int64) {
			read := written.Load()
			if n == 1 {
				close(held)
				<-out
			}
			if read {
				sawOnce.Do(func() { close(saw) })
			}
		})
		r.trigger()
		r.await(held, "first pass")
		written.Store(true)
		r.trigger()
		close(out)
		r.await(saw, "pass that read the emitter's write")
	})
	t.Run("emitted under the scan lock", func(t *testing.T) {
		t.Parallel()
		r := newScanRig(t, time.Hour, false)
		var written atomic.Bool
		saw := make(chan struct{})
		var sawOnce sync.Once
		r.duringPass(func(int64) {
			if written.Load() {
				sawOnce.Do(func() { close(saw) })
			}
		})
		r.e.fulfillment.Door(func() {
			r.trigger()
			time.Sleep(20 * time.Millisecond)
			written.Store(true) // the rest of the door, which the pass waits out
		})
		r.await(saw, "pass that read the whole door")
	})
}

// A stopped worker raises the alarm, with the scan lock free: what ran the
// passes is gone, and the probe on the lock still turns over. RED at 6bd55616,
// where a dead safety sweep was seen by nothing.
func TestScanWorker_AStoppedWorkerRaisesTheAlarm(t *testing.T) {
	t.Parallel()
	const interval = 100 * time.Millisecond
	r := newScanRig(t, interval, false)
	w := r.watched()
	died := make(chan struct{})
	r.duringPass(func(n int64) {
		if n == 1 {
			close(died)
			runtime.Goexit() // the goroutine that runs passes ends; its defers free the lock
		}
	})
	r.await(died, "pass from the safety timer")

	free := make(chan struct{})
	go func() { r.e.fulfillment.Door(func() {}); close(free) }()
	r.await(free, "turn of the scan lock")

	got, ok := r.awaitAlarm(w, scanStoppedSays, 3*time.Second)
	if !ok {
		t.Fatalf("no alarm in 3 s: what ran the passes is dead, %d pass(es) ever started at a %v timer, "+
			"and the scan lock still turns over", r.store.passes.Load(), interval)
	}
	for _, s := range got {
		if scanLongSays.MatchString(s) {
			t.Errorf("a stopped worker also read as a pass running long: %q", got)
		}
	}
}

// A pass running past the interval says so, with how long it has run, and does
// not read as a stopped worker however many triggers wait behind it. RED at
// 6bd55616, where nothing timed a pass.
func TestScanWorker_APassRunningLongSaysHowLong(t *testing.T) {
	t.Parallel()
	const interval = 100 * time.Millisecond
	r := newScanRig(t, interval, false)
	w := r.watched()
	held, release := r.holdFirstPass(0)
	r.await(held, "pass from the safety timer")
	r.trigger()

	got, ok := r.awaitAlarm(w, scanLongNamed, 3*time.Second)
	if !ok {
		t.Fatalf("no alarm in 3 s: a pass has run that long at a %v timer, and nothing says so or for how long", interval)
	}
	for _, s := range got {
		if scanStoppedSays.MatchString(s) {
			t.Errorf("a pass running long also read as a stopped worker: %q", got)
		}
	}
	release()
	deadline := time.Now().Add(3 * time.Second)
	for len(w.Frozen()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f := liveness.Sentences(w.Frozen()); len(f) > 0 {
		t.Errorf("the pass ended and the alarm still says %q", f)
	}
}

// Neither sentence for an idle healthy Core, for someone else holding the scan
// lock (the probe on the lock names that), or before the boot scan finishes.
func TestScanWorker_NeitherAlarmWhenTheWorkerIsWell(t *testing.T) {
	t.Parallel()
	const interval = 250 * time.Millisecond // room for a loaded box to wake the worker
	t.Run("idle", func(t *testing.T) {
		t.Parallel()
		r := newScanRig(t, interval, false)
		r.neverAlarms(r.watched(), 8*interval, "an idle healthy Core")
	})
	t.Run("a door holds the lock", func(t *testing.T) {
		t.Parallel()
		r := newScanRig(t, interval, false)
		w := r.watched()
		in, out := make(chan struct{}), make(chan struct{})
		t.Cleanup(func() { close(out) })
		go r.e.fulfillment.Door(func() { close(in); <-out })
		r.await(in, "door")
		r.trigger()
		r.neverAlarms(w, 8*interval, "a door held the lock")
	})
	t.Run("the boot scan runs past the interval", func(t *testing.T) {
		t.Parallel()
		r := newScanRig(t, interval, true)
		w := r.watched()
		// Set before the timer's first tick, so whichever pass runs first is held.
		held, release := r.holdFirstPass(0)
		r.e.startBootScan()
		r.await(held, "boot scan")
		r.trigger()
		r.neverAlarms(w, 8*interval, "the boot scan had not finished")
		release()
		r.await(r.e.bootScanned, "end of the boot scan")
		r.neverAlarms(w, 2*interval, "the boot scan had finished")
	})
	t.Run("a worker dies before the boot scan finishes", func(t *testing.T) {
		t.Parallel()
		r := newScanRig(t, interval, true)
		w := r.watched()
		r.duringPass(func(n int64) {
			if n == 1 {
				runtime.Goexit()
			}
		})
		r.neverAlarms(w, 8*interval, "the boot scan had not finished")
	})
}

// The alarm is judged in wall time: at 100x the timer ticks every hundredth of
// its interval, and neither an idle worker nor a pass that is shorter than the
// interval on the wall reads as stuck.
//
// Not parallel: it swaps the process default clock.
func TestScanWorker_TheAlarmIgnoresTheSimClocksSpeed(t *testing.T) {
	prev := clock.Default()
	clock.SetDefault(clock.NewRunningClock(100))
	t.Cleanup(func() { clock.SetDefault(prev) })

	r := newScanRig(t, 2*time.Second, false)
	w := r.watched()
	r.neverAlarms(w, 500*time.Millisecond, "an idle Core at 100x (50 s of sim)")
	if r.store.passes.Load() == 0 {
		t.Fatal("fixture: the timer at 100x ran no pass in 500 ms")
	}

	held, out := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(out) })
	var once sync.Once
	r.duringPass(func(int64) { once.Do(func() { close(held); <-out }) })
	r.await(held, "pass from the safety timer")
	r.neverAlarms(w, 700*time.Millisecond, "a 0.7 s pass at 100x (70 s of sim)")
}

const scanPanicEnv = "SHINGO_TEST_SCAN_PANIC"

// A panic in a pass kills the process: nothing recovers it, whoever asked for
// the pass. A worker that recovered and left its loop would be a Core that runs
// no pass.
func TestScanWorker_APanicInAPassKillsTheProcess(t *testing.T) {
	const said = "a pass panicked (test)"
	if by := os.Getenv(scanPanicEnv); by != "" {
		interval := time.Hour
		if by == "timer" {
			interval = 10 * time.Millisecond
		}
		r := newScanRig(t, interval, by == "boot")
		r.duringPass(func(int64) { panic(said) })
		switch by {
		case "trigger":
			r.trigger()
		case "boot":
			r.e.startBootScan()
		}
		time.Sleep(5 * time.Second)
		return // still alive: the parent fails
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("the path of this test binary: %v", err)
	}
	for _, by := range []string{"trigger", "timer", "boot"} {
		t.Run(by, func(t *testing.T) {
			cmd := exec.Command(exe, "-test.run=^TestScanWorker_APanicInAPassKillsTheProcess$", "-test.timeout=60s")
			cmd.Env = append(os.Environ(), scanPanicEnv+"="+by)
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "panic: "+said) {
				t.Fatalf("a pass asked for by the %s panicked and the process did not die of it (%v):\n%s", by, err, out)
			}
		})
	}
}

// Stop is clean: it returns when the running pass or boot scan has ended, so
// none reads a closed database; no pass starts after it; and it leaves no
// scanner goroutine. RED at 6bd55616, where Stop closed a channel and returned.
//
// Not parallel: it reads the stack of every goroutine.
func TestScanWorker_StopIsClean(t *testing.T) {
	for _, c := range []struct {
		name    string
		booting bool
		start   func(r *scanRig)
	}{
		{"a pass is running", false, (*scanRig).trigger},
		{"the boot scan is running", true, func(r *scanRig) { r.e.startBootScan() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newScanRig(t, time.Hour, c.booting)
			held, release := r.holdFirstPass(0)
			c.start(r)
			r.await(held, "first pass")
			r.trigger() // and one waiting

			stopped := make(chan struct{})
			go func() { r.stop(); close(stopped) }()
			select {
			case <-stopped:
				t.Errorf("Stop returned while %s: the database closes next", c.name)
			case <-time.After(200 * time.Millisecond):
			}
			release()
			r.await(stopped, "return from Stop")

			r.quietPasses(100 * time.Millisecond)
			at := r.store.passes.Load()
			r.trigger()
			time.Sleep(200 * time.Millisecond)
			if n := r.store.passes.Load(); n != at {
				t.Errorf("%d pass(es) started after Stop returned", n-at)
			}
			// A goroutine that has said it is done is still on the stack for the
			// instant it takes to return, so the read is repeated for a second.
			buf := make([]byte, 4<<20)
			var left []string
			for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
				left = left[:0]
				for _, g := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
					if strings.Contains(g, "fulfillment.(*Scanner)") {
						left = append(left, g)
					}
				}
				if len(left) == 0 || time.Now().After(deadline) {
					break
				}
			}
			for _, g := range left {
				t.Errorf("a scanner goroutine outlived Stop:\n%s", g)
			}
		})
	}
}

// Stop joins the after-queue: the work in hand finishes before Stop returns, so
// none runs on a closed database, and none queued behind it starts. Work that
// waits for the scan lock under a running pass is no deadlock: Stop holds no
// lock, the pass ends, the work takes the lock and ends. RED at 6bd55616, where
// Stop closed the channel of the queue and returned.
func TestAfterQueue_StopWaitsForTheWorkInHand(t *testing.T) {
	t.Parallel()
	r := newScanRig(t, time.Hour, false)
	go r.e.after.run(r.e.stopChan)
	held, release := r.holdFirstPass(0)
	r.trigger()
	r.await(held, "first pass")

	inHand := make(chan struct{})
	var did, next atomic.Bool
	r.e.after.later(func() {
		close(inHand)
		r.e.fulfillment.Door(func() {})
		did.Store(true)
	})
	r.e.after.later(func() { next.Store(true) })
	r.await(inHand, "work in hand")

	stopped := make(chan struct{})
	go func() { r.stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Error("Stop returned with after-queue work in hand: the database closes next")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	r.await(stopped, "return from Stop")
	if !did.Load() {
		t.Error("Stop returned before the work in hand had finished")
	}
	time.Sleep(100 * time.Millisecond) // negative assertion: the next work never starts
	if next.Load() {
		t.Error("work queued behind it started after Stop")
	}
}

// Engine.Stop gives up at one bound whatever is hung under it: here a holder of
// the scan lock, with a window pull and after-queue work waiting for that lock.
// The joins share the wait, so a hung call costs the bound once, and each that
// gave up says so in one line.
func TestEngineStop_GivesUpAtTheBound(t *testing.T) {
	t.Parallel()
	const bound = 300 * time.Millisecond
	r := newScanRig(t, time.Hour, false)
	r.e.stopWait = bound
	var gaveUp atomic.Int32
	r.e.logFn = func(format string, _ ...any) {
		if strings.Contains(format, "stop gave up") {
			gaveUp.Add(1)
		}
	}
	in, out := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(out) })
	go r.e.fulfillment.Door(func() { close(in); <-out })
	r.await(in, "hung holder of the scan lock")
	r.e.stage2.wg.Add(1)
	go func() { // a window pull, waiting to acquire
		defer r.e.stage2.wg.Done()
		r.e.fulfillment.Door(func() {})
	}()

	go r.e.after.run(r.e.stopChan)
	inHand := make(chan struct{})
	r.e.after.later(func() { close(inHand); r.e.fulfillment.Door(func() {}) })
	r.await(inHand, "after-queue work")

	stopped := make(chan struct{})
	began := time.Now()
	go func() { r.stop(); close(stopped) }()
	r.await(stopped, "return from Stop")
	if d := time.Since(began); d < bound || d > bound+5*time.Second {
		t.Errorf("Stop took %v with a bound of %v", d, bound)
	}
	if n := gaveUp.Load(); n != 2 {
		t.Errorf("Stop logged %d give-up lines, want two: the window pulls and the after-queue", n)
	}
}
