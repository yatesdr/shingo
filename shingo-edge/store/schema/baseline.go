package schema

import _ "embed"

// Baseline is baseline.sql, embedded: the whole schema and the
// schema_migrations rows through its head, which store.migrate applies in one
// transaction to a file with no tables. It is generated, never edited
// (make schema-baseline; TestBaselineIsCurrent).
//
//go:embed baseline.sql
var Baseline string
