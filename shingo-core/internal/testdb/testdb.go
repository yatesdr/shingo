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
)

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
		return n
	}
	return templateDBName
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
				t.Skipf("skipping integration test: %s", msg)
			}
			panic(r)
		}
	}()

	containerOnce.Do(startContainer)
	if containerErr != nil {
		if strings.Contains(strings.ToLower(containerErr.Error()), "docker") {
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
		t.Fatalf("create test database %s from template: %v", dbName, err)
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
		t.Fatalf("open test db %s: %v", dbName, err)
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
	return db, cfg
}

// sanitize strips characters that aren't safe for a Postgres database name.
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
			t.Fatalf("create STOR node type: %v", err)
		}
	}
	storageNode := &nodes.Node{Name: "STORAGE-A1", Zone: "A", Enabled: true, NodeTypeID: &storType.ID}
	if err := db.CreateNode(storageNode); err != nil {
		t.Fatalf("create storage node: %v", err)
	}
	lineNode := &nodes.Node{Name: "LINE1-IN", Enabled: true}
	if err := db.CreateNode(lineNode); err != nil {
		t.Fatalf("create line node: %v", err)
	}
	bp := &payloads.Payload{Code: "PART-A", Description: "Steel bracket tote", UOPCapacity: 1000}
	if err := db.CreatePayload(bp); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	bt := &bins.BinType{Code: "DEFAULT", Description: "Default test bin type"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	return &StandardData{
		StorageNode: storageNode,
		LineNode:    lineNode,
		Payload:     bp,
		BinType:     bt,
	}
}

// CreateBinAtNode creates a bin at the given node with a confirmed manifest matching
// the payload code. It ensures the DEFAULT bin type exists (idempotent). Returns the
// fully-loaded bin from the database.
func CreateBinAtNode(t *testing.T, db *store.DB, payloadCode string, nodeID int64, label string) *bins.Bin {
	t.Helper()
	// Ensure DEFAULT bin type exists (idempotent — safe to call multiple times)
	_, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		bt := &bins.BinType{Code: "DEFAULT", Description: "Default test bin type"}
		if err := db.CreateBinType(bt); err != nil {
			t.Fatalf("create default bin type: %v", err)
		}
	}
	bt, _ := db.GetBinTypeByCode("DEFAULT")
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
