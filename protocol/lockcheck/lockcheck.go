// Package lockcheck is the one type every Core and Edge lock is declared with.
//
// A production build gets the plain lock: Mutex[R] is sync.Mutex and RWMutex[R]
// is sync.RWMutex (plain.go), so no instruction, field or call differs from a
// lock declared with sync directly. A test or sim build (the lockcheck or sim
// build tag) gets the guard (guard.go): it fails loudly, with both stacks, when
// a goroutine takes a lock it already holds, or takes a lock ranked below one it
// holds.
//
// R names the lock's place in its process's lock order. Each app writes its
// order once, as rank types (shingo-core/internal/lockorder,
// shingo-edge/internal/lockorder); a lock is declared Mutex[lockorder.Scan], and
// its rank is read from R only by the guard.
package lockcheck

// Ranked is a lock's place in its process's lock order: a lock may be taken
// while holding only locks of a lower rank. Name is what a failure prints.
type Ranked interface {
	Rank() int
	Name() string
}

// Nester is a keyed lock class whose holders take several instances at once,
// always through one helper that sorts them (the Edge's node and prime locks).
// The guard lets instances of such a class be held together; it still fails a
// second take of one instance.
type Nester interface {
	Nests() bool
}
