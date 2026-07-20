//go:build docker

package messaging

import (
	"database/sql"
	"reflect"
	"sort"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/plantclaims"
)

// TestHandlePlantClaims_MirrorRebuildAfterWipe pins the property that replaces
// Kafka compaction for late joiners: after wiping the Core mirror, applying a
// full snapshot (one message per process) rebuilds the cache to match. Core
// persists the mirror on every message, so a late-joining Core needs only the
// snapshot — no compacted topic.
func TestHandlePlantClaims_MirrorRebuildAfterWipe(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{})

	publish := func(reports ...*protocol.PlantClaimsReport) {
		for _, r := range reports {
			svc.HandlePlantClaims(nil, r)
		}
	}

	// Seed: two processes with styles + claims.
	publish(
		plantClaimsReport("SNF2", 1, []styleSpec{
			{name: "A", claims: []claimSpec{{node: "STOR-01", role: protocol.ClaimRoleConsume, swap: protocol.SwapModeSingleRobot, payload: "BIN-A", allowed: []string{"BIN-A"}, cap: 100, reorder: 20}}},
			{name: "B", claims: []claimSpec{{node: "STOR-02", role: protocol.ClaimRoleProduce, swap: protocol.SwapModeTwoRobot, payload: "BIN-B", allowed: []string{"BIN-B"}, cap: 50, reorder: 5}}},
		}),
		plantClaimsReport("SNF3", 1, []styleSpec{
			{name: "X", claims: []claimSpec{
				{node: "LINE-01", role: protocol.ClaimRoleConsume, swap: protocol.SwapModeSequential, payload: "BIN-C", allowed: []string{"BIN-C", "BIN-D"}, cap: 80, reorder: 10},
			}},
		}),
	)

	// Sanity: dirty index reflects both processes before the wipe.
	idx, err := db.PlantClaimsDirtyIndex()
	if err != nil {
		t.Fatalf("dirty index before wipe: %v", err)
	}
	if got := payloadTargets(idx, "BIN-A"); !reflect.DeepEqual(got, []string{"SNF2|A"}) {
		t.Fatalf("dirty index BIN-A before wipe = %v, want [SNF2|A]", got)
	}

	// Wipe the mirror — a late-joining Core starts empty.
	if err := db.WipePlantClaims(); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	idx, _ = db.PlantClaimsDirtyIndex()
	if len(idx) != 0 {
		t.Fatalf("dirty index after wipe = %v, want empty", idx)
	}

	// Reapply the same full snapshot — the mirror must rebuild exactly.
	publish(
		plantClaimsReport("SNF2", 1, []styleSpec{
			{name: "A", claims: []claimSpec{{node: "STOR-01", role: protocol.ClaimRoleConsume, swap: protocol.SwapModeSingleRobot, payload: "BIN-A", allowed: []string{"BIN-A"}, cap: 100, reorder: 20}}},
			{name: "B", claims: []claimSpec{{node: "STOR-02", role: protocol.ClaimRoleProduce, swap: protocol.SwapModeTwoRobot, payload: "BIN-B", allowed: []string{"BIN-B"}, cap: 50, reorder: 5}}},
		}),
		plantClaimsReport("SNF3", 1, []styleSpec{
			{name: "X", claims: []claimSpec{
				{node: "LINE-01", role: protocol.ClaimRoleConsume, swap: protocol.SwapModeSequential, payload: "BIN-C", allowed: []string{"BIN-C", "BIN-D"}, cap: 80, reorder: 10},
			}},
		}),
	)

	idx, err = db.PlantClaimsDirtyIndex()
	if err != nil {
		t.Fatalf("dirty index after rebuild: %v", err)
	}
	// BIN-A → SNF2|A ; BIN-B → SNF2|B ; BIN-C and BIN-D → SNF3|X (allowed-set match).
	for payload, want := range map[string]string{
		"BIN-A": "SNF2|A", "BIN-B": "SNF2|B", "BIN-C": "SNF3|X", "BIN-D": "SNF3|X",
	} {
		if got := payloadTargets(idx, payload); !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("dirty index %s after rebuild = %v, want [%s]", payload, got, want)
		}
	}
}

// TestHandlePlantClaims_PerProcessReplaceIsAuthoritative pins that each
// message replaces the whole process: a style/claim dropped on Edge
// disappears from the mirror after the next publish (no stale rows linger).
func TestHandlePlantClaims_PerProcessReplaceIsAuthoritative(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{})

	// Process with two styles.
	svc.HandlePlantClaims(nil, plantClaimsReport("SNF2", 1, []styleSpec{
		{name: "A", claims: []claimSpec{{node: "N1", payload: "BIN-A", allowed: []string{"BIN-A"}}}},
		{name: "B", claims: []claimSpec{{node: "N2", payload: "BIN-B", allowed: []string{"BIN-B"}}}},
	}))
	// Re-publish the SAME process with style B removed entirely.
	svc.HandlePlantClaims(nil, plantClaimsReport("SNF2", 2, []styleSpec{
		{name: "A", claims: []claimSpec{{node: "N1", payload: "BIN-A", allowed: []string{"BIN-A"}}}},
	}))

	idx, err := db.PlantClaimsDirtyIndex()
	if err != nil {
		t.Fatalf("dirty index: %v", err)
	}
	if got := payloadTargets(idx, "BIN-B"); len(got) != 0 {
		t.Errorf("BIN-B after style B removed = %v, want empty (process replace must drop it)", got)
	}
	if got := payloadTargets(idx, "BIN-A"); !reflect.DeepEqual(got, []string{"SNF2|A"}) {
		t.Errorf("BIN-A after re-publish = %v, want [SNF2|A]", got)
	}
}

// TestHandlePlantClaims_StaleSnapshotIgnored pins the out-of-order guard: an
// older config_gen landing after a newer one is dropped, so the mirror keeps
// the newest snapshot.
func TestHandlePlantClaims_StaleSnapshotIgnored(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{})

	svc.HandlePlantClaims(nil, plantClaimsReport("SNF2", 5, []styleSpec{
		{name: "A", claims: []claimSpec{{node: "N1", payload: "NEW", allowed: []string{"NEW"}}}},
	}))
	// Older snapshot arrives after the newer one.
	svc.HandlePlantClaims(nil, plantClaimsReport("SNF2", 3, []styleSpec{
		{name: "A", claims: []claimSpec{{node: "N1", payload: "OLD", allowed: []string{"OLD"}}}},
	}))

	idx, err := db.PlantClaimsDirtyIndex()
	if err != nil {
		t.Fatalf("dirty index: %v", err)
	}
	if _, stale := idx["OLD"]; stale {
		t.Errorf("stale snapshot (config_gen 3) was applied over newer (5); mirror has OLD")
	}
	if got := payloadTargets(idx, "NEW"); !reflect.DeepEqual(got, []string{"SNF2|A"}) {
		t.Errorf("NEW after stale guard = %v, want [SNF2|A]", got)
	}
}

// TestHandlePlantClaims_EmptyProcessClearsMirror pins that a process published
// with no styles clears any prior mirror for that process (a process with all
// styles removed still publishes, so Core drops its stale rows).
func TestHandlePlantClaims_EmptyProcessClearsMirror(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{})

	svc.HandlePlantClaims(nil, plantClaimsReport("SNF2", 1, []styleSpec{
		{name: "A", claims: []claimSpec{{node: "N1", payload: "BIN-A", allowed: []string{"BIN-A"}}}},
	}))
	svc.HandlePlantClaims(nil, plantClaimsReport("SNF2", 2, nil))

	idx, err := db.PlantClaimsDirtyIndex()
	if err != nil {
		t.Fatalf("dirty index: %v", err)
	}
	if len(idx) != 0 {
		t.Errorf("dirty index after empty process publish = %v, want empty", idx)
	}
}

// TestPlantClaimsMirror_MigrationTablesExist pins the migration created both
// mirror tables (the "migrations up" gate). A clean template DB must have them
// after the migration stack runs.
func TestPlantClaimsMirror_MigrationTablesExist(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	for _, table := range []string{"process_styles", "style_claims"} {
		var exists bool
		if err := db.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name=$1)`, table,
		).Scan(&exists); err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if !exists {
			t.Errorf("mirror table %s missing after migrations", table)
		}
	}
}

// TestPlantClaimsMirror_MigrationIdempotent pins the "migrations down/up" gate:
// dropping both tables and re-running ReplaceProcess (the migration's CREATE
// TABLE IF NOT EXISTS re-creates them on the next migrate) restores the mirror.
// This models the down-then-up cycle: the cache is rebuildable from the feed.
func TestPlantClaimsMirror_MigrationIdempotent(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// Seed one process.
	svc := NewCoreDataService(db, &captureResponder{})
	svc.HandlePlantClaims(nil, plantClaimsReport("SNF2", 1, []styleSpec{
		{name: "A", claims: []claimSpec{{node: "N1", payload: "BIN-A", allowed: []string{"BIN-A"}}}},
	}))

	// "Down": drop both mirror tables.
	for _, table := range []string{"style_claims", "process_styles"} {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	// Re-run the v49 DDL by calling the migration func's CREATE statements
	// directly (the migration is idempotent — IF NOT EXISTS).
	if err := reseedMirrorTables(db.DB); err != nil {
		t.Fatalf("re-create mirror tables: %v", err)
	}

	// "Up" again: re-apply the snapshot.
	svc.HandlePlantClaims(nil, plantClaimsReport("SNF2", 1, []styleSpec{
		{name: "A", claims: []claimSpec{{node: "N1", payload: "BIN-A", allowed: []string{"BIN-A"}}}},
	}))

	idx, err := db.PlantClaimsDirtyIndex()
	if err != nil {
		t.Fatalf("dirty index after reseed: %v", err)
	}
	if got := payloadTargets(idx, "BIN-A"); !reflect.DeepEqual(got, []string{"SNF2|A"}) {
		t.Errorf("BIN-A after down/up cycle = %v, want [SNF2|A]", got)
	}
}

// --- helpers ---

type claimSpec struct {
	node    string
	role    protocol.ClaimRole
	swap    protocol.SwapMode
	payload string
	allowed []string
	cap     int
	reorder int
}

type styleSpec struct {
	name   string
	claims []claimSpec
}

func plantClaimsReport(process string, configGen int64, styles []styleSpec) *protocol.PlantClaimsReport {
	out := &protocol.PlantClaimsReport{ProcessID: process, ConfigGen: configGen}
	for _, st := range styles {
		ws := protocol.PlantClaimsStyle{StyleID: st.name}
		for _, c := range st.claims {
			role, swap := c.role, c.swap
			if role == "" {
				role = protocol.ClaimRoleConsume
			}
			if swap == "" {
				swap = protocol.SwapModeSingleRobot
			}
			ws.Claims = append(ws.Claims, protocol.PlantClaim{
				CoreNodeName:        c.node,
				Role:                role,
				SwapMode:            swap,
				PayloadCode:         c.payload,
				AllowedPayloadCodes: c.allowed,
				UOPCapacity:         c.cap,
				ReorderPoint:        c.reorder,
			})
		}
		out.Styles = append(out.Styles, ws)
	}
	return out
}

// payloadTargets returns the sorted "process|style" keys the dirty index maps
// a payload to. Order-independent comparison.
func payloadTargets(idx map[string][]plantclaims.ProcessKey, payload string) []string {
	keys := idx[payload]
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.ProcessID+"|"+k.StyleID)
	}
	sort.Strings(out)
	return out
}

// reseedMirrorTables re-runs the v49 CREATE TABLE statements. Mirrors the
// migration's idempotent CREATE ... IF NOT EXISTS so the down/up test can
// restore the tables without a full re-migrate.
func reseedMirrorTables(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS process_styles (
			process_id   TEXT NOT NULL,
			style_id     TEXT NOT NULL,
			config_gen   BIGINT NOT NULL DEFAULT 0,
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (process_id, style_id)
		)`,
		`CREATE TABLE IF NOT EXISTS style_claims (
			process_id          TEXT NOT NULL,
			style_id            TEXT NOT NULL,
			core_node_name      TEXT NOT NULL,
			role                TEXT NOT NULL,
			swap_mode           TEXT NOT NULL,
			payload_code        TEXT NOT NULL DEFAULT '',
			allowed_payload_codes TEXT NOT NULL DEFAULT '[]',
			uop_capacity        INTEGER NOT NULL DEFAULT 0,
			reorder_point       INTEGER NOT NULL DEFAULT 0,
			seq                 INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_style_claims_payload ON style_claims (payload_code)`,
		`CREATE INDEX IF NOT EXISTS idx_style_claims_process_style ON style_claims (process_id, style_id)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}
