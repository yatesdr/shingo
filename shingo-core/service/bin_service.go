package service

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/store"
	"shingocore/store/audit"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
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
func (s *BinService) Create(b *bins.Bin) error {
	if b.NodeID != nil {
		if err := s.ensurePhysicalNodeEmpty(*b.NodeID, 1); err != nil {
			return err
		}
	}
	return s.db.CreateBin(b)
}

// CreateBatch inserts `count` bins sharing a template (bin type, node,
// status, description), all-or-nothing in a single transaction. Label
// handling (see batchLabels): with count==1 the entered label is used
// verbatim; with count>1 a trailing digit run is incremented preserving
// zero-pad width, and a label with no trailing digits falls back to the
// historical label+NNNN scheme starting at 0001. Physical nodes may only
// receive one bin; synthetic nodes may receive many.
func (s *BinService) CreateBatch(template bins.Bin, label string, count int) error {
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

	labels := batchLabels(label, count)

	// Friendly pre-check: report every colliding label up front ("created
	// none") instead of failing on the first duplicate with a raw constraint
	// error. The partial unique index plus the transaction below are the
	// actual atomicity guarantee against a concurrent racer; this is UX only.
	existing, err := s.existingLabels(labels)
	if err != nil {
		return fmt.Errorf("check existing labels: %w", err)
	}
	if len(existing) > 0 {
		return fmt.Errorf("label(s) already exist, created none: %s", strings.Join(existing, ", "))
	}

	var nodeArg any
	if template.NodeID != nil {
		nodeArg = *template.NodeID
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	for _, lbl := range labels {
		if _, err := tx.Exec(
			`INSERT INTO bins (bin_type_id, label, description, node_id, status) VALUES ($1, $2, $3, $4, $5)`,
			template.BinTypeID, lbl, template.Description, nodeArg, template.Status,
		); err != nil {
			return fmt.Errorf("create bin %q: %w", lbl, err)
		}
	}
	return tx.Commit()
}

// trailingDigits captures a label's leading text and its final run of digits.
var trailingDigits = regexp.MustCompile(`^(.*?)(\d+)$`)

// batchLabels expands a starting label into `count` labels. count<=1 yields
// the label verbatim. For count>1 a trailing digit run is incremented from
// its parsed value, preserving zero-pad width — %0*d widens automatically on
// carry (e.g. "BIN98" → BIN98, BIN99, BIN100). A label with no trailing
// digits (or an unparseable run) falls back to label+NNNN starting at 0001.
func batchLabels(label string, count int) []string {
	if count <= 1 {
		return []string{label}
	}
	labels := make([]string, 0, count)
	if m := trailingDigits.FindStringSubmatch(label); m != nil {
		if start, err := strconv.Atoi(m[2]); err == nil {
			head, width := m[1], len(m[2])
			for i := 0; i < count; i++ {
				labels = append(labels, head+fmt.Sprintf("%0*d", width, start+i))
			}
			return labels
		}
	}
	for i := 0; i < count; i++ {
		labels = append(labels, label+fmt.Sprintf("%04d", i+1))
	}
	return labels
}

// existingLabels returns the subset of the given labels already present in the
// bins table, matching the idx_bins_label_unique predicate (non-empty labels).
func (s *BinService) existingLabels(labels []string) ([]string, error) {
	placeholders := make([]string, 0, len(labels))
	args := make([]any, 0, len(labels))
	for _, l := range labels {
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)+1))
		args = append(args, l)
	}
	rows, err := s.db.Query(
		`SELECT label FROM bins WHERE label != '' AND label IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		found = append(found, l)
	}
	return found, rows.Err()
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
//
// Validation is intentionally omitted: operators occasionally need to set
// off-spec states during incident recovery. domain.BinStatus.CanTransitionTo
// is available for callers (UI, future recovery flows) that want to gate
// transitions before invoking this.
func (s *BinService) ChangeStatus(binID int64, status domain.BinStatus) error {
	return s.db.UpdateBinStatus(binID, status)
}

// Release moves a staged bin back to the available state.
func (s *BinService) Release(binID int64) error {
	return s.db.ReleaseStagedBin(binID)
}

// Stage marks a bin as staged with no TTL. Operator-driven path used by
// the bin Actions panel toggle; arrivals go through ApplyArrival which
// also writes staged_expires_at from the destination's staging policy.
func (s *BinService) Stage(binID int64) error {
	return s.db.StageBin(binID, nil)
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

// LoadPayload validates that the payload code exists, that the payload's
// bin-type allow-list (if any) admits this bin's type, and sets the bin's
// manifest from the payload template. uopOverride of 0 uses the template's
// UOP capacity. Item 19: routes through BinManifestService.SetFromTemplate
// so the operator load-payload action audits via bin_uop_audit.
//
// Compat semantics mirror PayloadBinTypeAdvisoryClause used by FindSourceFIFO
// / FindEmptyCompatible: payload_bin_types is treated as an allow-list when
// populated, ignored when empty.
//
// Returns the new delta_epoch from SetFromTemplate so handlers shipping
// the bin row back to Edge can include it on the wire (Edge needs the
// fresh epoch before its next BinUOPDelta — see protocol/payloads.go).
func (s *BinService) LoadPayload(binID int64, payloadCode string, uopOverride int) (int64, error) {
	if payloadCode == "" {
		return 0, fmt.Errorf("payload_code is required")
	}
	p, err := s.db.GetPayloadByCode(payloadCode)
	if err != nil {
		return 0, fmt.Errorf("payload template %q not found", payloadCode)
	}
	b, err := s.db.GetBin(binID)
	if err != nil {
		return 0, fmt.Errorf("bin not found")
	}
	compat, err := s.db.ListBinTypesForPayload(p.ID)
	if err != nil {
		return 0, fmt.Errorf("check payload bin-type compat: %w", err)
	}
	if len(compat) > 0 {
		ok := false
		for _, bt := range compat {
			if bt.ID == b.BinTypeID {
				ok = true
				break
			}
		}
		if !ok {
			codes := make([]string, len(compat))
			for i, bt := range compat {
				codes[i] = bt.Code
			}
			return 0, fmt.Errorf("payload %q not compatible with bin type %q (allowed: %v)", payloadCode, b.BinTypeCode, codes)
		}
	}
	return s.manifest.SetFromTemplate(binID, payloadCode, uopOverride)
}

// --- Movement -------------------------------------------------------------

// MoveResult describes the destination a bin was moved to so callers can
// write audit entries and emit events without re-fetching the node.
type MoveResult struct {
	DestNode *nodes.Node
}

// Move relocates a bin to a new node. Validates:
//   - bin is not already at the destination
//   - destination node exists
//   - destination is either synthetic or empty
func (s *BinService) Move(b *bins.Bin, toNodeID int64) (*MoveResult, error) {
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
	// A manual Move bypasses the arrival paths (ApplyArrival / recovery) that
	// re-derive staging, so a bin staged at a lineside node would stay staged
	// after relocating to storage. Mirror the arrival behavior: clear staging
	// in the same tx when a staged bin lands on a storage slot.
	clearStaging := b.Status == domain.BinStatusStaged && s.destIsStorageSlot(destNode)
	if err := s.db.MoveBinClearingStaging(b.ID, toNodeID, clearStaging); err != nil {
		return nil, err
	}
	return &MoveResult{DestNode: destNode}, nil
}

// destIsStorageSlot reports whether a node is a storage slot — a LANE/NGRP
// itself or a direct child of one. Mirrors the engine-private
// engine.isStorageSlot; the staging-clear on Move needs the same
// classification at the service layer. Keep the two definitions in sync.
func (s *BinService) destIsStorageSlot(node *nodes.Node) bool {
	if node.NodeTypeCode == protocol.NodeClassLANE || node.NodeTypeCode == protocol.NodeClassNGRP {
		return true
	}
	if node.ParentID == nil {
		return false
	}
	parent, err := s.db.GetNode(*node.ParentID)
	if err != nil {
		return false
	}
	return parent.NodeTypeCode == protocol.NodeClassLANE || parent.NodeTypeCode == protocol.NodeClassNGRP
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
//
// Item 19 of the bin-as-truth refactor: the count + bin_uop_audit
// insert run in one transaction with op=OpCycleCount, before/suggested
// = the pre-count uop_remaining (system's expected), after =
// actualUOP. Without this row the Item 10 audit timeline UI would be
// silent for cycle counts even though they're a primary
// operator-vs-system divergence signal SCO uses to spot drift.
func (s *BinService) RecordCount(b *bins.Bin, actualUOP int, actor string) (*CountResult, error) {
	if b.PayloadCode == "" {
		return nil, fmt.Errorf("cannot validate UOP capacity — bin %d has no payload", b.ID)
	}
	pl, err := s.db.GetPayloadByCode(b.PayloadCode)
	if err != nil {
		return nil, fmt.Errorf("lookup payload %q: %w", b.PayloadCode, err)
	}
	if pl.UOPCapacity <= 0 {
		return nil, fmt.Errorf("cannot validate UOP capacity — payload %q has no UOP capacity set", b.PayloadCode)
	}
	if actualUOP < 0 || actualUOP > pl.UOPCapacity {
		return nil, fmt.Errorf("actual UOP must be between 0 and %d", pl.UOPCapacity)
	}

	expected := b.UOPRemaining

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := bins.RecordCount(tx, b.ID, actualUOP, actor); err != nil {
		return nil, fmt.Errorf("record count bin %d: %w", b.ID, err)
	}
	expectedCopy := expected
	uopCtx, err := resolveBinUOPContext(tx, b.ID, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve audit context bin %d: %w", b.ID, err)
	}
	if err := audit.AppendBinUOP(tx, b.ID, &expectedCopy, actualUOP,
		audit.OpCycleCount, "service/bin_service.go:RecordCount",
		nil, b.PayloadCode, actor, uopCtx); err != nil {
		return nil, fmt.Errorf("audit cycle count bin %d: %w", b.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit cycle count bin %d: %w", b.ID, err)
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
// *bins.Bin in place before calling UpdateBin.
func (s *BinService) Update(b *bins.Bin, label, description *string, binTypeID *int64) error {
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
func (s *BinService) GetBin(id int64) (*bins.Bin, error) {
	return s.db.GetBin(id)
}

// ListBins returns every bin in the store. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.2).
func (s *BinService) ListBins() ([]*bins.Bin, error) {
	return s.db.ListBins()
}

// Delete removes a bin row outright. Reserved for admin/DBA recovery
// paths where the caller has guaranteed no FK relationships point at
// the bin row. Operator-facing flows go through Retire instead — the
// admin UI's "Retire" button calls Retire; raw DELETE is no longer
// reachable from /bins.
func (s *BinService) Delete(id int64) error {
	return s.db.DeleteBin(id)
}

// Retire marks the bin retired and vacates its node assignment. This
// is the operator-driven replacement for the old "Delete" admin
// action, which raised FK violations on any bin with history
// (claimed_by, order rows, audit) and stranded operators trying to
// remove a physically out-of-service carrier from production.
//
// The store layer's bins.Retire is a single UPDATE — status='retired'
// AND node_id=NULL — so the bin disappears from operational queries
// (CountByAllNodes filters node_id IS NOT NULL; ListByNode and
// ListByClaim filter status != 'retired'; FindSourceFIFO,
// FindEmptyCompatible, FindEmptyCompatibleInGroup all exclude
// status='retired' or require status='available'). Audit/history
// rows pointing at bins.id remain intact for downstream reporting.
//
// Verification gating this design: see
// github.com/.../round3-item-b-verification.md — every *bin.NodeID
// deref in shingo-core/ is either nil-guarded at the site or
// reachable only through queries that already filter
// status != 'retired', so the node_id=NULL state cannot trigger a
// panic.
func (s *BinService) Retire(id int64) error {
	return s.db.RetireBin(id)
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
func (s *BinService) CreateBinType(bt *bins.BinType) error {
	return s.db.CreateBinType(bt)
}

// GetBinType loads a bin type by ID. Absorbed from engine_db_methods.go
// as part of the www-handler service migration (PR 3a.2).
func (s *BinService) GetBinType(id int64) (*bins.BinType, error) {
	return s.db.GetBinType(id)
}

// UpdateBinType persists changes to a bin type row. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.2).
func (s *BinService) UpdateBinType(bt *bins.BinType) error {
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
func (s *BinService) ListBinTypes() ([]*bins.BinType, error) {
	return s.db.ListBinTypes()
}

// GetBinTypeByCode fetches a bin type by its unique code.
func (s *BinService) GetBinTypeByCode(code string) (*bins.BinType, error) {
	return s.db.GetBinTypeByCode(code)
}

// GetEffectiveBinTypesForNode returns the bin types valid at a node based on
// its bin_type_mode property. An empty result means no restriction (mode="all"
// or unconfigured) — callers should treat nil/empty as "allow everything."
func (s *BinService) GetEffectiveBinTypesForNode(nodeID int64) ([]*bins.BinType, error) {
	return s.db.GetEffectiveBinTypes(nodeID)
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
func (s *BinService) GetByLabel(label string) (*bins.Bin, error) {
	return s.db.GetBinByLabel(label)
}

// GetManifest returns the confirmed manifest items currently loaded
// on a bin. Absorbed from engine_db_methods.go as part of the Phase
// 3a closeout (PR 3a.6).
func (s *BinService) GetManifest(binID int64) (*bins.Manifest, error) {
	return s.db.GetBinManifest(binID)
}

// ── Phase 6.1 additions ────────────────────────────────────────────

// ApplyArrival moves a claimed bin to its destination, unclaims it,
// and updates its staging state inside a single transaction. Owns the
// transaction directly; *store.DB is just the connection holder.
//
// Phase 6.1 introduced this method as a thin delegate; Phase 6.4a
// moved the orchestration body in from the (now-deleted) outer
// store/completion.go::ApplyBinArrival.
// Returns evicted=true when the destination already recorded a different
// non-retired bin and that stale ghost was evicted to _TRANSIT (see below);
// callers surface that as an operator alert. A normal arrival onto an empty
// slot returns evicted=false and does no extra node lookup.
func (s *BinService) ApplyArrival(binID, toNodeID int64, staged bool, expiresAt *time.Time) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Stale-ghost reconciliation, shared with ApplyMultiBinArrival via
	// EvictStaleGhostsTx so the single-bin and multi-bin arrival paths cannot
	// drift. A completed delivery is physical proof the slot was empty, so a
	// different bin still recorded at this destination is a stale ghost — evicted
	// to _TRANSIT (unclaimed + anomaly_at) so it surfaces in ListAnomalies and is
	// recoverable via RecoverTransitAnomaly; the newcomer is never rejected.
	// Synthetic nodes are exempt (handled inside the helper).
	evictedGhosts, err := s.db.EvictStaleGhostsTx(tx, toNodeID, binID)
	if err != nil {
		return false, err
	}
	evicted := len(evictedGhosts) > 0

	if _, err := tx.Exec(`UPDATE bins SET node_id=$1, updated_at=NOW() WHERE id=$2`, toNodeID, binID); err != nil {
		return false, fmt.Errorf("move bin: %w", err)
	}
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE id=$1`, binID); err != nil {
		return false, fmt.Errorf("unclaim bin: %w", err)
	}
	// A bin's reservation lives exactly as long as its claim: release it in the
	// same tx that clears claimed_by, so the delivered bin frees for
	// re-reservation now rather than lingering (blocked) until the owning order's
	// terminal transition.
	if err := reservations.ReleaseByBin(tx, binID); err != nil {
		return false, fmt.Errorf("release reservation on arrival bin %d: %w", binID, err)
	}
	// Release the destination slot's dispatch-time claim (the store dual of the
	// bin claim): the bin has arrived, so the dropoff claim is fulfilled. Atomic
	// with the arrival; a no-op for LINE deliveries (never slot-claimed).
	if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE id=$1`, toNodeID); err != nil {
		return false, fmt.Errorf("release destination slot claim node %d: %w", toNodeID, err)
	}
	// ...and its slot RESERVATION, in the SAME tx (the slot dual of the bin
	// ReleaseByBin above): a slot's reservation lives exactly as long as its
	// hard claim, so the slot frees for re-reservation at delivery. No-op for a
	// LINE delivery (never slot-reserved).
	if err := reservations.ReleaseByNode(tx, toNodeID); err != nil {
		return false, fmt.Errorf("release slot reservation on arrival node %d: %w", toNodeID, err)
	}
	if staged {
		// nullableTime: pass UTC time or nil, mirroring helpers.NullableTime
		// from the (internal) store helpers package — inlined here because
		// internal/ blocks cross-package imports.
		var expiresVal any
		if expiresAt != nil {
			expiresVal = expiresAt.UTC()
		}
		if _, err := tx.Exec(`UPDATE bins SET status='staged', staged_at=NOW(), staged_expires_at=$1, updated_at=NOW() WHERE id=$2`,
			expiresVal, binID); err != nil {
			return false, fmt.Errorf("stage bin: %w", err)
		}
	} else {
		if _, err := tx.Exec(`UPDATE bins SET status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=NOW() WHERE id=$1`, binID); err != nil {
			return false, fmt.Errorf("set available bin: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit arrival bin %d: %w", binID, err)
	}
	return evicted, nil
}

// ── Phase 1 of bin-transit-state: in-transit lifecycle ─────────────

// MoveToTransit moves a bin to the synthetic `_TRANSIT` node, marking
// it as physically in flight while preserving its `claimed_by` so the
// owning order still owns it. The source slot is freed for new
// placements as soon as this commits.
//
// Idempotent: if the bin is already at `_TRANSIT`, returns nil with no
// state change. This handles vendor pickup-event retries.
//
// Does NOT touch:
//   - `claimed_by`: the order still owns this bin until ApplyArrival
//     unclaims it at the destination (or until FailOrderAtomic /
//     CancelOrderAtomic clear it on order termination, which is exactly
//     the signal that creates the transit anomaly).
//   - `status`: location and readiness are orthogonal. A bin can be
//     `staged` at source, in transit, then `available` at destination —
//     the status transition belongs to ApplyArrival.
func (s *BinService) MoveToTransit(binID int64) error {
	transitNode, err := s.db.GetNodeByName(domain.TransitNodeName)
	if err != nil {
		return fmt.Errorf("lookup transit node %q: %w", domain.TransitNodeName, err)
	}
	if err := s.db.MoveBinToTransit(binID, transitNode.ID); err != nil {
		return fmt.Errorf("move bin %d to transit: %w", binID, err)
	}
	return nil
}

// MarkAnomaly stamps `bins.anomaly_at = NOW()` for the given bin. Called
// by the failure-completion path when an order terminates while one of
// its bins is still at `_TRANSIT`. Idempotent — repeated calls update
// the timestamp; that's fine because the anomaly state is "still
// unresolved" rather than "happened at exactly this moment."
func (s *BinService) MarkAnomaly(binID int64) error {
	if err := s.db.MarkBinAnomaly(binID); err != nil {
		return fmt.Errorf("mark bin %d anomaly: %w", binID, err)
	}
	return nil
}

// ListAnomalies returns bins parked at _TRANSIT with no live order
// claim — the binary anomaly signal. Wraps store/bins.ListAnomalousTransitBins.
func (s *BinService) ListAnomalies() ([]*bins.Bin, error) {
	return s.db.ListAnomalousTransitBins()
}

// ClearAnomaly clears `anomaly_at`. Called by the operator recovery
// action after a bin has been physically located and reassigned to a
// real node.
func (s *BinService) ClearAnomaly(binID int64) error {
	if err := s.db.ClearBinAnomaly(binID); err != nil {
		return fmt.Errorf("clear bin %d anomaly: %w", binID, err)
	}
	return nil
}

// RecoverTransitAnomaly is the operator's "I found this bin and put it
// at node X" action: moves the bin out of _TRANSIT to the chosen real
// node and clears the anomaly flag. Validates that the destination is
// physical (not _TRANSIT, not synthetic) and currently empty.
//
// actor identifies the operator for the recovery_actions audit row.
//
// Sequencing matches sibling RecoveryService recovery actions: mutate
// first, then record the recovery_actions row. If the audit write fails
// the bin move is durable but the error is returned so the operator sees
// the failure.
func (s *BinService) RecoverTransitAnomaly(binID, toNodeID int64, actor string) error {
	if actor == "" {
		return fmt.Errorf("actor is required for recovery")
	}
	dest, err := s.db.GetNode(toNodeID)
	if err != nil {
		return fmt.Errorf("destination node %d not found: %w", toNodeID, err)
	}
	if dest.Name == "_TRANSIT" {
		return fmt.Errorf("recovery destination cannot be _TRANSIT")
	}
	if dest.IsSynthetic {
		return fmt.Errorf("recovery destination must be a physical node, got synthetic %q", dest.Name)
	}
	if err := s.ensurePhysicalNodeEmpty(toNodeID, 1); err != nil {
		return err
	}

	if err := s.db.RecoverBinToNode(binID, toNodeID); err != nil {
		return fmt.Errorf("move bin to recovery node: %w", err)
	}
	if err := s.db.RecordRecoveryAction(
		"transit_anomaly_recover", "bin", binID,
		fmt.Sprintf("recovered to node %s", dest.Name), actor); err != nil {
		return fmt.Errorf("record recovery action: %w", err)
	}
	return nil
}
