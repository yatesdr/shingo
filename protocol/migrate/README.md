# protocol/migrate

Verified against: 74c3eb77

The shared, stdlib-only versioned-migration runner (Postgres and SQLite). **To add a migration:** append it to your module's migration list with `Version` = previous + 1, an idempotent `Fn` (it runs inside a per-version transaction with its `schema_migrations` row insert — either both commit or neither does), and an optional cheap `Verify` post-condition; the runner handles apply-once via the version table and self-heal (a recorded-but-failing Verify re-runs) on every startup. Edge's frozen chain up to `edgeBaselineVersion` (1) stays self-probing and untouched — only migrations with a version above it may be added. Core's Postgres chain and Edge's versioned migrations both run through this one `Run` call, so applied-version semantics cannot drift between the two sides. The package comment in `migrate.go` is the full account.
