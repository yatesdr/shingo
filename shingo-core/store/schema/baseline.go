package schema

import _ "embed"

// Baseline is baseline.sql, embedded: store.migrate applies it, as one
// statement, to a database with no tables.
//
//go:embed baseline.sql
var Baseline string

// Snapshot is schema.snapshot.sql, embedded: the shape this build ships, which
// the boot's drift report holds the live schema to (store.DB.SchemaDrift).
//
//go:embed schema.snapshot.sql
var Snapshot string

// Comments is comments.sql, embedded: the schema's rationale as COMMENT ON
// statements, which migration 185 applies as its last step.
//
//go:embed comments.sql
var Comments string
