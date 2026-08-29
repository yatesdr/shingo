// Package testdb provides shared test infrastructure for shingo-core integration tests.
// It gets a Postgres server (via sync.Once), builds a pre-migrated template database
// once, then clones the template for each test instead of re-running the full
// migration stack. Both engine and dispatch tests import this package instead of
// duplicating their own container and fixture setup.
//
// THE SERVER IS SHARED ACROSS PACKAGES WHEN $SHINGO_TEST_PG SAYS SO.
//
// Every Go package is its own test process, so "one container per process" is one
// container per PACKAGE — and shingo-core has 31 packages carrying docker-tagged
// tests. MEASURED on the dev host, that fixed setup is ~5.4s per package (~3s for
// Postgres to boot, ~2.4s to replay the migration stack into the template) against
// ~0.2s of actual query work in a small package like store/admin. Serialized by
// `go test -p 1`, it was ~167s of a ~274s suite: 61% of the docker run spent
// booting Postgres and building the same schema over and over. Worse, the
// containers are not reaped until well after they are finished with (see
// reaper.go's reapSlack), so by the back half of a run ~20 of them are competing
// for one Docker daemon and packages inflate several-fold — shingocore/uop
// measured 36.5s inside the suite against ~4.5s run on its own.
//
// So scripts/gate.sh now starts ONE Postgres for the whole docker step and puts
// its address in $SHINGO_TEST_PG. Processes that see it skip container creation
// entirely and share both the server and — under an advisory lock, see
// ensureTemplate — a single template build. Per-package setup drops from ~5.4s to
// the price of one CREATE DATABASE ... TEMPLATE, which is a file copy.
//
// With the variable unset, everything below behaves exactly as it did: this
// process creates and owns a container of its own. That is still the path for a
// hand-run of a single package, and it is why the container code and the reaper
// are still here.
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/go-connections/nat"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

const (
	// envSharedPG names a Postgres server, "host:port", that this process
	// should use instead of creating a container. scripts/gate.sh sets it for
	// the whole docker step. Unset means "create your own container", which is
	// what a hand-run of one package does.
	//
	// Credentials are NOT configurable: the server must carry the same
	// test/test that the container path creates, because that is what
	// adminConn and every cfg built here hand to pgx. One shape of connection
	// string, not two.
	envSharedPG = "SHINGO_TEST_PG"

	// envSharedTemplate overrides the template database name. gate.sh sets it
	// to a value unique per run, which is what keeps a server that outlives a
	// run from serving a template built by an older tree. See templateName.
	envSharedTemplate = "SHINGO_TEST_PG_TEMPLATE"

	// templateDBName is the default name of the pre-migrated database every
	// test gets cloned from. Must be a valid Postgres identifier; underscores
	// only.
	templateDBName = "template_test"

	// envRequireDocker, when set (any value), turns "docker is down, skip the
	// integration tests" into a FAILURE. The gate and the Sunday smoke set it:
	// their whole point is to run the docker suites, and a run where 327 files
	// skipped together exits 0 — "green" and "docker was down and nothing ran"
	// are indistinguishable from the exit code alone (the 2026-08-15 audit's
	// unit 2a, found the hard way: the docker step was green while Docker
	// Desktop was still starting).
	//
	// Unset (the default, and what a hand-run of one package sees) keeps the
	// old skip behavior — a developer without Docker still gets a useful unit
	// run rather than a wall of failures.
	envRequireDocker = "SHINGO_TEST_REQUIRE_DOCKER"
)

// dockerDownReported keeps the down-sentinel to one line per process: every
// docker-tagged test in the package funnels through the same two arms below, and
// a package with a hundred of them should not print a hundred identical lines.
var dockerDownReported atomic.Bool

// dockerDownSentinel is the exact line noteDockerDown prints. A function
// rather than an inline format so the pure test pins the REAL string the
// runtime emits — a copy in the test would keep passing while the runtime's
// line drifted, which is a fence that matches nothing.
//
// The prefix is a CONTRACT: scripts/gate.sh greps `^SHINGO-DOCKER-DOWN`
// against every module's log. Drift it and the gate's blind-spot check goes
// blind itself.
func dockerDownSentinel(err error) string {
	return fmt.Sprintf("SHINGO-DOCKER-DOWN: skipping integration tests: %v", err)
}

// noteDockerDown emits the sentinel the smoke and the gate log-grep for. A
// SKIP is invisible in non-verbose `go test` output — skipped tests print
// nothing — so the one line that says what happened has to come from here, to
// stderr, which `go test` passes through even non-verbose.
//
// The line's shape is pinned by TestRequireDockerSentinelShape (pure test): if
// the wording changes, the smoke's grep changes with it or the instrument goes
// quietly blind, which is the exact failure mode it exists to prevent.
func noteDockerDown(t testing.TB, err error) {
	if dockerDownReported.CompareAndSwap(false, true) {
		fmt.Fprintln(os.Stderr, dockerDownSentinel(err))
	}
}

// templateName is the database every test is cloned from.
//
// WHAT STOPS A STALE TEMPLATE BEING REUSED depends on which path is running,
// and the fixed default is safe on both of the paths that exist today:
//
//   - Own container (no $SHINGO_TEST_PG): the server is created empty moments
//     earlier, so there is never a template to be stale.
//   - scripts/gate.sh: it passes an explicit per-run name, so a run always
//     builds its own and never inherits one.
//
// The gap is a THIRD path nobody is on yet: exporting $SHINGO_TEST_PG by hand
// at a server kept up across branches, where a fixed name would let a tree with
// a new migration clone a template built without it. TestTemplateDB_HasAllSchema
// in this package is the backstop — it compares the template's applied head
// against the migration list this build defines — but it only fires in a run
// that includes this package. If long-lived shared servers ever become a normal
// way to work, key this name on the schema rather than adding a convention
// about when to drop the database by hand.
//
// NOT KEYED ON store.LatestMigrationVersion(), which reads as the obvious
// answer and is a trap: that value is a side effect of running migrations
// (migrations.go sets it inside runVersionedMigrations), so in exactly the
// processes that matter here — the ones that cloned a ready template and never
// migrated anything — it is 0.
func templateName() string {
	if n := os.Getenv(envSharedTemplate); n != "" {
		return sanitizeIdent(n, templateDBName)
	}
	return templateDBName
}

// ownContainerHint is the sentence a connect failure needs and did not have.
//
// A connect or CREATE DATABASE failure here has two completely different causes
// and the error text alone cannot tell them apart. Against the GATE's shared
// server it means something is actually wrong. Against a container this process
// started for itself, the overwhelmingly likely cause is CONTENTION: a bare
// `go test -tags docker ./...` gives every package its own Postgres, a dozen of
// them start at once, and the losers report "failed SASL auth: timeout" or a
// context deadline — which reads exactly like a code failure and is not one.
//
// Measured 2026-08-16: a hand-run of the module produced a different set of
// five-to-eight failures on every pass, none reproducible in isolation, while
// `gate.sh docker` — one shared server, -p 4 — was clean on the same tree. That
// cost half an hour of looking for a bug that was not there, which is the whole
// reason this sentence exists. The intermittent has been on the ledger's watch
// list since R.37 and this is what it actually was.
//
// Silent when the gate set SHINGO_TEST_PG: there the failure is real and a hint
// pointing at the thing already in use would be noise.
func ownContainerHint() string {
	if os.Getenv(envSharedPG) != "" {
		return ""
	}
	return "\n\nNOTE: this process started its own Postgres container because " + envSharedPG +
		" is unset. Running the whole module that way gives EVERY package its own server, and " +
		"under that load connect timeouts are expected and non-deterministic — they are not a " +
		"finding about the code. Use `bash scripts/gate.sh docker`, which points every package at " +
		"one shared server and caps package parallelism. A single package by hand is fine as-is."
}

// sanitizeIdent forces s into something safe to splice into `CREATE DATABASE
// %s`, falling back to fallback if nothing usable survives.
//
// THIS IS NOT PARANOIA, IT IS A BUG THAT SHIPPED. The name arrives from
// $SHINGO_TEST_PG_TEMPLATE and every use of it is string interpolation into
// SQL, unquoted. A CI change built the value out of a cache-key prefix that
// contained hyphens, and Postgres answered `syntax error at or near "-"` for
// every one of twelve shards — a failure that looks like the test suite
// breaking and is actually a database name.
//
// Sanitising rather than rejecting, because the caller is CI infrastructure
// and a slightly-renamed template is harmless: the name only has to be a
// stable, unique handle. What is not harmless is a name that cannot be created
// at all. Leading digits are prefixed too, since an identifier may not start
// with one.
func sanitizeIdent(s, fallback string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32) // Postgres folds unquoted identifiers to lower
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		return fallback
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "t_" + out
	}
	// Postgres truncates identifiers at 63 bytes; truncating here keeps the
	// name we ask for identical to the one we later look up in pg_database.
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

// containerState holds the shared Postgres container started once per test process.
// containerID is recorded so a test can read the labels back off the container
// this process actually created — see reaper.go for why they matter.
var (
	containerOnce sync.Once
	containerHost string
	containerPort int
	containerID   string
	containerErr  error
)

// ContainerID returns the Docker ID of this process's shared Postgres
// container, or "" before startContainer has run. Exported for the reaper
// test, which asserts the container carries the labels it is reaped by.
func ContainerID() string { return containerID }

// templateState gates one-time template-DB construction. setupTemplate runs
// migrations against templateDBName and then marks it IS_TEMPLATE=true so
// later CREATE DATABASE ... TEMPLATE template_test calls are file-level
// copies that skip migrations entirely.
var (
	templateOnce sync.Once
	templateErr  error
)

// Counters for trigger thresholds (see wave-2 plan triggers #2, #3):
//   - testDBsCreated: total number of per-test databases cloned from the template
//   - terminateFired: number of t.Cleanup paths that had to fall back to
//     pg_terminate_backend because DROP DATABASE hit "database is being
//     accessed by other users." Connection leak indicator.
var (
	testDBsCreated int64
	terminateFired int64
)

// TestDatabasesCreated returns the number of test databases cloned so far.
// Exported for the smoke test that checks pg_terminate_backend firing rate.
func TestDatabasesCreated() int64 { return atomic.LoadInt64(&testDBsCreated) }

// TerminateBackendFired returns the number of cleanup paths that had to
// fall back to pg_terminate_backend. >5% of TestDatabasesCreated indicates
// a connection leak somewhere in production code (the test pool isn't
// draining before DROP).
func TerminateBackendFired() int64 { return atomic.LoadInt64(&terminateFired) }

// startContainer spins up a single Postgres container for the entire test process.
// Called via sync.Once — all tests share this container but get their own database.
//
// Retries up to 3 times on transient failures: Host/MappedPort errors and the
// port=0 race that surfaces when many packages spin up containers in parallel
// (Docker port-mapping appears to lag behind container ready state). Errors
// used to be swallowed and produced confusing "lookup port=0" failures
// downstream — they now propagate via containerErr.
func startContainer() {
	// If anything inside panics — testcontainers occasionally does under
	// load — sync.Once still marks this Once done. Subsequent Open()
	// callers would otherwise find containerErr==nil and containerHost=="",
	// and fail downstream with "container vars not set" errors. Capture
	// the panic into containerErr first, then re-panic so the panicking
	// test sees the original failure.
	defer func() {
		if r := recover(); r != nil {
			if containerErr == nil {
				containerErr = fmt.Errorf("startContainer panic: %v", r)
			}
			panic(r)
		}
	}()
	const attempts = 3
	ctx := context.Background()

	// A server was handed to us, so there is no container to create, no
	// container to reap, and nothing for this process to own. Returning here
	// deliberately leaves containerID empty: this process did not create a
	// container and must never terminate the shared one.
	if addr := os.Getenv(envSharedPG); addr != "" {
		containerErr = useSharedServer(ctx, addr)
		return
	}

	// Clear other processes' abandoned containers BEFORE creating ours, so
	// this process can never be a candidate for its own reap. Best effort:
	// see reaper.go.
	reapOrphansBestEffort()

	var lastErr error
	for i := 0; i < attempts; i++ {
		container, err := postgres.Run(ctx, "postgres:16-alpine",
			postgres.WithDatabase("postgres"),
			postgres.WithUsername("test"),
			postgres.WithPassword("test"),
			// Declare, at creation time, when this container's creator is
			// provably gone. Stamped per attempt so a retry re-derives it
			// from the current clock rather than inheriting a stale one.
			testcontainers.CustomizeRequest(testcontainers.GenericContainerRequest{
				ContainerRequest: testcontainers.ContainerRequest{
					Labels: containerLabels(time.Now()),
				},
			}),
			// Wait for BOTH the postgres ready log AND the mapped host port to be
			// listening. Log-only waits caused MappedPort to return 0 under
			// heavy parallelism — the container had crossed the log threshold
			// but the host-side port forwarding hadn't completed yet.
			testcontainers.WithWaitStrategy(
				wait.ForAll(
					wait.ForLog("database system is ready to accept connections").
						WithOccurrence(2).
						WithStartupTimeout(60*time.Second),
					wait.ForListeningPort(nat.Port("5432/tcp")).
						WithStartupTimeout(60*time.Second),
				),
			),
		)
		if err != nil {
			lastErr = fmt.Errorf("postgres.Run: %w", err)
			continue
		}
		host, hostErr := container.Host(ctx)
		port, portErr := container.MappedPort(ctx, "5432")
		if hostErr == nil && portErr == nil && host != "" && port.Int() != 0 {
			containerHost = host
			containerPort = port.Int()
			containerID = container.GetContainerID()
			return
		}
		lastErr = fmt.Errorf("container host/port not ready (hostErr=%v host=%q portErr=%v portVal=%d)", hostErr, host, portErr, port.Int())
		_ = container.Terminate(ctx)
	}
	containerErr = fmt.Errorf("start container after %d attempts: %w", attempts, lastErr)
}

// useSharedServer points this process at an already-running Postgres named by
// $SHINGO_TEST_PG and waits for it to answer.
//
// The wait is not redundant with the one in gate.sh. gate.sh blocks on
// pg_isready before it runs anything, so in the normal case the first ping here
// succeeds immediately and this costs nothing. It is here for the case gate.sh
// cannot cover — a developer exporting the variable at a server that is still
// coming up — where the alternative is every package in the run failing at once
// with a connection-refused that reads like a code failure rather than a
// not-yet-listening one.
//
// A bad address fails fast and does NOT fall back to creating a container.
// Falling back would turn "your shared server is misconfigured" into "the suite
// is mysteriously as slow as it used to be", which is the kind of silence that
// takes a day to notice.
func useSharedServer(ctx context.Context, addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s=%q is not host:port: %w", envSharedPG, addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return fmt.Errorf("%s=%q has an unusable port", envSharedPG, addr)
	}
	containerHost, containerPort = host, port

	admin, err := adminConn()
	if err != nil {
		return fmt.Errorf("connect shared postgres at %s: %w", addr, err)
	}
	defer admin.Close()

	deadline := time.Now().Add(30 * time.Second)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = admin.PingContext(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			containerHost, containerPort = "", 0
			return fmt.Errorf("shared postgres at %s never answered: %w", addr, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// adminConn returns a connection to the server's default "postgres"
// database, used for CREATE/DROP DATABASE and template metadata changes.
func adminConn() (*sql.DB, error) {
	return sql.Open("pgx", fmt.Sprintf(
		"host=%s port=%d dbname=postgres user=test password=test sslmode=disable",
		containerHost, containerPort))
}

// setupTemplate makes templateName() exist and be a Postgres template, so that
// CREATE DATABASE ... TEMPLATE calls are file copies that skip migrations.
func setupTemplate() {
	if containerErr != nil {
		templateErr = containerErr
		return
	}
	if err := ensureTemplate(); err != nil {
		templateErr = err
	}
}

// ensureTemplate builds the template unless it is already there.
//
// ACROSS PROCESSES, NOT JUST WITHIN ONE. templateOnce makes the build happen
// once per process; on a shared server that is still once per PACKAGE, which is
// the ~2.4s x 31 this change exists to remove. The cross-process interlock is a
// Postgres advisory lock, which is the right primitive here because the thing
// being coordinated IS the database server — no lock file, no directory, no
// second source of truth about whether the template is ready.
//
// The lock is held on a PINNED CONNECTION. Advisory locks are session-scoped
// and database/sql hands out an arbitrary pooled connection per call, so taking
// the lock through *sql.DB can unlock on a different session than it locked
// on — which does not error, it just silently fails to hold anything.
//
// Losers of the race do not build and do not wait on a poll loop: they block in
// pg_advisory_lock until the winner finishes, then see the finished template.
func ensureTemplate() error {
	if containerHost == "" || containerPort == 0 {
		return fmt.Errorf("server vars not set: host=%q port=%d (startContainer didn't populate)", containerHost, containerPort)
	}
	admin, err := adminConn()
	if err != nil {
		return fmt.Errorf("connect admin for template build: %w", err)
	}
	defer admin.Close()

	ctx := context.Background()
	conn, err := admin.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin admin session for template build: %w", err)
	}
	defer conn.Close()

	name := templateName()
	key := advisoryKey(name)
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		return fmt.Errorf("acquire template build lock: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, key) }()

	ready, err := templateReady(ctx, conn, name)
	if err != nil {
		return err
	}
	if ready {
		return nil
	}
	return buildTemplate(ctx, conn, name)
}

// advisoryKey turns a template name into the bigint pg_advisory_lock wants.
// Hashed in Go rather than with Postgres's hashtext() because hashtext is an
// undocumented internal whose value is not promised to be stable across major
// versions — and a lock key that changes under us is a lock that stops
// excluding anything.
func advisoryKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("shingo.testdb.template:"))
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64())
}

// fnvHash is the shared-DB name suffix: a stable short digest of the sharing
// key, so two keys that sanitize() would collapse stay distinct in the
// database name.
func fnvHash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// templateReady reports whether name exists AND is a finished template.
//
// datistemplate IS THE READINESS FLAG, not a detail of how the template is
// used. buildTemplate sets it last, after the migrations have run and the
// database has been renamed into place, so there is no window in which a
// half-migrated database answers true here. A plain "does the name exist"
// check would have exactly that window, and the process that lost the race
// would clone a database whose schema build was still in flight.
func templateReady(ctx context.Context, conn *sql.Conn, name string) (bool, error) {
	var ready bool
	err := conn.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1 AND datistemplate)`,
		name).Scan(&ready)
	if err != nil {
		return false, fmt.Errorf("check template %s: %w", name, err)
	}
	return ready, nil
}

// buildTemplate runs the full migration stack into a staging database, then
// renames it into place and flips it to template mode. Caller must hold the
// advisory lock for name.
//
// BUILD-THEN-RENAME, not build-in-place. A process killed partway through the
// migration stack — Ctrl-C, a test timeout, the OOM killer — leaves a database
// carrying some prefix of the schema. Built in place under the real name, that
// wreckage is what the next run finds, and since it would be a plain database
// rather than a template, every later CREATE DATABASE ... TEMPLATE against it
// fails on a server nobody can explain. Staging confines the wreckage to a name
// nothing looks for, and the rename is the single step that publishes it.
func buildTemplate(ctx context.Context, conn *sql.Conn, name string) error {
	staging := fmt.Sprintf("%s_building_%d", name, os.Getpid())
	if err := dropDatabase(ctx, conn, staging); err != nil {
		return fmt.Errorf("clear stale staging db: %w", err)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", staging)); err != nil {
		return fmt.Errorf("create staging database: %w", err)
	}

	// Run migrations via the production Open path, so the template is built by
	// the same code a plant runs rather than by a copy of its SQL.
	tmplDB, err := store.Open(&config.DatabaseConfig{
		Postgres: config.PostgresConfig{
			Host:     containerHost,
			Port:     containerPort,
			Database: staging,
			User:     "test",
			Password: "test",
			SSLMode:  "disable",
		},
	})
	if err != nil {
		return fmt.Errorf("open + migrate template: %w", err)
	}
	tmplDB.Close()

	// Pool close above is best-effort; RENAME refuses while any session is
	// attached, so evict explicitly rather than racing the pool's teardown.
	if err := terminateBackends(ctx, conn, staging); err != nil {
		return err
	}

	// A previous run may have died between rename and the IS_TEMPLATE flip,
	// leaving a plain database under the real name. templateReady said no, so
	// whatever is sitting there is not a usable template — clear it.
	if err := dropDatabase(ctx, conn, name); err != nil {
		return fmt.Errorf("clear unusable database %s: %w", name, err)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("ALTER DATABASE %s RENAME TO %s", staging, name)); err != nil {
		return fmt.Errorf("publish template as %s: %w", name, err)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("ALTER DATABASE %s WITH IS_TEMPLATE = true", name)); err != nil {
		return fmt.Errorf("mark template: %w", err)
	}
	// ALLOW_CONNECTIONS = false is what lets many processes clone this template
	// at the same time: CREATE DATABASE ... TEMPLATE refuses while any session
	// is connected to the source, and the reliable way to guarantee none is to
	// make connecting impossible.
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS = false", name)); err != nil {
		return fmt.Errorf("disallow template connections: %w", err)
	}
	return nil
}

// dropDatabase removes name if it is there, clearing the template flags first
// so a template drops as readily as a plain database.
func dropDatabase(ctx context.Context, conn *sql.Conn, name string) error {
	// Both ALTERs fail on a database that does not exist, which is the common
	// case and not an error worth propagating — the DROP below is the step
	// whose failure means something.
	_, _ = conn.ExecContext(ctx, fmt.Sprintf("ALTER DATABASE %s WITH IS_TEMPLATE = false", name))
	_, _ = conn.ExecContext(ctx, fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS = true", name))
	if err := terminateBackends(ctx, conn, name); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", name)); err != nil {
		return fmt.Errorf("drop %s: %w", name, err)
	}
	return nil
}

// terminateBackends evicts every session attached to name except this one.
func terminateBackends(ctx context.Context, conn *sql.Conn, name string) error {
	if _, err := conn.ExecContext(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
		name); err != nil {
		return fmt.Errorf("terminate backends on %s: %w", name, err)
	}
	return nil
}

// Open returns a *store.DB connected to a fresh database cloned from the
// pre-migrated template. Each call creates a new database (test_<random>)
// so tests are fully isolated. The database and connection are cleaned up
// via t.Cleanup.
//
// If Docker is not running, the test is skipped (not failed).
// Open accepts testing.TB (not *testing.T) so benchmarks can clone the
// template database the same way tests do — every method it uses (Helper,
// Skipf, Fatalf, Name, Cleanup) is on the TB interface, and a *testing.T
// still satisfies it, so existing callers are unaffected.
func Open(t testing.TB) *store.DB {
	db, _ := OpenWithConfig(t)
	return db
}

// OpenWithConfig is Open plus the config that reaches the same database.
//
// It exists for one kind of test: a data migration. Open hands back a database
// whose migrations have already run, so there is no way to put "before" rows in
// front of one. With the config, a test can seed the old shape and then call
// store.Open again — which re-runs the versioned migrations against the same
// database, verify and self-heal included, rather than a copy of their SQL.
func OpenWithConfig(t testing.TB) (*store.DB, *config.DatabaseConfig) {
	t.Helper()

	// Guard against Docker panics from testcontainers
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprint(r)
			if strings.Contains(strings.ToLower(msg), "docker") {
				noteDockerDown(t, fmt.Errorf("%s", msg))
				if os.Getenv(envRequireDocker) != "" {
					t.Fatalf("SHINGO-DOCKER-DOWN (required): %s", msg)
				}
				t.Skipf("skipping integration test: %s", msg)
			}
			panic(r)
		}
	}()

	containerOnce.Do(startContainer)
	if containerErr != nil {
		if strings.Contains(strings.ToLower(containerErr.Error()), "docker") {
			noteDockerDown(t, containerErr)
			if os.Getenv(envRequireDocker) != "" {
				t.Fatalf("SHINGO-DOCKER-DOWN (required): start postgres container: %v", containerErr)
			}
			t.Skipf("skipping integration test: %v", containerErr)
		}
		t.Fatalf("start postgres container: %v", containerErr)
	}

	templateOnce.Do(setupTemplate)
	if templateErr != nil {
		t.Fatalf("setup template database: %v", templateErr)
	}

	// THE PID IS IN THE NAME BECAUSE THE SERVER IS SHARED. Test names are
	// unique within a package and nothing more: TestCoverage_… shapes repeat
	// across store/admin, store/audit and their neighbours, and once every
	// package clones into one server those namesakes are competing for one
	// database name behind a 1-in-100k random suffix. Per-process qualification
	// makes the collision impossible instead of unlikely. Worst case is 5 + 40
	// (sanitize's cap) + 2 + pid + 1 + 5, inside Postgres's 63-byte identifier
	// limit.
	dbName := fmt.Sprintf("test_%s_p%d_%d", sanitize(t.Name()), os.Getpid(), rand.Intn(100000))

	admin, err := adminConn()
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", dbName, templateName())); err != nil {
		t.Fatalf("create test database %s from template: %v%s", dbName, err, ownContainerHint())
	}
	atomic.AddInt64(&testDBsCreated, 1)

	cfg := &config.DatabaseConfig{
		Postgres: config.PostgresConfig{
			Host:     containerHost,
			Port:     containerPort,
			Database: dbName,
			User:     "test",
			Password: "test",
			SSLMode:  "disable",
		},
	}
	db, err := store.OpenWithoutMigrate(cfg)
	if err != nil {
		t.Fatalf("open test db %s: %v%s", dbName, err, ownContainerHint())
	}

	t.Cleanup(func() {
		db.Close()
		// Best-effort drop on a fresh connection. If DROP fails because
		// connections are still alive, fall back to pg_terminate_backend
		// and retry — and bump the counter so the smoke test can flag
		// connection-leak rates above 5% of TestDatabasesCreated.
		cleanup, err := adminConn()
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, dropErr := cleanup.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbName))
		if dropErr == nil {
			return
		}
		if strings.Contains(strings.ToLower(dropErr.Error()), "is being accessed") {
			cleanup.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName)
			cleanup.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbName))
			atomic.AddInt64(&terminateFired, 1)
		}
	})

	// ── THE END-OF-TEST INVARIANT SWEEP ──────────────────────────────────────
	//
	// REGISTERED SECOND SO IT RUNS FIRST. Cleanups are LIFO, and the drop above
	// takes the database with it; a sweep behind it would have nothing to read.
	//
	// It is here rather than at each test because the wedge is not a property any
	// one test is about. It is what happens when a write clears one ownership
	// book and not another, and the writes that can do that are all over the
	// tree: a steal, a release, a redirect, a recovery. A test that exercises one
	// of them pays this automatically, which is the only way an invariant with
	// that many potential authors gets checked at all.
	//
	// SKIPPED FOR A TEST THAT HAS ALREADY FAILED. Its database is mid-scenario by
	// definition, and a second finding on top of the first is noise pointing at
	// the wrong write.
	//
	// SKIPPED FOR A DATABASE THAT CANNOT ANSWER. A handful of tests break their
	// own database on purpose — closing the handle, or dropping a column — to prove
	// a reader fails closed instead of fabricating an answer. A sweep that fataled
	// on "database is closed" or "column does not exist" would report those as
	// findings, which is the fastest way to get an invariant check deleted. The
	// probe below is the difference between "no wedge" and "no answer"; the
	// exported assertion still fatals, because a caller who asks by name wants to
	// know its question did not run.
	//
	// A test that MANUFACTURES the wedge, or writes an order row that is not a
	// plant state at all, calls DisableWedgeSweep and says why.
	t.Cleanup(func() {
		if t.Failed() || wedgeSweepDisabled(t) {
			return
		}
		if db.DB.Ping() != nil {
			return
		}
		if _, err := db.DB.Exec(`SELECT id, bin_id, status FROM orders LIMIT 1`); err != nil {
			return
		}
		AssertNoPointerWedge(t, db)
	})
	return db, cfg
}

// wedgeSweepOff records the tests that build the pointer wedge on purpose.
var (
	wedgeSweepMu  sync.Mutex
	wedgeSweepOff = map[string]bool{}
)

// DisableWedgeSweep opts one test out of the end-of-test wedge sweep.
//
// FOR A TEST WHOSE ORDER ROWS ARE NOT A PLANT STATE, and nothing else. Three
// kinds qualify: one that pins the detector itself, one that arranges the broken
// state so a repair has something to repair, and one whose rows are probe values
// rather than a scenario (the column round-trip census stamps a bin_id because
// bin_id is a column, not because anything is sourcing).
//
// Every other failure of the sweep is a finding — a write that cleared one
// ownership book and not another — and silencing it here would delete the only
// automated notice of the shape.
func DisableWedgeSweep(t testing.TB, why string) {
	t.Helper()
	if why == "" {
		t.Fatal("DisableWedgeSweep needs a reason: the opt-out is only for a test that builds the wedge deliberately")
	}
	wedgeSweepMu.Lock()
	wedgeSweepOff[t.Name()] = true
	wedgeSweepMu.Unlock()
	t.Cleanup(func() {
		wedgeSweepMu.Lock()
		delete(wedgeSweepOff, t.Name())
		wedgeSweepMu.Unlock()
	})
}

// KnownPointerWedge quarantines a test whose end state is a REAL wedge produced
// by production code that has not been fixed yet.
//
// SEPARATE FROM DisableWedgeSweep ON PURPOSE, and the separation is the whole
// value: an opt-out that means "this row is not a plant state" and an opt-out
// that means "the plant does this and it is wrong" look identical once they are
// both a skip, and the second kind is a defect nobody can find again. This one
// is greppable, it takes the finding's name, and every call is an item of work.
//
// It exists because an invariant that lands on a tree with pre-existing
// violations has two options, and "do not land the invariant" is the worse one:
// everything the gate would have caught from here on goes uncaught while the
// known defect waits for its owner.
func KnownPointerWedge(t testing.TB, finding string) {
	t.Helper()
	if finding == "" {
		t.Fatal("KnownPointerWedge needs the finding: a quarantine nobody can name is a skip")
	}
	t.Logf("KNOWN POINTER WEDGE (quarantined, not fixed): %s", finding)
	DisableWedgeSweep(t, finding)
}

func wedgeSweepDisabled(t testing.TB) bool {
	wedgeSweepMu.Lock()
	defer wedgeSweepMu.Unlock()
	return wedgeSweepOff[t.Name()]
}

// sharedDBs tracks the per-key databases handed out by OpenShared. One clone
// per key per process; every test in the process using the same key operates
// on the same *store.DB. The entry is retired when the last test using it
// finishes (see releaseShared), so a file's database never outlives the run.
var (
	sharedMu    sync.Mutex
	sharedDBs   = map[string]*store.DB{}
	sharedNames = map[string]string{} // key -> database name, for the drop
	sharedCfgs  = map[string]*config.DatabaseConfig{}
	sharedRefs  = map[string]int{} // key -> live tests holding it
)

// OpenShared returns a *store.DB shared by every test that passes the same
// key. The first call for a key clones the template once; every later call —
// including from parallel tests in the same file — gets the SAME database.
// The database is closed and dropped when the LAST test using the key
// finishes (refcounted t.Cleanup), not per test.
//
// This is why a shared-key file must not contain DDL mutators
// (ALTER/RENAME), mid-test db.Close, or asserts over global unscoped state —
// see the dispatch OpenShared lint guard.
//
// The intended key is the test FILE (testFileKey()), so "shared" means
// "one database per file, all tests in the file on it". A hand-written key is
// allowed but must be stable, not derived from t.Name().
//
// A test's fixture writes persist for its file-siblings. Tests must
// therefore use unique fixture names (they already do: per-test prefixes
// like "DWFULL-", "CCC-"), not shared constants.
func OpenShared(t testing.TB, key string) *store.DB {
	db, _ := OpenSharedWithConfig(t, key)
	return db
}

// OpenSharedWithConfig is OpenShared plus the config reaching the same
// database, for the same migration-test niche as OpenWithConfig.
func OpenSharedWithConfig(t testing.TB, key string) (*store.DB, *config.DatabaseConfig) {
	t.Helper()

	// Docker-panic guard, same shape as OpenWithConfig's.
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprint(r)
			if strings.Contains(strings.ToLower(msg), "docker") {
				noteDockerDown(t, fmt.Errorf("%s", msg))
				if os.Getenv(envRequireDocker) != "" {
					t.Fatalf("SHINGO-DOCKER-DOWN (required): %s", msg)
				}
				t.Skipf("skipping integration test: %s", msg)
			}
			panic(r)
		}
	}()

	sharedMu.Lock()
	if db, ok := sharedDBs[key]; ok {
		cfg := sharedCfgs[key] // read under the lock; the maps are written by releaseShared and sibling opens
		sharedRefs[key]++
		sharedMu.Unlock()
		t.Cleanup(func() { releaseShared(key) })
		return db, cfg
	}
	sharedMu.Unlock()

	// Same startup sequence as OpenWithConfig — shared container, shared
	// template — then diverge: one clone per key, refcounted lifetime
	// instead of per-test drop.
	containerOnce.Do(startContainer)
	if containerErr != nil {
		if strings.Contains(strings.ToLower(containerErr.Error()), "docker") {
			noteDockerDown(t, containerErr)
			if os.Getenv(envRequireDocker) != "" {
				t.Fatalf("SHINGO-DOCKER-DOWN (required): start postgres container: %v", containerErr)
			}
			t.Skipf("skipping integration test: %v", containerErr)
		}
		t.Fatalf("start postgres container: %v", containerErr)
	}
	templateOnce.Do(setupTemplate)
	if templateErr != nil {
		t.Fatalf("setup template database: %v", templateErr)
	}

	// Re-check under the lock after the once-gates: a sibling test may have
	// cloned this key while we were waiting on the template.
	sharedMu.Lock()
	if db, ok := sharedDBs[key]; ok {
		cfg := sharedCfgs[key] // under the lock, same as the fast path above
		sharedRefs[key]++
		sharedMu.Unlock()
		t.Cleanup(func() { releaseShared(key) })
		return db, cfg
	}

	// The database name needs the file's IDENTITY, not its path: sanitize()
	// caps at 40 chars, and on this tree every dispatch file's absolute path
	// shares those first 40 chars — different keys, one name, 42P04 on the
	// second clone. Basename keeps it readable; the fnv hash of the full key
	// keeps it unique.
	dbName := fmt.Sprintf("shared_%s_%x_p%d", sanitize(filepath.Base(key)), fnvHash(key), os.Getpid())
	admin, err := adminConn()
	if err != nil {
		sharedMu.Unlock()
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", dbName, templateName())); err != nil {
		sharedMu.Unlock()
		t.Fatalf("create shared database %s: %v%s", dbName, err, ownContainerHint())
	}
	atomic.AddInt64(&testDBsCreated, 1)

	cfg := &config.DatabaseConfig{
		Postgres: config.PostgresConfig{
			Host:     containerHost,
			Port:     containerPort,
			Database: dbName,
			User:     "test",
			Password: "test",
			SSLMode:  "disable",
		},
	}
	db, err := store.OpenWithoutMigrate(cfg)
	if err != nil {
		sharedMu.Unlock()
		t.Fatalf("open shared db %s: %v%s", dbName, err, ownContainerHint())
	}
	sharedDBs[key] = db
	sharedNames[key] = dbName
	sharedCfgs[key] = cfg
	sharedRefs[key] = 1
	t.Cleanup(func() { releaseShared(key) })
	sharedMu.Unlock()
	return db, cfg
}

// releaseShared decrements the key's refcount; the caller that takes it to
// zero closes and drops the shared database. Registered per-test by
// OpenSharedWithConfig, so the drop lands when the last test in the file
// finishes — no process-exit hook needed (Go test binaries have none).
func releaseShared(key string) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	n := sharedRefs[key]
	if n > 1 {
		sharedRefs[key] = n - 1
		return
	}
	delete(sharedRefs, key)
	db := sharedDBs[key]
	name := sharedNames[key]
	delete(sharedDBs, key)
	delete(sharedNames, key)
	delete(sharedCfgs, key)
	if db != nil {
		db.Close()
	}
	if name == "" {
		return
	}
	admin, err := adminConn()
	if err != nil {
		return
	}
	defer admin.Close()
	if _, err := admin.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", name)); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "is being accessed") {
			admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
			admin.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", name))
			atomic.AddInt64(&terminateFired, 1)
		}
	}
}

// FileKey returns the sharing key for the caller's test file — the absolute
// file path of the first _test.go frame above this package. Stable per file,
// unique across files: exactly the key OpenShared wants. Wrappers like
// dispatch's testDBShared call this and hand the result to OpenShared.
func FileKey(t testing.TB) string {
	t.Helper()
	for skip := 2; skip < 8; skip++ {
		_, file, _, ok := runtime.Caller(skip)
		if !ok {
			break
		}
		if !strings.HasSuffix(file, "_test.go") {
			continue
		}
		if !strings.Contains(file, "internal/testdb/") {
			return file
		}
	}
	// Not called from a test file (a helper one frame below the test, or a
	// benchmark). Fall back to the immediate caller's file so sharing still
	// keys on something stable.
	_, file, _, _ := runtime.Caller(1)
	return file
}

func sanitize(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// StandardData holds the common entities created by SetupStandardData.
type StandardData struct {
	StorageNode *nodes.Node
	LineNode    *nodes.Node
	Payload     *payloads.Payload
	BinType     *bins.BinType
}

// SetupStandardData creates the minimal fixture shared by most tests:
// one storage node (STORAGE-A1, zone A), one line node (LINE1-IN),
// one payload (PART-A), and one bin type (DEFAULT).
//
// IDEMPOTENT: every entity is get-or-create by its natural key. Tests that
// share one database (OpenShared) call this concurrently, and two
// SetupStandardData calls against one DB must not fight over nodes_name_key,
// payloads_code_key or bin_types_code_key. The get-or-create on a shared DB
// returns the existing row, so the returned StandardData is identical for
// every caller on that DB — same IDs, no drift.
func SetupStandardData(t *testing.T, db *store.DB) *StandardData {
	t.Helper()
	// STORAGE-A1 must be a STOR node: store-destination handling keys on the STOR
	// type — isStorageDropoff (and the Stage-3 slot reservation it gates) only
	// treats STOR-typed nodes as storage slots, so an untyped storage fixture
	// wouldn't reserve/claim as storage. STOR is a plant-config type (seed_core's
	// ensureNodeType) that migrations don't ship (only LANE/NGRP) and tests
	// don't plant-seed, so create it if absent. LINE1-IN stays untyped — a line
	// node is not a storage destination.
	storType, err := db.GetNodeTypeByCode("STOR")
	if err != nil {
		storType = &nodes.NodeType{Code: "STOR", Name: "Storage Slot", IsSynthetic: false}
		if err := db.CreateNodeType(storType); err != nil {
			// Lost a create race on a shared DB — take the winner's row.
			if existing, rerr := db.GetNodeTypeByCode("STOR"); rerr == nil {
				storType = existing
			} else {
				t.Fatalf("create STOR node type: %v", err)
			}
		}
	}
	// Get-or-create each entity. A missed SELECT followed by a racing INSERT
	// is fine here too: these run on the same shared DB, so the loser of the
	// race re-reads and gets the winner's row.
	storageNode := getOrCreateNode(t, db, "STORAGE-A1", func(existing *nodes.Node) {
		existing.Zone = "A"
		existing.Enabled = true
		existing.NodeTypeID = &storType.ID
	})
	lineNode := getOrCreateNode(t, db, "LINE1-IN", func(existing *nodes.Node) {
		existing.Enabled = true
	})
	bp := getOrCreatePayload(t, db, "PART-A", func(existing *payloads.Payload) {
		existing.Description = "Steel bracket tote"
		existing.UOPCapacity = 1000
	})
	bt := getOrCreateBinType(t, db, "DEFAULT", func(existing *bins.BinType) {
		existing.Description = "Default test bin type"
	})
	return &StandardData{
		StorageNode: storageNode,
		LineNode:    lineNode,
		Payload:     bp,
		BinType:     bt,
	}
}

// getOrCreateNode reads a node by name, creating it with applyToNew applied
// when absent. When present, applyToNew is NOT applied — the first creator's
// shape wins and later callers see the same row.
func getOrCreateNode(t *testing.T, db *store.DB, name string, applyToNew func(n *nodes.Node)) *nodes.Node {
	t.Helper()
	if existing, err := db.GetNodeByName(name); err == nil {
		return existing
	}
	n := &nodes.Node{Name: name}
	applyToNew(n)
	if err := db.CreateNode(n); err != nil {
		// Lost a create race on a shared DB: re-read the winner's row.
		if existing, rerr := db.GetNodeByName(name); rerr == nil {
			return existing
		}
		t.Fatalf("create node %s: %v", name, err)
	}
	return n
}

func getOrCreatePayload(t *testing.T, db *store.DB, code string, applyToNew func(p *payloads.Payload)) *payloads.Payload {
	t.Helper()
	if existing, err := db.GetPayloadByCode(code); err == nil {
		return existing
	}
	p := &payloads.Payload{Code: code}
	applyToNew(p)
	if err := db.CreatePayload(p); err != nil {
		if existing, rerr := db.GetPayloadByCode(code); rerr == nil {
			return existing
		}
		t.Fatalf("create payload %s: %v", code, err)
	}
	return p
}

func getOrCreateBinType(t *testing.T, db *store.DB, code string, applyToNew func(bt *bins.BinType)) *bins.BinType {
	t.Helper()
	if existing, err := db.GetBinTypeByCode(code); err == nil {
		return existing
	}
	bt := &bins.BinType{Code: code}
	applyToNew(bt)
	if err := db.CreateBinType(bt); err != nil {
		if existing, rerr := db.GetBinTypeByCode(code); rerr == nil {
			return existing
		}
		t.Fatalf("create bin type %s: %v", code, err)
	}
	return bt
}

// CreateBinAtNode creates a bin at the given node with a confirmed manifest matching
// the payload code. It ensures the DEFAULT bin type exists (idempotent). Returns the
// fully-loaded bin from the database.
func CreateBinAtNode(t *testing.T, db *store.DB, payloadCode string, nodeID int64, label string) *bins.Bin {
	t.Helper()
	// Ensure DEFAULT bin type exists. One statement, not check-then-insert: two
	// concurrent calls against the same database (goroutine-spawning tests, and
	// any future per-file DB sharing) both miss the SELECT, both INSERT, and one
	// dies on bin_types_code_key — a flake that looks like a fixture bug.
	if _, err := db.Exec(`INSERT INTO bin_types (code, description) VALUES ('DEFAULT', 'Default test bin type')
		ON CONFLICT (code) DO NOTHING`); err != nil {
		t.Fatalf("ensure default bin type: %v", err)
	}
	bt, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		t.Fatalf("read default bin type: %v", err)
	}
	bin := &bins.Bin{BinTypeID: bt.ID, Label: label, NodeID: &nodeID, Status: "available"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin %s: %v", label, err)
	}
	if err := db.SetBinManifest(bin.ID, `{"items":[]}`, payloadCode, 100); err != nil {
		t.Fatalf("set manifest for bin %s: %v", label, err)
	}
	if err := db.ConfirmBinManifest(bin.ID, ""); err != nil {
		t.Fatalf("confirm manifest for bin %s: %v", label, err)
	}
	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin %s after setup: %v", label, err)
	}
	return got
}

// orderSeq keeps CreateOrder's EdgeUUIDs unique within a test process.
var orderSeq atomic.Int64

// CreateOrder inserts a minimal real order and returns it. Tests that reserve or
// claim a bin need a real order row — reservations.order_id and bins.claimed_by
// both FK to orders(id), so hardcoded/bogus order ids fail. Status defaults to
// queued; pass opts to override fields, e.g.
//
//	testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "delivered" })
func CreateOrder(t *testing.T, db *store.DB, opts ...func(*orders.Order)) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID:  fmt.Sprintf("testorder-%d", orderSeq.Add(1)),
		StationID: "test",
		OrderType: "retrieve",
		Status:    "queued",
		Quantity:  1,
	}
	for _, opt := range opts {
		opt(o)
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("testdb.CreateOrder: %v", err)
	}
	return o
}

// ClaimBinForTest reserves then claims binID for orderID, mirroring the
// production reserve-then-confirm path (service.ClaimForDispatch): Acquire (a
// pending reservation) → ClaimBin → Confirm. The claim primitives now carry a
// demoted-CAS guard (AND EXISTS a pending reservation for order+bin), so a bare
// db.ClaimBin without this sequence fails "bin is locked, already claimed, or
// does not exist". Use wherever a test needs a bin already claimed by a real
// order. orderID must reference a real order (see CreateOrder).
func ClaimBinForTest(t *testing.T, db *store.DB, binID, orderID int64) {
	t.Helper()
	if err := reservations.Acquire(db, orderID, binID, "test"); err != nil {
		t.Fatalf("testdb.ClaimBinForTest Acquire(bin=%d order=%d): %v", binID, orderID, err)
	}
	if err := db.ClaimBin(binID, orderID); err != nil {
		t.Fatalf("testdb.ClaimBinForTest ClaimBin(bin=%d order=%d): %v", binID, orderID, err)
	}
	if err := reservations.Confirm(db, orderID, binID); err != nil {
		t.Fatalf("testdb.ClaimBinForTest Confirm(bin=%d order=%d): %v", binID, orderID, err)
	}
}

// ClaimSlotForTest sets nodes.claimed_by directly for fixture setup — the raw slot
// claim the deleted nodes.ClaimSlot / db.ClaimSlot used to provide. It is the
// sanctioned test-only bypass of the slot seatbelt (forbidigo carveout), for tests
// that just need a slot already claimed by an order. The PRODUCTION path is reserve
// (AcquireSlot) → db.ConfirmSlotClaim; use that when a test needs the coupled slot
// reservation too. orderID must reference a real order (see CreateOrder).
func ClaimSlotForTest(t *testing.T, db *store.DB, nodeID, orderID int64) {
	t.Helper()
	if _, err := db.DB.Exec(`UPDATE nodes SET claimed_by=$1, updated_at=NOW() WHERE id=$2`, orderID, nodeID); err != nil {
		t.Fatalf("testdb.ClaimSlotForTest(node=%d order=%d): %v", nodeID, orderID, err)
	}
}

// ReserveBin acquires a pending reservation for orderID on binID and nothing
// else — for tests that then exercise a GUARDED claim primitive directly
// (svc.ClearAndClaim / SyncUOPAndClaim / db.ClaimBin), which need a pending
// reservation to exist but perform the claim themselves. orderID must reference
// a real order (see CreateOrder).
func ReserveBin(t *testing.T, db *store.DB, orderID, binID int64) {
	t.Helper()
	if err := reservations.Acquire(db, orderID, binID, "test"); err != nil {
		t.Fatalf("testdb.ReserveBin Acquire(bin=%d order=%d): %v", binID, orderID, err)
	}
}

// SeedOrderStatus forces an order to an arbitrary status via a raw write,
// bypassing both lifecycle validation and the terminal-status guard on
// orders.UpdateStatus. For fixtures that must seed an order already in a
// terminal state (failed/cancelled/skipped/confirmed) to exercise
// reconciliation/recovery/matrix logic — NOT a stand-in for the real lifecycle
// in behavior tests (those must go through TerminalizeOrder, which also releases
// claims + reservations).
func SeedOrderStatus(t *testing.T, db *store.DB, orderID int64, status, detail string) {
	t.Helper()
	if _, err := db.DB.Exec(`UPDATE orders SET status=$1, error_detail=$2, updated_at=NOW() WHERE id=$3`,
		status, detail, orderID); err != nil {
		t.Fatalf("testdb.SeedOrderStatus(order=%d, %s): %v", orderID, status, err)
	}
}

// Envelope returns a standard test envelope (Edge → Core, station "line-1").
func Envelope() *protocol.Envelope {
	return &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-1"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
}
