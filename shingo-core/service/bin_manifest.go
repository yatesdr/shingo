package service

import (
	"fmt"

	"shingocore/store"
)

// BinManifestService manages bin manifest lifecycle mutations.
// All manifest changes flow through this service so that validation,
// audit logging, and event emission are centralized.
type BinManifestService struct {
	db *store.DB
}

func NewBinManifestService(db *store.DB) *BinManifestService {
	return &BinManifestService{db: db}
}

// ClearForReuse empties a bin's manifest. The bin becomes visible
// to FindEmptyCompatibleBin after this call.
func (s *BinManifestService) ClearForReuse(binID int64) error {
	if err := s.db.ClearBinManifest(binID); err != nil {
		return fmt.Errorf("clear manifest bin %d: %w", binID, err)
	}
	return nil
}

// SyncUOP updates the remaining UOP on a bin without touching
// the manifest. Used for partial consumption where the manifest
// stays valid but the count changes.
func (s *BinManifestService) SyncUOP(binID int64, remaining int) error {
	_, err := s.db.Exec(
		`UPDATE bins SET uop_remaining=$1, updated_at=NOW() WHERE id=$2`,
		remaining, binID)
	if err != nil {
		return fmt.Errorf("sync uop bin %d: %w", binID, err)
	}
	return nil
}

// SetForProduction sets a bin's manifest and UOP from a payload template.
// Used when a produce node finalizes a bin or a manual_swap node loads a bin.
func (s *BinManifestService) SetForProduction(binID int64, manifestJSON, payloadCode string, uop int) error {
	if err := s.db.SetBinManifest(binID, manifestJSON, payloadCode, uop); err != nil {
		return fmt.Errorf("set manifest bin %d: %w", binID, err)
	}
	return nil
}

// Confirm marks a bin's manifest as confirmed by an operator or automated process.
func (s *BinManifestService) Confirm(binID int64, producedAt string) error {
	if err := s.db.ConfirmBinManifest(binID, producedAt); err != nil {
		return fmt.Errorf("confirm manifest bin %d: %w", binID, err)
	}
	return nil
}

// Unconfirm clears a bin's manifest confirmation flag. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.2).
func (s *BinManifestService) Unconfirm(binID int64) error {
	if err := s.db.UnconfirmBinManifest(binID); err != nil {
		return fmt.Errorf("unconfirm manifest bin %d: %w", binID, err)
	}
	return nil
}

// ClearAndClaim atomically clears manifest and claims the bin for an order.
// Closes the TOCTOU race where ClaimBin + ClearBinManifest are separate txns.
func (s *BinManifestService) ClearAndClaim(binID, orderID int64) error {
	res, err := s.db.Exec(`
		UPDATE bins SET
			payload_code='', manifest=NULL, uop_remaining=0,
			manifest_confirmed=false, loaded_at=NULL,
			claimed_by=$1, updated_at=NOW()
		WHERE id=$2 AND locked=false AND claimed_by IS NULL`,
		orderID, binID)
	if err != nil {
		return fmt.Errorf("clear+claim bin %d: %w", binID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bin %d is locked, already claimed, or does not exist", binID)
	}
	return nil
}

// SyncUOPAndClaim atomically syncs remaining UOP and claims the bin.
// For partial consumption: manifest preserved, only uop_remaining updated.
func (s *BinManifestService) SyncUOPAndClaim(binID, orderID int64, remainingUOP int) error {
	res, err := s.db.Exec(`
		UPDATE bins SET
			uop_remaining=$1, claimed_by=$2, updated_at=NOW()
		WHERE id=$3 AND locked=false AND claimed_by IS NULL`,
		remainingUOP, orderID, binID)
	if err != nil {
		return fmt.Errorf("sync+claim bin %d: %w", binID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bin %d is locked, already claimed, or does not exist", binID)
	}
	return nil
}

// ClaimForDispatch selects the correct bin operation based on remaining UOP
// and executes it atomically. Used by all dispatch paths that claim bins.
//
//   - remainingUOP == nil: plain claim (no manifest change)
//   - *remainingUOP == 0: clear manifest + claim (fully depleted)
//   - *remainingUOP > 0: sync UOP + claim (partial consumption)
func (s *BinManifestService) ClaimForDispatch(binID, orderID int64, remainingUOP *int) error {
	if remainingUOP != nil && *remainingUOP == 0 {
		return s.ClearAndClaim(binID, orderID)
	}
	if remainingUOP != nil {
		return s.SyncUOPAndClaim(binID, orderID, *remainingUOP)
	}
	return s.db.ClaimBin(binID, orderID)
}

