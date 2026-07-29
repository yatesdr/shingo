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
	{
		Key: "table style_node_claims: fresh only: auto_reorder INTEGER NOT NULL DEFAULT 0",
		Why: "b8836528 changed the baseline default from 1 to 0 (\"stop the claim editor " +
			"force-arming auto_reorder on every save\") and the change reached no plant, " +
			"because the table already existed. INERT in practice: both write paths — " +
			"UpsertStyleNodeClaim and cloneClaimColumns — name auto_reorder explicitly, so " +
			"the column default is never exercised by Edge's own code. It would only bite " +
			"hand-written SQL. Worth converging when style_node_claims is next rebuilt; on " +
			"the column that caused 07-21, so worth knowing about either way.",
	},
	{
		Key: "table style_node_claims: upgraded only: auto_reorder INTEGER NOT NULL DEFAULT 1",
		Why: "the other half of the auto_reorder default divergence above.",
	},
	{
		Key: "table style_node_claims: fresh only: swap_mode TEXT NOT NULL",
		Why: "same shape, older: the baseline dropped DEFAULT 'simple' and plants kept it. " +
			"Inert for the same reason — every writer names swap_mode.",
	},
	{
		Key: "table style_node_claims: upgraded only: swap_mode TEXT NOT NULL DEFAULT 'simple'",
		Why: "the other half of the swap_mode default divergence above.",
	},
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
