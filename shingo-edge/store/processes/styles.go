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

func scanStyle(scanner interface{ Scan(...interface{}) error }) (Style, error) {
	var s Style
	var createdAt string
	if err := scanner.Scan(&s.ID, &s.Name, &s.Description, &s.ProcessID, &createdAt); err != nil {
		return s, err
	}
	s.CreatedAt = helpers.ScanTime(createdAt)
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

// ListStyles returns all styles ordered by name.
func ListStyles(db *sql.DB) ([]Style, error) {
	rows, err := db.Query(`SELECT id, name, description, COALESCE(process_id, 0) as process_id, created_at FROM styles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStyles(rows)
}

// ListStylesByProcess returns styles for a single process_id.
func ListStylesByProcess(db *sql.DB, processID int64) ([]Style, error) {
	rows, err := db.Query(`SELECT id, name, description, COALESCE(process_id, 0) as process_id, created_at FROM styles WHERE process_id = ? ORDER BY name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStyles(rows)
}

// GetStyleByName looks up a single style by name.
func GetStyleByName(db *sql.DB, name string) (*Style, error) {
	s, err := scanStyle(db.QueryRow(`SELECT id, name, description, COALESCE(process_id, 0) as process_id, created_at FROM styles WHERE name = ?`, name))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// GetStyle looks up a single style by id.
func GetStyle(db *sql.DB, id int64) (*Style, error) {
	s, err := scanStyle(db.QueryRow(`SELECT id, name, description, COALESCE(process_id, 0) as process_id, created_at FROM styles WHERE id = ?`, id))
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
	_, err := db.Exec(`UPDATE styles SET name=?, description=?, process_id=? WHERE id=?`, name, description, processID, id)
	return err
}

// DeleteStyle removes a style row by id.
func DeleteStyle(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM styles WHERE id=?`, id)
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
	lineside_soft_threshold, second_paired_core_node, reuse_compatible_bins, auto_push`

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
