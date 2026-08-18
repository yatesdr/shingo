package store

// Phase 5 delegate file: cms_transactions CRUD lives in store/cms/.
// SumCatIDsAtBoundary stays here because it crosses into bin manifests
// (cross-aggregate read coordinator).

import (
	"encoding/json"

	"shingocore/store/bins"
	"shingocore/store/cms"
	"shingocore/store/internal/nodetree"
)

func (db *DB) CreateCMSTransactions(txns []*cms.Transaction) error {
	return cms.Create(db.DB, txns)
}

func (db *DB) ListCMSTransactions(nodeID int64, limit, offset int) ([]*cms.Transaction, error) {
	return cms.ListByNode(db.DB, nodeID, limit, offset)
}

func (db *DB) ListAllCMSTransactions(limit, offset int) ([]*cms.Transaction, error) {
	return cms.ListAll(db.DB, limit, offset)
}

// SumCatIDsAtBoundary returns total manifest quantities for all CATIDs
// across all bins at nodes under the given boundary, parsing from bin
// manifest JSON. Cross-aggregate (bins): kept at outer store/ level.
//
// SubtreeOf, NOT DescendantsOf — the boundary node's OWN bins count. This walk
// and the group-scoped empty finders' were both spelled "WITH RECURSIVE
// descendants" and are not the same question: those exclude the group node (it is
// synthetic and holds no carriers), this includes the boundary node (it can hold
// bins, and a total that skipped them would be wrong by exactly its contents).
// The two now have different names for that reason.
func (db *DB) SumCatIDsAtBoundary(boundaryID int64) map[string]int64 {
	totals := make(map[string]int64)
	rows, err := db.Query(nodetree.SubtreeOf(1)+`
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
		var m bins.Manifest
		if json.Unmarshal([]byte(manifestJSON), &m) != nil {
			continue
		}
		for _, item := range m.Items {
			totals[item.CatID] += item.Quantity
		}
	}
	return totals
}
