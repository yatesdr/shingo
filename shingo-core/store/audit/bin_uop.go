package audit

import (
	"database/sql"
	"fmt"
)

// Standard op tags for bin_uop_audit.op. Stable strings — historical
// rows will reference these, so renames must come with a migration.
//
// Phase 0a of the UOP bin-as-truth refactor. Phase 1+ adds the delta-
// driven shadow ops; until then these cover every BinManifestService
// mutation point.
const (
	OpSetForProduction        = "set_for_production"
	OpClearForReuse           = "clear_for_reuse"
	OpClearAndClaim           = "clear_and_claim"
	OpSyncUOPAndClaim         = "sync_uop_and_claim"
	OpSyncUOP                 = "sync_uop"
	OpReleasedEmpty           = "released_empty"
	OpReleasedPartial         = "released_partial"
	OpReleasedEmptyFallback   = "released_empty_fallback"
	OpReleasedPartialFallback = "released_partial_fallback"

	// OpReleasedCaptureEmpty tags the manifest-clear that fires
	// inside ApplyBinUOPDelta when a capture_reduction delta drives
	// uop_remaining to zero — the PULL PARTS LINESIDE path. Distinct
	// from OpReleasedEmpty (the explicit RELEASE EMPTY button) so
	// audit consumers can tell apart "operator confirmed empty
	// outright" from "operator pulled parts and the bin was empty
	// after the math."
	OpReleasedCaptureEmpty = "released_capture_empty"

	// OpCycleCount tags an operator-driven cycle count from the Bins
	// admin page. before_uop / suggested_uop carries the system's
	// expected count (what bins.uop_remaining read just before the
	// operator's number); after_uop carries the value the operator
	// submitted. Discrepancy = operator override of the system count;
	// agreement = system was right and the count just confirms it.
	OpCycleCount = "cycle_count"

	// OpReleasedUnderpack tags a release where the operator declared
	// the bin physically empty before the tracked count reached zero
	// (bin labeled 1200 actually held 1190; cell starves at runtime=10).
	// Wire shape mirrors RELEASE EMPTY (RemainingUOP = &0; manifest
	// cleared) but the audit op is distinct so forensics can trend
	// missing-inventory patterns separately from the
	// system-and-operator-agreed-empty case. before_uop carries the
	// system's expected count at click time; after_uop = 0; the gap
	// (before_uop - after_uop) is the missing-inventory delta.
	OpReleasedUnderpack = "released_underpack"

	// Phase 0b — operator override observations at release time. These
	// are not bin writes; they record that the operator submitted a
	// value different from what the system would have suggested at
	// modal-open. before_uop holds the system-suggested value (the
	// runtime / manifest snapshot) and after_uop holds the operator's
	// submitted value. The metadata column carries the disposition kind
	// for cross-row context. One row per overridden part for pull_parts
	// (payload_code = part_number); one row total for release_partial
	// (payload_code = bin's payload code).
	OpOperatorOverridePullParts      = "operator_override_pull_parts"
	OpOperatorOverrideReleasePartial = "operator_override_release_partial"
)

// BinUOPExecer is the minimal interface satisfied by *sql.Tx and *sql.DB.
// AppendBinUOP takes it so the audit insert participates in the caller's
// transaction when one exists, falling back to the connection pool when
// the caller has no tx (degraded log path — atomicity lost but the row
// still lands).
type BinUOPExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// AppendBinUOP records a single write to bins.uop_remaining. Called from
// inside the same transaction as the bin update; the caller is
// responsible for ordering (read-old → update → audit → commit) so the
// before/after values match what actually committed.
//
// beforeUOP is *int because a set_for_production on a freshly-created
// bin row has no prior count — passing nil records that fact instead of
// reporting a misleading 0.
//
// orderID is *int64 for paths that don't have an associated order
// (cycle count, operator clear, manual load).
//
// payloadCode and actor are passed verbatim; empty string is acceptable
// when not applicable. Both columns default to ” at the schema level.
func AppendBinUOP(execer BinUOPExecer, binID int64, beforeUOP *int, afterUOP int, op, source string, orderID *int64, payloadCode, actor string) error {
	var before any
	if beforeUOP != nil {
		before = *beforeUOP
	}
	var ord any
	if orderID != nil {
		ord = *orderID
	}
	if _, err := execer.Exec(`INSERT INTO bin_uop_audit
		(bin_id, before_uop, after_uop, op, source, order_id, payload_code, actor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		binID, before, afterUOP, op, source, ord, payloadCode, actor); err != nil {
		return fmt.Errorf("append bin_uop_audit bin=%d op=%q: %w", binID, op, err)
	}
	return nil
}

// AppendBinUOPOverride records an operator-override observation at
// release time. Unlike AppendBinUOP, this is not paired with a bin
// write — the row exists to make the divergence between the system-
// suggested value and the operator-submitted value visible to
// forensics. before_uop holds the suggested value, after_uop the
// submitted one. metadata is a JSON-encoded blob carrying disposition
// kind and any extra context the caller wants preserved.
//
// Phase 0b. Per the SME contract (plan §2.5): the operator has full
// authority to override; the audit row exists so management / SCO can
// review aggregate override patterns (mislabelled bins, upstream
// overfill, miscount drift) without re-running the operator's keypad
// session.
// BinUOPRow is one row read from bin_uop_audit. Returned by the
// list helpers (Item 10) so the audit UI can render timelines without
// re-querying for every row's columns. Nullable columns surface as
// pointers; metadata stays a raw JSON string (the UI parses it).
type BinUOPRow struct {
	ID          int64   `json:"id"`
	BinID       int64   `json:"bin_id"`
	BeforeUOP   *int    `json:"before_uop,omitempty"`
	AfterUOP    int     `json:"after_uop"`
	Op          string  `json:"op"`
	Source      string  `json:"source"`
	OrderID     *int64  `json:"order_id,omitempty"`
	PayloadCode string  `json:"payload_code"`
	Actor       string  `json:"actor"`
	Metadata    *string `json:"metadata,omitempty"`
	AppliedAt   string  `json:"applied_at"`
}

func scanBinUOPRows(rows *sql.Rows) ([]BinUOPRow, error) {
	defer rows.Close()
	var out []BinUOPRow
	for rows.Next() {
		var r BinUOPRow
		var before sql.NullInt64
		var orderID sql.NullInt64
		var meta sql.NullString
		if err := rows.Scan(&r.ID, &r.BinID, &before, &r.AfterUOP, &r.Op, &r.Source,
			&orderID, &r.PayloadCode, &r.Actor, &meta, &r.AppliedAt); err != nil {
			return nil, err
		}
		if before.Valid {
			v := int(before.Int64)
			r.BeforeUOP = &v
		}
		if orderID.Valid {
			v := orderID.Int64
			r.OrderID = &v
		}
		if meta.Valid {
			s := meta.String
			r.Metadata = &s
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const binUOPSelectCols = `id, bin_id, before_uop, after_uop, op, source, order_id, payload_code, actor, metadata, applied_at`

// ListBinUOPByBin returns the audit timeline for one bin, newest
// first. Item 10's per-bin endpoint pages on this. limit clamps the
// caller-supplied page size to a sane upper bound; offset is the
// caller's chosen pagination cursor.
func ListBinUOPByBin(db *sql.DB, binID int64, limit, offset int) ([]BinUOPRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := db.Query(`SELECT `+binUOPSelectCols+`
		FROM bin_uop_audit
		WHERE bin_id = $1
		ORDER BY applied_at DESC, id DESC
		LIMIT $2 OFFSET $3`, binID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list bin_uop_audit by bin %d: %w", binID, err)
	}
	return scanBinUOPRows(rows)
}

// ListBinUOPByOperator returns recent activity by one actor (operator
// or system), newest first. Filter is exact match on the actor
// column; callers that want fuzzy match can do that client-side.
func ListBinUOPByOperator(db *sql.DB, actor string, limit, offset int) ([]BinUOPRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := db.Query(`SELECT `+binUOPSelectCols+`
		FROM bin_uop_audit
		WHERE actor = $1
		ORDER BY applied_at DESC, id DESC
		LIMIT $2 OFFSET $3`, actor, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list bin_uop_audit by actor %q: %w", actor, err)
	}
	return scanBinUOPRows(rows)
}

// ListBinUOPOverridesByStation returns recent operator-override rows
// for a station, newest first. Filters to OpOperatorOverridePullParts
// and OpOperatorOverrideReleasePartial — the audit-relevant override
// observations — so the per-station UI surfaces the divergence
// patterns SCO and management actually want to review.
func ListBinUOPOverridesByStation(db *sql.DB, station string, limit, offset int) ([]BinUOPRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := db.Query(`SELECT `+binUOPSelectCols+`
		FROM bin_uop_audit
		WHERE actor = $1
		AND op IN ($2, $3)
		ORDER BY applied_at DESC, id DESC
		LIMIT $4 OFFSET $5`,
		station, OpOperatorOverridePullParts, OpOperatorOverrideReleasePartial,
		limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list bin_uop_audit overrides by station %q: %w", station, err)
	}
	return scanBinUOPRows(rows)
}

func AppendBinUOPOverride(execer BinUOPExecer, binID int64, suggestedUOP, operatorUOP int, op, source string, orderID *int64, payloadCode, actor string, metadata []byte) error {
	var ord any
	if orderID != nil {
		ord = *orderID
	}
	var meta any
	if len(metadata) > 0 {
		meta = metadata
	}
	if _, err := execer.Exec(`INSERT INTO bin_uop_audit
		(bin_id, before_uop, after_uop, op, source, order_id, payload_code, actor, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		binID, suggestedUOP, operatorUOP, op, source, ord, payloadCode, actor, meta); err != nil {
		return fmt.Errorf("append bin_uop_audit override bin=%d op=%q: %w", binID, op, err)
	}
	return nil
}
