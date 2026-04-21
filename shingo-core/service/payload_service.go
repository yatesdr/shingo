package service

import (
	"shingocore/store"
)

// PayloadService centralizes payload-template CRUD, manifest-item
// mutations, bin-type associations, and node-compatibility lookups.
// Handlers call PayloadService instead of reaching through engine
// passthroughs to *store.DB.
//
// Absorbed from engine_db_methods.go as part of the Phase 3a closeout
// (PR 3a.6). Methods are thin delegates today.
type PayloadService struct {
	db *store.DB
}

func NewPayloadService(db *store.DB) *PayloadService {
	return &PayloadService{db: db}
}

// --- Payload CRUD ---------------------------------------------------------

// Create inserts a new payload template.
func (s *PayloadService) Create(p *store.Payload) error {
	return s.db.CreatePayload(p)
}

// Get loads a payload template by ID.
func (s *PayloadService) Get(id int64) (*store.Payload, error) {
	return s.db.GetPayload(id)
}

// GetByCode loads a payload template by its catalogue code.
func (s *PayloadService) GetByCode(code string) (*store.Payload, error) {
	return s.db.GetPayloadByCode(code)
}

// Update persists field changes on a payload template.
func (s *PayloadService) Update(p *store.Payload) error {
	return s.db.UpdatePayload(p)
}

// Delete removes a payload template.
func (s *PayloadService) Delete(id int64) error {
	return s.db.DeletePayload(id)
}

// List returns every payload template.
func (s *PayloadService) List() ([]*store.Payload, error) {
	return s.db.ListPayloads()
}

// --- Manifest items -------------------------------------------------------

// ListManifest returns the manifest items defined for a payload
// template.
func (s *PayloadService) ListManifest(payloadID int64) ([]*store.PayloadManifestItem, error) {
	return s.db.ListPayloadManifest(payloadID)
}

// CreateManifestItem inserts a manifest item on a payload template.
func (s *PayloadService) CreateManifestItem(item *store.PayloadManifestItem) error {
	return s.db.CreatePayloadManifestItem(item)
}

// UpdateManifestItem adjusts a manifest item's part number or
// quantity.
func (s *PayloadService) UpdateManifestItem(id int64, partNumber string, quantity int64) error {
	return s.db.UpdatePayloadManifestItem(id, partNumber, quantity)
}

// DeleteManifestItem removes a manifest item from a payload template.
func (s *PayloadService) DeleteManifestItem(id int64) error {
	return s.db.DeletePayloadManifestItem(id)
}

// ReplaceManifest swaps out the entire manifest item list for a
// payload template in a single pass.
func (s *PayloadService) ReplaceManifest(payloadID int64, items []*store.PayloadManifestItem) error {
	return s.db.ReplacePayloadManifest(payloadID, items)
}

// --- Bin-type associations ------------------------------------------------

// SetBinTypes replaces the set of compatible bin types for a payload
// template.
func (s *PayloadService) SetBinTypes(payloadID int64, binTypeIDs []int64) error {
	return s.db.SetPayloadBinTypes(payloadID, binTypeIDs)
}

// ListBinTypes returns the bin types compatible with the given
// payload template.
func (s *PayloadService) ListBinTypes(payloadID int64) ([]*store.BinType, error) {
	return s.db.ListBinTypesForPayload(payloadID)
}

// --- Node compatibility ---------------------------------------------------

// ListCompatibleNodes returns the nodes that accept the given payload
// template (via explicit assignment or inherited-all mode).
func (s *PayloadService) ListCompatibleNodes(payloadID int64) ([]*store.Node, error) {
	return s.db.ListNodesForPayload(payloadID)
}
