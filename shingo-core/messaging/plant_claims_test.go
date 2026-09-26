//go:build docker

package messaging

import (
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store/plantclaims"
	"shingocore/store/sourceability"
)

// TestHandlePlantClaims_MirrorRebuildAfterWipe pins the property that replaces
// Kafka compaction for late joiners: after wiping the Core mirror, applying a
// full snapshot (one message per process) rebuilds the cache to match. Core
// persists the mirror on every message, so a late-joining Core needs only the
// snapshot — no compacted topic.
func TestHandlePlantClaims_MirrorRebuildAfterWipe(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})

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
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})

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
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})

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
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})

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
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	svc.HandlePlantClaims(nil, plantClaimsReport("SNF2", 1, []styleSpec{
		{name: "A", claims: []claimSpec{{node: "N1", payload: "BIN-A", allowed: []string{"BIN-A"}}}},
	}))

	// "Down": drop both mirror tables.
	for _, table := range []string{"style_claims", "process_styles"} {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	// Recreate them in their migrated shape, as a re-migrate would.
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
	node     string
	role     protocol.ClaimRole
	swap     protocol.SwapMode
	payload  string
	allowed  []string
	cap      int
	reorder  int
	inbound  string
	outbound string
	paired   string
	second   string
}

type styleSpec struct {
	name   string
	active bool
	claims []claimSpec
}

func plantClaimsReport(process string, configGen int64, styles []styleSpec) *protocol.PlantClaimsReport {
	out := &protocol.PlantClaimsReport{ProcessID: process, ConfigGen: configGen}
	for _, st := range styles {
		ws := protocol.PlantClaimsStyle{StyleID: st.name, Active: st.active}
		for _, c := range st.claims {
			role, swap := c.role, c.swap
			if role == "" {
				role = protocol.ClaimRoleConsume
			}
			if swap == "" {
				swap = protocol.SwapModeSingleRobot
			}
			ws.Claims = append(ws.Claims, protocol.PlantClaim{
				CoreNodeName:         c.node,
				Role:                 role,
				SwapMode:             swap,
				PayloadCode:          c.payload,
				AllowedPayloadCodes:  c.allowed,
				UOPCapacity:          c.cap,
				ReorderPoint:         c.reorder,
				InboundSource:        c.inbound,
				OutboundDestination:  c.outbound,
				PairedCoreNode:       c.paired,
				SecondPairedCoreNode: c.second,
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

// snapshotPath is the committed pg_dump of the fully migrated schema,
// relative to this package. internal/schemadump's TestSchemaSnapshotIsCurrent
// keeps it equal to what the migrations produce.
const snapshotPath = "../store/schema/schema.snapshot.sql"

// reseedMirrorTables recreates the two mirror tables, with their constraints
// and indexes, from the schema snapshot — the migrated shape, whatever
// migrations have since added to it. Hand-written DDL here fell a column
// behind once (v133's containment_destination), and ReplaceProcess's INSERT
// then failed inside a transaction HandlePlantClaims only logs, which reads
// as an empty mirror rather than a schema error.
func reseedMirrorTables(db *sql.DB) error {
	stmts, err := snapshotStatementsFor("process_styles", "style_claims")
	if err != nil {
		return err
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%w\n%s", err, s)
		}
	}
	return nil
}

// snapshotStatementsFor returns the snapshot's CREATE TABLE, ALTER TABLE ONLY
// (constraints) and CREATE [UNIQUE] INDEX statements for the named tables, in
// file order. pg_dump ends every statement with ";" at the end of a line, so
// splitting there yields whole statements. A table with no CREATE TABLE in the
// snapshot is an error, not an empty result.
func snapshotStatementsFor(tables ...string) ([]string, error) {
	b, err := os.ReadFile(snapshotPath)
	if err != nil {
		return nil, err
	}
	var out []string
	created := map[string]bool{}
	for _, chunk := range strings.Split(string(b), ";\n") {
		stmt := strings.TrimSpace(chunk)
		for _, t := range tables {
			q := "public." + t
			switch {
			case strings.HasPrefix(stmt, "CREATE TABLE "+q+" ("):
				created[t] = true
			case strings.HasPrefix(stmt, "ALTER TABLE ONLY "+q+"\n"):
			case (strings.HasPrefix(stmt, "CREATE INDEX ") || strings.HasPrefix(stmt, "CREATE UNIQUE INDEX ")) &&
				strings.Contains(stmt, " ON "+q+" "):
			default:
				continue
			}
			out = append(out, stmt)
		}
	}
	for _, t := range tables {
		if !created[t] {
			return nil, fmt.Errorf("%s: no CREATE TABLE public.%s", snapshotPath, t)
		}
	}
	return out, nil
}

// TestHandlePlantClaims_MirrorsTheRunningStyle pins the running-style signal end
// to end: Edge marks one style active on the feed, Core persists it, and the
// sourcing read returns it. Core had no notion of a running style before this —
// the feed carried a process's styles and claims but no active flag, so the
// sourcing page could only say what a process COULD change over to.
func TestHandlePlantClaims_MirrorsTheRunningStyle(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})

	svc.HandlePlantClaims(nil, plantClaimsReport("SNF2", 1, []styleSpec{
		{name: "SYN-PART08A.95", claims: []claimSpec{{node: "STOR-01", payload: "BIN-A", allowed: []string{"BIN-A"}}}},
		{name: "SYN-PART11E.95", active: true, claims: []claimSpec{{node: "STOR-02", payload: "BIN-B", allowed: []string{"BIN-B"}}}},
		{name: "Default", claims: []claimSpec{{node: "STOR-03", payload: "BIN-C", allowed: []string{"BIN-C"}}}},
	}))

	active, err := sourceability.ActiveStyles(db.DB)
	if err != nil {
		t.Fatalf("ActiveStyles: %v", err)
	}
	if got := active["SNF2"]; got != "SYN-PART11E.95" {
		t.Fatalf("running style = %q, want SYN-PART11E.95", got)
	}
	if len(active) != 1 {
		t.Fatalf("active styles = %v, want exactly one process marked", active)
	}
}

// TestHandlePlantClaims_NoActiveStyleIsNotGuessed is the honesty half. A report
// with no style marked active must leave Core with no running style for that
// process — not the first style, not a default. Core showing a confident wrong
// style is worse than showing none.
func TestHandlePlantClaims_NoActiveStyleIsNotGuessed(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})

	svc.HandlePlantClaims(nil, plantClaimsReport("P47", 1, []styleSpec{
		{name: "SYN-PART14A.95", claims: []claimSpec{{node: "STOR-01", payload: "BIN-A", allowed: []string{"BIN-A"}}}},
		{name: "SYN-PART14B.95", claims: []claimSpec{{node: "STOR-02", payload: "BIN-B", allowed: []string{"BIN-B"}}}},
	}))

	active, err := sourceability.ActiveStyles(db.DB)
	if err != nil {
		t.Fatalf("ActiveStyles: %v", err)
	}
	if got, ok := active["P47"]; ok {
		t.Fatalf("running style = %q, want absent — no style was marked active", got)
	}
}

// TestHandlePlantClaims_ActiveStyleFollowsAChangeover pins that the flag TRACKS
// rather than accumulates: a later snapshot moving the active style must leave
// exactly one active, not two. The mirror is replaced per process wholesale, so
// this is really a guard on that replace covering the new column.
func TestHandlePlantClaims_ActiveStyleFollowsAChangeover(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})

	before := []styleSpec{
		{name: "A", active: true, claims: []claimSpec{{node: "N1", payload: "BIN-A", allowed: []string{"BIN-A"}}}},
		{name: "B", claims: []claimSpec{{node: "N2", payload: "BIN-B", allowed: []string{"BIN-B"}}}},
	}
	after := []styleSpec{
		{name: "A", claims: []claimSpec{{node: "N1", payload: "BIN-A", allowed: []string{"BIN-A"}}}},
		{name: "B", active: true, claims: []claimSpec{{node: "N2", payload: "BIN-B", allowed: []string{"BIN-B"}}}},
	}

	svc.HandlePlantClaims(nil, plantClaimsReport("SNF4", 1, before))
	svc.HandlePlantClaims(nil, plantClaimsReport("SNF4", 2, after))

	active, err := sourceability.ActiveStyles(db.DB)
	if err != nil {
		t.Fatalf("ActiveStyles: %v", err)
	}
	if got := active["SNF4"]; got != "B" {
		t.Fatalf("running style after changeover = %q, want B", got)
	}
}

// ── THE CLAIM'S LEGS IN THE MIRROR ────────────────────────────────────────

// legsOf reads one claim's four leg columns straight out of style_claims.
// Deliberately a raw query and not a store reader: no store reader projects
// these columns, and adding one to make this test shorter would be the test
// inventing the very coupling the sourceability guard exists to prevent.
func legsOf(t *testing.T, db *sql.DB, process, style, node string) [4]string {
	t.Helper()
	var legs [4]string
	if err := db.QueryRow(
		`SELECT inbound_source, outbound_destination, paired_core_node, second_paired_core_node
		   FROM style_claims WHERE process_id = $1 AND style_id = $2 AND core_node_name = $3`,
		process, style, node,
	).Scan(&legs[0], &legs[1], &legs[2], &legs[3]); err != nil {
		t.Fatalf("read legs for %s|%s|%s: %v", process, style, node, err)
	}
	return legs
}

// TestHandlePlantClaims_MirrorsTheClaimLegs pins the lane end to end on Core's
// side: the four node names a claim reports arrive in style_claims exactly as
// sent. Without them Core's mirror holds a set of nodes and no edges between
// them, which is a list and not a loop — the demand loop compiler has nothing
// to read.
func TestHandlePlantClaims_MirrorsTheClaimLegs(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})

	svc.HandlePlantClaims(nil, plantClaimsReport("SNF7", 1, []styleSpec{
		{name: "A", claims: []claimSpec{{
			node: "PLN_002", payload: "BIN-A", allowed: []string{"BIN-A"},
			inbound: "SMN_SYN_A", outbound: "SMN_SYN_B",
			paired: "PLN_003", second: "PLN_004",
		}}},
	}))

	want := [4]string{"SMN_SYN_A", "SMN_SYN_B", "PLN_003", "PLN_004"}
	if got := legsOf(t, db.DB, "SNF7", "A", "PLN_002"); got != want {
		t.Errorf("mirrored legs = %v, want %v", got, want)
	}
}

// TestHandlePlantClaims_AnOlderEdgeLandsBlankLegs is the rollout half. Edge
// deploys before Core, so for a window a NEW Core is fed by an OLD Edge whose
// report simply has no leg keys — the decoder leaves them "", and the
// empty-string-defaulted columns take that without an error.
//
// Blank is the correct record of what Core was told. It must not become a
// guess: a leg Core invented would be an arc in the material loop that nobody
// configured, and the compiler downstream cannot tell an invented arc from a
// real one.
func TestHandlePlantClaims_AnOlderEdgeLandsBlankLegs(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})

	// No legs on the spec at all — exactly what an Edge too old to publish
	// them sends, because an absent JSON key and an unset field are the same
	// thing at the decoder.
	svc.HandlePlantClaims(nil, plantClaimsReport("SNF8", 1, []styleSpec{
		{name: "A", claims: []claimSpec{{node: "PLN_002", payload: "BIN-A", allowed: []string{"BIN-A"}}}},
	}))

	if got := legsOf(t, db.DB, "SNF8", "A", "PLN_002"); got != [4]string{} {
		t.Errorf("legs from an older Edge = %v, want four blanks", got)
	}
	// And the rest of the mirror is untouched by their absence.
	idx, err := db.PlantClaimsDirtyIndex()
	if err != nil {
		t.Fatalf("dirty index: %v", err)
	}
	if got := payloadTargets(idx, "BIN-A"); !reflect.DeepEqual(got, []string{"SNF8|A"}) {
		t.Errorf("dirty index BIN-A = %v, want [SNF8|A] — blank legs must not disturb sourceability", got)
	}
}

// TestHandlePlantClaims_LegsDoNotDirtyTheRecompute is the other half of the
// boundary the source guard in store/plantclaims states: the dirty index is
// built from payloads, so two reports that differ ONLY in their legs produce
// the same index. A changeover-position edit changes no payload and no stock,
// and a sourceability verdict that moved for one would be a false alarm on an
// operator screen.
func TestHandlePlantClaims_LegsDoNotDirtyTheRecompute(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})

	withLegs := func(gen int64, inbound string) *protocol.PlantClaimsReport {
		return plantClaimsReport("SNF9", gen, []styleSpec{
			{name: "A", claims: []claimSpec{{
				node: "PLN_002", payload: "BIN-A", allowed: []string{"BIN-A"},
				inbound: inbound, outbound: "SMN_SYN_B", paired: "PLN_003",
			}}},
		})
	}

	svc.HandlePlantClaims(nil, withLegs(1, "SMN_SYN_A"))
	before, err := db.PlantClaimsDirtyIndex()
	if err != nil {
		t.Fatalf("dirty index before: %v", err)
	}

	svc.HandlePlantClaims(nil, withLegs(2, "SMN_SYN_C"))
	after, err := db.PlantClaimsDirtyIndex()
	if err != nil {
		t.Fatalf("dirty index after: %v", err)
	}

	if !reflect.DeepEqual(before, after) {
		t.Errorf("moving a leg changed the dirty index:\n before: %v\n after:  %v", before, after)
	}
	// The leg itself did move, so the comparison above is about the index and
	// not about a replace that did nothing.
	if got := legsOf(t, db.DB, "SNF9", "A", "PLN_002")[0]; got != "SMN_SYN_C" {
		t.Fatalf("inbound_source after the second report = %q, want SMN_SYN_C", got)
	}
}
