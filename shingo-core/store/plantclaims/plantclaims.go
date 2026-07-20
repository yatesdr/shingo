// Package plantclaims holds the Core mirror of Edge's plant-spec claim set
// (the plant.claims feed, Edge → Core). process_styles + style_claims are a
// pure mirror: the handler replaces a process's rows on every message, so
// the tables reflect Edge's plant spec at a point in time. The dirty index
// the sourceability recompute consumes (payload_code → set of (process,
// style)) is derived here from style_claims.
//
// Loaders/unloaders (manual_swap claims) never reach this mirror — they are
// excluded at the publisher and enter the computation as pool supply/demand
// via the loader aggregate, not as style claims.
package plantclaims

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"shingo/protocol"
)

// StyleRow is one (process, style) Edge reports in a plant-claims message.
type StyleRow struct {
	ProcessID string
	StyleID   string
	ConfigGen int64
	// IsActive marks the style the process is currently running, mirrored from
	// Edge's processes.active_style_id. At most one style per process carries
	// it; false for every style when Edge has no active style set, or when the
	// report came from an Edge too old to publish the flag.
	IsActive bool
}

// ClaimRow is one sourceability-relevant node claim under a (process, style).
// AllowedPayloadCodes is the effective payload set (mirrors
// PlantClaim.AllowedPayloadCodes on the wire).
type ClaimRow struct {
	ProcessID           string
	StyleID             string
	CoreNodeName        string
	Role                protocol.ClaimRole
	SwapMode            protocol.SwapMode
	PayloadCode         string
	AllowedPayloadCodes []string
	UOPCapacity         int
	ReorderPoint        int
	Seq                 int
}

// ReplaceProcess replaces the mirror for one process in a single transaction.
// Every message is authoritative for its process, so this DELETEs the process's
// existing rows (process_styles + style_claims) then re-inserts the reported
// set. staleGuardConfigGen, when > 0, aborts the replace if the mirror already
// holds a NEWER config_gen for this process — so an out-of-order (older)
// snapshot landing after a newer one is dropped, not applied.
//
// styles with no sourceability-relevant claims still get a process_styles row
// (so an all-styles recompute knows the style exists) and an empty claim set.
func ReplaceProcess(db *sql.DB, processID string, styles []StyleRow, claims []ClaimRow, staleGuardConfigGen int64) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("plantclaims replace %s: begin: %w", processID, err)
	}
	defer tx.Rollback()

	if staleGuardConfigGen > 0 {
		var have sql.NullInt64
		if err := tx.QueryRow(
			`SELECT MAX(config_gen) FROM process_styles WHERE process_id = $1`, processID,
		).Scan(&have); err != nil {
			return fmt.Errorf("plantclaims replace %s: stale-guard read: %w", processID, err)
		}
		if have.Valid && have.Int64 > staleGuardConfigGen {
			// A newer snapshot already landed for this process. Drop this one.
			return nil
		}
	}

	if _, err := tx.Exec(`DELETE FROM process_styles WHERE process_id = $1`, processID); err != nil {
		return fmt.Errorf("plantclaims replace %s: delete styles: %w", processID, err)
	}
	if _, err := tx.Exec(`DELETE FROM style_claims WHERE process_id = $1`, processID); err != nil {
		return fmt.Errorf("plantclaims replace %s: delete claims: %w", processID, err)
	}

	for _, s := range styles {
		if _, err := tx.Exec(
			`INSERT INTO process_styles (process_id, style_id, config_gen, is_active)
			 VALUES ($1, $2, $3, $4)`,
			s.ProcessID, s.StyleID, s.ConfigGen, s.IsActive,
		); err != nil {
			return fmt.Errorf("plantclaims replace %s: insert style %s: %w", processID, s.StyleID, err)
		}
	}

	styleInsert, err := tx.Prepare(
		`INSERT INTO style_claims
		 (process_id, style_id, core_node_name, role, swap_mode, payload_code,
		  allowed_payload_codes, uop_capacity, reorder_point, seq)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`)
	if err != nil {
		return fmt.Errorf("plantclaims replace %s: prepare claim insert: %w", processID, err)
	}
	defer styleInsert.Close()

	for _, c := range claims {
		allowed, err := json.Marshal(c.AllowedPayloadCodes)
		if err != nil {
			return fmt.Errorf("plantclaims replace %s: marshal allowed for %s: %w", processID, c.CoreNodeName, err)
		}
		if c.AllowedPayloadCodes == nil {
			allowed = []byte("[]")
		}
		if _, err := styleInsert.Exec(
			c.ProcessID, c.StyleID, c.CoreNodeName, string(c.Role), string(c.SwapMode),
			c.PayloadCode, string(allowed), c.UOPCapacity, c.ReorderPoint, c.Seq,
		); err != nil {
			return fmt.Errorf("plantclaims replace %s: insert claim %s: %w", processID, c.CoreNodeName, err)
		}
	}

	return tx.Commit()
}

// WipeAll deletes every mirror row. Used by the full-snapshot rebuild path
// (a late-joining Core applies a full snapshot by wiping then re-inserting
// every process) and by the mirror-rebuild test. Safe on an empty mirror.
func WipeAll(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("plantclaims wipe: begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM style_claims`); err != nil {
		return fmt.Errorf("plantclaims wipe: claims: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM process_styles`); err != nil {
		return fmt.Errorf("plantclaims wipe: styles: %w", err)
	}
	return tx.Commit()
}

// ProcessKey is the (process, style) pair — the sourceability computation key.
type ProcessKey struct {
	ProcessID string
	StyleID   string
}

// DirtyIndex is payload_code → the set of (process, style) pairs whose claims
// require that payload. Built from style_claims: a claim contributes its
// primary PayloadCode plus every code in AllowedPayloadCodes. The recompute
// marks every entry under DirtyIndex[changedPayload] dirty.
//
// Returns a map keyed by payload; each value is a deduplicated []ProcessKey
// (order is not stable — callers treat it as a set).
func DirtyIndex(db *sql.DB) (map[string][]ProcessKey, error) {
	rows, err := db.Query(
		`SELECT process_id, style_id, payload_code, allowed_payload_codes
		 FROM style_claims`)
	if err != nil {
		return nil, fmt.Errorf("plantclaims dirty index: query: %w", err)
	}
	defer rows.Close()

	idx := make(map[string]map[ProcessKey]struct{})
	add := func(payload, process, style string) {
		if payload == "" {
			return
		}
		set, ok := idx[payload]
		if !ok {
			set = make(map[ProcessKey]struct{})
			idx[payload] = set
		}
		set[ProcessKey{process, style}] = struct{}{}
	}

	for rows.Next() {
		var process, style, payload, allowedJSON string
		if err := rows.Scan(&process, &style, &payload, &allowedJSON); err != nil {
			return nil, fmt.Errorf("plantclaims dirty index: scan: %w", err)
		}
		add(payload, process, style)
		var allowed []string
		if err := json.Unmarshal([]byte(allowedJSON), &allowed); err == nil {
			for _, p := range allowed {
				add(p, process, style)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plantclaims dirty index: rows: %w", err)
	}

	out := make(map[string][]ProcessKey, len(idx))
	for payload, set := range idx {
		keys := make([]ProcessKey, 0, len(set))
		for k := range set {
			keys = append(keys, k)
		}
		out[payload] = keys
	}
	return out, nil
}
