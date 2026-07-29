package schemadump

// Known shape divergences between a fresh edge database and an upgraded one.
//
// These all predate the convergence test — they are what it found the first
// time it ran, and every one of them is the same bug: schema.Apply runs
// CREATE TABLE IF NOT EXISTS, so a change to sqlite_ddl.go's CREATE TABLE
// reaches a fresh edge and never reaches a plant.
//
// They are listed rather than fixed because every fix is a decision, not a
// mechanism: SQLite cannot ALTER COLUMN DROP DEFAULT (it needs a table
// rebuild), and dropping the leftover column and table destroys plant data.
// None is urgent — see each entry — and none should be quietly carried
// forever either, which is why the test also fails when an entry stops
// applying.
//
// NOTHING NEW GOES IN THIS LIST. It exists to record what was already true on
// 2026-07-25, so the test can catch the next one instead of drowning in these.
var KnownDivergences = []KnownDivergence{
	// GONE 2026-07-25 — style_node_claims.auto_reorder (DEFAULT 1 at plants,
	// none fresh) and .swap_mode (DEFAULT 'simple' at plants, none fresh) were
	// here. Both are fixed by the v33 rebuild in store/migrations_style_claims.go,
	// which folds them into the same table rebuild that adds
	// below_reorder_since — one rebuild, three changes, rather than rebuilding
	// style_node_claims twice in a week.
	//
	// They are deleted rather than left as passing history because an entry that
	// outlives its finding is a suppression, not a record: the test now goes RED
	// if the rebuild ever silently stops taking. That is the difference between
	// "known and accepted" and "hidden".
	{
		Key: "table orders: upgraded only: bin_uop_remaining INTEGER",
		Why: "a column removed from the baseline and never dropped from plant databases. " +
			"Nothing reads or writes it. Dropping it destroys whatever is in it, which is a " +
			"decision rather than a cleanup — an unread column costs a few bytes a row.",
	},
	{
		Key: "table loader_payload_thresholds: present in the upgraded database only",
		Why: "the v5->v6 replacement drops this table only when it detects the v5 column " +
			"shape (hasV5LoaderColumn). An edge already on the v6 shape keeps the table, " +
			"while a fresh install no longer creates it at all. Unused; dropping it is a " +
			"data decision.",
	},
}

// KnownDivergence is one recorded difference between a fresh edge database and
// an upgraded one.
type KnownDivergence struct {
	// Key is the exact diff line the convergence test produces.
	Key string
	// Why is what was found when it was examined. An allowlist entry without
	// this is indistinguishable from someone silencing a failure.
	Why string
}
