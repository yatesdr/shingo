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
func FindSourceFIFO(db *sql.DB, payloadCode string) (*Bin, error) {
	row := db.QueryRow(fmt.Sprintf(`%s
		WHERE b.payload_code = $1
		  AND n.enabled = true
		  AND n.is_synthetic = false
		  AND b.claimed_by IS NULL
		  AND b.locked = false
		  AND b.manifest_confirmed = true
		  AND b.status NOT IN ('staged', 'maintenance', 'flagged', 'retired', 'quality_hold')
		ORDER BY COALESCE(b.loaded_at, b.created_at) ASC
		LIMIT 1`, BinJoinQuery), payloadCode)
	return ScanBin(row)
}
