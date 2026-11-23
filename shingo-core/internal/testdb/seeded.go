package testdb

// seeded.go: templates one step past the migrated one.
//
// A fixture that seeds a whole plant costs seconds (seedCore on
// plants/master.yaml: 3.3-5.2 s a test alone, measured 2026-11-22, docket T4),
// and every test that wants that plant wants the same rows. A Seed names such a
// fixture. Its rows are written once a process into a template of the process's
// own, and each test gets a clone of it, the way Open clones the migrated
// template. Only rows are shared: each clone opens its own handle, so nothing a
// seed left cached on a handle reaches a test.
//
// BUILT BEFORE THE LEAVES. A package builds its seeds with BuildSeeded from a
// top-level test that does not call t.Parallel. go test runs every top-level
// test as a subtest of one root test (testing.runTests), and a subtest that
// calls t.Parallel waits on its parent's barrier, which tRunner closes only
// after the parent's function has returned ((*T).Parallel; go1.25.6). So every
// serial top-level test has ended before any parallel one passes its
// t.Parallel(), wherever it is declared
// (TestSeeded_ASerialSiblingEndsBeforeAParallelOneGoesOn). The build is then
// paid once, before the leaves, and no leaf holds a slot waiting on it. Not
// from TestMain: what a TestMain does before m.Run is outside the test log,
// and the reuse wrapper keys any package that declares one on the whole tree
// (shared/scripts/gatetime/reuse/testmain.go).
//
// A test that asks for a seed nobody built yet (a -run that leaves the build
// test out) builds it itself, and the tests that ask for it meanwhile wait.
//
// NEVER DROPPED HERE. A template lives as long as its server: the gate's shared
// server is removed at the end of its step (gate.sh stop_shared_pg), a container
// this process started is reaped with it, and CI's server goes with its job.
// Dropping it needs a hook after the last test, and only TestMain has one
// (above). Its name carries the pid, so no other process clones it, and a
// process given a dead one's pid drops that leftover before it builds.

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shingocore/config"
	"shingocore/store"
)

// Seed is one seeded template: Write fills a migrated database, and Key names
// what it writes. Two Seeds with one Key are one template, whichever Write runs,
// so a Key must change whenever the rows its Write writes would.
type Seed struct {
	Key   string
	Write func(*store.DB) error
}

// seededTemplate is one Key's template in this process, built once; the build's
// error and time are kept for every later caller.
type seededTemplate struct {
	name    string
	once    sync.Once
	err     error
	builtAt time.Time
	built   atomic.Bool // set after builtAt, so a reader that sees it sees builtAt
}

var (
	// seededMu guards the two maps and nothing else: a build runs with it
	// released. A plain mutex, not a lockcheck one, because a lockcheck rank is
	// declared in internal/lockorder, which product code imports; this lock is
	// a leaf and calls nothing that locks while it is held.
	seededMu    sync.Mutex
	seededAll   = map[string]*seededTemplate{} // Key -> its template
	seededNames = map[string]string{}          // template name -> Key
)

// seededFor is s's template in this process, registered on first sight and not
// yet built. Two keys whose names would collide are an error, not one template.
func seededFor(s Seed) (*seededTemplate, error) {
	seededMu.Lock()
	defer seededMu.Unlock()
	if st := seededAll[s.Key]; st != nil {
		return st, nil
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(s.Key))
	name := fmt.Sprintf("seeded_p%d_%016x", os.Getpid(), h.Sum64())
	if other, taken := seededNames[name]; taken {
		return nil, fmt.Errorf("the seeds %q and %q would share the template name %s", other, s.Key, name)
	}
	st := &seededTemplate{name: name}
	seededAll[s.Key] = st
	seededNames[name] = s.Key
	return st, nil
}

// build builds st from s's Write the first time it is asked, and returns that
// build's error to every caller.
func (st *seededTemplate) build(s Seed) error {
	st.once.Do(func() {
		// Set before the build runs, so a Write that panics or calls Goexit
		// leaves every later caller a reason and not a missing template.
		st.err = fmt.Errorf("the build of %s did not return", st.name)
		st.err = buildSeeded(st.name, s.Write)
		if st.err == nil {
			st.builtAt = time.Now()
			st.built.Store(true)
		}
	})
	return st.err
}

// BuildSeeded builds the template of every seed not built yet, side by side, and
// fails t if any did not build. For a package's serial build test (BUILT BEFORE
// THE LEAVES, above).
func BuildSeeded(t testing.TB, seeds ...Seed) {
	t.Helper()
	ensureServer(t, true)
	errs := make([]error, len(seeds))
	var wg sync.WaitGroup
	for i, s := range seeds {
		st, err := seededFor(s)
		if err != nil {
			errs[i] = err
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = st.build(s)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("seeded template for %q: %v", seeds[i].Key, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

// OpenSeeded is OpenWithConfig on a clone of s's template instead of the
// migrated one: a database of t's own holding the rows s.Write wrote, dropped at
// t's end, with the end-of-test sweep. The rows' times are the template's, not
// the clone's, so a test that reads an age against now sees the template's age
// added to it (SeededAge).
func OpenSeeded(t testing.TB, s Seed) (*store.DB, *config.DatabaseConfig) {
	t.Helper()
	ensureServer(t, true)
	return openClone(t, seededName(t, s))
}

// OpenSharedSeeded is OpenSharedWithConfig on a clone of s's template: one
// database for every test that passes key, holding s's rows, dropped when the
// last of them ends.
func OpenSharedSeeded(t testing.TB, key string, s Seed) (*store.DB, *config.DatabaseConfig) {
	t.Helper()
	st, err := seededFor(s)
	if err != nil {
		t.Fatalf("seeded template for %q: %v", s.Key, err)
	}
	return openShared(t, key, st.name, func() { seededName(t, s) })
}

// SeededAge is how long ago s's template was built in this process, and false
// if it is not built.
func SeededAge(s Seed) (time.Duration, bool) {
	seededMu.Lock()
	st := seededAll[s.Key]
	seededMu.Unlock()
	if st == nil || !st.built.Load() {
		return 0, false
	}
	return time.Since(st.builtAt), true
}

// seededName is the name of s's template, built if no one has built it; it fails
// t if the build failed.
func seededName(t testing.TB, s Seed) string {
	t.Helper()
	st, err := seededFor(s)
	if err == nil {
		err = st.build(s)
	}
	if err != nil {
		t.Fatalf("seeded template for %q: %v", s.Key, err)
	}
	return st.name
}

// buildSeeded makes name a template: a clone of the migrated template, written
// by write, then closed to connections, so any number of tests can clone it at
// once (buildTemplate's two flags, for buildTemplate's reason).
func buildSeeded(name string, write func(*store.DB) error) error {
	admin, err := adminConn()
	if err != nil {
		return fmt.Errorf("connect admin: %w", err)
	}
	ctx := context.Background()
	conn, err := admin.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin an admin session: %w", err)
	}
	defer conn.Close()

	if err := dropDatabase(ctx, conn, name); err != nil {
		return fmt.Errorf("clear a leftover %s: %w", name, err)
	}
	createSlot <- struct{}{}
	_, err = conn.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateName()))
	<-createSlot
	if err != nil {
		return fmt.Errorf("create %s: %w%s", name, err, ownContainerHint())
	}
	db, err := store.OpenWithoutMigrate(&config.DatabaseConfig{Postgres: config.PostgresConfig{
		Host: containerHost, Port: containerPort, Database: name,
		User: "test", Password: "test", SSLMode: "disable",
	}})
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	err = write(db)
	db.Close()
	if err != nil {
		return fmt.Errorf("write the seed: %w", err)
	}
	if err := terminateBackends(ctx, conn, name); err != nil {
		return err
	}
	for _, flag := range []string{"IS_TEMPLATE = true", "ALLOW_CONNECTIONS = false"} {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("ALTER DATABASE %s WITH %s", name, flag)); err != nil {
			return fmt.Errorf("set %s on %s: %w", flag, name, err)
		}
	}
	return nil
}
