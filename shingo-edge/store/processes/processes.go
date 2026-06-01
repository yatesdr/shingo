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

func scanProcess(scanner interface{ Scan(...interface{}) error }) (Process, error) {
	var p Process
	var createdAt string
	if err := scanner.Scan(&p.ID, &p.Name, &p.Description, &p.ActiveStyleID, &p.TargetStyleID, &p.ProductionState, &p.CounterPLCName, &p.CounterTagName, &p.CounterEnabled, &p.AutoCutoverEnabled, &createdAt); err != nil {
		return p, err
	}
	p.CreatedAt = helpers.ScanTime(createdAt)
	return p, nil
}

const processSelect = `id, name, description, active_style_id, target_style_id, production_state, counter_plc_name, counter_tag_name, counter_enabled, auto_cutover_enabled, created_at`

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
func Create(db *sql.DB, name, description, productionState string, counterPLC, counterTag string, counterEnabled, autoCutoverEnabled bool) (int64, error) {
	if productionState == "" {
		productionState = "active_production"
	}
	res, err := db.Exec(`INSERT INTO processes (name, description, production_state, counter_plc_name, counter_tag_name, counter_enabled, auto_cutover_enabled) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, description, productionState, counterPLC, counterTag, counterEnabled, autoCutoverEnabled)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Update modifies a process row.
func Update(db *sql.DB, id int64, name, description, productionState string, counterPLC, counterTag string, counterEnabled, autoCutoverEnabled bool) error {
	if productionState == "" {
		productionState = "active_production"
	}
	_, err := db.Exec(`UPDATE processes SET name=?, description=?, production_state=?, counter_plc_name=?, counter_tag_name=?, counter_enabled=?, auto_cutover_enabled=? WHERE id=?`,
		name, description, productionState, counterPLC, counterTag, counterEnabled, autoCutoverEnabled, id)
	return err
}

// Delete removes a process row.
func Delete(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM processes WHERE id=?`, id)
	return err
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

// --- process nodes ---

const nodeSelect = `n.id, n.process_id, n.operator_station_id, n.core_node_name, n.code, n.name,
	n.sequence, n.enabled, n.created_at, n.updated_at, COALESCE(s.name, ''), COALESCE(p.name, '')`

const nodeJoin = `FROM process_nodes n
	LEFT JOIN operator_stations s ON s.id = n.operator_station_id
	LEFT JOIN processes p ON p.id = n.process_id`

func scanNode(scanner interface{ Scan(...interface{}) error }) (Node, error) {
	var n Node
	var createdAt, updatedAt string
	var stationID sql.NullInt64
	err := scanner.Scan(
		&n.ID, &n.ProcessID, &stationID, &n.CoreNodeName, &n.Code, &n.Name,
		&n.Sequence, &n.Enabled, &createdAt, &updatedAt, &n.StationName, &n.ProcessName,
	)
	if err != nil {
		return n, err
	}
	n.CreatedAt = helpers.ScanTime(createdAt)
	n.UpdatedAt = helpers.ScanTime(updatedAt)
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

// ListNodes returns every process_nodes row.
func ListNodes(db *sql.DB) ([]Node, error) {
	rows, err := db.Query(`SELECT ` + nodeSelect + ` ` + nodeJoin + ` ORDER BY n.process_id, n.sequence, n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ListNodesByProcess returns process_nodes rows for one process.
func ListNodesByProcess(db *sql.DB, processID int64) ([]Node, error) {
	rows, err := db.Query(`SELECT `+nodeSelect+` `+nodeJoin+` WHERE n.process_id=? ORDER BY n.sequence, n.name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ListNodesByStation returns process_nodes rows for one operator_station.
func ListNodesByStation(db *sql.DB, stationID int64) ([]Node, error) {
	rows, err := db.Query(`SELECT `+nodeSelect+` `+nodeJoin+` WHERE n.operator_station_id=? ORDER BY n.sequence, n.name`, stationID)
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

// DeleteNode removes a process_node row.
func DeleteNode(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM process_nodes WHERE id=?`, id)
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

func scanRuntime(scanner interface{ Scan(...interface{}) error }) (RuntimeState, error) {
	var r RuntimeState
	var updatedAt string
	err := scanner.Scan(&r.ID, &r.ProcessNodeID, &r.ActiveClaimID, &r.ActiveBinID, &r.ActiveBinEpoch, &r.CachedBinID, &r.RemainingUOPCached,
		&r.ActiveOrderID, &r.StagedOrderID, &r.ActivePull, &updatedAt)
	if err != nil {
		return r, err
	}
	r.UpdatedAt = helpers.ScanTime(updatedAt)
	return r, nil
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
	r, err := scanRuntime(db.QueryRow(`SELECT id, process_node_id, active_claim_id, active_bin_id, active_bin_epoch, cached_bin_id, remaining_uop_cached,
		active_order_id, staged_order_id, active_pull, updated_at
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

// SetRuntimeForDeliveredBin is the four-field write used when a bin
// physically arrives at the slot: active_claim_id, active_bin_id, and
// cached_bin_id all become the delivered bin's id, remaining_uop_cached
// becomes the bin's authoritative uop_remaining. After this write the
// PLC tick gate sees active_bin_id == cached_bin_id (steady state) and
// resumes cache decrements/increments. binID must not be nil — this is
// the delivered-bin handler's atomic write; callers gate on
// order.DeliveryNode == ctx.node.CoreNodeName before invoking.
func SetRuntimeForDeliveredBin(db *sql.DB, processNodeID int64, activeClaimID *int64, binID int64, remainingUOPCached int) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET
		active_claim_id=?, active_bin_id=?, cached_bin_id=?, remaining_uop_cached=?, updated_at=datetime('now')
		WHERE process_node_id=?`,
		activeClaimID, binID, binID, remainingUOPCached, processNodeID)
	return err
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

// SetCachedBin writes cached_bin_id and remaining_uop_cached together.
// Used at release-click (cachedBinID = supply leg's bin, value = bin's
// uop_remaining; or nil/0 when no incoming supply leg) and at delivery
// (re-affirms with the actually-arrived bin's id and uop). active_bin_id
// is updated separately — at release-click it stays pointing at the old
// bin (or nil after pickup); at delivery the caller writes both this
// row's fields plus active_bin_id via SetRuntimeWithBin.
func SetCachedBin(db *sql.DB, processNodeID int64, cachedBinID *int64, remainingUOPCached int) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET
		cached_bin_id=?, remaining_uop_cached=?, updated_at=datetime('now')
		WHERE process_node_id=?`,
		cachedBinID, remainingUOPCached, processNodeID)
	return err
}

// UpdateRuntimeOrders writes the active and staged order pointers on a
// runtime row.
func UpdateRuntimeOrders(db *sql.DB, processNodeID int64, activeOrderID, stagedOrderID *int64) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET active_order_id=?, staged_order_id=?, updated_at=datetime('now') WHERE process_node_id=?`,
		activeOrderID, stagedOrderID, processNodeID)
	return err
}

// UpdateRuntimeUOP writes the cached UOP on a runtime row.
func UpdateRuntimeUOP(db *sql.DB, processNodeID int64, remainingUOPCached int) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET remaining_uop_cached=?, updated_at=datetime('now') WHERE process_node_id=?`,
		remainingUOPCached, processNodeID)
	return err
}

// activePullExecer is the minimal write-only interface satisfied by
// both *sql.DB and *sql.Tx. SetActivePull accepts it so the A/B flip
// path can wrap its two writes in a single transaction (Item 5);
// callers without a tx pass *sql.DB and get autocommit behavior.
type activePullExecer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

// SetActivePull marks a node as the active pull point for A/B cycling.
// Only the active-pull node gets counter delta decrements.
func SetActivePull(db activePullExecer, processNodeID int64, active bool) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET active_pull=?, updated_at=datetime('now') WHERE process_node_id=?`,
		active, processNodeID)
	return err
}
