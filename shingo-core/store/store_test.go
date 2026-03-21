package store

import (
	"context"
	"fmt"
	"testing"

	"shingocore/config"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// testDB creates a temporary PostgreSQL database via testcontainers for testing.
func testDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("shingocore_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		pgContainer.Terminate(ctx)
	})

	host, err := pgContainer.Host(ctx)
	if err != nil {
		t.Fatalf("get container host: %v", err)
	}
	port, err := pgContainer.MappedPort(ctx, "5432")
	if err != nil {
		t.Fatalf("get container port: %v", err)
	}

	db, err := Open(&config.DatabaseConfig{
		Postgres: config.PostgresConfig{
			Host:     host,
			Port:     port.Int(),
			Database: "shingocore_test",
			User:     "test",
			Password: "test",
			SSLMode:  "disable",
		},
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Verify connection
	if err := db.Ping(); err != nil {
		t.Fatalf("ping test db: %v", err)
	}
	fmt.Println("test database ready")
	return db
}
