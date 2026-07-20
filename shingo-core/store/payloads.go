package store

// Stage 2D delegate file: payload CRUD lives in store/payloads/. The
// cross-aggregate ListBinTypesForPayload stays here because it bridges the
// payloads and bins aggregates.

import (
	"shingocore/store/bins"
	"shingocore/store/payloads"
)

func (db *DB) CreatePayload(p *payloads.Payload) error        { return payloads.Create(db.DB, p) }
func (db *DB) UpdatePayload(p *payloads.Payload) error        { return payloads.Update(db.DB, p) }
func (db *DB) DeletePayload(id int64) error                   { return payloads.Delete(db.DB, id) }
func (db *DB) GetPayload(id int64) (*payloads.Payload, error) { return payloads.Get(db.DB, id) }
func (db *DB) GetPayloadByCode(code string) (*payloads.Payload, error) {
	return payloads.GetByCode(db.DB, code)
}
func (db *DB) ListPayloads() ([]*payloads.Payload, error) { return payloads.List(db.DB) }

// GetLoadSequence returns the load_sequences registry entry for name (the
// ordered binTask-name list a payload's advanced_load_sequence selects), or
// (nil, nil) when no such sequence is registered.
func (db *DB) GetLoadSequence(name string) (*payloads.LoadSequence, error) {
	return payloads.GetLoadSequence(db.DB, name)
}

// ListLoadSequenceNames returns every registered load-sequence name for the
// payload-editor dropdown.
func (db *DB) ListLoadSequenceNames() ([]string, error) {
	return payloads.ListLoadSequenceNames(db.DB)
}

// ListBinTypesForPayload returns all bin types associated with a payload
// template. Cross-aggregate (payloads ↔ bins): the function returns *bins.BinType
// so it's owned by bins/, but the entry point lives in the payloads delegate.
func (db *DB) ListBinTypesForPayload(payloadID int64) ([]*bins.BinType, error) {
	return bins.ListTypesForPayload(db.DB, payloadID)
}

// SetPayloadBinTypes replaces all bin type associations for a payload template.
func (db *DB) SetPayloadBinTypes(payloadID int64, binTypeIDs []int64) error {
	return payloads.SetBinTypes(db.DB, payloadID, binTypeIDs)
}

// ListPayloadBinTypeMappings returns every (payload_code, bin_type_code) pair
// from payload_bin_types, ordered by payload then type. One call replaces the
// N+1 per-node GetEffectiveBinTypes queries on the NodeListResponse path.
func (db *DB) ListPayloadBinTypeMappings() ([][2]string, error) {
	rows, err := db.DB.Query(`
		SELECT p.code, bt.code
		FROM payload_bin_types pbt
		JOIN payloads p  ON p.id  = pbt.payload_id
		JOIN bin_types bt ON bt.id = pbt.bin_type_id
		ORDER BY p.code, bt.code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			return nil, err
		}
		out = append(out, pair)
	}
	return out, rows.Err()
}
