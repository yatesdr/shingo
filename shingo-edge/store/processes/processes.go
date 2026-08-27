// Package processes holds process, process_node, and
// process_node_runtime persistence for shingo-edge. All three sit on
// the same aggregate: a process owns a set of process_nodes, each of
// which has a runtime row that tracks the active claim, remaining UOP,
// and currently-tracked orders.
//
// Phase 5b of the architecture plan moved this CRUD out of the flat
// store/ package and into this sub-package. The outer store/ keeps
// type aliases (`store.Process = processes.Process`, etc.) and one-line
// delegate methods on *store.DB so external callers see no API change.
package processes

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// Process, Node, NodeInput, and RuntimeState are the process-aggregate
// data types. The structs live in shingoedge/domain (Stage 2A.2);
// these aliases keep the unprefixed processes.X names used by every
// scan helper, Create/Update call site, and the outer store/
// re-exports. www handlers reference the types via shingoedge/domain
// instead of importing this persistence sub-package.
type (
	Process      = domain.Process
	Node         = domain.Node
	NodeInput    = domain.NodeInput
	RuntimeState = domain.RuntimeState
)

// --- processes ---

func scanProcess(scanner interface{ Scan(...any) error }) (Process, error) {
	var p Process
	var createdAt string
	var groupID sql.NullInt64
	if err := scanner.Scan(&p.ID, &p.Name, &p.Description, &p.ActiveStyleID, &p.TargetStyleID, &p.ProductionState, &p.CounterPLCName, &p.CounterTagName, &p.CounterEnabled, &p.ChangeoverAutoArm, &groupID, &createdAt); err != nil {
		return p, err
	}
	p.CreatedAt = helpers.ScanTime(createdAt)
	if groupID.Valid {
		id := groupID.Int64
		p.GroupID = &id
	}
	return p, nil
}

// auto_cutover_enabled is deliberately absent: PLC-driven cutover was removed
// (the Changeover_Active tag was never wired at any plant). The column stays on
// disk — dropping it means a SQLite table rebuild, and a rebuild is what
// generates the dangling REFERENCES clauses the FK repair exists to fix.
const processSelect = `id, name, description, active_style_id, target_style_id, production_state, counter_plc_name, counter_tag_name, counter_enabled, changeover_auto_arm, group_id, created_at`

// List returns every process row sorted by name.
func List(db *sql.DB) ([]Process, error) {
	rows, err := db.Query(`SELECT ` + processSelect + ` FROM processes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Process
	for rows.Next() {
		l, err := scanProcess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Get returns one process by id.
func Get(db *sql.DB, id int64) (*Process, error) {
	l, err := scanProcess(db.QueryRow(`SELECT `+processSelect+` FROM processes WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// Create inserts a process and returns the new row id.
func Create(db *sql.DB, name, description, productionState string, counterPLC, counterTag string, counterEnabled bool) (int64, error) {
	if productionState == "" {
		productionState = "active_production"
	}
	res, err := db.Exec(`INSERT INTO processes (name, description, production_state, counter_plc_name, counter_tag_name, counter_enabled) VALUES (?, ?, ?, ?, ?, ?)`,
		name, description, productionState, counterPLC, counterTag, counterEnabled)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Update modifies a process row.
func Update(db *sql.DB, id int64, name, description, productionState string, counterPLC, counterTag string, counterEnabled bool) error {
	if productionState == "" {
		productionState = "active_production"
	}
	_, err := db.Exec(`UPDATE processes SET name=?, description=?, production_state=?, counter_plc_name=?, counter_tag_name=?, counter_enabled=? WHERE id=?`,
		name, description, productionState, counterPLC, counterTag, counterEnabled, id)
	return err
}

// ErrProcessHasStock refuses a process delete while lineside stock is still
// booked against its nodes.
//
// A node_lineside_bucket row is an INVENTORY RECORD — it says how many parts
// are at a node right now — and a routine config action must not be able to
// destroy one quietly. Refusing is also the more useful answer: "you still have
// parts booked here" is a sentence somebody can act on, where a vanished count
// is not.
var ErrProcessHasStock = errors.New("process still has lineside stock booked at its nodes: clear or consume it first")

// Delete removes a process and retires the rows that are meaningless without it.
//
// It used to be one statement — `DELETE FROM processes WHERE id=?` — with the
// children left to ON DELETE CASCADE. THAT CASCADE NEVER FIRES: edge SQLite runs
// with foreign keys OFF, so every process ever deleted left its styles, nodes and
// stations behind. The live Springfield edge carries five orphaned styles, two
// stale sourcing_state rows, and — until 2026-08-26 — an orphaned process_node
// from a process deleted an hour earlier, which SetNodes was still willing to
// ADOPT by core_node_name onto a live station.
//
// Each child is retired exactly the way deleting it on its own would, rather than
// uniformly hard-deleted, because both soft deletes exist for reasons that do not
// stop applying just because the parent is going:
//
//   - STYLES are soft-deleted and their reporting points disabled, mirroring
//     DeleteStyle. hourly_counts and daily_counts key on style_id, so hard-deleting
//     a style strands the production record it counted — the exact defect soft
//     delete was introduced to fix.
//   - PROCESS_NODES are soft-deleted and their runtime states dropped, mirroring
//     DeleteNode. changeover_node_tasks.process_node_id is NOT NULL ON DELETE
//     CASCADE, so a hard delete destroys per-node changeover detail rather than
//     detaching it. Soft delete is also what closes the adoption hole:
//     ListNodesByProcess filters on liveNodes, so a retired row can no longer be
//     adopted by name.
//
// DELIBERATELY NOT TOUCHED: hourly_counts and daily_counts (the permanent
// production record — they carry no FK precisely so a config action cannot reach
// them, see store/schema/sqlite_ddl.go), process_changeovers and orders (history
// that stays readable), and style_node_claims (owned by their now-retired style,
// and kept so that restoring the style is still a restore).
func Delete(db *sql.DB, id int64) error {
	var booked int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_lineside_bucket b
		JOIN process_nodes n ON n.id = b.node_id
		WHERE n.process_id = ? AND b.qty > 0`, id).Scan(&booked); err != nil {
		return fmt.Errorf("process %d: check lineside stock: %w", id, err)
	}
	if booked > 0 {
		return fmt.Errorf("%w (%d bucket(s) still hold parts)", ErrProcessHasStock, booked)
	}

	// sourcing_state and demand_origins_open key on the process NAME, not its id,
	// so the name must be read before the row goes. Leaving them is not merely
	// untidy — a later process created with the same name inherits the stale state.
	var name string
	switch err := db.QueryRow(`SELECT name FROM processes WHERE id=?`, id).Scan(&name); {
	case errors.Is(err, sql.ErrNoRows):
		return nil // already gone; deleting twice is not an error
	case err != nil:
		return fmt.Errorf("process %d: %w", id, err)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	steps := []struct {
		q   string
		arg any
	}{
		{`UPDATE styles SET deleted_at = datetime('now') WHERE process_id=? AND` + liveStyles, id},
		{`UPDATE reporting_points SET enabled = 0 WHERE style_id IN (SELECT id FROM styles WHERE process_id=?)`, id},
		{`DELETE FROM process_node_runtime_states WHERE process_node_id IN (SELECT id FROM process_nodes WHERE process_id=?)`, id},
		// operator_station_id is cleared because the station row itself goes in
		// the next statement: a retired node pointing at a deleted station is a
		// dangling id that RestoreNode would hand back.
		{`UPDATE process_nodes SET deleted_at = datetime('now'), updated_at = datetime('now'), operator_station_id = NULL
			WHERE process_id=? AND deleted_at IS NULL`, id},
		{`DELETE FROM operator_stations WHERE process_id=?`, id},
		{`DELETE FROM sourcing_state WHERE process_id=?`, name},
		{`DELETE FROM demand_origins_open WHERE process_id=?`, name},
		{`DELETE FROM processes WHERE id=?`, id},
	}
	for _, s := range steps {
		if _, err := tx.Exec(s.q, s.arg); err != nil {
			return fmt.Errorf("process %d delete: %w", id, err)
		}
	}
	return tx.Commit()
}

// SetActiveStyle changes the active_style_id on a process.
func SetActiveStyle(db *sql.DB, processID int64, styleID *int64) error {
	_, err := db.Exec(`UPDATE processes SET active_style_id=? WHERE id=?`, styleID, processID)
	return err
}

// SetTargetStyle changes the target_style_id on a process (used during
// changeovers).
func SetTargetStyle(db *sql.DB, processID int64, styleID *int64) error {
	_, err := db.Exec(`UPDATE processes SET target_style_id=? WHERE id=?`, styleID, processID)
	return err
}

// GetActiveStyleID returns just the active_style_id pointer for a
// process.
func GetActiveStyleID(db *sql.DB, processID int64) (*int64, error) {
	var id *int64
	err := db.QueryRow(`SELECT active_style_id FROM processes WHERE id = ?`, processID).Scan(&id)
	if err != nil {
		return nil, err
	}
	return id, nil
}

// SetProductionState writes the production_state on a process.
func SetProductionState(db *sql.DB, processID int64, state string) error {
	_, err := db.Exec(`UPDATE processes SET production_state=? WHERE id=?`, state, processID)
	return err
}

// SetChangeoverAutoArm writes the changeover_auto_arm mode on a process,
// normalized to one of auto|prompt|off (empty/unknown ⇒ auto). Kept as a focused
// setter rather than another positional arg on Create/Update so the CATID auto-arm
// mode threads through without churning every process-CRUD call site.
func SetChangeoverAutoArm(db *sql.DB, processID int64, mode string) error {
	_, err := db.Exec(`UPDATE processes SET changeover_auto_arm=? WHERE id=?`, domain.NormalizeChangeoverAutoArm(mode), processID)
	return err
}

// SetGroupID assigns a process to a group, or unassigns it (pass nil) back
// to "Ungrouped". Pure UI taxonomy — the runtime never reads group_id.
func SetGroupID(db *sql.DB, processID int64, groupID *int64) error {
	_, err := db.Exec(`UPDATE processes SET group_id=? WHERE id=?`, groupID, processID)
	return err
}

// --- process nodes ---

const nodeSelect = `n.id, n.process_id, n.operator_station_id, n.core_node_name, n.code, n.name,
	n.sequence, n.enabled, n.created_at, n.updated_at, n.deleted_at, COALESCE(s.name, ''), COALESCE(p.name, '')`

// liveNodes is the WHERE fragment for "process_nodes that still exist as far as
// the plant is concerned". Applied to the LIST paths, which are pickers and
// board sources, and NOT to GetNode / GetNodeByCoreNodeName, which resolve an
// id or a name somebody already holds — the same split as liveStyles.
const liveNodes = ` n.deleted_at IS NULL`

const nodeJoin = `FROM process_nodes n
	LEFT JOIN operator_stations s ON s.id = n.operator_station_id
	LEFT JOIN processes p ON p.id = n.process_id`

func scanNode(scanner interface{ Scan(...any) error }) (Node, error) {
	var n Node
	var createdAt, updatedAt string
	var stationID sql.NullInt64
	var deletedAt sql.NullString
	err := scanner.Scan(
		&n.ID, &n.ProcessID, &stationID, &n.CoreNodeName, &n.Code, &n.Name,
		&n.Sequence, &n.Enabled, &createdAt, &updatedAt, &deletedAt, &n.StationName, &n.ProcessName,
	)
	if err != nil {
		return n, err
	}
	n.CreatedAt = helpers.ScanTime(createdAt)
	n.UpdatedAt = helpers.ScanTime(updatedAt)
	n.DeletedAt = helpers.ScanTimePtr(deletedAt)
	if stationID.Valid {
		id := stationID.Int64
		n.OperatorStationID = &id
	}
	return n, nil
}

func scanNodes(rows helpers.RowScanner) ([]Node, error) {
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListNodes returns every LIVE process_nodes row.
func ListNodes(db *sql.DB) ([]Node, error) {
	rows, err := db.Query(`SELECT ` + nodeSelect + ` ` + nodeJoin + ` WHERE` + liveNodes + ` ORDER BY n.process_id, n.sequence, n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ListNodesByProcess returns process_nodes rows for one process.
func ListNodesByProcess(db *sql.DB, processID int64) ([]Node, error) {
	rows, err := db.Query(`SELECT `+nodeSelect+` `+nodeJoin+` WHERE n.process_id=? AND`+liveNodes+` ORDER BY n.sequence, n.name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ListNodesByStation returns process_nodes rows for one operator_station.
func ListNodesByStation(db *sql.DB, stationID int64) ([]Node, error) {
	rows, err := db.Query(`SELECT `+nodeSelect+` `+nodeJoin+` WHERE n.operator_station_id=? AND`+liveNodes+` ORDER BY n.sequence, n.name`, stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetNode returns one process_node row by id.
func GetNode(db *sql.DB, id int64) (*Node, error) {
	n, err := scanNode(db.QueryRow(`SELECT `+nodeSelect+` `+nodeJoin+` WHERE n.id=?`, id))
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// GetNodeByCoreNodeName returns one process_node row by its
// core_node_name. Used by the UOP adjustment handler to resolve the
// target Edge node from a Core-originated adjustment envelope.
//
// ── CORE_NODE_NAME IS NOT UNIQUE HERE, AND THIS USED TO PRETEND IT WAS ────
//
// A core node name is plant-unique on CORE (nodes.name is TEXT NOT NULL
// UNIQUE). It is NOT unique in process_nodes: one physical slot can be named by
// several processes — a shared loader window is the ordinary case, and the
// manual_swap scoping note in operator_bin_ops calls it out in as many words
// ("a shared node has many process_node rows for one slot").
//
// This was a bare QueryRow, which takes whatever row the engine hands back
// first. With no ORDER BY that is not merely unscoped, it is UNSTABLE: the same
// question can answer differently across restarts, index changes or a VACUUM,
// and every caller believed it had THE node. Six callers, all of them resolving
// a Core-originated name to an Edge row — a UOP adjustment, a bin-epoch
// refresh, two loader demand paths, the order projection, and the delivered
// fallback. An adjustment applied to the wrong process's row is a count written
// to the wrong slot.
//
// ── WHY IT IS NOT SCOPED BY PROCESS INSTEAD ───────────────────────────────
//
// Because none of the six callers HAS a process to scope by. They are handed a
// Core node name and the process is the thing they are trying to learn; a
// process parameter would have to be invented at each site, which is guessing
// wearing a signature. The honest fix for a lookup whose key is not unique is
// to be deterministic about the answer and LOUD about the ambiguity.
//
// So: lowest id wins, stably, and a match count above one is reported with the
// processes named. The disposition is unchanged — every caller still gets a
// node — because turning this into an error would stop UOP tracking on a shared
// loader, which is a working configuration. What changes is that the ambiguity
// stops being invisible.
func GetNodeByCoreNodeName(db *sql.DB, coreNodeName string) (*Node, error) {
	rows, err := db.Query(`SELECT `+nodeSelect+` `+nodeJoin+
		` WHERE n.core_node_name=? ORDER BY n.id`, coreNodeName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		first     *Node
		processes []string
	)
	for rows.Next() {
		n, serr := scanNode(rows)
		if serr != nil {
			return nil, serr
		}
		if first == nil {
			node := n
			first = &node
		}
		processes = append(processes, fmt.Sprintf("process=%d node=%s", n.ProcessID, n.Name))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if first == nil {
		// sql.ErrNoRows, preserved: resolveProjectionNode and the delivered
		// fallback both branch on it, and both treat it as the ordinary "we do
		// not own this destination" answer rather than a fault.
		return nil, sql.ErrNoRows
	}
	if len(processes) > 1 {
		log.Printf("WARN: core node %q maps to %d process_node rows (%s) — resolving to the lowest "+
			"id, which is STABLE but arbitrary. This lookup has no process to scope by; every "+
			"caller is turning a Core-originated name into an Edge row. If the rows belong to "+
			"different processes, whichever answer this gives, some caller is getting a node it "+
			"did not mean — a UOP adjustment applied here is a count written to one process's slot "+
			"on behalf of another's.",
			coreNodeName, len(processes), strings.Join(processes, "; "))
	}
	return first, nil
}

// CreateNode inserts a process_node row, generating the code and
// sequence number when not supplied.
func CreateNode(db *sql.DB, in NodeInput) (int64, error) {
	in.CoreNodeName = strings.TrimSpace(in.CoreNodeName)
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = in.CoreNodeName
	}
	if in.OperatorStationID != nil && *in.OperatorStationID <= 0 {
		in.OperatorStationID = nil
	}
	if in.Code == "" {
		code, err := generateNodeCode(db, in.ProcessID, in.CoreNodeName, in.Name)
		if err != nil {
			return 0, err
		}
		in.Code = code
	}
	if in.Sequence <= 0 {
		next, err := nextNodeSequence(db, in.ProcessID)
		if err != nil {
			return 0, err
		}
		in.Sequence = next
	}
	res, err := db.Exec(`INSERT INTO process_nodes (
		process_id, operator_station_id, core_node_name, code, name, sequence, enabled
	) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		in.ProcessID, in.OperatorStationID, in.CoreNodeName, in.Code, in.Name, in.Sequence, in.Enabled,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateNode modifies an existing process_node row, falling back to
// the existing code/sequence when the input leaves them blank.
func UpdateNode(db *sql.DB, id int64, in NodeInput) error {
	existing, err := GetNode(db, id)
	if err != nil {
		return err
	}
	in.CoreNodeName = strings.TrimSpace(in.CoreNodeName)
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = in.CoreNodeName
	}
	if in.OperatorStationID != nil && *in.OperatorStationID <= 0 {
		in.OperatorStationID = nil
	}
	if in.Code == "" {
		in.Code = existing.Code
	}
	if in.Sequence <= 0 {
		in.Sequence = existing.Sequence
	}
	_, err = db.Exec(`UPDATE process_nodes SET
		process_id=?, operator_station_id=?, core_node_name=?, code=?, name=?,
		sequence=?, enabled=?, updated_at=datetime('now')
		WHERE id=?`,
		in.ProcessID, in.OperatorStationID, in.CoreNodeName, in.Code, in.Name,
		in.Sequence, in.Enabled, id,
	)
	return err
}

// DeleteNode RETIRES a process_node: it sets deleted_at rather than removing
// the row.
//
// The reason is narrower and sharper than the styles one.
// changeover_node_tasks.process_node_id is NOT NULL and declares ON DELETE
// CASCADE, so a hard delete does not detach that node's per-node changeover
// detail — it destroys it, with no SET NULL alternative available. The
// Springfield edge already carries 118 such rows whose node is gone while all
// 118 parent changeovers survive: a changeover you can open and read, with the
// part that says what each node was supposed to do missing.
//
// It also cascades into process_node_runtime_states, which is genuinely
// ephemeral, so that row IS deleted here — carrying stale runtime state for a
// node nobody can address is how a phantom badge appears.
func DeleteNode(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE process_nodes SET deleted_at = datetime('now'), updated_at = datetime('now')
		WHERE id=? AND deleted_at IS NULL`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM process_node_runtime_states WHERE process_node_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RestoreNode un-retires a process_node. The runtime state row is NOT restored
// — it is rebuilt from live state by EnsureNodeRuntime, and inventing one here
// would assert a bin position nobody has observed.
func RestoreNode(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE process_nodes SET deleted_at = NULL, updated_at = datetime('now')
		WHERE id=? AND deleted_at IS NOT NULL`, id)
	return err
}

func nextNodeSequence(db *sql.DB, processID int64) (int, error) {
	var maxSeq sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(sequence) FROM process_nodes WHERE process_id=?`, processID).Scan(&maxSeq); err != nil {
		return 0, err
	}
	if !maxSeq.Valid {
		return 1, nil
	}
	return int(maxSeq.Int64) + 1, nil
}

func generateNodeCode(db *sql.DB, processID int64, coreNodeName, name string) (string, error) {
	base := helpers.SlugName(coreNodeName, "")
	if base == "" {
		base = helpers.SlugName(name, "")
	}
	return helpers.GenerateUniqueCode(db, "process_nodes", "process_id", processID, base, "node")
}

// --- process node runtime states ---

func scanRuntime(scanner interface{ Scan(...any) error }) (RuntimeState, error) {
	var r RuntimeState
	var updatedAt string
	err := scanner.Scan(&r.ID, &r.ProcessNodeID, &r.ActiveClaimID, &r.ActiveBinID, &r.ActiveBinEpoch, &r.RemainingUOPCached,
		&r.PendingUOPDelta, &r.ActiveOrderID, &r.StagedOrderID, &r.ActivePull, &updatedAt)
	if err != nil {
		return r, err
	}
	r.UpdatedAt = helpers.ScanTime(updatedAt)
	return r, nil
}

// RuntimesForNodes returns the existing runtime rows for a set of process_nodes
// in ONE query, keyed by process_node_id. Nodes with no row yet are simply
// absent from the map.
//
// This is the read half of EnsureRuntime, split out so a caller building a whole
// board can do one SELECT instead of one per tile — every read serialises on a
// single connection (store.Open sets SetMaxOpenConns(1)). It deliberately does
// NOT insert: the caller falls back to EnsureRuntime for exactly the ids that
// come back missing, so the write happens for the same set of nodes as before
// and the "ensure" semantics are preserved rather than reinterpreted.
func RuntimesForNodes(db *sql.DB, processNodeIDs []int64) (map[int64]*RuntimeState, error) {
	out := map[int64]*RuntimeState{}
	if len(processNodeIDs) == 0 {
		return out, nil
	}
	seen := make(map[int64]bool, len(processNodeIDs))
	args := make([]any, 0, len(processNodeIDs))
	var placeholders strings.Builder
	for _, id := range processNodeIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if placeholders.Len() > 0 {
			placeholders.WriteByte(',')
		}
		placeholders.WriteByte('?')
		args = append(args, id)
	}
	rows, err := db.Query(`SELECT id, process_node_id, active_claim_id, active_bin_id, active_bin_epoch, remaining_uop_cached,
		pending_uop_delta, active_order_id, staged_order_id, active_pull, updated_at
		FROM process_node_runtime_states WHERE process_node_id IN (`+placeholders.String()+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRuntime(rows)
		if err != nil {
			return nil, err
		}
		state := r
		out[r.ProcessNodeID] = &state
	}
	return out, rows.Err()
}

// EnsureRuntime returns the runtime row for a process_node, inserting
// a fresh row when none exists yet. INSERT OR IGNORE makes the
// check-then-insert race-safe when concurrent callers (engine tick,
// HMI handler, station service) hit a node whose runtime row hasn't
// been materialized yet.
func EnsureRuntime(db *sql.DB, processNodeID int64) (*RuntimeState, error) {
	if r, err := GetRuntime(db, processNodeID); err == nil {
		return r, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO process_node_runtime_states (process_node_id) VALUES (?)`, processNodeID); err != nil {
		return nil, err
	}
	return GetRuntime(db, processNodeID)
}

// GetRuntime returns the runtime row for a process_node.
func GetRuntime(db *sql.DB, processNodeID int64) (*RuntimeState, error) {
	r, err := scanRuntime(db.QueryRow(`SELECT id, process_node_id, active_claim_id, active_bin_id, active_bin_epoch, remaining_uop_cached,
		pending_uop_delta, active_order_id, staged_order_id, active_pull, updated_at
		FROM process_node_runtime_states WHERE process_node_id=?`, processNodeID))
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// SetRuntime updates the active claim and the cached UOP on a runtime
// row. Does NOT touch active_bin_id — callers that need to set or
// clear the bin pointer in the same write should use SetRuntimeWithBin
// (atomic three-field update) instead. Existing code paths that
// don't have a meaningful bin pointer (test setup, A/B flips, etc.)
// can keep calling this without churn.
func SetRuntime(db *sql.DB, processNodeID int64, activeClaimID *int64, remainingUOPCached int) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET
		active_claim_id=?, remaining_uop_cached=?, updated_at=datetime('now')
		WHERE process_node_id=?`,
		activeClaimID, remainingUOPCached, processNodeID)
	return err
}

// SetRuntimeClaimCountAndEpoch writes active_claim_id and
// remaining_uop_cached and advances the stamp for the carrier the write
// names, leaving the bin pointer alone. Used by the clear routes: Core
// starts a new life for the carrier when it clears it and returns the new
// stamp in the same reply, but the carrier itself does not move, so the
// pointer must not be rewritten. binID is the carrier Core named; the
// epoch lands only if that carrier is the one bound here — see
// epochAssignForBoundBin.
func SetRuntimeClaimCountAndEpoch(db *sql.DB, processNodeID int64, activeClaimID *int64, remainingUOPCached int, binID, deltaEpoch int64) error {
	args := []any{activeClaimID, remainingUOPCached}
	args = append(args, epochArgs(binID, deltaEpoch)...)
	args = append(args, processNodeID)
	_, err := db.Exec(`UPDATE process_node_runtime_states SET
		active_claim_id=?, remaining_uop_cached=?, `+epochAssignForBoundBin+`, updated_at=datetime('now')
		WHERE process_node_id=?`, args...)
	return err
}

// SetRuntimeWithBin updates active_claim_id, active_bin_id, and
// remaining_uop_cached in one atomic write. Used by every delivery-
// completion handler so the bin pointer turns over at the same instant
// the new bin is logically present. activeBinID is the bin physically
// arriving at the slot, or nil for removal-shaped completions where
// the slot ends up empty.
func SetRuntimeWithBin(db *sql.DB, processNodeID int64, activeClaimID, activeBinID *int64, remainingUOPCached int) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET
		active_claim_id=?, active_bin_id=?, remaining_uop_cached=?, updated_at=datetime('now')
		WHERE process_node_id=?`,
		activeClaimID, activeBinID, remainingUOPCached, processNodeID)
	return err
}

// epochAssignOnBind is the monotonicity rule for active_bin_epoch on
// statements that also write active_bin_id — the write is binding a
// carrier to the slot and carrying that carrier's stamp with it.
//
// active_bin_epoch is a copy of Core's bins.delta_epoch — a generation
// stamp the Edge puts on every count it reports, and that Core uses to
// discard counts belonging to a bin's previous life. Five separate paths
// write it, and last-write-wins is only safe while messages arrive in the
// order Core sent them. They do not: the outbox drainer publishes in id
// order but retries a failed message in place, so one failure reorders
// everything behind it. A message that lost the race then stamps an old
// generation over a new one and every count reported afterwards is thrown
// away at Core, silently.
//
// So: for the SAME bin the stamp only ever moves forward. When the bin at
// the slot changes, any stamp binds — each bin carries its own generation
// counter, so an arriving bin's 4 is not "older" than the departing bin's
// 9, it is unrelated. Refusing it would make the new carrier report under
// a stamp that was never its own.
//
// The rule guards the epoch COLUMN, not the statement: a write carrying an
// older stamp still lands its count and its bin pointer. Those are separate
// facts and the count is the one the operator's tile renders.
//
// Column references on the right-hand side of an UPDATE read the row as it
// was before the write, so `active_bin_id` here is the bin that was bound.
// Takes three bind parameters, in epochArgs order.
const epochAssignOnBind = `active_bin_epoch=CASE
			WHEN active_bin_id IS ? AND active_bin_epoch > ? THEN active_bin_epoch
			ELSE ? END`

// epochAssignForBoundBin is the same rule for statements that do NOT move
// the bin pointer. Here the stamp arrives naming a carrier, so it lands only
// if that carrier is the one already bound at the slot — and then only
// forward. The default is to keep what is there.
//
// The difference from epochAssignOnBind is deliberate and is the whole
// distinction between the two shapes. A binding write says "this carrier is
// here now, at this generation", so a stamp for a different carrier is the
// point of the write. A stamp-only write says "the carrier you are holding
// has moved on a generation", and if the slot is holding something else then
// the message is about a carrier that is not here — writing it would put one
// carrier's generation on another's counts.
//
// Takes the same three bind parameters, in the same epochArgs order.
const epochAssignForBoundBin = `active_bin_epoch=CASE
			WHEN active_bin_id IS ? AND active_bin_epoch < ? THEN ?
			ELSE active_bin_epoch END`

// epochArgs supplies either rule's three bind parameters: the bin the write
// is for, then the incoming stamp twice (once to compare, once to store).
func epochArgs(activeBinID any, deltaEpoch int64) []any {
	return []any{activeBinID, deltaEpoch, deltaEpoch}
}

// SetRuntimeWithBinAndEpoch updates active_claim_id, active_bin_id,
// active_bin_epoch, and remaining_uop_cached atomically. Same as
// SetRuntimeWithBin but also writes the epoch — used by ManualLoad
// (operator imprint) where the epoch comes from Core's LoadBin response
// rather than the OrderDelivered envelope. The epoch is subject to
// epochAssignOnBind; the other three columns are written unconditionally.
func SetRuntimeWithBinAndEpoch(db *sql.DB, processNodeID int64, activeClaimID, activeBinID *int64, deltaEpoch int64, remainingUOPCached int) error {
	args := []any{activeClaimID, activeBinID}
	args = append(args, epochArgs(activeBinID, deltaEpoch)...)
	args = append(args, remainingUOPCached, processNodeID)
	_, err := db.Exec(`UPDATE process_node_runtime_states SET
		active_claim_id=?, active_bin_id=?, `+epochAssignOnBind+`, remaining_uop_cached=?, updated_at=datetime('now')
		WHERE process_node_id=?`, args...)
	return err
}

// SetRuntimeForDeliveredBin is the atomic write used when a bin
// physically arrives at the slot: active_claim_id and active_bin_id
// become the delivered bin's id, active_bin_epoch becomes the bin's
// load-lifecycle epoch, and remaining_uop_cached becomes the bin's
// authoritative uop_remaining (carried on the OrderDelivered envelope).
// binID is not a pointer — this is the delivered-bin handler's atomic
// write and a delivery always names a carrier; callers gate on
// order.DeliveryNode == ctx.node.CoreNodeName before invoking.
//
// The body was a byte-identical copy of SetRuntimeWithBinAndEpoch's,
// differing only in that non-pointer parameter. Kept as its own name
// because the two call sites mean different things — a delivery versus
// an operator's imprint — and the name is the only place that shows.
func SetRuntimeForDeliveredBin(db *sql.DB, processNodeID int64, activeClaimID *int64, binID int64, deltaEpoch int64, remainingUOPCached int) error {
	return SetRuntimeWithBinAndEpoch(db, processNodeID, activeClaimID, &binID, deltaEpoch, remainingUOPCached)
}

// SetActiveBinID writes only the active_bin_id pointer on a runtime
// row, leaving the claim, UOP, and order pointers untouched. Used by
// the bin-pickup handler (clear when bin physically leaves) and any
// path that needs to update the bin pointer without touching the
// claim or count.
func SetActiveBinID(db *sql.DB, processNodeID int64, activeBinID *int64) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET
		active_bin_id=?, updated_at=datetime('now')
		WHERE process_node_id=?`,
		activeBinID, processNodeID)
	return err
}

// SetActiveBinIDAndEpoch writes active_bin_id and active_bin_epoch
// together without touching claim or count. Used by BindActiveBin
// (L1 retrieve confirm at a loader) where Core's LoadBin response
// provides the epoch but the count was already set by the delivery
// handler. The epoch is subject to epochAssignOnBind.
func SetActiveBinIDAndEpoch(db *sql.DB, processNodeID int64, activeBinID *int64, deltaEpoch int64) error {
	args := []any{activeBinID}
	args = append(args, epochArgs(activeBinID, deltaEpoch)...)
	args = append(args, processNodeID)
	_, err := db.Exec(`UPDATE process_node_runtime_states SET
		active_bin_id=?, `+epochAssignOnBind+`, updated_at=datetime('now')
		WHERE process_node_id=?`, args...)
	return err
}

// UpdateRuntimeOrders writes the active and staged order pointers on a
// runtime row.
func UpdateRuntimeOrders(db *sql.DB, processNodeID int64, activeOrderID, stagedOrderID *int64) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET active_order_id=?, staged_order_id=?, updated_at=datetime('now') WHERE process_node_id=?`,
		activeOrderID, stagedOrderID, processNodeID)
	return err
}

// ClearRuntimeOrderRefs nulls every runtime order pointer that references
// orderID, leaving the sibling slot and every other row untouched. Called
// when an order reaches a non-success terminal state, so a dead order can
// never keep a node's slot occupied.
//
// The two slots are cleared independently: a two-robot swap whose supply
// leg is skipped must keep pointing at its surviving evac leg, and vice
// versa. A blanket (nil, nil) write would drop the live sibling too.
//
// Keyed by order id rather than by node because one Core node fans out to
// several process_nodes rows and an order's own process_node_id may be
// nil — "wherever this order is referenced" is the only formulation that
// reliably drops every reference. Mirrors the boot-time cleanup in
// migrations.go (v25b), which is otherwise the only thing that clears
// these pointers.
func ClearRuntimeOrderRefs(db *sql.DB, orderID int64) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET
		active_order_id = CASE WHEN active_order_id=? THEN NULL ELSE active_order_id END,
		staged_order_id = CASE WHEN staged_order_id=? THEN NULL ELSE staged_order_id END,
		updated_at = datetime('now')
		WHERE active_order_id=? OR staged_order_id=?`,
		orderID, orderID, orderID, orderID)
	return err
}

// UpdateRuntimeUOP writes the cached UOP on a runtime row.
func UpdateRuntimeUOP(db *sql.DB, processNodeID int64, remainingUOPCached int) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET remaining_uop_cached=?, updated_at=datetime('now') WHERE process_node_id=?`,
		remainingUOPCached, processNodeID)
	return err
}

// AddPendingUOPDelta accumulates count change held while no bin is bound
// at the slot (hold-and-replay). Signed: consume holds positive magnitude
// to subtract on bind, produce holds positive to add. Atomic increment.
func AddPendingUOPDelta(db *sql.DB, processNodeID int64, delta int) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET pending_uop_delta = pending_uop_delta + ?, updated_at=datetime('now') WHERE process_node_id=?`,
		delta, processNodeID)
	return err
}

// SetRuntimeUOPClearPending writes the cached UOP and resets pending to 0
// in one statement — used when a tick finds a bound bin and has applied
// (current + pending) to the count, so the held delta is consumed exactly
// once.
func SetRuntimeUOPClearPending(db *sql.DB, processNodeID int64, remainingUOPCached int) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET remaining_uop_cached=?, pending_uop_delta=0, updated_at=datetime('now') WHERE process_node_id=?`,
		remainingUOPCached, processNodeID)
	return err
}

// activePullExecer is the minimal write-only interface satisfied by
// both *sql.DB and *sql.Tx. SetActivePull accepts it so the A/B flip
// path can wrap its two writes in a single transaction (Item 5);
// callers without a tx pass *sql.DB and get autocommit behavior.
type activePullExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// SetActivePull marks a node as the active pull point for A/B cycling.
// Only the active-pull node gets counter delta decrements.
func SetActivePull(db activePullExecer, processNodeID int64, active bool) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET active_pull=?, updated_at=datetime('now') WHERE process_node_id=?`,
		active, processNodeID)
	return err
}
