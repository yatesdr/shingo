package bins

import (
	"database/sql"
	"fmt"
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

// ConfirmManifest marks a bin's manifest as confirmed by an operator.
// producedAt is the Edge-side timestamp (RFC3339) of when the operator finalized the bin.
// If empty, falls back to the current server time.
func ConfirmManifest(db *sql.DB, binID int64, producedAt string) error {
	ts := producedAt
	if ts == "" {
		ts = time.Now().UTC().Format("2006-01-02 15:04:05")
	}
	_, err := db.Exec(`UPDATE bins SET manifest_confirmed=true, loaded_at=$1, updated_at=NOW() WHERE id=$2`,
		ts, binID)
	return err
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
