package store

// Phase 5 delegate file: cms_transactions CRUD lives in store/cms/.
// SumCatIDsAtBoundary stays here because it crosses into bin manifests
// (cross-aggregate read coordinator).

import (
	"encoding/json"

	"shingocore/store/cms"
)

// CMSTransaction preserves the store.CMSTransaction public API.
type CMSTransaction = cms.Transaction

func (db *DB) CreateCMSTransactions(txns []*CMSTransaction) error {
	return cms.Create(db.DB, txns)
}

func (db *DB) ListCMSTransactions(nodeID int64, limit, offset int) ([]*CMSTransaction, error) {
	return cms.ListByNode(db.DB, nodeID, limit, offset)
}

func (db *DB) ListAllCMSTransactions(limit, offset int) ([]*CMSTransaction, error) {
	return cms.ListAll(db.DB, limit, offset)
}

// SumCatIDsAtBoundary returns total manifest quantities for all CATIDs
// across all bins at nodes under the given boundary, parsing from bin
// manifest JSON. Cross-aggregate (bins): kept at outer store/ level.
func (db *DB) SumCatIDsAtBoundary(boundaryID int64) map[string]int64 {
	totals := make(map[string]int64)
	rows, err := db.Query(`
		WITH RECURSIVE descendants AS (
			SELECT id FROM nodes WHERE id = $1
			UNION ALL
			SELECT n.id FROM nodes n
			JOIN descendants d ON n.parent_id = d.id
		)
		SELECT b.manifest FROM bins b
		JOIN descendants d ON b.node_id = d.id
		WHERE b.manifest IS NOT NULL
	`, boundaryID)
	if err != nil {
		return totals
	}
	defer rows.Close()

	for rows.Next() {
		var manifestJSON string
		if rows.Scan(&manifestJSON) != nil {
			continue
		}
		var m BinManifest
		if json.Unmarshal([]byte(manifestJSON), &m) != nil {
			continue
		}
		for _, item := range m.Items {
			totals[item.CatID] += item.Quantity
		}
	}
	return totals
}
