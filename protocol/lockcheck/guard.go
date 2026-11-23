//go:build lockcheck || sim

package lockcheck

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unsafe"
)

// Enabled reports whether this build carries the guard.
const Enabled = true

// Mutex is a sync.Mutex that checks re-entry and lock order.
type Mutex[R Ranked] struct {
	mu sync.Mutex
}

func (m *Mutex[R]) id() uintptr { return uintptr(unsafe.Pointer(m)) }

// Lock checks, then takes the lock.
func (m *Mutex[R]) Lock() {
	var r R
	before(m.id(), r, false)
	m.mu.Lock()
	acquired(m.id(), r, false)
}

// TryLock takes the lock if it is free. It cannot wait, so it is not checked
// against the order; a lock it takes is held like any other.
func (m *Mutex[R]) TryLock() bool {
	if !m.mu.TryLock() {
		return false
	}
	var r R
	acquired(m.id(), r, false)
	return true
}

// Unlock releases the lock.
func (m *Mutex[R]) Unlock() {
	released(m.id(), false)
	m.mu.Unlock()
}

// RWMutex is a sync.RWMutex that checks re-entry and lock order. A read lock is
// checked like a write lock: a goroutine that read-locks twice deadlocks as soon
// as a writer waits between the two.
type RWMutex[R Ranked] struct {
	mu sync.RWMutex
}

func (m *RWMutex[R]) id() uintptr { return uintptr(unsafe.Pointer(m)) }

// Lock checks, then takes the write lock.
func (m *RWMutex[R]) Lock() {
	var r R
	before(m.id(), r, false)
	m.mu.Lock()
	acquired(m.id(), r, false)
}

// TryLock takes the write lock if it is free.
func (m *RWMutex[R]) TryLock() bool {
	if !m.mu.TryLock() {
		return false
	}
	var r R
	acquired(m.id(), r, false)
	return true
}

// Unlock releases the write lock.
func (m *RWMutex[R]) Unlock() {
	released(m.id(), false)
	m.mu.Unlock()
}

// RLock checks, then takes a read lock.
func (m *RWMutex[R]) RLock() {
	var r R
	before(m.id(), r, true)
	m.mu.RLock()
	acquired(m.id(), r, true)
}

// TryRLock takes a read lock if no writer holds the lock.
func (m *RWMutex[R]) TryRLock() bool {
	if !m.mu.TryRLock() {
		return false
	}
	var r R
	acquired(m.id(), r, true)
	return true
}

// RUnlock releases a read lock.
func (m *RWMutex[R]) RUnlock() {
	released(m.id(), true)
	m.mu.RUnlock()
}

// RLocker returns a Locker over the read lock, as sync.RWMutex does.
func (m *RWMutex[R]) RLocker() sync.Locker { return rlocker[R]{m} }

type rlocker[R Ranked] struct{ m *RWMutex[R] }

func (l rlocker[R]) Lock()   { l.m.RLock() }
func (l rlocker[R]) Unlock() { l.m.RUnlock() }

// ── the book of held locks ─────────────────────────────────────────────────

type hold struct {
	id   uintptr
	rank int
	name string
	read bool
	at   []uintptr
}

var (
	bookMu sync.Mutex
	book   = map[uint64][]hold{}
)

// fail ends the process with the report, unless this goroutine is inside Catch.
func fail(report string) {
	if _, ok := catching.Load(goid()); ok {
		panic(caught{report})
	}
	fmt.Fprint(os.Stderr, report)
	// Panicked on a fresh goroutine, so no recover on the caller's stack (the
	// event bus's, net/http's) can swallow it: the process ends here.
	done := make(chan struct{})
	go func() {
		defer close(done)
		panic(report)
	}()
	<-done
	select {}
}

// catching is the goroutines inside Catch.
var catching sync.Map

type caught struct{ report string }

// before checks a blocking take against what this goroutine already holds.
func before(id uintptr, r Ranked, read bool) {
	g := goid()
	bookMu.Lock()
	held := append([]hold(nil), book[g]...)
	bookMu.Unlock()
	for _, h := range held {
		switch {
		case h.id == id:
			fail(report("re-entry", g, h, r, read))
			return
		case h.rank == r.Rank() && nests(r):
			// A keyed class whose instances its holders take together, in
			// their own sorted order (lockcheck.go, Nester).
		case h.rank >= r.Rank():
			fail(report("order taken backwards", g, h, r, read))
			return
		}
	}
}

func nests(r Ranked) bool {
	n, ok := r.(Nester)
	return ok && n.Nests()
}

func acquired(id uintptr, r Ranked, read bool) {
	g := goid()
	pcs := make([]uintptr, 32)
	pcs = pcs[:runtime.Callers(3, pcs)]
	bookMu.Lock()
	book[g] = append(book[g], hold{id: id, rank: r.Rank(), name: r.Name(), read: read, at: pcs})
	bookMu.Unlock()
}

// released forgets a hold. A lock may be released on another goroutine than
// took it (sync allows it), so a hold this goroutine does not have is looked
// for on every goroutine.
func released(id uintptr, read bool) {
	g := goid()
	bookMu.Lock()
	defer bookMu.Unlock()
	if drop(g, id, read) {
		return
	}
	for other := range book {
		if drop(other, id, read) {
			return
		}
	}
}

func drop(g uint64, id uintptr, read bool) bool {
	hs := book[g]
	for i := len(hs) - 1; i >= 0; i-- {
		if hs[i].id == id && hs[i].read == read {
			hs = append(hs[:i], hs[i+1:]...)
			if len(hs) == 0 {
				delete(book, g)
			} else {
				book[g] = hs
			}
			return true
		}
	}
	return false
}

func report(kind string, g uint64, h hold, r Ranked, read bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nlockcheck: %s on goroutine %d: taking %s (rank %d%s) while holding %s (rank %d%s)\n",
		kind, g, r.Name(), r.Rank(), mode(read), h.name, h.rank, mode(h.read))
	fmt.Fprintf(&b, "--- %s was taken here:\n", h.name)
	writeFrames(&b, h.at)
	fmt.Fprintf(&b, "--- %s is being taken here:\n", r.Name())
	pcs := make([]uintptr, 32)
	writeFrames(&b, pcs[:runtime.Callers(4, pcs)])
	return b.String()
}

func mode(read bool) string {
	if read {
		return ", read"
	}
	return ""
}

func writeFrames(b *strings.Builder, pcs []uintptr) {
	frames := runtime.CallersFrames(pcs)
	for {
		f, more := frames.Next()
		fmt.Fprintf(b, "    %s\n        %s:%d\n", f.Function, f.File, f.Line)
		if !more {
			return
		}
	}
}

// goid is the calling goroutine's id, the number its stack trace prints
// ("goroutine 18 [running]:"). The runtime never gives it to another goroutine,
// so a hold a goroutine leaves in the book when it exits binds no later one
// (TestGuard_AHoldLeftByAnExitedGoroutineBindsNoLaterOne). It is read from the
// goroutine's g, at the word calibrate found, or else from runtime.Stack. Pin:
// TestGoid_IsTheRuntimesGoroutineID. Test and sim builds only.
func goid() uint64 {
	if goidInG {
		return gWord(goidOff)
	}
	return stackGoid()
}

// goidOff is the offset of the goroutine id in the runtime's g, and goidInG
// says calibrate found it.
var goidOff, goidInG = calibrate()

// gSpan is how many bytes of a g calibrate reads. In go1.25's runtime.g on a
// 64-bit machine the id is at byte 152 (after the stack bounds, the scheduler's
// saved registers and the status word) and the g runs past byte 400, so every
// word read should be inside the g.
const gSpan = 256

// calibrate finds the word of the runtime's g that holds the goroutine id, so
// goid can read it there instead of printing a stack header. runtime.Stack
// walks the caller's whole stack under the runtime's global print lock, and
// every guarded Lock, Unlock and check asks for the id, so parallel tests
// queue on that lock. Each probe goroutine takes its id from runtime.Stack and
// reads every word of its own g; the id's word is the one where every probe
// holds exactly its own id. None, or more than one, leaves goid on
// runtime.Stack. The offset is found at run time, never written down, so a Go
// release that moves the field moves it here too.
func calibrate() (off uintptr, ok bool) {
	if !canReadG {
		return 0, false
	}
	const probes = 8
	type probe struct {
		id    uint64
		words [gSpan / 8]uint64
	}
	ps := make([]probe, probes)
	var wg sync.WaitGroup
	for i := range ps {
		wg.Add(1)
		go func(p *probe) {
			defer wg.Done()
			p.id = stackGoid()
			for w := range p.words {
				p.words[w] = gWord(uintptr(w) * 8)
			}
		}(&ps[i])
	}
	wg.Wait()
	found := 0
	for w := range gSpan / 8 {
		every := true
		for _, p := range ps {
			if p.id == 0 || p.words[w] != p.id {
				every = false
				break
			}
		}
		if every {
			off, found = uintptr(w)*8, found+1
		}
	}
	if found != 1 {
		return 0, false
	}
	return off, true
}

// stackGoid reads the calling goroutine's id from its stack header.
func stackGoid() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	f := bytes.Fields(buf[:n])
	if len(f) < 2 {
		return 0
	}
	id, _ := strconv.ParseUint(string(f[1]), 10, 64)
	return id
}

// Catch runs fn with a failure on this goroutine reported to the caller instead
// of ending the process, and returns the report ("" when fn broke no rule). The
// take that failed does not happen. For a test that pins a rule the guard
// enforces; guard builds only. Other goroutines are not covered.
func Catch(fn func()) (report string) {
	g := goid()
	catching.Store(g, struct{}{})
	defer func() {
		catching.Delete(g)
		if r := recover(); r != nil {
			c, ok := r.(caught)
			if !ok {
				panic(r)
			}
			report = c.report
		}
	}()
	fn()
	return ""
}
