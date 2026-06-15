package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (CGO-free), same as shingo-edge

	"shingocore/plantspec"
	"shingocore/store/nodes"
)

// seedEdge writes the plant's edge topology into the (already-migrated) edge
// SQLite via RAW SQL — seeddev does not import the shingo-edge module (decision
// 2026-06-07), so this is the recorded engine-API gap: there is no edge seeding
// API reachable from core. Column lists track shingo-edge/store/schema. Every
// insert is INSERT OR IGNORE keyed on the table's natural-key UNIQUE index, so
// re-runs are idempotent. process_node_runtime_states are created lazily by the
// edge (EnsureRuntime), so we don't seed them.
//
// The edge DB must already exist + be migrated (the edge container boots before
// `make dev-seed`); seeddev only INSERTs.
// sqlExec is the subset of *sql.DB / *sql.Tx that seedEdgeDB uses, so the same
// code runs inside a transaction (production, to avoid lock churn) or directly
// against a DB (tests).
type sqlExec interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

func seedEdge(dbPath string, p *plantspec.Plant, binIDByNode map[string]int64) error {
	// The edge process holds this SQLite open (WAL) and writes counter snapshots
	// on every poll; a long busy_timeout + seeding inside ONE transaction grabs
	// the single writer lock once instead of racing the edge per-INSERT (the
	// SQLITE_BUSY we hit otherwise).
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=20000&_foreign_keys=on")
	if err != nil {
		return fmt.Errorf("open edge sqlite %s: %w", dbPath, err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping edge sqlite %s (is the edge migrated/running?): %w", dbPath, err)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin edge tx: %w", err)
	}
	if err := seedEdgeDB(tx, p, binIDByNode); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// seedEdgeDB is split out so tests can pass a plain *sql.DB. binIDByNode
// (node name → core bin id, from seedCore) lets it bind each produce/consume
// node's runtime active_bin_id; an empty map (edge-only re-seed) skips that.
func seedEdgeDB(db sqlExec, p *plantspec.Plant, binIDByNode map[string]int64) error {
	// processes
	procIDs := make(map[string]int64)
	for _, pr := range p.Processes {
		id, err := edgeUpsert(db,
			`INSERT OR IGNORE INTO processes(name, description) VALUES(?, ?)`,
			`SELECT id FROM processes WHERE name=?`,
			[]any{pr.Name, pr.Name + " (dev)"}, []any{pr.Name})
		if err != nil {
			return fmt.Errorf("process %s: %w", pr.Name, err)
		}
		procIDs[pr.Name] = id
	}

	// styles (process_id + name unique)
	styleIDs := make(map[string]int64)
	styleProc := make(map[string]string)
	for _, s := range p.Styles {
		pid, ok := procIDs[s.Process]
		if !ok {
			return fmt.Errorf("style %s: process %q not seeded", s.Name, s.Process)
		}
		id, err := edgeUpsert(db,
			`INSERT OR IGNORE INTO styles(process_id, name, description) VALUES(?, ?, ?)`,
			`SELECT id FROM styles WHERE process_id=? AND name=?`,
			[]any{pid, s.Name, s.Payload}, []any{pid, s.Name})
		if err != nil {
			return fmt.Errorf("style %s: %w", s.Name, err)
		}
		styleIDs[s.Name] = id
		styleProc[s.Name] = s.Process
	}

	// Set each process's active style. Without active_style_id, findActiveClaim
	// returns nil and the counter-delta tick handler skips every node — no
	// UOP tracking, no reorder/relief, no orders. This is the bootstrap key.
	activeStyleByProcess := make(map[string]string)
	for _, pr := range p.Processes {
		if pr.ActiveStyle == "" {
			continue
		}
		activeStyleByProcess[pr.Name] = pr.ActiveStyle
		sid, ok := styleIDs[pr.ActiveStyle]
		pid, pok := procIDs[pr.Name]
		if !ok || !pok {
			continue
		}
		if _, err := db.Exec(`UPDATE processes SET active_style_id=? WHERE id=?`, sid, pid); err != nil {
			return fmt.Errorf("set active_style for process %s: %w", pr.Name, err)
		}
	}

	// operator stations come from cell_configs (each ties a station to a process)
	stationIDs := make(map[string]int64)  // station code → id
	procStation := make(map[string]int64) // process name → operator station id
	for _, cc := range p.CellConfigs {
		pid, ok := procIDs[cc.Process]
		if !ok {
			continue
		}
		id, err := edgeUpsert(db,
			`INSERT OR IGNORE INTO operator_stations(process_id, code, name) VALUES(?, ?, ?)`,
			`SELECT id FROM operator_stations WHERE process_id=? AND code=?`,
			[]any{pid, cc.Station, cc.Station}, []any{pid, cc.Station})
		if err != nil {
			return fmt.Errorf("operator station %s: %w", cc.Station, err)
		}
		stationIDs[cc.Station] = id
		procStation[cc.Process] = id
	}

	// Per-window operator stations. A claim may pin its node to its OWN station
	// (operator_station) — the per-window-HMI model, one window per physical screen,
	// so an operator loads the bin in front of them with no window-picker. Create each
	// such station on the claim's process and map the node to it; the process_node
	// below uses this instead of the process default.
	claimStationByNode := make(map[string]int64) // core_node → operator station id
	for _, c := range p.Claims {
		if c.OperatorStation == "" {
			continue
		}
		pid, ok := procIDs[styleProc[c.Style]]
		if !ok {
			continue
		}
		id, err := edgeUpsert(db,
			`INSERT OR IGNORE INTO operator_stations(process_id, code, name) VALUES(?, ?, ?)`,
			`SELECT id FROM operator_stations WHERE process_id=? AND code=?`,
			[]any{pid, c.OperatorStation, c.OperatorStation}, []any{pid, c.OperatorStation})
		if err != nil {
			return fmt.Errorf("operator station %s (node %s): %w", c.OperatorStation, c.CoreNode, err)
		}
		stationIDs[c.OperatorStation] = id
		claimStationByNode[c.CoreNode] = id
	}

	// process_nodes: one per distinct (process, core_node) drawn from claims.
	pnIDByNode := make(map[string]int64) // core_node → process_node id (for runtime seeding)
	seenPN := make(map[string]bool)
	for _, c := range p.Claims {
		proc := styleProc[c.Style]
		pid, ok := procIDs[proc]
		if !ok {
			continue
		}
		key := proc + "|" + c.CoreNode
		if seenPN[key] {
			continue
		}
		seenPN[key] = true
		opStation := sql.NullInt64{}
		if sid, ok := claimStationByNode[c.CoreNode]; ok {
			opStation = sql.NullInt64{Int64: sid, Valid: true} // per-window station
		} else if sid, ok := procStation[proc]; ok {
			opStation = sql.NullInt64{Int64: sid, Valid: true}
		}
		pnID, err := edgeUpsert(db,
			`INSERT OR IGNORE INTO process_nodes(process_id, operator_station_id, core_node_name, code, name) VALUES(?, ?, ?, ?, ?)`,
			`SELECT id FROM process_nodes WHERE process_id=? AND code=?`,
			[]any{pid, opStation, c.CoreNode, c.CoreNode, c.CoreNode}, []any{pid, c.CoreNode})
		if err != nil {
			return fmt.Errorf("process_node %s/%s: %w", proc, c.CoreNode, err)
		}
		pnIDByNode[c.CoreNode] = pnID
	}

	// style_node_claims (the full claim row)
	claimIDByStyleNode := make(map[string]int64) // "style|core_node" → claim id
	for _, c := range p.Claims {
		sid, ok := styleIDs[c.Style]
		if !ok {
			return fmt.Errorf("claim %s/%s: style %q not seeded", c.CoreNode, c.Style, c.Style)
		}
		if _, err := db.Exec(`
			INSERT OR IGNORE INTO style_node_claims
			  (style_id, core_node_name, role, swap_mode, payload_code, uop_capacity,
			   reorder_point, auto_reorder, inbound_staging, outbound_staging,
			   inbound_source, outbound_destination, allowed_payload_codes,
			   paired_core_node, auto_push, auto_confirm)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sid, c.CoreNode, c.Role, c.SwapMode, c.Payload, c.UOPCapacity,
			c.ReorderPoint, b2i(c.AutoReorder), c.InboundStaging, c.OutboundStaging,
			c.InboundSource, c.OutboundDestination, strings.Join(c.AllowedPayloads, ","),
			c.PairedCoreNode, b2i(c.AutoPush), b2i(c.AutoConfirm)); err != nil {
			return fmt.Errorf("claim %s/%s: %w", c.CoreNode, c.Style, err)
		}
		var claimID int64
		if err := db.QueryRow(`SELECT id FROM style_node_claims WHERE style_id=? AND core_node_name=?`, sid, c.CoreNode).Scan(&claimID); err != nil {
			return fmt.Errorf("claim %s/%s id: %w", c.CoreNode, c.Style, err)
		}
		claimIDByStyleNode[c.Style+"|"+c.CoreNode] = claimID
	}

	// reporting points (style_id + plc/tag)
	for _, rp := range p.ReportingPoints {
		var sid int64
		if rp.Style != "" {
			id, ok := styleIDs[rp.Style]
			if !ok {
				return fmt.Errorf("reporting point %s/%s: style %q not seeded", rp.PLCName, rp.TagName, rp.Style)
			}
			sid = id
		}
		if _, err := db.Exec(
			`INSERT OR IGNORE INTO reporting_points(style_id, plc_name, tag_name, enabled) VALUES(?,?,?,1)`,
			sid, rp.PLCName, rp.TagName); err != nil {
			return fmt.Errorf("reporting point %s/%s: %w", rp.PLCName, rp.TagName, err)
		}
	}

	// payload_catalog (edge mirror of core payloads). id is explicit; we assign
	// sequential ids in spec order — on a fresh core DB these match core's serial
	// payload ids (created in the same order). Code is the authoritative key.
	for i, pl := range p.Payloads {
		if _, err := db.Exec(
			`INSERT OR REPLACE INTO payload_catalog(id, name, code, uop_capacity) VALUES(?,?,?,?)`,
			int64(i+1), pl.Code, pl.Code, pl.UOPCapacity); err != nil {
			return fmt.Errorf("payload_catalog %s: %w", pl.Code, err)
		}
	}

	// Runtime states: bind each produce/consume cell node's active bin so the
	// counter-delta ticks have something to fill/drain. Auto-relief (produce, at
	// capacity) and auto-reorder (consume, at reorder_point) only fire when
	// active_bin_id is set — without this the loops never bootstrap. Bind to the
	// node's ACTIVE-style claim; manual_swap loaders/unloaders don't tick, so
	// they're left to the edge's lazy EnsureRuntime. The A/B parked side gets
	// active_pull=0 so ticks skip it. INSERT OR REPLACE overwrites any lazily-
	// created row (process_node_id is UNIQUE), resetting runtime on re-seed.
	binUOPByNode := make(map[string]int64)
	for _, b := range p.Bins {
		binUOPByNode[b.Slot] = b.UOP
	}
	runtimeSeeded := 0
	for _, c := range p.Claims {
		if c.IsManualSwap() || (c.Role != "produce" && c.Role != "consume") {
			continue
		}
		if activeStyleByProcess[styleProc[c.Style]] != c.Style {
			continue // only the node's active-style claim binds the runtime
		}
		binID, hasBin := binIDByNode[c.CoreNode]
		pnID, hasPN := pnIDByNode[c.CoreNode]
		claimID, hasClaim := claimIDByStyleNode[c.Style+"|"+c.CoreNode]
		if !hasBin || !hasPN || !hasClaim {
			continue
		}
		if _, err := db.Exec(
			`INSERT OR REPLACE INTO process_node_runtime_states
			  (process_node_id, active_claim_id, active_bin_id, active_bin_epoch, remaining_uop_cached, active_pull)
			  VALUES (?,?,?,1,?,?)`,
			pnID, claimID, binID, binUOPByNode[c.CoreNode], b2i(c.IsActivePull())); err != nil {
			return fmt.Errorf("runtime state for %s: %w", c.CoreNode, err)
		}
		runtimeSeeded++
	}

	log.Printf("edge: %d processes, %d styles, %d operator stations, %d claims, %d reporting points, %d catalog payloads, %d runtime states",
		len(procIDs), len(styleIDs), len(stationIDs), len(p.Claims), len(p.ReportingPoints), len(p.Payloads), runtimeSeeded)
	return nil
}

// edgeUpsert runs an INSERT-OR-IGNORE then selects the natural-key row's id.
func edgeUpsert(db sqlExec, insertSQL, selectSQL string, insertArgs, selectArgs []any) (int64, error) {
	if _, err := db.Exec(insertSQL, insertArgs...); err != nil {
		return 0, err
	}
	var id int64
	if err := db.QueryRow(selectSQL, selectArgs...).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// crossValidate confirms every edge node reference resolves to a core node: the
// process_nodes' core_node_name and each claim's inbound_source /
// outbound_destination. The spec validator already enforces internal
// consistency, so this catches a *seeding* bug (a node that didn't get created)
// rather than a spec error. Reports all mismatches at once.
func crossValidate(coreDB coreNodeChecker, edgeDBPath string) error {
	db, err := sql.Open("sqlite", edgeDBPath+"?_busy_timeout=5000")
	if err != nil {
		return fmt.Errorf("open edge sqlite: %w", err)
	}
	defer db.Close()

	var problems []string
	resolves := func(name string) bool {
		if name == "" {
			return true
		}
		n, err := coreDB.GetNodeByName(name)
		return err == nil && n != nil
	}

	pnRows, err := db.Query(`SELECT DISTINCT core_node_name FROM process_nodes WHERE core_node_name != ''`)
	if err != nil {
		return fmt.Errorf("query process_nodes: %w", err)
	}
	for pnRows.Next() {
		var name string
		if err := pnRows.Scan(&name); err != nil {
			pnRows.Close()
			return err
		}
		if !resolves(name) {
			problems = append(problems, fmt.Sprintf("edge process_node core_node_name %q has no matching core node", name))
		}
	}
	pnRows.Close()

	clRows, err := db.Query(`SELECT core_node_name, inbound_source, outbound_destination FROM style_node_claims`)
	if err != nil {
		return fmt.Errorf("query style_node_claims: %w", err)
	}
	for clRows.Next() {
		var node, inSrc, outDst string
		if err := clRows.Scan(&node, &inSrc, &outDst); err != nil {
			clRows.Close()
			return err
		}
		if !resolves(inSrc) {
			problems = append(problems, fmt.Sprintf("claim %s inbound_source %q has no matching core node", node, inSrc))
		}
		if !resolves(outDst) {
			problems = append(problems, fmt.Sprintf("claim %s outbound_destination %q has no matching core node", node, outDst))
		}
	}
	clRows.Close()

	if len(problems) > 0 {
		return fmt.Errorf("%d cross-DB mismatch(es):\n  - %s", len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

// coreNodeChecker is the slice of *store.DB crossValidate needs (also lets tests
// pass a fake).
type coreNodeChecker interface {
	GetNodeByName(name string) (*nodes.Node, error)
}
