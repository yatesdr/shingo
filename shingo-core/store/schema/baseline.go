package schema

import _ "embed"

// Baseline is baseline.sql, embedded: store.migrate applies it, as one
// statement, to a database with no tables.
//
//go:embed baseline.sql
var Baseline string
