// Package testdb provides shared test infrastructure for shingo-core integration tests.
// It manages a single Postgres container per test process (via sync.Once) and creates
// a fresh database per test for isolation. Both engine and dispatch tests import this
// package instead of duplicating their own container and fixture setup.
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// containerState holds the shared Postgres container started once per test process.
var (
	containerOnce sync.Once
	containerHost string
	containerPort int
	containerErr  error
	pgContainer   *postgres.PostgresContainer
)

// startContainer spins up a single Postgres container for the entire test process.
// Called via sync.Once — all tests share this container but get their own database.
func startContainer() {
	ctx := context.Background()
	pgContainer, containerErr = postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second)),
	)
	if containerErr != nil {
		return
	}
	containerHost, _ = pgContainer.Host(ctx)
	p, _ := pgContainer.MappedPort(ctx, "5432")
	containerPort = p.Int()
}

// Open returns a *store.DB connected to a fresh database inside the shared Postgres
// container. Each call creates a new database (test_<random>) so tests are fully
// isolated. The database and connection are cleaned up via t.Cleanup.
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

	// Create a fresh database for this test
	dbName := fmt.Sprintf("test_%s_%d", sanitize(t.Name()), rand.Intn(100000))

	adminDB, err := sql.Open("pgx", fmt.Sprintf("host=%s port=%d dbname=postgres user=test password=test sslmode=disable", containerHost, containerPort))
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer adminDB.Close()
	if _, err := adminDB.Exec(fmt.Sprintf("CREATE DATABASE %s", dbName)); err != nil {
		t.Fatalf("create test database %s: %v", dbName, err)
	}

	db, err := store.Open(&config.DatabaseConfig{
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
		// Best-effort drop — the container dies at process exit anyway.
		// Use a fresh connection to avoid "database is being accessed" errors.
		cleanup, err := sql.Open("pgx", fmt.Sprintf("host=%s port=%d dbname=postgres user=test password=test sslmode=disable", containerHost, containerPort))
		if err == nil {
			cleanup.Exec(fmt.Sprintf(
				"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid <> pg_backend_pid()", dbName))
			cleanup.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbName))
			cleanup.Close()
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
	storageNode := &nodes.Node{Name: "STORAGE-A1", Zone: "A", Enabled: true}
	if err := db.CreateNode(storageNode); err != nil {
		t.Fatalf("create storage node: %v", err)
	}
	lineNode := &nodes.Node{Name: "LINE1-IN", Enabled: true}
	if err := db.CreateNode(lineNode); err != nil {
		t.Fatalf("create line node: %v", err)
	}
	bp := &payloads.Payload{Code: "PART-A", Description: "Steel bracket tote"}
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

// Envelope returns a standard test envelope (Edge → Core, station "line-1").
func Envelope() *protocol.Envelope {
	return &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-1"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
}
