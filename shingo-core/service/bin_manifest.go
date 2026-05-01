package service

import (
	"encoding/json"
	"fmt"

	"shingocore/store"
)

// reconstructSinglePayloadManifest builds the manifest JSON for a partial-
// release sync. The single-payload normalization assumption (one
// payload_code per bin → consumption distributes evenly across items)
// makes the manifest fully recoverable from payload_code + uop_remaining:
//
//	{"items":[{"catid": payload_code, "qty": uop_remaining}]}
//
// Note: ManifestEntry.CatID is the part-catalog identifier; payload_code
// is the bin's payload type. Equating them here is the normalization
// assumption — single-payload bins have catid == payload_code at the
// manifest level. If a future design supports multi-payload-per-bin,
// this reconstruction is the seam where the assumption breaks.
func reconstructSinglePayloadManifest(payloadCode string, uop int) (string, error) {
	payload := map[string]any{
		"items": []map[string]any{
			{"catid": payloadCode, "qty": uop},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("reconstruct manifest: %w", err)
	}
	return string(b), nil
}

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

// SyncOrClearForReleased applies the operator's release-time remainingUOP value
// to a bin that is already claimed by orderID. Routes nil/zero/positive
// identically to ClaimForDispatch but operates on the existing claim — does
// not set claimed_by (already set during creation-time claim) and does not
// require claimed_by IS NULL.
//
// Used by HandleOrderRelease to late-bind the bin's manifest at the operator's
// release click. Complex orders are claimed at creation time for poaching
// protection, but the count of consumed parts isn't known until the operator
// commits to releasing — this method bridges that gap.
//
//   - remainingUOP == nil: no-op (manifest unchanged — preserves legacy behavior)
//   - *remainingUOP == 0: clear manifest, keep claim (e.g. NOTHING PULLED disposition)
//   - *remainingUOP > 0: sync UOP, keep manifest + claim (e.g. SEND PARTIAL BACK)
//
// actor is the operator identity for the audit row (typically the station
// name from the HTTP request body's called_by field). Empty falls back to
// "system" so internal callers (wiring fallbacks, etc.) get a consistent
// audit shape with the rest of the codebase.
//
// SQL guards: WHERE id=$ AND claimed_by=$ AND locked=false. The claimed_by
// guard prevents a stale release from stomping a bin that has been reassigned
// to a different order. The locked guard mirrors ClearAndClaim/SyncUOPAndClaim.
//
// Idempotent: re-running with the same arguments produces the same row state,
// so retries after a failed fleet release are safe.
func (s *BinManifestService) SyncOrClearForReleased(binID, orderID int64, remainingUOP *int, actor string) error {
	if remainingUOP == nil {
		return nil
	}
	if actor == "" {
		actor = "system"
	}
	if *remainingUOP == 0 {
		// Clear manifest, preserve claim
		res, err := s.db.Exec(`
			UPDATE bins SET
				payload_code='', manifest=NULL, uop_remaining=0,
				manifest_confirmed=false, loaded_at=NULL,
				updated_at=NOW()
			WHERE id=$1 AND claimed_by=$2 AND locked=false`,
			binID, orderID)
		if err != nil {
			return fmt.Errorf("clear manifest for released bin %d: %w", binID, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("bin %d not claimed by order %d (or locked)", binID, orderID)
		}
		s.db.AppendAudit("bin", binID, "released_empty",
			"", fmt.Sprintf("order=%d", orderID), actor)
		return nil
	}
	// Defense in depth: Edge's computeReleaseRemainingUOP guards against
	// non-positive values reaching this branch, but a direct Core caller
	// (test, automation, future bypass) could still hand us a negative
	// pointer. Reject loudly rather than corrupt the bin row.
	if *remainingUOP < 0 {
		return fmt.Errorf("remainingUOP must be nil, 0, or positive; got %d", *remainingUOP)
	}
	// Positive: sync UOP AND reconstruct manifest, preserve claim.
	//
	// Manifest reconstruction (single-payload normalization assumption):
	// the bin's manifest is rewritten to {"items":[{"catid": payload_code,
	// "qty": remaining_uop}]}. Pre-2026-05 this branch only updated
	// uop_remaining, leaving the manifest carrying the pre-release qty
	// — the SMN_003/ALN_002 stale-manifest bug class. The reconstruction
	// is atomic with the UOP update via jsonb_build_object reading
	// payload_code from the same row, so no read-then-update race.
	//
	// The CASE guard preserves the prior manifest if payload_code is
	// empty (a malformed state — partial-release should always have a
	// payload). Erroring would be cleaner but risks regressing release
	// flows in the field; preserving the prior manifest matches the
	// pre-fix observable behavior for the edge case.
	res, err := s.db.Exec(`
		UPDATE bins SET
			uop_remaining=$1,
			manifest=CASE
				WHEN COALESCE(payload_code, '') = '' THEN manifest
				ELSE jsonb_build_object(
					'items', jsonb_build_array(
						jsonb_build_object('catid', payload_code, 'qty', $1::int)
					)
				)
			END,
			updated_at=NOW()
		WHERE id=$2 AND claimed_by=$3 AND locked=false`,
		*remainingUOP, binID, orderID)
	if err != nil {
		return fmt.Errorf("sync UOP for released bin %d: %w", binID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bin %d not claimed by order %d (or locked)", binID, orderID)
	}
	s.db.AppendAudit("bin", binID, "released_partial",
		"", fmt.Sprintf("uop=%d order=%d", *remainingUOP, orderID), actor)
	return nil
}

// SyncOrClearForReleasedNoOwner is the source-node-fallback variant of
// SyncOrClearForReleased. Identical routing (nil → no-op, 0 → clear,
// >0 → sync UOP), identical audit, but the SQL guard drops the
// claimed_by check — used by HandleOrderRelease when order.BinID is nil
// and we've located the bin by source-node lookup instead.
//
// Why no claim guard: the bin we're targeting wasn't claimed by this
// order (claimComplexBins missed it at creation time, which is the bug
// this method is the safety net for). Requiring claimed_by=$orderID
// would always fail. The locked=false guard stays — actively-handled
// bins must not be mutated mid-flight regardless of how we found them.
//
// orderID is still threaded through for audit-trail completeness so the
// row identifies which release request triggered the change.
//
// Idempotent: re-running with the same arguments produces the same row
// state, so retries after a failed fleet release are safe.
func (s *BinManifestService) SyncOrClearForReleasedNoOwner(binID, orderID int64, remainingUOP *int, actor string) error {
	if remainingUOP == nil {
		return nil
	}
	if actor == "" {
		actor = "system"
	}
	if *remainingUOP == 0 {
		res, err := s.db.Exec(`
			UPDATE bins SET
				payload_code='', manifest=NULL, uop_remaining=0,
				manifest_confirmed=false, loaded_at=NULL,
				updated_at=NOW()
			WHERE id=$1 AND locked=false`,
			binID)
		if err != nil {
			return fmt.Errorf("clear manifest for released bin %d (no-owner fallback): %w", binID, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("bin %d not found or locked (no-owner fallback)", binID)
		}
		s.db.AppendAudit("bin", binID, "released_empty_fallback",
			"", fmt.Sprintf("order=%d (source-node fallback)", orderID), actor)
		return nil
	}
	if *remainingUOP < 0 {
		return fmt.Errorf("remainingUOP must be nil, 0, or positive; got %d", *remainingUOP)
	}
	// Same manifest-reconstruction logic as SyncOrClearForReleased but
	// without the claimed_by guard — see the parent method's comment for
	// the rationale on the JSON shape and the empty-payload_code CASE.
	res, err := s.db.Exec(`
		UPDATE bins SET
			uop_remaining=$1,
			manifest=CASE
				WHEN COALESCE(payload_code, '') = '' THEN manifest
				ELSE jsonb_build_object(
					'items', jsonb_build_array(
						jsonb_build_object('catid', payload_code, 'qty', $1::int)
					)
				)
			END,
			updated_at=NOW()
		WHERE id=$2 AND locked=false`,
		*remainingUOP, binID)
	if err != nil {
		return fmt.Errorf("sync UOP for released bin %d (no-owner fallback): %w", binID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bin %d not found or locked (no-owner fallback)", binID)
	}
	s.db.AppendAudit("bin", binID, "released_partial_fallback",
		"", fmt.Sprintf("uop=%d order=%d (source-node fallback)", *remainingUOP, orderID), actor)
	return nil
}

