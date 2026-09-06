package store

// Stage 2D delegate file: payload_manifest CRUD lives in store/payloads/.

import "shingocore/store/payloads"

func (db *DB) CreatePayloadManifestItem(item *payloads.ManifestItem, catid string) error {
	return payloads.CreateItem(db.DB, item, catid)
}

func (db *DB) UpdatePayloadManifestItem(id int64, partNumber, catid string, partsPerCycle int64) error {
	return payloads.UpdateItem(db.DB, id, partNumber, catid, partsPerCycle)
}

func (db *DB) DeletePayloadManifestItem(id int64) error {
	return payloads.DeleteItem(db.DB, id)
}

func (db *DB) ListPayloadManifest(payloadID int64) ([]*payloads.ManifestItem, error) {
	return payloads.ListManifest(db.DB, payloadID)
}

func (db *DB) ReplacePayloadManifest(payloadID int64, lines []payloads.Line) error {
	return payloads.ReplaceManifest(db.DB, payloadID, lines)
}

// PayloadCATIDs returns payload id → the cat ids its manifest lines resolve to,
// comma-joined. See payloads.PayloadCATIDs for what "resolve to" means during
// the correction window.
func (db *DB) PayloadCATIDs() (map[int64]string, error) {
	return payloads.PayloadCATIDs(db.DB)
}

// --- parts ----------------------------------------------------------------

func (db *DB) ListParts() ([]*payloads.Part, error) { return payloads.ListParts(db.DB) }

func (db *DB) GetPartByNumber(partNumber string) (*payloads.Part, error) {
	return payloads.GetPartByNumber(db.DB, partNumber)
}

func (db *DB) SetPartCATID(partNumber, catid string) error {
	return payloads.SetPartCATID(db.DB, partNumber, catid)
}
