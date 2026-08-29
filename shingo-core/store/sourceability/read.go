package sourceability

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"shingocore/store/plantclaims"
	"shingocore/store/reservations"
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
	onLine, err := onLinePoolByProcess(db)
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
	return Inputs{Styles: styles, Claims: claims, Pool: pool, OnLine: onLine, LineUOP: lineUOP, RatePerSec: rate}, nil
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
		  AND NOT ` + reservations.BinSpokenForSQL + `
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

// onLinePoolByProcess counts bins that are ALREADY INSIDE a process and carrying
// what it needs, but which dispatch cannot go and fetch — today that means
// status='staged'. Keyed process → payload → count.
//
// WHY THIS EXISTS. availablePoolByPayload answers "could a robot bring one?", and
// a bin standing at the consuming node answers something better: it is already
// there. Excluding it lumped "the parts are at the line" together with "the parts
// do not exist", and the page said "no available bin in Shingo" about a bin
// sitting at ALN_007 holding 30 parts of the payload the claim named (SPR,
// 2026-07-29, style 63181-6SA0B.95 — CARRIER-0010 staged there for ~50h).
//
// The codebase already treated that bin as present: lineUOPByNode below applies
// NO status filter, so its UOP feeds the at-risk projection. One bin was counted
// present for time-to-empty and absent for the verdict. This closes that.
//
// SCOPE IS THE PROCESS, not the node (owner's rule, 2026-07-29) — and it matches
// the demand grain's own axiom that a cell is one process spanning several nodes.
// The process's node set comes from style_claims.core_node_name, hence the join;
// COUNT(DISTINCT b.id) because a node appears once per (style, claim) there and
// the join would otherwise multiply one bin by its claim count.
//
// ONLY 'staged' gets the exemption. maintenance, flagged, quality_hold and
// retired are not usable parts no matter where they sit, so they stay excluded.
// claimed / locked / reserved stay excluded too: a bin already spoken for is on
// its way out, not stock in place. manifest_confirmed stays required — an
// unconfirmed bin's contents are not trusted anywhere else either.
//
// Disjoint from Pool by construction: availablePoolByPayload excludes 'staged',
// this counts only 'staged', so nothing is counted twice.
func onLinePoolByProcess(db *sql.DB) (map[string]map[string]int, error) {
	rows, err := db.Query(`
		SELECT sc.process_id, b.payload_code, COUNT(DISTINCT b.id)
		FROM bins b
		JOIN nodes n ON n.id = b.node_id
		JOIN style_claims sc ON sc.core_node_name = n.name
		WHERE b.payload_code <> ''
		  AND n.enabled = true
		  AND n.is_synthetic = false
		  AND b.claimed_by IS NULL
		  AND b.locked = false
		  AND b.manifest_confirmed = true
		  AND b.status = 'staged'
		  AND NOT ` + reservations.BinSpokenForSQL + `
		GROUP BY sc.process_id, b.payload_code`)
	if err != nil {
		return nil, fmt.Errorf("sourceability: on-line pool: %w", err)
	}
	defer rows.Close()
	out := make(map[string]map[string]int)
	for rows.Next() {
		var (
			process string
			payload string
			n       int
		)
		if err := rows.Scan(&process, &payload, &n); err != nil {
			return nil, fmt.Errorf("sourceability: scan on-line pool: %w", err)
		}
		if out[process] == nil {
			out[process] = make(map[string]int)
		}
		out[process][payload] = n
	}
	return out, rows.Err()
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
		FROM bin_uop_ledger
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

// ActiveStyles returns the style each process is currently running, keyed by
// process ID, from the plant-claims mirror. A process with no active style is
// absent from the map — Core must not guess at one.
//
// This is the running-style signal the sourcing page lacked entirely: Edge has
// always known (processes.active_style_id, the field it resolves node claims
// through) but it did not cross the wire until the plant.claims feed gained an
// active flag.
func ActiveStyles(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(
		`SELECT process_id, style_id FROM process_styles WHERE is_active`)
	if err != nil {
		return nil, fmt.Errorf("sourceability: load active styles: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var processID, styleID string
		if err := rows.Scan(&processID, &styleID); err != nil {
			return nil, fmt.Errorf("sourceability: scan active style: %w", err)
		}
		out[processID] = styleID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sourceability: active styles: %w", err)
	}
	return out, nil
}
