package bins

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"shingocore/domain"
)

// ManifestEntry is the bin-manifest line-item domain type — one CatID
// at a given quantity. Lives in shingocore/domain (Stage 2A); aliased
// here so existing references to bins.ManifestEntry compile unchanged.
type ManifestEntry = domain.ManifestEntry

// Manifest is the parsed form of a bin's manifest JSON field. Lives
// in shingocore/domain (Stage 2A) along with Bin.ParseManifest; outer
// store/ re-exports this as store.BinManifest for backward compat.
type Manifest = domain.Manifest

// SetManifest populates a bin's contents from a payload template.
func SetManifest(db *sql.DB, binID int64, manifestJSON string, payloadCode string, uopRemaining int) error {
	_, err := db.Exec(`UPDATE bins SET payload_code=$1, manifest=$2, uop_remaining=$3, manifest_confirmed=false, updated_at=NOW() WHERE id=$4`,
		payloadCode, manifestJSON, uopRemaining, binID)
	return err
}

// loadedAtLayouts are the timestamp layouts resolveLoadedAt accepts, tried
// in order. Edge's wire contract is RFC3339 UTC (produce_plan.go builds
// now.UTC().Format(time.RFC3339)), so that's first. The rest cover zoneless
// inputs that legacy data, admin/manual paths, and tests still produce —
// Postgres-style "2006-01-02 15:04:05" and the T-separated zoneless form,
// each with optional fractional seconds. Go's time.Parse reads a zoneless
// layout as UTC, which is exactly the intent: R20-1 was a zoneless value
// being re-localized to the session TimeZone and skewing FIFO ordering.
// Parsing it as UTC here — with the session TZ also pinned to UTC in
// store.go — closes that gap from both directions instead of rejecting the
// value and poisoning loaded_at with server time.
var loadedAtLayouts = []string{
	time.RFC3339Nano,                      // 2006-01-02T15:04:05[.frac]Z07:00 (subsumes RFC3339)
	"2006-01-02T15:04:05.999999999",       // T-separated, zoneless -> UTC
	"2006-01-02 15:04:05.999999999-07:00", // space-separated, with offset
	"2006-01-02 15:04:05.999999999",       // space-separated, zoneless -> UTC
}

// resolveLoadedAt converts Edge's producedAt into the instant to store in
// loaded_at, always as a definite UTC time.Time. Empty is the normal "no
// explicit timestamp" case and falls back to now. A non-empty value is
// matched against loadedAtLayouts; zoneless layouts resolve as UTC so a
// missing offset is never re-localized to the session TimeZone. Only a value
// matching no layout falls back to now, returning an error so the caller can
// surface the broken input rather than silently poisoning loaded_at.
// Returning a time.Time (not a string) lets the driver bind a definite
// instant into TIMESTAMPTZ rather than a zoneless literal the session would
// re-localize and skew FIFO ordering in FindSourceFIFO.
func resolveLoadedAt(producedAt string, now time.Time) (time.Time, error) {
	if producedAt == "" {
		return now, nil
	}
	for _, layout := range loadedAtLayouts {
		if t, err := time.Parse(layout, producedAt); err == nil {
			return t.UTC(), nil
		}
	}
	return now, fmt.Errorf("unparseable producedAt %q (want RFC3339 or 2006-01-02 15:04:05)", producedAt)
}

// ConfirmManifest marks a bin's manifest as confirmed by an operator.
// producedAt is the Edge-side timestamp (RFC3339) of when the operator
// finalized the bin; empty falls back to server time.
func ConfirmManifest(db *sql.DB, binID int64, producedAt string) error {
	loadedAt, err := resolveLoadedAt(producedAt, time.Now().UTC())
	if err != nil {
		log.Printf("bins: ConfirmManifest bin %d: %v; using server time", binID, err)
	}
	_, err = db.Exec(`UPDATE bins SET manifest_confirmed=true, loaded_at=$1, updated_at=NOW() WHERE id=$2`,
		loadedAt, binID)
	return err
}

// ConfirmManifestTx is ConfirmManifest inside a caller's transaction. Returns
// the resolved loaded_at plus the bin's current uop_remaining and payload_code
// so the caller can write a same-tx bin_uop_audit row for the confirm event
// (§16 PR 3 — confirm was previously a silent mutation). Reuses resolveLoadedAt
// so producedAt parsing matches ConfirmManifest.
func ConfirmManifestTx(tx *sql.Tx, binID int64, producedAt string) (loadedAt time.Time, uop int, payloadCode string, err error) {
	var perr error
	loadedAt, perr = resolveLoadedAt(producedAt, time.Now().UTC())
	if perr != nil {
		log.Printf("bins: ConfirmManifestTx bin %d: %v; using server time", binID, perr)
	}
	if err = tx.QueryRow(`UPDATE bins SET manifest_confirmed=true, loaded_at=$1, updated_at=NOW()
		WHERE id=$2 RETURNING uop_remaining, payload_code`, loadedAt, binID).Scan(&uop, &payloadCode); err != nil {
		err = fmt.Errorf("confirm manifest bin %d: %w", binID, err)
	}
	return loadedAt, uop, payloadCode, err
}

// ClearManifest empties a bin's manifest (bin is now empty).
func ClearManifest(db *sql.DB, binID int64) error {
	_, err := db.Exec(`UPDATE bins SET payload_code='', manifest=NULL, uop_remaining=0, manifest_confirmed=false, loaded_at=NULL, updated_at=NOW() WHERE id=$1`,
		binID)
	return err
}

// GetManifest fetches a bin and parses its manifest. The Bin type
// owns the JSON-decoding logic (domain.Bin.ParseManifest); this
// helper just stitches a DB read to that pure-data step.
func GetManifest(db *sql.DB, binID int64) (*Manifest, error) {
	bin, err := Get(db, binID)
	if err != nil {
		return nil, err
	}
	return bin.ParseManifest()
}

// FindSourceFIFO finds the best unclaimed bin at an enabled storage node
// matching the given payload code, using FIFO ordering.
// FindSourceFIFO looks for the FIFO-oldest manifest-confirmed bin matching
// payloadCode at an enabled storage node. excludeNodeID > 0 skips bins at
// that node. Pass the order's destination node so a same-node retrieve is
// impossible. See SHINGO_TODO.md "Same-node retrieve" entry.
//
// Compatibility enforcement (post-2026-04-27 v2 fix): advisory.
// Uses PayloadBinTypeAdvisoryClause to keep this reader coherent with
// FindEmptyCompatible — when payload_bin_types has rules for the payload,
// only matching bin types are returned; when no rules exist, any bin
// matching payload_code is returned. Pre-fix this function ignored the
// table entirely, producing an asymmetry where the empty-bin retrieve
// rejected types the full-bin retrieve happily returned. The plant
// starvation symptom was the empty-bin side; aligning here prevents the
// inverse footgun (full bin loaded into a type the rules say is forbidden,
// then sourceable as that incompatible type forever).
func FindSourceFIFO(db *sql.DB, payloadCode string, excludeNodeID int64) (*Bin, error) {
	// Empty payloadCode is always a bug here. After the bin-as-truth
	// refactor, unattached bins store payload_code = "" instead of
	// NULL, so `WHERE b.payload_code = $1` with $1 = "" silently
	// matches every unattached bin. Reject at the boundary; mirror the
	// no-match sentinel returned by ScanBin so callers don't need new
	// error handling.
	if payloadCode == "" {
		return nil, sql.ErrNoRows
	}
	row := db.QueryRow(fmt.Sprintf(`%s
		WHERE b.payload_code = $1
		  AND n.enabled = true
		  AND n.is_synthetic = false
		  AND b.claimed_by IS NULL
		  AND b.locked = false
		  AND b.manifest_confirmed = true
		  AND b.status NOT IN ('staged', 'maintenance', 'flagged', 'retired', 'quality_hold')
		  AND ($2 = 0 OR b.node_id != $2)%s
		ORDER BY COALESCE(b.loaded_at, b.created_at) ASC
		LIMIT 1`, BinJoinQuery, PayloadBinTypeAdvisoryClause), payloadCode, excludeNodeID)
	return ScanBin(row)
}
