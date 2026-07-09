// Package testdb provides shared test infrastructure for shingo-core integration tests.
// It manages a single Postgres container per test process (via sync.Once), builds
// a pre-migrated template database once, then clones the template for each test
// instead of re-running the full migration stack. Both engine and dispatch tests
// import this package instead of duplicating their own container and fixture setup.
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
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

// templateDBName is the name of the pre-migrated database every test gets
// cloned from. Must be a valid Postgres identifier; underscores only.
const templateDBName = "template_test"

// containerState holds the shared Postgres container started once per test process.
var (
	containerOnce sync.Once
	containerHost string
	containerPort int
	containerErr  error
)

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
	var lastErr error
	for i := 0; i < attempts; i++ {
		container, err := postgres.Run(ctx, "postgres:16-alpine",
			postgres.WithDatabase("postgres"),
			postgres.WithUsername("test"),
			postgres.WithPassword("test"),
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
			return
		}
		lastErr = fmt.Errorf("container host/port not ready (hostErr=%v host=%q portErr=%v portVal=%d)", hostErr, host, portErr, port.Int())
		_ = container.Terminate(ctx)
	}
	containerErr = fmt.Errorf("start container after %d attempts: %w", attempts, lastErr)
}

// adminConn returns a connection to the container's default "postgres"
// database, used for CREATE/DROP DATABASE and template metadata changes.
func adminConn() (*sql.DB, error) {
	return sql.Open("pgx", fmt.Sprintf(
		"host=%s port=%d dbname=postgres user=test password=test sslmode=disable",
		containerHost, containerPort))
}

// setupTemplate builds templateDBName by running migrations once, then
// marks it as a Postgres template so further CREATE DATABASE x TEMPLATE
// calls become near-instant file copies. On first-attempt failure (e.g.,
// migration crashed mid-flight) it drops the broken template and retries
// once; a second failure latches templateErr.
func setupTemplate() {
	if containerErr != nil {
		templateErr = containerErr
		return
	}
	if err := buildTemplate(); err != nil {
		// Fallback: tear down and retry once. Covers the case where a
		// partial template was left from a prior in-process attempt
		// (shouldn't happen with sync.Once, but cheap to defend).
		if dropErr := dropTemplate(); dropErr != nil {
			templateErr = fmt.Errorf("template build failed (%w); cleanup also failed (%w)", err, dropErr)
			return
		}
		if err2 := buildTemplate(); err2 != nil {
			templateErr = fmt.Errorf("template build failed on retry: %w", err2)
			return
		}
	}
}

// buildTemplate creates templateDBName, runs the full migration stack
// against it, terminates any lingering connections, and flips it into
// template mode.
func buildTemplate() error {
	if containerHost == "" || containerPort == 0 {
		return fmt.Errorf("container vars not set: host=%q port=%d (startContainer didn't populate)", containerHost, containerPort)
	}
	admin, err := adminConn()
	if err != nil {
		return fmt.Errorf("connect admin for template build: %w", err)
	}
	defer admin.Close()

	if _, err := admin.Exec(fmt.Sprintf("CREATE DATABASE %s", templateDBName)); err != nil {
		return fmt.Errorf("create template database: %w", err)
	}

	// Run migrations once against the template via the production Open path.
	tmplDB, err := store.Open(&config.DatabaseConfig{
		Postgres: config.PostgresConfig{
			Host:     containerHost,
			Port:     containerPort,
			Database: templateDBName,
			User:     "test",
			Password: "test",
			SSLMode:  "disable",
		},
	})
	if err != nil {
		return fmt.Errorf("open + migrate template: %w", err)
	}
	tmplDB.Close()

	// Pool close above is best-effort; explicitly evict anything still
	// holding the template open so the IS_TEMPLATE flip can't be blocked.
	if _, err := admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, templateDBName); err != nil {
		return fmt.Errorf("terminate template backends: %w", err)
	}

	if _, err := admin.Exec(fmt.Sprintf("ALTER DATABASE %s WITH IS_TEMPLATE = true", templateDBName)); err != nil {
		return fmt.Errorf("mark template: %w", err)
	}
	if _, err := admin.Exec(fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS = false", templateDBName)); err != nil {
		return fmt.Errorf("disallow template connections: %w", err)
	}
	return nil
}

// dropTemplate removes a previously-built template database. Used by the
// retry path in setupTemplate when a partial build needs to be cleared.
func dropTemplate() error {
	admin, err := adminConn()
	if err != nil {
		return err
	}
	defer admin.Close()
	// IS_TEMPLATE must be cleared before DROP succeeds; ignore errors here
	// since the prior failure may have left it unset.
	_, _ = admin.Exec(fmt.Sprintf("ALTER DATABASE %s WITH IS_TEMPLATE = false", templateDBName))
	_, _ = admin.Exec(fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS = true", templateDBName))
	_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, templateDBName)
	if _, err := admin.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", templateDBName)); err != nil {
		return fmt.Errorf("drop template: %w", err)
	}
	return nil
}

// Open returns a *store.DB connected to a fresh database cloned from the
// pre-migrated template. Each call creates a new database (test_<random>)
// so tests are fully isolated. The database and connection are cleaned up
// via t.Cleanup.
//
// If Docker is not running, the test is skipped (not failed).
func Open(t *testing.T) *store.DB {
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

	dbName := fmt.Sprintf("test_%s_%d", sanitize(t.Name()), rand.Intn(100000))

	admin, err := adminConn()
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", dbName, templateDBName)); err != nil {
		t.Fatalf("create test database %s from template: %v", dbName, err)
	}
	atomic.AddInt64(&testDBsCreated, 1)

	db, err := store.OpenWithoutMigrate(&config.DatabaseConfig{
		Postgres: config.PostgresConfig{
			Host:     containerHost,
			Port:     containerPort,
			Database: dbName,
			User:     "test",
			Password: "test",
			SSLMode:  "disable",
		},
	})
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
	return db
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
	// STORAGE-A1 must be a STOR node: FindStorageDestination only consolidates
	// onto / falls back to STOR-typed nodes, so an untyped storage fixture is
	// invisible to the finder. STOR is a plant-config type (seed_core's
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
