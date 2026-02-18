package store

import (
	"fmt"
	"strings"
)

type Dialect interface {
	Placeholder(n int) string
	AutoIncrementPK() string
	BlobType() string
	JSONType() string
	Now() string
	TimestampType() string
	BoolType() string
	BoolTrue() string
	BoolFalse() string
}

type sqliteDialect struct{}

func (d sqliteDialect) Placeholder(_ int) string    { return "?" }
func (d sqliteDialect) AutoIncrementPK() string     { return "INTEGER PRIMARY KEY AUTOINCREMENT" }
func (d sqliteDialect) BlobType() string             { return "BLOB" }
func (d sqliteDialect) JSONType() string             { return "TEXT" }
func (d sqliteDialect) Now() string                  { return "datetime('now','localtime')" }
func (d sqliteDialect) TimestampType() string        { return "TEXT" }
func (d sqliteDialect) BoolType() string             { return "INTEGER" }
func (d sqliteDialect) BoolTrue() string             { return "1" }
func (d sqliteDialect) BoolFalse() string            { return "0" }

type postgresDialect struct{}

func (d postgresDialect) Placeholder(n int) string   { return fmt.Sprintf("$%d", n) }
func (d postgresDialect) AutoIncrementPK() string    { return "BIGSERIAL PRIMARY KEY" }
func (d postgresDialect) BlobType() string            { return "BYTEA" }
func (d postgresDialect) JSONType() string            { return "JSONB" }
func (d postgresDialect) Now() string                 { return "NOW()" }
func (d postgresDialect) TimestampType() string       { return "TIMESTAMPTZ" }
func (d postgresDialect) BoolType() string            { return "BOOLEAN" }
func (d postgresDialect) BoolTrue() string            { return "TRUE" }
func (d postgresDialect) BoolFalse() string           { return "FALSE" }

// Rebind rewrites ? placeholders to $1, $2, ... for PostgreSQL.
func Rebind(query string) string {
	n := 0
	var b strings.Builder
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteString(fmt.Sprintf("$%d", n))
		} else {
			b.WriteByte(query[i])
		}
	}
	return b.String()
}
