package store

// Stage 2D delegate file: payload_manifest CRUD lives in store/payloads/.

import "shingocore/store/payloads"

// PayloadManifestItem aliases the payloads sub-package's manifest row type.
type PayloadManifestItem = payloads.ManifestItem

func (db *DB) CreatePayloadManifestItem(item *PayloadManifestItem) error {
	return payloads.CreateItem(db.DB, item)
}

func (db *DB) UpdatePayloadManifestItem(id int64, partNumber string, quantity int64) error {
	return payloads.UpdateItem(db.DB, id, partNumber, quantity)
}

func (db *DB) DeletePayloadManifestItem(id int64) error {
	return payloads.DeleteItem(db.DB, id)
}

func (db *DB) ListPayloadManifest(payloadID int64) ([]*PayloadManifestItem, error) {
	return payloads.ListManifest(db.DB, payloadID)
}

func (db *DB) ReplacePayloadManifest(payloadID int64, items []*PayloadManifestItem) error {
	return payloads.ReplaceManifest(db.DB, payloadID, items)
}
