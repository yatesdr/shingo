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
	"fmt"
	"strings"

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
// It is deliberately NOT applied to every read. Of the 23 sites in shingo-edge
// that name the styles table, only six filter, and the split is the whole point
// of soft delete:
//
//   - PICKERS filter. ListStyles, ListStylesByProcess and GetStyleByName feed
//     dropdowns, the changeover target list and name-based ingress. A retired
//     part number must not be selectable, and a tombstone must not resolve by
//     name or a re-created style becomes ambiguous with its own predecessor.
//     Same for the three joins that decide live behaviour rather than render
//     text: claims.IsPairedOnDeckNode, walk.PayloadsForManualSwapNodes and the
//     sim readiness gate.
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

// GetStyleByName looks up a single LIVE style by name.
//
// PICKER-side: name is an ingress key, and a retired style must not resolve
// through it. Two rows can legitimately share a name once one is retired (the
// live-only unique index permits exactly that), so an unfiltered name lookup
// would be ambiguous rather than merely generous.
func GetStyleByName(db *sql.DB, name string) (*Style, error) {
	s, err := scanStyle(db.QueryRow(`SELECT `+styleSelectColumns+` FROM styles WHERE name = ? AND`+liveStyles, name))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// GetStyle looks up a single style by id, INCLUDING retired ones.
//
// Deliberately unfiltered. Every caller already holds the id — from a
// changeover row, a process's active_style_id, a claim, a CATID monitor — and
// is asking "what is this", not "what may I pick". Filtering here would turn a
// retired style into a nil and reintroduce the blank-name rendering that soft
// delete exists to fix. Callers that need liveness check DeletedAt.
func GetStyle(db *sql.DB, id int64) (*Style, error) {
	s, err := scanStyle(db.QueryRow(`SELECT `+styleSelectColumns+` FROM styles WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateStyle inserts a new style and returns the new row id.
func CreateStyle(db *sql.DB, name, description string, processID int64) (int64, error) {
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
// node_lineside_bucket and every process_changeover that used this style as its
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
// mirrors UpsertClaim's INSERT in claims.go exactly: a claim column added
// there MUST be added here too, or clones silently drop it. Excludes id
// (autoincrement), style_id (set to the new style), and created_at (defaults
// to now). Kept as a single const so the SELECT and INSERT lists can't drift
// apart from each other.
const cloneClaimColumns = `core_node_name, role, swap_mode, payload_code,
	uop_capacity, reorder_point, reorder_point_source, auto_reorder, inbound_staging, outbound_staging,
	inbound_source, outbound_destination, allowed_payload_codes, auto_request_payload,
	keep_staged, evacuate_on_changeover, paired_core_node, auto_confirm, sequence,
	lineside_soft_threshold, second_paired_core_node, reuse_compatible_bins, auto_push,
	changeover_evac_seats, changeover_evac_destination`

// cloneStyleTx inserts a new style in src's process and copies every one of
// src's style_node_claims verbatim, within the caller's transaction. Returns
// the new style id. Used by both CloneStyle (single) and GenerateStyles
// (batch) so the copy logic lives in exactly one place.
func cloneStyleTx(tx *sql.Tx, src *Style, name, description string) (int64, error) {
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
	// swap_mode is copied verbatim. This trusts that live claims already hold a
	// real (configurable) mode: the upsert allowlist has rejected "simple" since
	// the ingress lockdown, and the pre-merge diagnostic confirmed zero simple
	// rows — so there is nothing stale to re-validate on the copy.
	_, err = tx.Exec(`INSERT INTO style_node_claims (style_id, `+cloneClaimColumns+`)
		SELECT ?, `+cloneClaimColumns+` FROM style_node_claims WHERE style_id = ?`,
		newID, src.ID)
	if err != nil {
		return 0, err
	}
	return newID, nil
}

// CloneStyle creates a new style in the same process as src, copying all of
// src's style_node_claims verbatim. Returns the new style id. The new style
// starts inactive — cloning is a config-time scaffold, not a changeover
// trigger. Operators use this to add a style whose robot choreography matches
// an existing one, then edit only the per-payload fields on the result.
func CloneStyle(db *sql.DB, srcID int64, name, description string) (int64, error) {
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
	newID, err := cloneStyleTx(tx, src, name, description)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newID, nil
}

// GenerateStyles scaffolds a family of styles from one base style in a single
// transaction: each variant is a clone of base with its per-claim payload
// overrides applied (matched by core_node_name). The batch is atomic — a
// duplicate style name or any other error rolls back every variant, so the
// operator never ends up with a half-generated family. Returns the new style
// ids in variant order.
//
// Only payload-shaped fields are overridden (payload_code, uop_capacity,
// allowed_payload_codes); the cloned choreography is left untouched, so the
// override can never violate a swap-mode invariant the base already satisfied.
// An override whose core_node_name matches no cloned claim updates zero rows
// and is silently skipped — generation is for setting payloads on the base's
// existing claims, not for adding new nodes.
func GenerateStyles(db *sql.DB, baseID int64, variants []domain.StyleVariant) ([]int64, error) {
	base, err := GetStyle(db, baseID)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return nil, fmt.Errorf("base style %d not found", baseID)
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	ids := make([]int64, 0, len(variants))
	for _, v := range variants {
		name := strings.TrimSpace(v.Name)
		if name == "" {
			return nil, fmt.Errorf("variant name is required")
		}
		newID, err := cloneStyleTx(tx, base, name, strings.TrimSpace(v.Description))
		if err != nil {
			return nil, fmt.Errorf("clone variant %q: %w", name, err)
		}
		for _, o := range v.Overrides {
			coreNode := strings.TrimSpace(o.CoreNodeName)
			if coreNode == "" {
				continue
			}
			allowedJSON := marshalAllowedPayloads(o.AllowedPayloadCodes)
			if _, err := tx.Exec(`UPDATE style_node_claims
				SET payload_code=?, uop_capacity=?, allowed_payload_codes=?
				WHERE style_id=? AND core_node_name=?`,
				o.PayloadCode, o.UOPCapacity, allowedJSON, newID, coreNode); err != nil {
				return nil, fmt.Errorf("override %s on variant %q: %w", coreNode, name, err)
			}
		}
		ids = append(ids, newID)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
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
	LinesideBuckets int `json:"lineside_buckets"`

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
		{&imp.Claims, `SELECT count(*) FROM style_node_claims WHERE style_id = ?`},
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
		{&imp.LinesideBuckets, `SELECT count(*) FROM node_lineside_bucket WHERE style_id = ?`},
		{&imp.ChangeoversFrom, `SELECT count(*) FROM process_changeovers WHERE from_style_id = ?`},
	} {
		if err := db.QueryRow(q.sql, styleID).Scan(q.dst); err != nil {
			// A table absent on this vintage counts as zero rather than failing
			// the whole confirmation: refusing to show a dialog because
			// node_lineside_bucket does not exist yet would be worse than
			// showing one number short.
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			return nil, fmt.Errorf("style %d impact: %w", styleID, err)
		}
	}
	imp.TotalDeleted = 1 + imp.Claims + imp.ReportingPoints + imp.Snapshots + imp.HourlyCounts +
		imp.ChangeoversTo + imp.NodeTasks + imp.StationTasks + imp.Participants +
		imp.Payloads + imp.LinesideBuckets
	return imp, nil
}
