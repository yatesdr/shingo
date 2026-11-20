package schema

import _ "embed"

// Baseline is baseline.sql, embedded: store.migrate applies it, as one
// statement, to a database with no tables.
//
//go:embed baseline.sql
var Baseline string

// Comments is comments.sql, embedded: the schema's rationale as COMMENT ON
// statements, which migration 185 applies as its last step.
//
//go:embed comments.sql
var Comments string
