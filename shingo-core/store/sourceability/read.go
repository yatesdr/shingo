package sourceability

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"shingocore/store/plantclaims"
)

// BuildInputs assembles the plant snapshot Compute consumes: the mirrored styles
// and claims, the available-bin pool per payload, the current line UOP per node,
// and the per-payload consumption rate over rateWindow. Every query is a plain
// READ. rateWindow is the look-back for the consumption rate (only used to fill
// RatePerSec — the at-risk tier that ships dark).
func BuildInputs(db *sql.DB, rateWindow time.Duration) (Inputs, error) {
	styles, claims, err := loadStylesAndClaims(db)
	if err != nil {
		return Inputs{}, err
	}
	pool, err := availablePoolByPayload(db)
	if err != nil {
		return Inputs{}, err
	}
	lineUOP, err := lineUOPByNode(db)
	if err != nil {
		return Inputs{}, err
	}
	rate, err := consumptionRateByPayload(db, rateWindow)
	if err != nil {
		return Inputs{}, err
	}
	return Inputs{Styles: styles, Claims: claims, Pool: pool, LineUOP: lineUOP, RatePerSec: rate}, nil
}

// loadStylesAndClaims reads the whole plant.claims mirror: every configured
// (process, style) plus its sourceability claims. Styles with no claims still
// appear (they are trivially GREEN) so an all-styles recompute reports them.
func loadStylesAndClaims(db *sql.DB) ([]plantclaims.ProcessKey, map[plantclaims.ProcessKey][]plantclaims.ClaimRow, error) {
	styleRows, err := db.Query(`SELECT process_id, style_id FROM process_styles ORDER BY process_id, style_id`)
	if err != nil {
		return nil, nil, fmt.Errorf("sourceability: load styles: %w", err)
	}
	defer styleRows.Close()
	var styles []plantclaims.ProcessKey
	for styleRows.Next() {
		var k plantclaims.ProcessKey
		if err := styleRows.Scan(&k.ProcessID, &k.StyleID); err != nil {
			return nil, nil, fmt.Errorf("sourceability: scan style: %w", err)
		}
		styles = append(styles, k)
	}
	if err := styleRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("sourceability: styles rows: %w", err)
	}

	claimRows, err := db.Query(
		`SELECT process_id, style_id, core_node_name, payload_code, allowed_payload_codes, seq
		 FROM style_claims`)
	if err != nil {
		return nil, nil, fmt.Errorf("sourceability: load claims: %w", err)
	}
	defer claimRows.Close()
	claims := make(map[plantclaims.ProcessKey][]plantclaims.ClaimRow)
	for claimRows.Next() {
		var (
			c           plantclaims.ClaimRow
			allowedJSON string
		)
		if err := claimRows.Scan(&c.ProcessID, &c.StyleID, &c.CoreNodeName, &c.PayloadCode, &allowedJSON, &c.Seq); err != nil {
			return nil, nil, fmt.Errorf("sourceability: scan claim: %w", err)
		}
		if allowedJSON != "" {
			_ = json.Unmarshal([]byte(allowedJSON), &c.AllowedPayloadCodes)
		}
		k := plantclaims.ProcessKey{ProcessID: c.ProcessID, StyleID: c.StyleID}
		claims[k] = append(claims[k], c)
	}
	if err := claimRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("sourceability: claims rows: %w", err)
	}
	return styles, claims, nil
}

// availablePoolByPayload counts, per payload, the bins dispatch could source
// right now. The predicate is exactly FindSourceFIFO's (bin_manifest.go): a bin
// that is unclaimed, unlocked, manifest-confirmed, healthy-status, on a real
// enabled non-synthetic node, with no pending reservation. This is a pure count
// — it holds nothing.
func availablePoolByPayload(db *sql.DB) (map[string]int, error) {
	rows, err := db.Query(`
		SELECT b.payload_code, COUNT(*)
		FROM bins b
		JOIN nodes n ON n.id = b.node_id
		WHERE b.payload_code <> ''
		  AND n.enabled = true
		  AND n.is_synthetic = false
		  AND b.claimed_by IS NULL
		  AND b.locked = false
		  AND b.manifest_confirmed = true
		  AND b.status NOT IN ('staged', 'maintenance', 'flagged', 'retired', 'quality_hold')
		  AND NOT EXISTS (SELECT 1 FROM reservations r WHERE r.bin_id = b.id AND r.state = 'pending')
		GROUP BY b.payload_code`)
	if err != nil {
		return nil, fmt.Errorf("sourceability: available pool: %w", err)
	}
	defer rows.Close()
	pool := make(map[string]int)
	for rows.Next() {
		var (
			payload string
			n       int
		)
		if err := rows.Scan(&payload, &n); err != nil {
			return nil, fmt.Errorf("sourceability: scan pool: %w", err)
		}
		pool[payload] = n
	}
	return pool, rows.Err()
}

// lineUOPByNode returns the UOP currently present at each node (the numerator of
// a line's time-to-empty), keyed by node name. Bins with no content are
// excluded; a node with nothing staged is simply absent from the map.
func lineUOPByNode(db *sql.DB) (map[string]int, error) {
	rows, err := db.Query(`
		SELECT n.name, COALESCE(SUM(b.uop_remaining), 0)
		FROM bins b
		JOIN nodes n ON n.id = b.node_id
		WHERE b.uop_remaining > 0
		GROUP BY n.name`)
	if err != nil {
		return nil, fmt.Errorf("sourceability: line uop: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var (
			node string
			uop  int
		)
		if err := rows.Scan(&node, &uop); err != nil {
			return nil, fmt.Errorf("sourceability: scan line uop: %w", err)
		}
		out[node] = uop
	}
	return out, rows.Err()
}

// consumptionRateByPayload derives a per-payload consumption velocity (UOP/sec)
// from the bin_uop_delta audit history over window. Consumption is a negative
// delta — here (before_uop - after_uop) > 0, equivalent to summing the negated
// metadata.delta but reading the first-class columns the (op, applied_at) index
// already covers. rate = total consumed ÷ window seconds.
//
// This feeds only the at-risk (yellow) tier, which ships dark until the owner
// validates the window on real plant data.
func consumptionRateByPayload(db *sql.DB, window time.Duration) (map[string]float64, error) {
	secs := window.Seconds()
	if secs <= 0 {
		return map[string]float64{}, nil
	}
	rows, err := db.Query(`
		SELECT payload_code, COALESCE(SUM(before_uop - after_uop), 0)
		FROM bin_uop_audit
		WHERE op = 'bin_uop_delta'
		  AND after_uop < before_uop
		  AND payload_code <> ''
		  AND applied_at >= NOW() - make_interval(secs => $1)
		GROUP BY payload_code`, secs)
	if err != nil {
		return nil, fmt.Errorf("sourceability: consumption rate: %w", err)
	}
	defer rows.Close()
	rate := make(map[string]float64)
	for rows.Next() {
		var (
			payload  string
			consumed int64
		)
		if err := rows.Scan(&payload, &consumed); err != nil {
			return nil, fmt.Errorf("sourceability: scan rate: %w", err)
		}
		if consumed > 0 {
			rate[payload] = float64(consumed) / secs
		}
	}
	return rate, rows.Err()
}
