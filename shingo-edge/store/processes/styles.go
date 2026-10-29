// styles.go — recipe style persistence inside the processes aggregate.
//
// Phase 6.0c of the architecture refactor folded shingo-edge/store/styles/
// into store/processes/ because styles are part of the process domain
// cluster (process runs a style, style has claims on core nodes, changeover
// transitions between styles). Function names carry the Style suffix so
// they don't collide with the equivalent Process / Node / Claim /
// Changeover functions in their sibling files within this package.

package processes

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// Style represents a product/recipe style that maps to a BOM. The
// struct lives in shingoedge/domain (Stage 2A.2); this alias keeps
// the processes.Style name used by every scan helper, Create/Update
// call site, and the outer store/ re-export.
type Style = domain.Style

// styleSelectColumns is the shared column list for every style SELECT so the
// scan order in scanStyle stays in lockstep. expected_catid is COALESCEd
// because it is added by an idempotent ALTER (older rows land at ”).
const styleSelectColumns = `id, name, description, COALESCE(process_id, 0) as process_id, created_at, COALESCE(expected_catid, '') as expected_catid, deleted_at`

// liveStyles is the WHERE fragment for "styles an operator may still choose".
//
// It is deliberately NOT applied to every read. Of the sites in shingo-edge
// that name the styles table, few filter, and the split is the whole point
// of soft delete:
//
//   - PICKERS filter. ListStyles and ListStylesByProcess feed dropdowns
//     and the changeover target list. A retired
//     part number must not be selectable, and a tombstone must not resolve by
//     name or a re-created style becomes ambiguous with its own predecessor.
//     Same for the two joins that decide live behaviour rather than render
//     text: claims.IsPairedOnDeckNode and the sim readiness gate.
//
//   - DISPLAY JOINS DO NOT FILTER. The eight LEFT JOIN styles in changeovers.go
//     and the three in counters.go exist to resolve a name or a process_id for
//     a row that already happened. Today a deleted style makes them render a
//     blank name — soft delete is what FIXES that, and filtering would re-break
//     it. GetStyle(id) is in the same category: every caller already holds the
//     id and wants to know what it was.
//
// The rule is: filter where the answer is "what may I pick now", never where
// the answer is "what was this".
const liveStyles = ` deleted_at IS NULL`

func scanStyle(scanner interface{ Scan(...any) error }) (Style, error) {
	var s Style
	var createdAt string
	var deletedAt sql.NullString
	if err := scanner.Scan(&s.ID, &s.Name, &s.Description, &s.ProcessID, &createdAt, &s.ExpectedCATID, &deletedAt); err != nil {
		return s, err
	}
	s.CreatedAt = helpers.ScanTime(createdAt)
	s.DeletedAt = helpers.ScanTimePtr(deletedAt)
	return s, nil
}

func scanStyles(rows *sql.Rows) ([]Style, error) {
	var styles []Style
	for rows.Next() {
		s, err := scanStyle(rows)
		if err != nil {
			return nil, err
		}
		styles = append(styles, s)
	}
	return styles, rows.Err()
}

// ListStyles returns all LIVE styles ordered by name.
//
// PICKER (see liveStyles): this is the whole-plant style list behind
// StyleService().List, the admin styles page and every style dropdown. A
// retired part number must not be selectable.
func ListStyles(db *sql.DB) ([]Style, error) {
	rows, err := db.Query(`SELECT ` + styleSelectColumns + ` FROM styles WHERE` + liveStyles + ` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStyles(rows)
}

// ListStylesByProcess returns the LIVE styles for a single process_id.
//
// PICKER: this is view.AvailableStyles on the operator station — the
// changeover target list — and the claim set plant_claims_publisher pushes to
// Core. Both answer "what may this process run now".
func ListStylesByProcess(db *sql.DB, processID int64) ([]Style, error) {
	rows, err := db.Query(`SELECT `+styleSelectColumns+` FROM styles WHERE process_id = ? AND`+liveStyles+` ORDER BY name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStyles(rows)
}

// scanFunc adapts a function to the Scan interface scanStyle reads, so a query
// that selects the style columns plus more can still scan them in one place.
type scanFunc func(dest ...any) error

func (f scanFunc) Scan(dest ...any) error { return f(dest...) }

// positionSep separates the names ListStylesAndPositionsByProcess packs into
// one column (the ASCII unit separator, char(31), which no node name carries).
const positionSep = "\x1f"

// ListStylesAndPositionsByProcess is ListStylesByProcess plus the process's
// positions: the core node names of its live process_nodes that no live,
// non-manual_swap claim on a live style of the process names, as its own node
// or a paired one. Those are the seats the process binds at that its claims do
// not tell Core about — loader windows and homes and FG drains, which are
// process_nodes served by Loader.SynthClaim with no claim row.
//
// ONE statement, for the plant-claims publisher: the positions ride the styles
// read as one uncorrelated scalar column (evaluated once), because the
// publisher builds every process's report on every heartbeat's reconcile and a
// separate read would be one more statement per process per heartbeat on the
// Pi. A process with no live style returns no rows, and so no positions: its
// report carries no styles either, and Core drops its mirror.
func ListStylesAndPositionsByProcess(db *sql.DB, processID int64) ([]Style, []string, error) {
	rows, err := db.Query(`SELECT `+styleSelectColumns+`,
		COALESCE((SELECT group_concat(core_node_name, char(31)) FROM (
			SELECT DISTINCT n.core_node_name FROM process_nodes n
			 WHERE n.process_id = ?1 AND`+liveNodes+` AND n.core_node_name <> ''
			   AND NOT EXISTS (SELECT 1 FROM style_node_claims WHERE`+liveClaims+`
			          AND swap_mode <> 'manual_swap'
			          AND n.core_node_name IN (core_node_name, paired_core_node, second_paired_core_node)
			          AND style_id IN (SELECT id FROM styles WHERE process_id = ?1 AND`+liveStyles+`)))), '')
		FROM styles WHERE process_id = ?1 AND`+liveStyles+` ORDER BY name`, processID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var styles []Style
	var packed string
	withPositions := scanFunc(func(dest ...any) error { return rows.Scan(append(dest, &packed)...) })
	for rows.Next() {
		s, err := scanStyle(withPositions)
		if err != nil {
			return nil, nil, err
		}
		styles = append(styles, s)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var positions []string
	if packed != "" {
		positions = strings.Split(packed, positionSep)
		slices.Sort(positions)
	}
	return styles, positions, nil
}

// GetStyle looks up a single style by id, INCLUDING retired ones.
//
// Deliberately unfiltered. Every caller already holds the id — from a
// changeover row, a process's active_style_id, a claim, a CATID monitor — and
// is asking "what is this", not "what may I pick". Filtering here would turn a
// retired style into a nil and reintroduce the blank-name rendering that soft
// delete exists to fix. Callers that need liveness check DeletedAt.
func GetStyle(db DBTX, id int64) (*Style, error) {
	s, err := scanStyle(db.QueryRow(`SELECT `+styleSelectColumns+` FROM styles WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateStyle inserts a new style and returns the new row id.
func CreateStyle(db DBTX, name, description string, processID int64) (int64, error) {
	res, err := db.Exec(`INSERT INTO styles (name, description, process_id) VALUES (?, ?, ?)`, name, description, processID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateStyle modifies an existing style.
func UpdateStyle(db *sql.DB, id int64, name, description string, processID int64) error {
	// Guarded on liveness: editing a retired style should not silently succeed.
	_, err := db.Exec(`UPDATE styles SET name=?, description=?, process_id=? WHERE id=? AND`+liveStyles, name, description, processID, id)
	return err
}

// SetStyleExpectedCATID sets (or clears, when empty) the style's expected PLC
// part-identity value. Kept as its own setter rather than a parameter on
// Create/UpdateStyle so the dozens of existing Create/Update call sites stay
// untouched — expected_catid is an independent, optional field the style
// editor writes alongside a save. The value is trimmed by the caller.
func SetStyleExpectedCATID(db *sql.DB, id int64, expectedCATID string) error {
	_, err := db.Exec(`UPDATE styles SET expected_catid=? WHERE id=? AND`+liveStyles, expectedCATID, id)
	return err
}

// DeleteStyle RETIRES a style: it sets deleted_at rather than removing the row.
//
// A hard DELETE cascades into style_node_claims, reporting_points (and from
// there every counter_snapshot beneath them), hourly_counts, payloads,
// and every process_changeover that used this style as its
// TO style — which takes that changeover's node and station tasks with it.
// Measured on the Springfield edge, the worst single style is 91,581 rows. The
// operator who retires a superseded part number is not asking for that, and
// it does not come back.
//
// Retiring instead: the row stays, so nothing that points at it can dangle,
// changeover history keeps rendering a name, and the decision is reversible.
// StyleDeleteImpact reports what a style carries so the confirmation can say
// so.
//
// Its reporting points are DISABLED in the same transaction. That is the one
// piece of the old cascade worth keeping: a retired style must stop counting,
// and rp.enabled is the poll gate (counters.ListEnabledReportingPoints). The
// rows themselves survive, so their snapshots stay attributable.
func DeleteStyle(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE styles SET deleted_at = datetime('now') WHERE id=? AND`+liveStyles, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE reporting_points SET enabled = 0 WHERE style_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RestoreStyle un-retires a style. Soft delete is only worth having if the undo
// exists; without it "reversible" is a claim nobody can act on.
//
// It does NOT re-enable the reporting points: resuming counting is a separate,
// deliberate act, and silently restarting a PLC poll because somebody undid a
// delete is exactly the kind of surprise this whole change is avoiding.
func RestoreStyle(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE styles SET deleted_at = NULL WHERE id=? AND deleted_at IS NOT NULL`, id)
	return err
}

// cloneClaimColumns is the verbatim-copy column list for cloneStyleTx. It
// mirrors UpsertClaim's INSERT in claims.go: a claim column added there MUST be
// added here too, or clones silently drop it. Excludes id (autoincrement),
// style_id (set to the new style) and created_at (defaults to now). Kept as a
// single const so the SELECT and INSERT lists can't drift apart from each other.
//
// keep_staged_node IS copied: a clone or copy carries the kept spare and then meets
// the dedicated-spot check at the end of its transaction
// (CheckKeepStagedSpots), which allows the spot across the styles of one
// process and refuses it across processes.
//
// Attribution is the other deliberate exception: source, called_by and
// updated_at are NOT copied — the clone is its own write and stamps its own
// (see cloneStyleTx) — and retired_at is not copied because only live claims
// are cloned. source_preset_id / _version ARE copied: a clone of a
// preset-built flow is still that preset's shape.
//
// uop_capacity is NOT here, and is not a drop: the column is dead, resolved
// from the payload catalog on read rather than stored (capacity.SQL).
// A clone that copied it would copy a number nothing reads.
const cloneClaimColumns = `core_node_name, role, swap_mode, payload_code,
	reorder_point, reorder_point_source, auto_reorder, inbound_staging, outbound_staging,
	inbound_source, outbound_destination, containment_destination, partial_destination, curtain_required, strict_pack, pack_tolerance_pct, allowed_payload_codes, auto_request_payload,
	keep_staged_node, evacuate_on_changeover, paired_core_node, auto_confirm, sequence,
	lineside_soft_threshold, second_paired_core_node, reuse_compatible_bins, auto_push,
	changeover_evac_nodes, changeover_evac_destination,
	index_robot_supplies, key_route, key_task, changeover_carryover_disposition,
	source_preset_id, source_preset_version`

// notLoaderClaim is the WHERE fragment the style copy (cloneStyleTx) selects
// by, beside liveClaims: no stored loader claim. The INSERT never meets
// UpsertClaim, so the rows the write gate refuses are left behind rather than
// spread to another style:
//
//   - a manual_swap claim. The mode is retired as a persisted value: it is not
//     in protocol.ConfigurableSwapModes, so UpsertClaim refuses it, and the
//     first non-empty loader sync quarantines any stored row
//     (QuarantineLoaderClaims). A row exists only until that sync, and copied
//     before it it would be a second authority for loader configuration Core
//     owns, on a style the quarantine has never seen.
//   - a RETIRED claim. `liveClaims` is the flow composer's soft delete: a
//     claim a changeover's history still points at is kept rather than
//     dropped, and every live read skips it. Copying one would revive a
//     deleted claim.
//
// Pinned by TestCloneStyle_LeavesWithheldConfigurationBehind (store).
const notLoaderClaim = ` swap_mode != '` + string(protocol.SwapModeManualSwap) + `'`

// cloneStyleTx inserts a new style in src's process and copies every one of
// src's LIVE style_node_claims verbatim, less what the write gate refuses
// (below), within the caller's transaction, stamping the copies with the given
// source and caller. Returns the new style id. Used by CloneStyle.
//
// A copied claim that names a lane on any leg refuses the clone, naming the
// source style, the claim's node and the leg (refuseStoredLaneLegs). The
// source is fixed first; nothing is cleared on the way through.
func cloneStyleTx(tx *sql.Tx, kinds domain.CoreNodeKinds, src *Style, name, description, source, calledBy string) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO styles (name, description, process_id) VALUES (?, ?, ?)`,
		name, description, src.ProcessID)
	if err != nil {
		return 0, err
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// The copy is verbatim less what notLoaderClaim leaves behind (a stored
	// loader claim, a retired claim). The dedicated-spot check is the caller's,
	// once its transaction has written everything (CloneStyle).
	//
	// The copies are ATTRIBUTED to this write rather than carrying src's: a
	// clone is its own act, by whoever asked for it.
	_, err = tx.Exec(`INSERT INTO style_node_claims (style_id, source, called_by, updated_at, `+cloneClaimColumns+`)
		SELECT ?, ?, ?, datetime('now'), `+cloneClaimColumns+` FROM style_node_claims
		WHERE style_id = ? AND`+notLoaderClaim+` AND`+liveClaims,
		newID, source, calledBy, src.ID)
	if err != nil {
		return 0, err
	}
	if err := refuseStoredLaneLegs(tx, kinds, newID, fmt.Sprintf("style %q", src.Name)); err != nil {
		return 0, err
	}
	return newID, nil
}

// CloneStyle creates a new style in the same process as src, copying src's
// style_node_claims verbatim, less what the write gate refuses (see
// cloneStyleTx). Returns the new style id. The new style
// starts inactive — cloning is a config-time scaffold, not a changeover
// trigger. Operators use this to add a style whose robot choreography matches
// an existing one, then edit only the per-payload fields on the result. The
// copies are attributed source='cloned', called_by=calledBy.
func CloneStyle(db *sql.DB, kinds domain.CoreNodeKinds, srcID int64, name, description, calledBy string) (int64, error) {
	src, err := GetStyle(db, srcID)
	if err != nil {
		return 0, err
	}
	if src == nil {
		return 0, fmt.Errorf("source style %d not found", srcID)
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	newID, err := cloneStyleTx(tx, kinds, src, name, description, domain.ClaimSourceCloned, calledBy)
	if err != nil {
		return 0, err
	}
	if _, err := CheckKeepStagedSpots(tx, nil); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newID, nil
}

// copiedClaim is the slice of a freshly copied claim row the clone's lane
// refusal reads (readCopiedClaims, refuseCopiedLaneLegs). The copy-to override
// layer that also rewrote it went in pick-a-part U6.
type copiedClaim struct {
	node                               string
	swapMode                           protocol.SwapMode
	payload                            string
	allowed                            []string
	inboundSource, outboundDestination string
	inboundStaging, outboundStaging    string
	evacDestination, containment       string
	partial                            string
	paired, second                     string
	keepStagedNode                     string
}

// legs is the copied row's seven legs as a claim input, for the lane refusal.
func (c *copiedClaim) legs() NodeClaimInput {
	return NodeClaimInput{
		InboundSource: c.inboundSource, OutboundDestination: c.outboundDestination,
		ChangeoverEvacDestination: &c.evacDestination, ContainmentDestination: c.containment,
		PartialDestination: c.partial,
		InboundStaging:     c.inboundStaging, OutboundStaging: c.outboundStaging,
		KeepStagedNode: &c.keepStagedNode,
	}
}

// refuseStoredLaneLegs reads one style's live claims as this transaction has
// written them and refuses the first leg that names a lane. One SELECT, and
// none when no lane is known (empty kinds refuse nothing).
func refuseStoredLaneLegs(tx *sql.Tx, kinds domain.CoreNodeKinds, styleID int64, from string) error {
	if len(kinds.Lanes) == 0 {
		return nil
	}
	rows, err := readCopiedClaims(tx, styleID)
	if err != nil {
		return fmt.Errorf("read copied claims for the lane check: %w", err)
	}
	return refuseCopiedLaneLegs(kinds, rows, from)
}

// refuseCopiedLaneLegs refuses the first copied claim, by node name, with a
// leg naming a lane: "<from>, claim at <node>: <leg>: <sentence>".
func refuseCopiedLaneLegs(kinds domain.CoreNodeKinds, rows map[string]*copiedClaim, from string) error {
	nodes := make([]string, 0, len(rows))
	for node := range rows {
		nodes = append(nodes, node)
	}
	slices.Sort(nodes)
	for _, node := range nodes {
		if err := kinds.RefuseLaneLegs(rows[node].legs()); err != nil {
			return fmt.Errorf("%s, claim at %s: %w", from, node, err)
		}
	}
	return nil
}

// readCopiedClaims loads the just-inserted copies for the override layer.
// allowed_payload_codes tolerates the column's legacy empty default: an
// unparseable or empty value reads as no allowed list, which only ever
// widens what the payload-append pass below writes back.
func readCopiedClaims(tx *sql.Tx, targetID int64) (map[string]*copiedClaim, error) {
	gr, err := tx.Query(`SELECT core_node_name, swap_mode, payload_code, allowed_payload_codes,
		inbound_source, outbound_destination, inbound_staging, outbound_staging,
		changeover_evac_destination, containment_destination, partial_destination,
		paired_core_node, second_paired_core_node, keep_staged_node FROM style_node_claims WHERE style_id = ? AND`+liveClaims, targetID)
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	out := map[string]*copiedClaim{}
	for gr.Next() {
		var c copiedClaim
		var allowed string
		if err := gr.Scan(&c.node, &c.swapMode, &c.payload, &allowed, &c.inboundSource,
			&c.outboundDestination, &c.inboundStaging, &c.outboundStaging,
			&c.evacDestination, &c.containment, &c.partial, &c.paired, &c.second, &c.keepStagedNode); err != nil {
			return nil, err
		}
		var allowedList []string
		_ = json.Unmarshal([]byte(allowed), &allowedList)
		c.allowed = allowedList
		out[c.node] = &c
	}
	return out, gr.Err()
}

// StyleImpact is what a style is carrying, counted so a confirmation dialog can
// say it out loud.
//
// Every field is a row count that a HARD delete of this style would destroy
// under the schema's declared foreign-key actions. DeleteStyle does not do a
// hard delete — it retires the row — so these are not losses. They are the
// answer to "what am I retiring", and they are also exactly what a future purge
// would cost, which is the number nobody had before.
//
// The shape of this matters more than the total. On the Springfield edge the
// median style carries one row (itself) and the worst carries 91,581, of which
// 91,256 are raw counter snapshots. A dialog that says "delete this style?" is
// asking the same question in both cases and they are not the same question.
type StyleImpact struct {
	StyleID int64  `json:"style_id"`
	Name    string `json:"name"`
	Retired bool   `json:"retired"`

	Claims          int `json:"claims"`
	ReportingPoints int `json:"reporting_points"`
	Snapshots       int `json:"counter_snapshots"`
	HourlyCounts    int `json:"hourly_counts"`
	PartsCounted    int `json:"parts_counted"`
	ChangeoversTo   int `json:"changeovers_to"`
	NodeTasks       int `json:"changeover_node_tasks"`
	StationTasks    int `json:"changeover_station_tasks"`
	Participants    int `json:"changeover_participants"`
	Payloads        int `json:"payloads"`

	// ChangeoversFrom is SET NULL, not CASCADE: those changeover rows survive a
	// hard delete and merely lose the pointer that says what they came from.
	// Reported separately because "60 rows lose a field" and "60 rows cease to
	// exist" should never share a total.
	ChangeoversFrom int `json:"changeovers_from_detached"`

	// TotalDeleted is the sum of the CASCADE side plus the style row itself.
	TotalDeleted int `json:"total_deleted"`
}

// StyleDeleteImpact counts everything a hard delete of styleID would take.
//
// It walks the cascade explicitly rather than trusting the declared actions to
// be what anybody expects, because on this schema they are not: styles ->
// reporting_points -> counter_snapshots is two CASCADE hops, and the second one
// is where the volume lives. Two hops is exactly far enough that nobody reading
// the styles table would guess the number.
func StyleDeleteImpact(db *sql.DB, styleID int64) (*StyleImpact, error) {
	imp := &StyleImpact{StyleID: styleID}
	var deletedAt sql.NullString
	err := db.QueryRow(`SELECT name, deleted_at FROM styles WHERE id = ?`, styleID).Scan(&imp.Name, &deletedAt)
	if err != nil {
		return nil, err
	}
	imp.Retired = deletedAt.Valid

	for _, q := range []struct {
		dst *int
		sql string
	}{
		{&imp.Claims, `SELECT count(*) FROM style_node_claims WHERE style_id = ? AND retired_at IS NULL`},
		{&imp.ReportingPoints, `SELECT count(*) FROM reporting_points WHERE style_id = ?`},
		{&imp.Snapshots, `SELECT count(*) FROM counter_snapshots
			WHERE reporting_point_id IN (SELECT id FROM reporting_points WHERE style_id = ?)`},
		{&imp.HourlyCounts, `SELECT count(*) FROM hourly_counts WHERE style_id = ?`},
		{&imp.PartsCounted, `SELECT COALESCE(SUM(delta), 0) FROM hourly_counts WHERE style_id = ?`},
		{&imp.ChangeoversTo, `SELECT count(*) FROM process_changeovers WHERE to_style_id = ?`},
		{&imp.NodeTasks, `SELECT count(*) FROM changeover_node_tasks
			WHERE process_changeover_id IN (SELECT id FROM process_changeovers WHERE to_style_id = ?)`},
		{&imp.StationTasks, `SELECT count(*) FROM changeover_station_tasks
			WHERE process_changeover_id IN (SELECT id FROM process_changeovers WHERE to_style_id = ?)`},
		{&imp.Participants, `SELECT count(*) FROM changeover_participants
			WHERE process_changeover_id IN (SELECT id FROM process_changeovers WHERE to_style_id = ?)`},
		{&imp.Payloads, `SELECT count(*) FROM payloads WHERE job_style_id = ?`},
		{&imp.ChangeoversFrom, `SELECT count(*) FROM process_changeovers WHERE from_style_id = ?`},
	} {
		if err := db.QueryRow(q.sql, styleID).Scan(q.dst); err != nil {
			// A table absent on this vintage counts as zero rather than failing
			// the whole confirmation: refusing to show a dialog because a
			// table does not exist yet would be worse than showing one
			// number short.
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			return nil, fmt.Errorf("style %d impact: %w", styleID, err)
		}
	}
	imp.TotalDeleted = 1 + imp.Claims + imp.ReportingPoints + imp.Snapshots + imp.HourlyCounts +
		imp.ChangeoversTo + imp.NodeTasks + imp.StationTasks + imp.Participants +
		imp.Payloads
	return imp, nil
}
