package service

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"shingo/protocol"

	"shingocore/domain"
	"shingocore/store/audit"
	"shingocore/store/bins"
	"shingocore/store/reservations"
)

// BinManifestService manages bin manifest lifecycle mutations.
// All manifest changes flow through this service so that validation,
// audit logging, and event emission are centralized.
//
// The db dependency is declared as the BinManifestStore interface
// (see bin_manifest_store.go) rather than *store.DB. *store.DB
// satisfies it structurally; engine wiring is unchanged.
type BinManifestService struct {
	db BinManifestStore
}

func NewBinManifestService(db BinManifestStore) *BinManifestService {
	return &BinManifestService{db: db}
}

// readBinUOPInTx returns the bin's current uop_remaining inside a tx,
// for capture as before_uop on a bin_uop_audit row. Returns nil when the
// bin row does not exist (a path that's only legitimate for
// SetForProduction on freshly created bins; every other caller has
// already validated the bin's presence upstream).
func readBinUOPInTx(tx *sql.Tx, binID int64) (*int, error) {
	var v int
	if err := tx.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, binID).Scan(&v); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("read uop bin %d: %w", binID, err)
	}
	return &v, nil
}

// binTypeCodeInTx looks up a bin type's code by ID inside an open tx.
// Best-effort only (returns "" on zero id or scan failure); callers use it
// for audit-detail enrichment, not correctness decisions.
func binTypeCodeInTx(tx *sql.Tx, id int64) string {
	if id == 0 {
		return ""
	}
	var code string
	_ = tx.QueryRow(`SELECT code FROM bin_types WHERE id=$1`, id).Scan(&code)
	return code
}

// bumpEpoch increments a bin's delta_epoch inside the caller's tx and returns
// the new value. Every count-reset/clear path calls this so "a reset starts a
// fresh delta stream" is structural — a new reset path can't silently forget
// it. That omission is the failure this guards against: a reset that didn't
// bump left late cross-epoch ticks looking same-epoch, so the applier's
// stale-epoch guard couldn't tell them apart from live ticks and dropped (or
// misapplied) them.
func bumpEpoch(tx *sql.Tx, binID int64) (int64, error) {
	var epoch int64
	if err := tx.QueryRow(
		`UPDATE bins SET delta_epoch=delta_epoch+1 WHERE id=$1 RETURNING delta_epoch`,
		binID).Scan(&epoch); err != nil {
		return 0, fmt.Errorf("bump delta_epoch bin %d: %w", binID, err)
	}
	return epoch, nil
}

// resolveBinUOPContext builds the bin_uop_audit enrichment context inside the caller's
// tx (keystone step 2): the bin's CURRENT node and the loader that owns that node, via
// bin_loader_homes.position_node_id (UNIQUE — one loader per member node). Stamping the
// loader_id at event time is what lets loads (set_for_production) and unloads
// (release-family ops) group per loader per window/position. Both are NULL when the bin
// is not at a loader member node — an ordinary produce/consume event — which the
// per-loader analytics filter (loader_id IS NOT NULL) correctly excludes. node_id is
// unaffected by the manifest UPDATE the caller runs around this (UPDATE never touches
// node_id), so resolving here is order-independent. detail is passed through (only the
// confirm path carries one).
func resolveBinUOPContext(tx *sql.Tx, binID int64, detail json.RawMessage) (audit.BinUOPContext, error) {
	var nodeID, loaderID sql.NullInt64
	err := tx.QueryRow(`SELECT b.node_id, h.loader_id
		FROM bins b
		LEFT JOIN bin_loader_homes h ON h.position_node_id = b.node_id
		WHERE b.id=$1`, binID).Scan(&nodeID, &loaderID)
	if err != nil && err != sql.ErrNoRows {
		return audit.BinUOPContext{Detail: detail}, fmt.Errorf("resolve bin_uop context bin %d: %w", binID, err)
	}
	ctx := audit.BinUOPContext{Detail: detail}
	if nodeID.Valid {
		v := nodeID.Int64
		ctx.NodeID = &v
	}
	if loaderID.Valid {
		v := loaderID.Int64
		ctx.LoaderID = &v
	}
	return ctx, nil
}

// ClearForReuse empties a bin's manifest. The bin becomes visible
// to FindEmptyCompatibleBin after this call. Owns its transaction.
// ClearForReuse begins its own transaction and delegates to
// ClearForReuseTx. Returns the new delta_epoch so callers that ship
// the bin's state to Edge (e.g. handlers that return JSON containing
// the bin's row) can include it in their response. Callers that don't
// care can ignore the value.
//
// binTypeID is optional (nil = leave bin_type_id unchanged). When
// non-nil the type is written atomically with the manifest clear so
// a floating carrier's dunnage identity is always consistent with its
// empty state. Callers that don't set dunnage (UOP-applier auto-clear,
// admin clear) pass nil.
func (s *BinManifestService) ClearForReuse(binID int64, binTypeID *int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	epoch, err := s.ClearForReuseTx(tx, binID, binTypeID, audit.OpClearForReuse, "service/bin_manifest.go:ClearForReuse")
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return epoch, nil
}

// ClearForReuseTx performs the same manifest-clear write as
// ClearForReuse but inside a caller-provided transaction. Used by
// InventoryDeltaService.ApplyBinUOPDelta so the manifest clear and
// the bin-update both commit (or roll back) atomically — without it,
// a half-applied state would leave the bin reachable as "empty" while
// uop_remaining still carried stale value, or vice versa.
//
// The op tag and source are caller-supplied so audit consumers can
// distinguish the trigger (released_capture_empty vs the direct
// admin clear_for_reuse).
// ClearForReuseTx ends a bin's current load-lifecycle: clears the
// manifest, resets uop_remaining to 0, AND bumps delta_epoch so the
// next set_for_production starts a fresh delta stream. Returns the
// new delta_epoch.
//
// The epoch bump is on this path (not on SetForProduction) so a bin
// that's released_empty then sits idle still has a clean dedup space
// for whatever delta arrives next — the SetForProduction would bump
// a second time, but the (load-side) Edge cache learns the post-clear
// epoch on the bin-state refresh that follows the clear.
//
// binTypeID is optional (nil = preserve existing bin_type_id). When
// non-nil, bin_type_id is updated atomically with the manifest clear.
func (s *BinManifestService) ClearForReuseTx(tx *sql.Tx, binID int64, binTypeID *int64, op, source string) (int64, error) {
	before, err := readBinUOPInTx(tx, binID)
	if err != nil {
		return 0, err
	}
	// Capture the old bin_type_id before the UPDATE so the audit row can
	// record the from→to transition. Only paid when the caller supplies a
	// new type; the common nil path skips this read entirely.
	var oldTypeID sql.NullInt64
	if binTypeID != nil {
		_ = tx.QueryRow(`SELECT bin_type_id FROM bins WHERE id=$1`, binID).Scan(&oldTypeID)
	}
	// COALESCE($2, bin_type_id): when binTypeID is nil the column is
	// left unchanged; when non-nil the dunnage type is re-stamped
	// atomically with the manifest clear + epoch bump.
	if _, err := tx.Exec(`UPDATE bins SET payload_code='', manifest=NULL, uop_remaining=0,
		manifest_confirmed=false, loaded_at=NULL,
		bin_type_id=COALESCE($2, bin_type_id), updated_at=NOW()
		WHERE id=$1`, binID, binTypeID); err != nil {
		return 0, fmt.Errorf("clear manifest bin %d: %w", binID, err)
	}
	newEpoch, err := bumpEpoch(tx, binID)
	if err != nil {
		return 0, err
	}
	uopCtx, err := resolveBinUOPContext(tx, binID, nil)
	if err != nil {
		return 0, err
	}
	// Record from→to bin-type codes in the audit detail so a floating
	// carrier's dunnage history is reconstructable from bin_uop_audit rows.
	if binTypeID != nil {
		fromCode := binTypeCodeInTx(tx, oldTypeID.Int64) // "" when NULL or missing
		toCode := binTypeCodeInTx(tx, *binTypeID)
		uopCtx.Detail, _ = json.Marshal(map[string]string{
			"dunnage_from": fromCode,
			"dunnage_to":   toCode,
		})
	}
	if err := audit.AppendBinUOP(tx, binID, before, 0, op, source, nil, "", "", uopCtx); err != nil {
		return 0, err
	}
	return newEpoch, nil
}

// (Item 14 D8: SyncUOP deleted — zero production callers. Partial-
// consumption sync goes through ApplyBinUOPDelta in the post-bin-as-
// truth flow; SyncUOPAndClaim covers the claim-with-uop case
// directly. The OpSyncUOP audit tag stays in store/audit/bin_uop.go
// for historical rows.)

// SetFromTemplate resolves a payload template (manifest items + UOP
// capacity) and writes the bin via SetForProduction. Used by the
// dispatch ingest path and the operator load-payload action — both
// previously called the lower-level *store.DB.SetBinManifestFromTemplate
// which bypassed audit. Item 19 of the bin-as-truth refactor: routing
// through this service method ensures every manifest write surfaces
// in bin_uop_audit so the Item 10 audit timeline UI sees the
// freshly-loaded bin's 0→capacity initial fill alongside the
// downstream consume / capture deltas.
//
// uopOverride of 0 falls back to the template's UOPCapacity. Non-zero
// uopOverride lets callers (produce ingest, partial-fill operator
// loads) record an actual count rather than the template default.
//
// Returns the new delta_epoch from the underlying SetForProduction
// call so handlers that ship the bin's row to Edge can include it in
// their response.
func (s *BinManifestService) SetFromTemplate(binID int64, payloadCode string, uopOverride int) (int64, error) {
	manifestJSON, uop, err := s.resolveTemplateManifest(payloadCode, uopOverride)
	if err != nil {
		return 0, err
	}
	return s.SetForProduction(binID, manifestJSON, payloadCode, uop)
}

// resolveTemplateManifest resolves a payload template into a marshalled manifest
// JSON and the UOP to write (uopOverride, or the template's UOPCapacity when
// uopOverride is 0). Shared by SetFromTemplate and RecordProducedBinFromTemplate.
func (s *BinManifestService) resolveTemplateManifest(payloadCode string, uopOverride int) (string, int, error) {
	p, err := s.db.GetPayloadByCode(payloadCode)
	if err != nil {
		return "", 0, fmt.Errorf("payload template %q: %w", payloadCode, err)
	}
	items, err := s.db.ListPayloadManifest(p.ID)
	if err != nil {
		return "", 0, fmt.Errorf("payload manifest: %w", err)
	}
	manifest := domain.Manifest{Items: make([]domain.ManifestEntry, len(items))}
	for i, item := range items {
		manifest.Items[i] = domain.ManifestEntry{
			CatID:    item.PartNumber,
			Quantity: item.Quantity,
		}
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return "", 0, fmt.Errorf("marshal manifest: %w", err)
	}
	uop := uopOverride
	if uop == 0 {
		uop = p.UOPCapacity
	}
	return string(manifestJSON), uop, nil
}

// SetForProduction sets a bin's manifest and UOP from a payload template,
// AND bumps delta_epoch so the new load gets its own dedup space on Core
// (and the corresponding Edge seq-allocator entry). Used when a produce
// node finalizes a bin or a manual_swap node loads a bin. Returns the
// new delta_epoch so callers can ship it to Edge in the response that
// triggered the load (typically the BinLoad handler).
func (s *BinManifestService) SetForProduction(binID int64, manifestJSON, payloadCode string, uop int) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	newEpoch, err := s.setForProductionTx(tx, binID, manifestJSON, payloadCode, uop)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newEpoch, nil
}

// setForProductionTx is SetForProduction's body against a caller-provided tx, so
// the manifest write can commit in the SAME transaction as the confirm (see
// RecordProducedBin). Writes the OpSetForProduction audit row and bumps the
// delta_epoch; returns the new epoch.
func (s *BinManifestService) setForProductionTx(tx *sql.Tx, binID int64, manifestJSON, payloadCode string, uop int) (int64, error) {
	before, err := readBinUOPInTx(tx, binID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE bins SET payload_code=$1, manifest=$2, uop_remaining=$3,
		manifest_confirmed=false, updated_at=NOW()
		WHERE id=$4`,
		payloadCode, manifestJSON, uop, binID); err != nil {
		return 0, fmt.Errorf("set manifest bin %d: %w", binID, err)
	}
	newEpoch, err := bumpEpoch(tx, binID)
	if err != nil {
		return 0, err
	}
	uopCtx, err := resolveBinUOPContext(tx, binID, nil)
	if err != nil {
		return 0, err
	}
	if err := audit.AppendBinUOP(tx, binID, before, uop,
		audit.OpSetForProduction, "service/bin_manifest.go:SetForProduction",
		nil, payloadCode, "", uopCtx); err != nil {
		return 0, err
	}
	return newEpoch, nil
}

// Confirm marks a bin's manifest as confirmed by an operator or automated
// process. Writes a same-tx manifest_confirmed bin_uop_audit row so the
// confirm is no longer a silent mutation (§16 PR 3); detail carries loaded_at.
// after_uop is the bin's unchanged uop_remaining (confirm records a lifecycle
// event, not a count change).
func (s *BinManifestService) Confirm(binID int64, producedAt string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.confirmTx(tx, binID, producedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// confirmTx is Confirm's body against a caller-provided tx, so a produce
// finalize can set-and-confirm atomically (see RecordProducedBin). Writes the
// OpManifestConfirmed audit row; loaded_at resolves to producedAt when present,
// server time when blank.
func (s *BinManifestService) confirmTx(tx *sql.Tx, binID int64, producedAt string) error {
	before, err := readBinUOPInTx(tx, binID)
	if err != nil {
		return err
	}
	loadedAt, uop, payloadCode, err := bins.ConfirmManifestTx(tx, binID, producedAt)
	if err != nil {
		return err
	}
	detail, _ := json.Marshal(struct {
		LoadedAt time.Time `json:"loaded_at"`
	}{loadedAt})
	uopCtx, err := resolveBinUOPContext(tx, binID, detail)
	if err != nil {
		return err
	}
	return audit.AppendBinUOP(tx, binID, before, uop,
		audit.OpManifestConfirmed, "service/bin_manifest.go:Confirm",
		nil, payloadCode, "", uopCtx)
}

// RecordProducedBin writes a produced bin's manifest AND confirms it in ONE
// transaction: the set-for-production write (payload + count + manifest,
// OpSetForProduction audit + epoch bump) and the confirm (manifest_confirmed +
// OpManifestConfirmed audit, loaded_at from producedAt or server time) commit
// together or not at all.
//
// The ingest apply path uses this so a confirm failure can no longer leave a
// counted-but-unconfirmed bin — a stranded state, since manifest_confirmed is a
// hard gate for a full bin to be a drain/retrieve source (kanban never sees an
// unconfirmed bin).
func (s *BinManifestService) RecordProducedBin(binID int64, manifestJSON, payloadCode string, uop int, producedAt string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := s.setForProductionTx(tx, binID, manifestJSON, payloadCode, uop); err != nil {
		return err
	}
	if err := s.confirmTx(tx, binID, producedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordProducedBinFromTemplate is RecordProducedBin for the no-manifest ingest
// path: it resolves the payload template (like SetFromTemplate) then sets and
// confirms the bin in one transaction.
func (s *BinManifestService) RecordProducedBinFromTemplate(binID int64, payloadCode string, uopOverride int, producedAt string) error {
	manifestJSON, uop, err := s.resolveTemplateManifest(payloadCode, uopOverride)
	if err != nil {
		return err
	}
	return s.RecordProducedBin(binID, manifestJSON, payloadCode, uop, producedAt)
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
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if err := s.clearAndClaimTx(tx, binID, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

// clearAndClaimTx is ClearAndClaim's body against a caller-provided tx, so the
// claim can commit in the SAME transaction as the reservation confirm.
func (s *BinManifestService) clearAndClaimTx(tx *sql.Tx, binID, orderID int64) error {
	before, err := readBinUOPInTx(tx, binID)
	if err != nil {
		return err
	}
	// Demoted-CAS guard: requires this order's pending reservation to
	// exist, so a concurrent claimer without a reservation cannot steal the
	// bin even if it passes the claimed_by check. claimed_by IS NULL is
	// defense-in-depth for the mixed-binary rollback window; the OR claimed_by=$1
	// leg makes a re-claim by THIS order idempotent (the wedge heal),
	// mirroring bins.Claim / nodes.ClaimSlot.
	res, err := tx.Exec(`
		UPDATE bins SET
			payload_code='', manifest=NULL, uop_remaining=0,
			manifest_confirmed=false, loaded_at=NULL,
			claimed_by=$1, updated_at=NOW()
		WHERE id=$2 AND locked=false AND (claimed_by IS NULL OR claimed_by=$1)
		  AND EXISTS (SELECT 1 FROM reservations WHERE order_id=$1 AND bin_id=$2 AND state='pending')`,
		orderID, binID)
	if err != nil {
		return fmt.Errorf("clear+claim bin %d: %w", binID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bin %d is locked, already claimed, or does not exist", binID)
	}
	if _, err := bumpEpoch(tx, binID); err != nil {
		return err
	}
	uopCtx, err := resolveBinUOPContext(tx, binID, nil)
	if err != nil {
		return err
	}
	return audit.AppendBinUOP(tx, binID, before, 0,
		audit.OpClearAndClaim, "service/bin_manifest.go:ClearAndClaim",
		&orderID, "", "", uopCtx)
}

// SyncUOPAndClaim atomically syncs remaining UOP and claims the bin.
// For partial consumption: manifest preserved, only uop_remaining updated.
func (s *BinManifestService) SyncUOPAndClaim(binID, orderID int64, remainingUOP int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if err := s.syncUOPAndClaimTx(tx, binID, orderID, remainingUOP); err != nil {
		return err
	}
	return tx.Commit()
}

// syncUOPAndClaimTx is SyncUOPAndClaim's body against a caller-provided tx.
func (s *BinManifestService) syncUOPAndClaimTx(tx *sql.Tx, binID, orderID int64, remainingUOP int) error {
	before, err := readBinUOPInTx(tx, binID)
	if err != nil {
		return err
	}
	// Demoted-CAS guard + owner-idempotent: mirrors clearAndClaimTx.
	res, err := tx.Exec(`
		UPDATE bins SET
			uop_remaining=$1, claimed_by=$2, updated_at=NOW()
		WHERE id=$3 AND locked=false AND (claimed_by IS NULL OR claimed_by=$2)
		  AND EXISTS (SELECT 1 FROM reservations WHERE order_id=$2 AND bin_id=$3 AND state='pending')`,
		remainingUOP, orderID, binID)
	if err != nil {
		return fmt.Errorf("sync+claim bin %d: %w", binID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bin %d is locked, already claimed, or does not exist", binID)
	}
	uopCtx, err := resolveBinUOPContext(tx, binID, nil)
	if err != nil {
		return err
	}
	return audit.AppendBinUOP(tx, binID, before, remainingUOP,
		audit.OpSyncUOPAndClaim, "service/bin_manifest.go:SyncUOPAndClaim",
		&orderID, "", "", uopCtx)
}

// ClaimForDispatch selects the correct bin operation based on remaining UOP
// and executes it atomically. Used by all dispatch paths that claim bins.
//
//   - remainingUOP == nil: plain claim (no manifest change)
//   - *remainingUOP <= 0: clear manifest + claim (depleted; <= 0 covers the
//     SME-lock-permitted overpack washout where the captured count
//     exceeded the tracked count, landing the bin negative)
//   - *remainingUOP > 0: sync UOP + claim (partial consumption)
//
// Reserve-then-confirm. The race now resolves at Acquire (unique index
// on reservations.bin_id WHERE state IN ('pending','confirmed')) rather than
// at the SQL CAS claimed_by IS NULL. Sequence:
//  1. Acquire a pending reservation — unique index makes this exactly-one-winner.
//  2. claimAndConfirm: run the claim SQL (ClearAndClaim / SyncUOPAndClaim / ClaimBin,
//     demoted-CAS guard requires reservation EXISTS) AND Confirm the reservation
//     (pending → confirmed) in ONE transaction — both or neither. This atomicity
//     is what closes the claim/confirm wedge (see docs/reservations.md).
//  3. On ANY failure after Acquire → Release (best-effort; Expire is the backstop).
func (s *BinManifestService) ClaimForDispatch(binID, orderID int64, remainingUOP *int) error {
	if err := reservations.Acquire(s.db, orderID, binID, "ClaimForDispatch"); err != nil {
		return err // ErrReservationConflict or transient DB error — both surface as codeClaimFailed
	}
	if err := s.claimAndConfirm(binID, orderID, remainingUOP); err != nil {
		if rErr := reservations.Release(s.db, orderID, binID); rErr != nil {
			log.Printf("dispatch: reservation-release failed (ClaimForDispatch rollback) order=%d bin=%d: %v", orderID, binID, rErr)
		}
		return err
	}
	return nil
}

// claimUnderReservationTx runs the correct hard-claim UPDATE for the bin's
// remainingUOP disposition inside tx — the three-flavored primitive:
//
//   - remainingUOP == nil: plain claim (no manifest change)
//   - *remainingUOP <= 0:  clear manifest + claim (depleted)
//   - *remainingUOP > 0:   sync UOP + claim (partial consumption)
//
// All three carry the same demoted-CAS + owner-idempotent seatbelt. It does NOT
// confirm the reservation — callers pair it with reservations.Confirm in the SAME
// tx (see claimAndConfirm) so the claim and the pending→confirmed flip commit
// atomically.
func (s *BinManifestService) claimUnderReservationTx(tx *sql.Tx, binID, orderID int64, remainingUOP *int) error {
	switch {
	case remainingUOP != nil && *remainingUOP <= 0:
		return s.clearAndClaimTx(tx, binID, orderID)
	case remainingUOP != nil:
		return s.syncUOPAndClaimTx(tx, binID, orderID, *remainingUOP)
	default:
		return bins.ClaimTx(tx, binID, orderID)
	}
}

// claimAndConfirm hard-claims the bin (per its remainingUOP disposition) AND flips
// its reservation pending→confirmed in ONE transaction — both or neither. This
// atomicity is what the wedge fix requires: the pre-fix code committed the claim
// and then confirmed as a SEPARATE statement, so a transient DB error / core
// restart between them left the bin claimed_by=order with the reservation still
// pending. Combined with the owner-idempotent claim CAS, this makes the half-state
// unreachable on the happy path and self-healing on retry if one ever exists.
func (s *BinManifestService) claimAndConfirm(binID, orderID int64, remainingUOP *int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if err := s.claimUnderReservationTx(tx, binID, orderID, remainingUOP); err != nil {
		return err
	}
	if err := reservations.Confirm(tx, orderID, binID); err != nil {
		return err
	}
	return tx.Commit()
}

// ConfirmClaim commits an ALREADY-RESERVED bin to a hard claim, then confirms the
// reservation (pending → confirmed). It is the apply-as-confirm half of the
// plan-time split: the pending reservation was placed earlier by reserveComplexPlan,
// so ConfirmClaim does NOT Acquire — it runs the same demoted-CAS claim SQL as
// ClaimForDispatch (seatbelt: claimed_by IS NULL AND EXISTS a pending reservation)
// and then Confirms.
//
// If the claim SQL affects 0 rows — the pending reservation vanished (reaped
// between reserve and confirm) or the bin was claimed by another order — it
// returns the claim error so the caller surfaces claim_failed and requeues. It
// never re-acquires or proceeds without a reservation. Unlike ClaimForDispatch it
// does NOT release on failure: the reservation belongs to the plan-time reserve
// step, and the caller's reconcile owns keep/release on the next tick.
//
// The claim and the reservation confirm now land in ONE transaction
// (claimAndConfirm), and the claim CAS is owner-idempotent — so a re-run against a
// bin THIS order already claimed (a prior confirm whose reservation-confirm write
// was lost to a transient error / restart) heals to confirmed instead of wedging
// codeClaimFailed forever.
func (s *BinManifestService) ConfirmClaim(binID, orderID int64, remainingUOP *int) error {
	return s.claimAndConfirm(binID, orderID, remainingUOP)
}

// ConfirmHeldReservation flips a reservation pending→confirmed WITHOUT re-running
// the claim SQL — for the crash-replay case where confirmComplexPlan finds the bin
// already hard-claimed by THIS order but its reservation still pending (the wedge
// half-state). Re-claiming is unnecessary (we already own the bin) and would only
// risk a false 0-rows failure, so the reconcile confirms the reservation directly.
// Idempotent (reservations.Confirm no-ops an already-confirmed row).
func (s *BinManifestService) ConfirmHeldReservation(orderID, binID int64) error {
	return reservations.Confirm(s.db, orderID, binID)
}

// ReserveForDispatch places a pending reservation on binID for orderID — the
// plan-time soft hold the reserve/reconcile acquires before it can confirm.
// Returns reservations.ErrReservationConflict when an active reservation already
// exists on the bin (the caller treats it as a lost race and retries next tick).
// The hold has no expiry: reaping keys on the owning order's liveness, so a held
// partial survives across ticks until the order sources or terminalizes
// (demand is operator-driven, never aged out).
func (s *BinManifestService) ReserveForDispatch(binID, orderID int64) error {
	return reservations.Acquire(s.db, orderID, binID, "reserveComplexPlan")
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
// kind is the wire-shape disposition kind that the operator picked. It only
// affects the audit op tag for the zero path: DispositionReleaseUnderpack
// writes OpReleasedUnderpack, anything else (including the zero value) writes
// OpReleasedEmpty. The positive path is always OpReleasedPartial regardless
// of kind. Pass "" for legacy / non-disposition-aware callers.
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
func (s *BinManifestService) SyncOrClearForReleased(binID, orderID int64, remainingUOP *int, kind protocol.UOPDispositionKind, actor string) error {
	return s.syncOrClearForReleased(binID, orderID, remainingUOP, kind, actor, false)
}

// SyncOrClearForReleasedNoOwner is the source-node-fallback variant of
// SyncOrClearForReleased. Identical routing (nil → no-op, 0 → clear,
// >0 → sync UOP), identical audit, but the SQL guard drops the
// claimed_by check — used by HandleOrderRelease when order.BinID is nil
// and we've located the bin by source-node lookup instead.
//
// Why no claim guard: the bin we're targeting wasn't claimed by this
// order (ApplyComplexPlan missed it at creation time, which is the bug
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
	return s.syncOrClearForReleased(binID, orderID, remainingUOP, "", actor, true)
}

// syncOrClearForReleased is the shared body of the two exported variants.
// sourceNodeFallback toggles the claimed_by SQL guard, audit-op constants,
// legacy-tag suffixes, and error-message strings. kind is honored only for
// the owner path (sourceNodeFallback=false); the fallback path passes ""
// and never emits an underpack tag.
func (s *BinManifestService) syncOrClearForReleased(binID, orderID int64, remainingUOP *int, kind protocol.UOPDispositionKind, actor string, sourceNodeFallback bool) error {
	if remainingUOP == nil {
		return nil
	}
	if actor == "" {
		actor = "system"
	}
	// Defense in depth: Edge's computeReleaseRemainingUOP guards against
	// non-positive values reaching this branch, but a direct Core caller
	// (test, automation, future bypass) could still hand us a negative
	// pointer. Reject loudly rather than corrupt the bin row.
	if *remainingUOP < 0 {
		return fmt.Errorf("remainingUOP must be nil, 0, or positive; got %d", *remainingUOP)
	}

	// Per-variant strings — these are the differences enumerated in
	// bin-manifest-dedup-analysis.md.
	errSuffix := ""
	tagSuffix := ""
	finalMsgSuffix := ""
	sourceLabel := "service/bin_manifest.go:SyncOrClearForReleased"
	notFoundErr := fmt.Errorf("bin %d not claimed by order %d (or locked)", binID, orderID)
	if sourceNodeFallback {
		errSuffix = " (no-owner fallback)"
		tagSuffix = "_fallback"
		finalMsgSuffix = " (source-node fallback)"
		sourceLabel = "service/bin_manifest.go:SyncOrClearForReleasedNoOwner"
		notFoundErr = fmt.Errorf("bin %d not found or locked (no-owner fallback)", binID)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	before, err := readBinUOPInTx(tx, binID)
	if err != nil {
		return err
	}
	// Capture the part code before the zero-path clears payload_code, so the
	// release audit row names the part (the discrepancy view reads it).
	// Best-effort: a missing bin surfaces below via the UPDATE's 0-rows guard.
	var releasedPayloadCode string
	_ = tx.QueryRow(`SELECT COALESCE(payload_code, '') FROM bins WHERE id=$1`, binID).Scan(&releasedPayloadCode)

	if *remainingUOP == 0 {
		// Clear manifest, preserve claim (if applicable). The claimed_by guard
		// is added only for the owner variant.
		clearSQL := `
			UPDATE bins SET
				payload_code='', manifest=NULL, uop_remaining=0,
				manifest_confirmed=false, loaded_at=NULL,
				updated_at=NOW()
			WHERE id=$1 AND locked=false`
		var res sql.Result
		if sourceNodeFallback {
			res, err = tx.Exec(clearSQL, binID)
		} else {
			res, err = tx.Exec(clearSQL+` AND claimed_by=$2`, binID, orderID)
		}
		if err != nil {
			return fmt.Errorf("clear manifest for released bin %d%s: %w", binID, errSuffix, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return notFoundErr
		}
		if _, err := bumpEpoch(tx, binID); err != nil {
			return err
		}
		op := audit.OpReleasedEmpty
		legacyTag := "released_empty"
		switch {
		case sourceNodeFallback:
			op = audit.OpReleasedEmptyFallback
		case kind == protocol.DispositionReleaseUnderpack:
			op = audit.OpReleasedUnderpack
			legacyTag = "released_underpack"
		}
		uopCtx, err := resolveBinUOPContext(tx, binID, nil)
		if err != nil {
			return err
		}
		if err := audit.AppendBinUOP(tx, binID, before, 0,
			op, sourceLabel,
			&orderID, releasedPayloadCode, actor, uopCtx); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit clear-for-released bin %d%s: %w", binID, errSuffix, err)
		}
		s.db.AppendAudit("bin", binID, legacyTag+tagSuffix,
			"", fmt.Sprintf("order=%d%s", orderID, finalMsgSuffix), actor)
		return nil
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
	syncSQL := `
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
		WHERE id=$2 AND locked=false`
	var res sql.Result
	if sourceNodeFallback {
		res, err = tx.Exec(syncSQL, *remainingUOP, binID)
	} else {
		res, err = tx.Exec(syncSQL+` AND claimed_by=$3`, *remainingUOP, binID, orderID)
	}
	if err != nil {
		return fmt.Errorf("sync UOP for released bin %d%s: %w", binID, errSuffix, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return notFoundErr
	}
	if _, err := bumpEpoch(tx, binID); err != nil {
		return err
	}
	op := audit.OpReleasedPartial
	if sourceNodeFallback {
		op = audit.OpReleasedPartialFallback
	}
	uopCtx, err := resolveBinUOPContext(tx, binID, nil)
	if err != nil {
		return err
	}
	if err := audit.AppendBinUOP(tx, binID, before, *remainingUOP,
		op, sourceLabel,
		&orderID, releasedPayloadCode, actor, uopCtx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sync-for-released bin %d%s: %w", binID, errSuffix, err)
	}
	s.db.AppendAudit("bin", binID, "released_partial"+tagSuffix,
		"", fmt.Sprintf("uop=%d order=%d%s", *remainingUOP, orderID, finalMsgSuffix), actor)
	return nil
}

// AuditReleaseOverride records operator-override observations for a
// release-time disposition. Compares the operator-submitted values
// (Count / Captures) against the system-suggested baseline
// (CountSuggested / CapturesSuggested) and writes one bin_uop_audit
// row per divergence.
//
// Phase 0b. Independent of the manifest-sync flow: the override audit
// captures *what the operator decided*, regardless of whether
// downstream sync succeeds or fails. Called from HandleOrderRelease
// before SyncOrClearForReleased.
//
// Behavior matrix:
//
//   - disposition == nil → no-op (legacy Edge clients that don't ship
//     the new shape).
//   - Kind == DispositionReleasePartial: if CountSuggested is nil, no
//     baseline to compare against (legacy UI that hasn't been updated).
//     If *CountSuggested == Count, no override. Otherwise one row.
//   - Kind == DispositionPullParts: for every part where
//     CapturesSuggested[k] != Captures[k], one row. Parts present only
//     in one map count as a divergence (operator added or skipped a
//     part the system listed). When CapturesSuggested is nil/empty no
//     baseline exists and nothing is written.
//   - Other kinds (DispositionReleaseEmpty): no-op. RELEASE EMPTY is
//     by definition "system and operator agree the bin is empty";
//     no override semantic.
//
// Each row's metadata column carries the disposition kind plus the
// full suggested/operator maps so a single row contains the full
// release context (forensics doesn't need to reconstruct from sibling
// rows).
func (s *BinManifestService) AuditReleaseOverride(binID, orderID int64, disposition *protocol.UOPDisposition, actor string) error {
	if disposition == nil {
		return nil
	}
	if actor == "" {
		actor = "system"
	}

	switch disposition.Kind {
	case protocol.DispositionReleasePartial:
		if disposition.CountSuggested == nil {
			return nil
		}
		suggested := *disposition.CountSuggested
		operator := disposition.Count
		if suggested == operator {
			return nil
		}
		meta, err := json.Marshal(map[string]any{
			"kind":           string(disposition.Kind),
			"auto_count":     suggested,
			"operator_count": operator,
		})
		if err != nil {
			return fmt.Errorf("marshal override metadata bin=%d: %w", binID, err)
		}
		return audit.AppendBinUOPOverride(s.db, binID, suggested, operator,
			audit.OpOperatorOverrideReleasePartial,
			"service/bin_manifest.go:AuditReleaseOverride",
			&orderID, "", actor, meta)

	case protocol.DispositionPullParts:
		if len(disposition.CapturesSuggested) == 0 {
			return nil
		}
		// Stable order so audit rows / tests don't depend on map iteration.
		parts := make([]string, 0, len(disposition.CapturesSuggested)+len(disposition.Captures))
		seen := make(map[string]struct{}, len(parts))
		add := func(k string) {
			if _, ok := seen[k]; ok {
				return
			}
			seen[k] = struct{}{}
			parts = append(parts, k)
		}
		for k := range disposition.CapturesSuggested {
			add(k)
		}
		for k := range disposition.Captures {
			add(k)
		}
		sort.Strings(parts)

		for _, part := range parts {
			suggested := disposition.CapturesSuggested[part]
			operator := disposition.Captures[part]
			if suggested == operator {
				continue
			}
			meta, err := json.Marshal(map[string]any{
				"kind":              string(disposition.Kind),
				"part_number":       part,
				"auto_qty":          suggested,
				"operator_qty":      operator,
				"auto_captures":     disposition.CapturesSuggested,
				"operator_captures": disposition.Captures,
			})
			if err != nil {
				return fmt.Errorf("marshal override metadata bin=%d part=%q: %w", binID, part, err)
			}
			if err := audit.AppendBinUOPOverride(s.db, binID, suggested, operator,
				audit.OpOperatorOverridePullParts,
				"service/bin_manifest.go:AuditReleaseOverride",
				&orderID, part, actor, meta); err != nil {
				return err
			}
		}
		return nil

	default:
		return nil
	}
}
