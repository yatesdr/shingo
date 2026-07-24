package store

// Stage 2D delegate file: payload_manifest CRUD lives in store/payloads/.

import "shingocore/store/payloads"

func (db *DB) CreatePayloadManifestItem(item *payloads.ManifestItem) error {
	return payloads.CreateItem(db.DB, item)
}

func (db *DB) UpdatePayloadManifestItem(id int64, partNumber string, quantity int64) error {
	return payloads.UpdateItem(db.DB, id, partNumber, quantity)
}

func (db *DB) DeletePayloadManifestItem(id int64) error {
	return payloads.DeleteItem(db.DB, id)
}

func (db *DB) ListPayloadManifest(payloadID int64) ([]*payloads.ManifestItem, error) {
	return payloads.ListManifest(db.DB, payloadID)
}

func (db *DB) ReplacePayloadManifest(payloadID int64, items []*payloads.ManifestItem) error {
	return payloads.ReplaceManifest(db.DB, payloadID, items)
}

// PayloadCATIDs returns payload id → its single distinct manifest part number
// (the payload's part identity), for the unambiguous payloads only.
func (db *DB) PayloadCATIDs() (map[int64]string, error) {
	return payloads.PayloadCATIDs(db.DB)
}
