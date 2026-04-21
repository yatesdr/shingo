package service

import (
	"fmt"

	"shingocore/store"
)

// BinService centralizes bin validation and mutation. Handlers call BinService
// for create/move/load/lock/status changes instead of touching *store.DB
// directly; audit logging and event emission stay at the handler layer (same
// boundary BinManifestService established).
//
// Stage 3 of the architecture plan introduces BinService as the pilot for
// the service layer. The scope is deliberately narrow: move the validation
// and mutation logic out of www/handlers_bins.go so www/ can be migrated
// off direct store calls in Stage 4 alongside OrderService / NodeService.
type BinService struct {
	db       *store.DB
	manifest *BinManifestService
}

func NewBinService(db *store.DB, manifest *BinManifestService) *BinService {
	return &BinService{db: db, manifest: manifest}
}

// Manifest returns the bin manifest service. BinService composes the
// manifest service so callers that already hold a *BinService don't have
// to plumb both references through the handler layer.
func (s *BinService) Manifest() *BinManifestService { return s.manifest }

// --- Creation --------------------------------------------------------------

// Create inserts a single bin. If the bin is placed at a physical (non-
// synthetic) node, the destination must be empty. Synthetic nodes (LANE,
// NGRP) hold bins via their children and are not subject to the one-bin-
// per-node rule.
func (s *BinService) Create(b *store.Bin) error {
	if b.NodeID != nil {
		if err := s.ensurePhysicalNodeEmpty(*b.NodeID, 1); err != nil {
			return err
		}
	}
	return s.db.CreateBin(b)
}

// CreateBatch inserts `count` bins sharing a template (bin type, node,
// status, description). Labels are formed as `labelPrefix + NNNN` starting
// at 0001. Physical nodes may only receive one bin; synthetic nodes may
// receive many.
func (s *BinService) CreateBatch(template store.Bin, labelPrefix string, count int) error {
	if count <= 0 {
		count = 1
	}
	if template.NodeID != nil {
		if count > 1 {
			node, err := s.db.GetNode(*template.NodeID)
			if err != nil {
				return fmt.Errorf("node %d not found", *template.NodeID)
			}
			if !node.IsSynthetic {
				return fmt.Errorf("cannot create multiple bins at a single physical node")
			}
		} else {
			if err := s.ensurePhysicalNodeEmpty(*template.NodeID, 1); err != nil {
				return err
			}
		}
	}

	for i := 0; i < count; i++ {
		b := template
		b.Label = labelPrefix + fmt.Sprintf("%04d", i+1)
		if err := s.db.CreateBin(&b); err != nil {
			return err
		}
	}
	return nil
}

// ensurePhysicalNodeEmpty guards the one-bin-per-physical-node invariant.
// The dispatch path has equivalent guards (fulfillment_scanner.go
// destination-occupancy check); this mirrors that at the admin UI entry
// point.
func (s *BinService) ensurePhysicalNodeEmpty(nodeID int64, addCount int) error {
	node, err := s.db.GetNode(nodeID)
	if err != nil {
		return fmt.Errorf("node %d not found", nodeID)
	}
	if node.IsSynthetic {
		return nil
	}
	if addCount > 1 {
		return fmt.Errorf("cannot create multiple bins at a single physical node")
	}
	existing, err := s.db.CountBinsByNode(nodeID)
	if err != nil {
		return fmt.Errorf("check node occupancy: %w", err)
	}
	if existing > 0 {
		return fmt.Errorf("node %d already has %d bin(s); move or delete existing bin first", nodeID, existing)
	}
	return nil
}

// --- Status transitions ---------------------------------------------------

// ChangeStatus updates a bin's status without additional validation.
func (s *BinService) ChangeStatus(binID int64, status string) error {
	return s.db.UpdateBinStatus(binID, status)
}

// Release moves a staged bin back to the available state.
func (s *BinService) Release(binID int64) error {
	return s.db.ReleaseStagedBin(binID)
}

// Lock acquires a lock on the bin for the given actor. Actor is required.
func (s *BinService) Lock(binID int64, actor string) error {
	if actor == "" {
		return fmt.Errorf("actor is required for lock")
	}
	return s.db.LockBin(binID, actor)
}

// Unlock releases the lock on a bin.
func (s *BinService) Unlock(binID int64) error {
	return s.db.UnlockBin(binID)
}

// --- Payload loading ------------------------------------------------------

// LoadPayload validates that the payload code exists and sets the bin's
// manifest from the payload template. uopOverride of 0 uses the template's
// UOP capacity.
func (s *BinService) LoadPayload(binID int64, payloadCode string, uopOverride int) error {
	if payloadCode == "" {
		return fmt.Errorf("payload_code is required")
	}
	if _, err := s.db.GetPayloadByCode(payloadCode); err != nil {
		return fmt.Errorf("payload template %q not found", payloadCode)
	}
	return s.db.SetBinManifestFromTemplate(binID, payloadCode, uopOverride)
}

// --- Movement -------------------------------------------------------------

// MoveResult describes the destination a bin was moved to so callers can
// write audit entries and emit events without re-fetching the node.
type MoveResult struct {
	DestNode *store.Node
}

// Move relocates a bin to a new node. Validates:
//   - bin is not already at the destination
//   - destination node exists
//   - destination is either synthetic or empty
func (s *BinService) Move(b *store.Bin, toNodeID int64) (*MoveResult, error) {
	if toNodeID == 0 {
		return nil, fmt.Errorf("node_id is required")
	}
	if b.NodeID != nil && *b.NodeID == toNodeID {
		return nil, fmt.Errorf("bin is already at this location")
	}
	destNode, err := s.db.GetNode(toNodeID)
	if err != nil {
		return nil, fmt.Errorf("node not found")
	}
	if !destNode.IsSynthetic {
		existing, err := s.db.CountBinsByNode(toNodeID)
		if err != nil {
			return nil, fmt.Errorf("check destination occupancy: %w", err)
		}
		if existing > 0 {
			return nil, fmt.Errorf("destination node %d already has %d bin(s); move or delete existing bin first", toNodeID, existing)
		}
	}
	if err := s.db.MoveBin(b.ID, toNodeID); err != nil {
		return nil, err
	}
	return &MoveResult{DestNode: destNode}, nil
}

// --- Counting -------------------------------------------------------------

// CountResult reports the outcome of a cycle count so callers can log
// discrepancies in the audit trail.
type CountResult struct {
	Expected    int
	Actual      int
	Discrepancy bool
}

// RecordCount writes a cycle count for the bin and returns the expected vs.
// actual counts. Discrepancy notes are written by the caller so the note's
// actor matches the audit actor convention already used by handlers.
func (s *BinService) RecordCount(b *store.Bin, actualUOP int, actor string) (*CountResult, error) {
	expected := b.UOPRemaining
	if err := s.db.RecordBinCount(b.ID, actualUOP, actor); err != nil {
		return nil, err
	}
	return &CountResult{
		Expected:    expected,
		Actual:      actualUOP,
		Discrepancy: expected != actualUOP,
	}, nil
}

// --- Notes ----------------------------------------------------------------

// AddNote validates message presence and attaches a note to the bin.
// noteType defaults to "general" when empty.
func (s *BinService) AddNote(binID int64, noteType, message, actor string) error {
	if message == "" {
		return fmt.Errorf("message is required")
	}
	if noteType == "" {
		noteType = "general"
	}
	return s.db.AddBinNote(binID, noteType, message, actor)
}

// --- Update ---------------------------------------------------------------

// Update applies partial field updates to a bin. Nil pointers mean "leave
// this field alone". Fields supported today: Label, Description, BinTypeID.
// This helper exists so handlers don't have to mutate the caller-owned
// *store.Bin in place before calling UpdateBin.
func (s *BinService) Update(b *store.Bin, label, description *string, binTypeID *int64) error {
	if label != nil {
		b.Label = *label
	}
	if description != nil {
		b.Description = *description
	}
	if binTypeID != nil {
		b.BinTypeID = *binTypeID
	}
	return s.db.UpdateBin(b)
}

// --- Queries --------------------------------------------------------------

// GetBin loads a bin by ID. Absorbed from engine_db_methods.go as part
// of the www-handler service migration (PR 3a.2).
func (s *BinService) GetBin(id int64) (*store.Bin, error) {
	return s.db.GetBin(id)
}

// ListBins returns every bin in the store. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.2).
func (s *BinService) ListBins() ([]*store.Bin, error) {
	return s.db.ListBins()
}

// Delete removes a bin row. Absorbed from engine_db_methods.go as part
// of the www-handler service migration (PR 3a.2).
func (s *BinService) Delete(id int64) error {
	return s.db.DeleteBin(id)
}

// HasNotes returns a map indicating which of the supplied bin IDs have
// any notes attached. Absorbed from engine_db_methods.go as part of the
// www-handler service migration (PR 3a.2).
func (s *BinService) HasNotes(binIDs []int64) (map[int64]bool, error) {
	return s.db.BinHasNotes(binIDs)
}

// --- Bin types ------------------------------------------------------------

// CreateBinType inserts a new bin type row. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.2).
func (s *BinService) CreateBinType(bt *store.BinType) error {
	return s.db.CreateBinType(bt)
}

// GetBinType loads a bin type by ID. Absorbed from engine_db_methods.go
// as part of the www-handler service migration (PR 3a.2).
func (s *BinService) GetBinType(id int64) (*store.BinType, error) {
	return s.db.GetBinType(id)
}

// UpdateBinType persists changes to a bin type row. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.2).
func (s *BinService) UpdateBinType(bt *store.BinType) error {
	return s.db.UpdateBinType(bt)
}

// DeleteBinType removes a bin type row. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.2).
func (s *BinService) DeleteBinType(id int64) error {
	return s.db.DeleteBinType(id)
}

// ListBinTypes returns every bin type in the store. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.2).
func (s *BinService) ListBinTypes() ([]*store.BinType, error) {
	return s.db.ListBinTypes()
}

// CountBinsByAllNodes returns a map of node_id -> bin count for every
// node that has at least one bin. Absorbed from engine_db_methods.go
// as part of the nodesPageDataStore dissolution (PR 3a.5.1).
func (s *BinService) CountBinsByAllNodes() (map[int64]int, error) {
	return s.db.CountBinsByAllNodes()
}

// ── PR 3a.6 additions: remaining www-reachable bin lookups ───────────────

// GetByLabel resolves a bin by its human-readable label. Absorbed
// from engine_db_methods.go as part of the Phase 3a closeout
// (PR 3a.6).
func (s *BinService) GetByLabel(label string) (*store.Bin, error) {
	return s.db.GetBinByLabel(label)
}

// GetManifest returns the confirmed manifest items currently loaded
// on a bin. Absorbed from engine_db_methods.go as part of the Phase
// 3a closeout (PR 3a.6).
func (s *BinService) GetManifest(binID int64) (*store.BinManifest, error) {
	return s.db.GetBinManifest(binID)
}
