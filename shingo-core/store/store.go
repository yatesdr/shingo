package store

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"

	"shingocore/config"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type DB struct {
	mu sync.RWMutex
	*sql.DB
	driver string
}

// ResetDatabase removes all data so the next Open() starts fresh.
// For SQLite it deletes the database file; for Postgres it drops all tables.
func ResetDatabase(cfg *config.DatabaseConfig) error {
	switch cfg.Driver {
	case "sqlite":
		path := cfg.SQLite.Path
		for _, suffix := range []string{"", "-wal", "-shm"} {
			os.Remove(path + suffix)
		}
		return nil
	case "postgres":
		dsn := fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s",
			cfg.Postgres.Host, cfg.Postgres.Port, cfg.Postgres.Database,
			cfg.Postgres.User, cfg.Postgres.Password, cfg.Postgres.SSLMode)
		sqlDB, err := sql.Open("pgx", dsn)
		if err != nil {
			return fmt.Errorf("connect postgres for reset: %w", err)
		}
		defer sqlDB.Close()
		_, err = sqlDB.Exec(`DO $$ DECLARE r RECORD;
			BEGIN
				FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' LOOP
					EXECUTE 'DROP TABLE IF EXISTS public.' || quote_ident(r.tablename) || ' CASCADE';
				END LOOP;
			END $$`)
		if err != nil {
			return fmt.Errorf("drop postgres tables: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported driver: %s", cfg.Driver)
	}
}

func Open(cfg *config.DatabaseConfig) (*DB, error) {
	switch cfg.Driver {
	case "sqlite":
		return openSQLite(cfg.SQLite.Path)
	case "postgres":
		return openPostgres(&cfg.Postgres)
	default:
		return nil, fmt.Errorf("unsupported database driver: %s", cfg.Driver)
	}
}

func openSQLite(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	db := &DB{DB: sqlDB, driver: "sqlite"}
	if err := db.migrateRenames(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate renames sqlite: %w", err)
	}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate sqlite: %w", err)
	}
	return db, nil
}

func openPostgres(cfg *config.PostgresConfig) (*DB, error) {
	dsn := fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password, cfg.SSLMode)
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db := &DB{DB: sqlDB, driver: "postgres"}
	if err := db.migrateRenames(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate renames postgres: %w", err)
	}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate postgres: %w", err)
	}
	return db, nil
}

func (db *DB) Driver() string { return db.driver }

// Reconnect swaps the underlying database connection in-place.
// The old connection is closed after the swap. All holders of *DB
// see the new connection immediately.
func (db *DB) Reconnect(cfg *config.DatabaseConfig) error {
	newDB, err := Open(cfg)
	if err != nil {
		return err
	}
	if err := newDB.Ping(); err != nil {
		newDB.Close()
		return fmt.Errorf("ping new db: %w", err)
	}
	db.mu.Lock()
	old := db.DB
	db.DB = newDB.DB
	db.driver = newDB.driver
	db.mu.Unlock()
	old.Close()
	return nil
}

// Q rewrites ? placeholders and datetime literals for PostgreSQL, passes through for SQLite.
func (db *DB) Q(query string) string {
	if db.driver == "postgres" {
		query = strings.ReplaceAll(query, "datetime('now')", "NOW()")
		return Rebind(query)
	}
	return query
}
