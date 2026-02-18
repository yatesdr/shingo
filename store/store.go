package store

import (
	"database/sql"
	"fmt"
	"strings"

	"warpath/config"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
	dialect Dialect
	driver  string
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
	db := &DB{DB: sqlDB, dialect: sqliteDialect{}, driver: "sqlite"}
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
	db := &DB{DB: sqlDB, dialect: postgresDialect{}, driver: "postgres"}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate postgres: %w", err)
	}
	return db, nil
}

func (db *DB) Dialect() Dialect { return db.dialect }
func (db *DB) Driver() string   { return db.driver }

// Q rewrites ? placeholders and datetime literals for PostgreSQL, passes through for SQLite.
func (db *DB) Q(query string) string {
	if db.driver == "postgres" {
		query = strings.ReplaceAll(query, "datetime('now','localtime')", "NOW()")
		return Rebind(query)
	}
	return query
}

func (db *DB) migrate() error {
	var schema string
	switch db.driver {
	case "sqlite":
		schema = schemaSQLite
	case "postgres":
		schema = schemaPostgres
	default:
		return fmt.Errorf("no schema for driver: %s", db.driver)
	}
	_, err := db.Exec(schema)
	return err
}
