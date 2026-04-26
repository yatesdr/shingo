package store

// Stage 2D delegate file: payload CRUD lives in store/payloads/. The
// cross-aggregate ListBinTypesForPayload stays here because it bridges the
// payloads and bins aggregates.

import (
	"shingocore/store/bins"
	"shingocore/store/payloads"
)

func (db *DB) CreatePayload(p *payloads.Payload) error             { return payloads.Create(db.DB, p) }
func (db *DB) UpdatePayload(p *payloads.Payload) error             { return payloads.Update(db.DB, p) }
func (db *DB) DeletePayload(id int64) error               { return payloads.Delete(db.DB, id) }
func (db *DB) GetPayload(id int64) (*payloads.Payload, error)      { return payloads.Get(db.DB, id) }
func (db *DB) GetPayloadByCode(code string) (*payloads.Payload, error) {
	return payloads.GetByCode(db.DB, code)
}
func (db *DB) ListPayloads() ([]*payloads.Payload, error) { return payloads.List(db.DB) }

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
