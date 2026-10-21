package service

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"

	"shingocore/domain"
	"shingocore/store/audit"
	"shingocore/store/bins"
	"shingocore/store/messaging"
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
	db       BinManifestStore
	announce messaging.EpochAnnounce
	// binChanged is called after a station door's write to a bin commits
	// (bin_changed.go).
	binChanged atomic.Pointer[func(binID int64)]
}

// EpochAnnounce is re-exported here so call sites that only import service can
// name it. The type lives in store/messaging because the delta applier needs
// the same addressing and must not import this package.
type EpochAnnounce = messaging.EpochAnnounce

func NewBinManifestService(db BinManifestStore, announce EpochAnnounce) *BinManifestService {
	return &BinManifestService{db: db, announce: announce}
}

// readBinUOPInTx returns the bin's current uop_remaining inside a tx,
// for capture as before_uop on a bin_uop_ledger row. Returns nil when the
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

// bumpEpochRaw increments a bin's delta_epoch inside the caller's tx and
// returns the new generation, the bin's count as it stands after the caller's
// own UPDATE, the node the bin is at (empty when it is at none), and where the
// announcement may go (bins.LifecycleAddressSQL).
//
// Not to be called directly — bumpEpoch is the entry point, and the census
// test in store/audit holds that. Bumping without announcing is the defect
// this whole change exists to remove, and the two must not be separable.
//
// The count, the node and the address come back from the SAME statement as the
// bump so the announcement cannot carry a mismatched pair, and addressing it
// costs no statement. Every reset path writes an absolute uop_remaining in the
// UPDATE immediately before this one, inside the same transaction, so the
// value read here is the one that reset just wrote.
func bumpEpochRaw(tx *sql.Tx, binID int64) (epoch int64, remaining int, nodeName string, addr bins.LifecycleAddress, err error) {
	addrDest, address := bins.ScanLifecycleAddress()
	if err := tx.QueryRow(
		`UPDATE bins SET delta_epoch=delta_epoch+1 WHERE id=$1
		 RETURNING delta_epoch, uop_remaining,
		           COALESCE((SELECT n.name FROM nodes n WHERE n.id = bins.node_id), ''),
		           `+bins.LifecycleAddressSQL,
		binID).Scan(append([]any{&epoch, &remaining, &nodeName}, addrDest...)...); err != nil {
		return 0, 0, "", bins.LifecycleAddress{}, fmt.Errorf("bump delta_epoch bin %d: %w", binID, err)
	}
	return epoch, remaining, nodeName, address(), nil
}

// bumpEpoch ends a bin's current life, starts the next one, and tells the
// station holding it — in one transaction, as one act.
//
// Every count-reset/clear path calls this, so "a reset starts a fresh delta
// stream" is structural: a new reset path cannot silently forget it. That
// omission is the failure this guards against — a reset that did not bump left
// late cross-epoch ticks looking same-epoch, so the applier's stale-epoch guard
// could not tell them from live ticks.
//
// The announcement is here for the same reason the bump is. Five reset paths
// bumped; before this, ONE of them told the Edge, and the Edge went on reporting
// counts under a generation that had ended while Core discarded every one —
// half of all production counts at Hopkinsville, continuously. A shared body
// plus a census test already existed for the bump and were not enough, because
// what was missing was never a chokepoint: it was the wire. Putting the wire
// inside the chokepoint is what makes the two impossible to separate.
//
// It is an outbox ROW, not a send, and that distinction is the design. The row
// commits with the bump or rolls back with it; the drainer delivers it after
// the commit by construction. An after-commit send would leave a window where
// the reset lands and the process dies before the notification — an unbounded
// silent stall with nothing recording it, which is this system's signature
// failure and precisely what we are here to fix.
//
// The message carries the count as well as the generation, and that is
// correct: a reset is a DECLARATION. Somebody or some lifecycle event decided
// this carrier now holds this many, and a declaration always carries its
// number. (The repair message in the other direction carries no count, because
// nobody declared anything there — see protocol.BinEpochRefresh.)
//
// by says whether a person declared this reset or Core's own bookkeeping did.
// It is REQUIRED, not defaulted, because it decides whether the station binds
// an empty slot to the carrier this names, and the two reset paths a person
// drives are indistinguishable here from the two a machine drives. Passing it
// is the call site being asked the question; see protocol.Declarer for why it
// is not the audit actor.
//
// Where it goes is the landing's rule for a placement with no order
// (announceLifecycle; Lane 0 item 5): not to a place no station binds at, to
// the one station that claims the seat, to nobody when several do, and to
// every registered Edge (the last arm) when none does. It used to go to every
// Edge, and an Edge that does not model the node parsed it and logged "process
// node not found".
func (s *BinManifestService) bumpEpoch(tx *sql.Tx, binID int64, by protocol.Declarer) (int64, error) {
	epoch, remaining, nodeName, addr, err := bumpEpochRaw(tx, binID)
	if err != nil {
		return 0, err
	}
	if nodeName == "" {
		// The carrier is not at a node, so there is no station modelling it and
		// nothing to tell. It learns the generation when it is next delivered.
		return epoch, nil
	}
	if !s.announce.Wired() {
		// Unwired. Logged rather than swallowed: this is the state the plant was
		// in for months and nothing said so.
		log.Printf("bin_manifest: bin %d bumped to epoch %d at node %s with no announce topic wired — "+
			"the station will keep reporting under the old generation and Core will discard it",
			binID, epoch, nodeName)
		return epoch, nil
	}
	if err := announceLifecycle(s.announce, tx, addr, "epoch", &protocol.UOPAdjustment{
		BinID:        binID,
		CoreNodeName: nodeName,
		NewRemaining: remaining,
		Epoch:        epoch,
		Actor:        by.Actor(),
		AdjustedAt:   time.Now().UTC(),
	}); err != nil {
		return 0, fmt.Errorf("announce epoch %d for bin %d: %w", epoch, binID, err)
	}
	return epoch, nil
}

// resolveBinUOPContext builds the bin_uop_ledger enrichment context inside the caller's
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

// emptiedCarrierSET is what emptying a carrier writes, shared by the three
// statements that do it: ClearForReuseTx, clearAndClaimTx and the zero arm of
// syncOrClearForReleased. Their column lists are pinned by
// TestEmptyingStatements_ColumnLists.
//
// undeclared_carrier_at GOES WITH THE PAYLOAD: the finding is a statement
// about a payload standing in a carrier that may not hold it, and an empty
// carrier holds nothing.
//
// totes_on_cart GOES TO UNKNOWN, not to 0. It counts the totes standing on the
// cart, filled or empty, and what stands on a cart once its contents are gone
// nobody has said: a full built on a base (0) is not a base again once
// emptied. A person declares the number with the CLEAR (DeclareTotesTx), after
// this statement. Pin: TestEmptyingStatements_AFullBuiltOnABaseReadsUnknownOnceEmptied.
const emptiedCarrierSET = `payload_code='', manifest=NULL, uop_remaining=0,
	manifest_confirmed=false, loaded_at=NULL, undeclared_carrier_at=NULL, totes_on_cart=NULL`

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
func (s *BinManifestService) ClearForReuse(binID int64, binTypeID *int64, by protocol.Declarer) (int64, error) {
	return s.ClearForReuseResolved(binID, func(*sql.Tx) (*int64, error) { return binTypeID, nil }, by, nil)
}

// ClearForReuseResolved is ClearForReuse with the type to stamp decided INSIDE
// the clear's transaction, by resolve, against the bin as it stands at that
// moment. The two-stage stamp needs it: the marker a stage-1 CLEAR stamps is
// derived from the cart's current type and may be created by the same
// transaction (bins.ResolveBareStampTx).
//
// layers is the height a person declared with the CLEAR, nil when none was:
// written after the clear and the stamp, in the same transaction
// (DeclareTotesTx). nil adds no statement.
func (s *BinManifestService) ClearForReuseResolved(binID int64, resolve func(*sql.Tx) (*int64, error), by protocol.Declarer, layers *int) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	binTypeID, err := resolve(tx)
	if err != nil {
		return 0, err
	}
	epoch, err := s.ClearForReuseTx(tx, binID, binTypeID, audit.OpClearForReuse, "service/bin_manifest.go:ClearForReuse", by)
	if err != nil {
		return 0, err
	}
	if layers != nil {
		if err := s.DeclareTotesTx(tx, binID, *layers, "service/bin_manifest.go:ClearForReuse"); err != nil {
			return 0, err
		}
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
// a second time, but the station is told the post-clear generation by the
// announcement the bump itself sends, in this same transaction.
//
// This used to say the station learned it "on the bin-state refresh that
// follows the clear". There was no bin-state refresh. Nothing polled.
//
// binTypeID is optional (nil = preserve existing bin_type_id). When
// non-nil, bin_type_id is updated atomically with the manifest clear.
func (s *BinManifestService) ClearForReuseTx(tx *sql.Tx, binID int64, binTypeID *int64, op, source string, by protocol.Declarer) (int64, error) {
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
	//
	// The emptying columns are emptiedCarrierSET's: undeclared_carrier_at goes
	// with the payload in the same statement, which is how the flag stays true
	// without a sweeper, and totes_on_cart goes to unknown.
	if _, err := tx.Exec(`UPDATE bins SET `+emptiedCarrierSET+`,
		bin_type_id=COALESCE($2, bin_type_id), updated_at=NOW()
		WHERE id=$1`, binID, binTypeID); err != nil {
		return 0, fmt.Errorf("clear manifest bin %d: %w", binID, err)
	}
	newEpoch, err := s.bumpEpoch(tx, binID, by)
	if err != nil {
		return 0, err
	}
	uopCtx, err := resolveBinUOPContext(tx, binID, nil)
	if err != nil {
		return 0, err
	}
	// Record from→to bin-type codes in the audit detail so a floating
	// carrier's dunnage history is reconstructable from bin_uop_ledger rows.
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

// DeclareTotesTx writes the height a person declared with the CLEAR: the
// cart's count of totes becomes layers × its carrier type's totes per layer,
// read from the type the cart has NOW, so a call after a stamp reads the
// stamped type (stage 2's SEND ON: the carrier's number, not the bare
// marker's). A type that says no totes per layer gets no write and no ledger
// row. layers 0 declares a base.
//
// Called after the clear, in its transaction: the clear has just written the
// count to unknown (emptiedCarrierSET), so a ledger row's "from" is always
// unknown and is not recorded. One statement, and a ledger row when it wrote.
// Pins: TestBinClear_DeclaresTheHeight.
func (s *BinManifestService) DeclareTotesTx(tx *sql.Tx, binID int64, layers int, source string) error {
	if layers < 0 {
		return fmt.Errorf("declare totes on bin %d: %d layers is not a height", binID, layers)
	}
	var (
		totes, perLayer, uop int
		payloadCode          string
		nodeID               sql.NullInt64
	)
	err := tx.QueryRow(`UPDATE bins b SET totes_on_cart = $2 * bt.totes_per_layer, updated_at = NOW()
		FROM bin_types bt
		WHERE b.id = $1 AND bt.id = b.bin_type_id AND bt.totes_per_layer IS NOT NULL
		RETURNING b.totes_on_cart, bt.totes_per_layer, b.uop_remaining, b.payload_code, b.node_id`,
		binID, layers).Scan(&totes, &perLayer, &uop, &payloadCode, &nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("declare totes on bin %d: %w", binID, err)
	}
	detail, err := json.Marshal(map[string]int{"layers": layers, "totes_per_layer": perLayer, "totes_on_cart": totes})
	if err != nil {
		return err
	}
	ctx := audit.BinUOPContext{Detail: detail}
	if nodeID.Valid {
		ctx.NodeID = &nodeID.Int64
	}
	return audit.AppendBinUOP(tx, binID, &uop, uop, audit.OpTotesDeclared, source, nil, payloadCode, "", ctx)
}

// RebaseAfterEdgeRollbackTx starts the carrier's next count generation because
// the station counting it went backward (SYNTH-round2 S6): the applier saw a
// message at or below the scope's last applied seq whose window ends after the
// last applied one, which a duplicate or a late message cannot produce and a
// restored or reinstalled station does. Returns the new delta_epoch.
//
// THE COUNT IS NOT TOUCHED. Core's number is the one that stands; the bump's
// announcement carries it, the station adopts it under the new generation, and
// its seq stream and running net start over there. Whatever the station
// re-sends under the old generation is then a stale-epoch drop, which is
// recorded. Nobody declared anything, so DeclaredByLifecycle: the station must
// not bind an empty slot to the carrier on the strength of this.
//
// The ledger row (edge_rollback, before == after) is the boundary the bump
// makes, and AppendBinUOP writes its boundary exception; the applier writes
// the edge_rollback exception with the seqs and windows.
func (s *BinManifestService) RebaseAfterEdgeRollbackTx(tx *sql.Tx, binID int64, payloadCode, station string) (int64, error) {
	before, err := readBinUOPInTx(tx, binID)
	if err != nil {
		return 0, err
	}
	if before == nil {
		return 0, fmt.Errorf("rebase after edge rollback: bin %d does not exist", binID)
	}
	newEpoch, err := s.bumpEpoch(tx, binID, protocol.DeclaredByLifecycle)
	if err != nil {
		return 0, err
	}
	uopCtx, err := resolveBinUOPContext(tx, binID, nil)
	if err != nil {
		return 0, err
	}
	uopCtx.Station = station
	if err := audit.AppendBinUOP(tx, binID, before, *before, audit.OpEdgeRollback,
		"service/bin_manifest.go:RebaseAfterEdgeRollback", nil, payloadCode, station, uopCtx); err != nil {
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
// in bin_uop_ledger so the Item 10 audit timeline UI sees the
// freshly-loaded bin's 0→capacity initial fill alongside the
// downstream consume / capture deltas.
//
// uopOverride nil falls back to the template's UOPCapacity. Non-nil
// uopOverride lets callers (produce ingest, partial-fill operator
// loads) record an actual count rather than the template default —
// including an explicit ZERO, the operator's "this carrier is labeled
// but empty" declaration. The previous int-zero sentinel could not
// express that, which is how HK 2026-07-16 ended up with hand-labeled
// empties carrying phantom full-capacity counts that then had to be
// chased with cycle counts.
//
// Returns the new delta_epoch from the underlying SetForProduction
// call so handlers that ship the bin's row to Edge can include it in
// their response.
//
// THE CARRIER RULE READS `by` HERE TOO. BinService.LoadPayload passes
// DeclaredByPerson and is refused an undeclared carrier; the dispatch ingest
// path named in the paragraph above reaches the bin through
// RecordProducedBinFromTemplate with DeclaredByLifecycle, and is flagged rather
// than refused. See judgeBinTypeCarriesPayload.
func (s *BinManifestService) SetFromTemplate(binID int64, payloadCode string, uopOverride *int, by protocol.Declarer) (int64, error) {
	manifestJSON, uop, err := s.resolveTemplateManifest(payloadCode, uopOverride)
	if err != nil {
		return 0, err
	}
	return s.SetForProduction(binID, manifestJSON, payloadCode, uop, by)
}

// resolveTemplateManifest resolves a payload template into a marshalled manifest
// JSON and the UOP to write (*uopOverride, or the template's UOPCapacity when
// uopOverride is nil). Shared by SetFromTemplate and RecordProducedBinFromTemplate.
func (s *BinManifestService) resolveTemplateManifest(payloadCode string, uopOverride *int) (string, int, error) {
	p, err := s.db.GetPayloadByCode(payloadCode)
	if err != nil {
		return "", 0, fmt.Errorf("payload template %q: %w", payloadCode, err)
	}
	items, err := s.db.ListPayloadManifest(p.ID)
	if err != nil {
		return "", 0, fmt.Errorf("payload manifest: %w", err)
	}
	// The manifest records WHICH parts, not how many. The count is
	// uop_remaining x parts_per_cycle wherever it is asked for, so a bin
	// loaded at 5 of 24 answers 5. This loop used to copy the template's
	// per-cycle ratio into the manifest as if it were a count, regardless of
	// uopOverride — a number that meant nothing about this bin.
	manifest := domain.Manifest{Items: make([]domain.ManifestEntry, len(items))}
	for i, item := range items {
		manifest.Items[i] = domain.ManifestEntry{PartNumber: item.PartNumber}
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return "", 0, fmt.Errorf("marshal manifest: %w", err)
	}
	if uopOverride != nil && *uopOverride < 0 {
		return "", 0, fmt.Errorf("uop override %d is negative", *uopOverride)
	}
	return string(manifestJSON), standardPackUOP(p.UOPCapacity, uopOverride), nil
}

// SetForProduction sets a bin's manifest and UOP from a payload template,
// AND bumps delta_epoch so the new load gets its own dedup space on Core
// (and the corresponding Edge seq-allocator entry). Used when a produce
// node finalizes a bin or a manual_swap node loads a bin. Returns the
// new delta_epoch so callers can ship it to Edge in the response that
// triggered the load (typically the BinLoad handler).
//
// `by` also decides what the CARRIER RULE does here (carrierNoFor, see
// judgeBinTypeCarriesPayload). The operator's Load Payload reaches it through
// SetFromTemplate with DeclaredByPerson and is refused. The loader's LOAD (the
// telemetry bin-load) does not come through here: it writes through LoadAtLine,
// refused like a person and announced as lifecycle.
func (s *BinManifestService) SetForProduction(binID int64, manifestJSON, payloadCode string, uop int, by protocol.Declarer) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	newEpoch, err := s.setForProductionTx(tx, binID, manifestJSON, payloadCode, uop, by)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newEpoch, nil
}

// carrierNo is what a write door does with the carrier rule's "no": land the
// write and flag the carrier, or refuse it. See judgeBinTypeCarriesPayload.
type carrierNo uint8

const (
	// flagUndeclared: the parts are already in the carrier (a press, a tick, an
	// ingest); the write lands and the carrier carries the finding.
	flagUndeclared carrierNo = iota
	// refuseUndeclared: a person declared the load; it is refused and nothing
	// is written.
	refuseUndeclared
)

// carrierNoFor is the consequence a door's Declarer implies: a person's
// declaration is refused, anything else is flagged. Written as "== person" so
// a third Declarer value, if one is ever added, lands on the flagging side and
// keeps the record of a load that physically happened.
func carrierNoFor(by protocol.Declarer) carrierNo {
	if by == protocol.DeclaredByPerson {
		return refuseUndeclared
	}
	return flagUndeclared
}

// setForProductionTx is SetForProduction's body against a caller-provided tx, so
// the manifest write can commit in the SAME transaction as the confirm (see
// RecordProducedBin). Writes the OpSetForProduction audit row and bumps the
// delta_epoch; returns the new epoch. The carrier verdict follows `by`.
func (s *BinManifestService) setForProductionTx(tx *sql.Tx, binID int64, manifestJSON, payloadCode string, uop int, by protocol.Declarer) (int64, error) {
	return s.writeProductionTx(tx, binID, manifestJSON, payloadCode, uop, by, carrierNoFor(by))
}

// writeProductionTx is the write itself, with the announcement's declarer and
// the carrier verdict's consequence asked separately. They agree at every door
// but one: the loader's LOAD (LoadAtLine) is a person's declaration for the
// carrier rule and a lifecycle announcement on the wire, because the Edge
// applied that act from the LOAD's own reply (see LoadAtLine).
func (s *BinManifestService) writeProductionTx(tx *sql.Tx, binID int64, manifestJSON, payloadCode string, uop int, by protocol.Declarer, no carrierNo) (int64, error) {
	undeclared, err := judgeBinTypeCarriesPayload(tx, binID, payloadCode, no)
	if err != nil {
		return 0, err
	}
	before, err := readBinUOPInTx(tx, binID)
	if err != nil {
		return 0, err
	}
	// THE FINDING RIDES THE WRITE. undeclared_carrier_at is set in the same
	// statement that writes the payload, so a bin can never be loaded and
	// unflagged for even an instant — and the ELSE arm is what clears a stale
	// finding when a bin is relabelled onto something its carrier does hold.
	// COALESCE keeps the FIRST stamp: a bin relabelled from one undeclared
	// payload to another has been wrong continuously, and the question the
	// timestamp answers is how long, not when it was last noticed. Same shape
	// as bins.MarkAnomalyWithNote.
	if _, err := tx.Exec(`UPDATE bins SET payload_code=$1, manifest=$2, uop_remaining=$3,
		manifest_confirmed=false, updated_at=NOW(),
		undeclared_carrier_at = CASE WHEN $5 THEN COALESCE(undeclared_carrier_at, NOW()) ELSE NULL END
		WHERE id=$4`,
		payloadCode, manifestJSON, uop, binID, undeclared); err != nil {
		return 0, fmt.Errorf("set manifest bin %d: %w", binID, err)
	}
	newEpoch, err := s.bumpEpoch(tx, binID, by)
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

// judgeBinTypeCarriesPayload is the bin-type rule at the WRITE end. It returns
// whether the bin is in an UNDECLARED carrier for this payload, refusing
// outright when a PERSON declared the load (refuseUndeclared).
//
// ── THE VERDICT IS ONE RULE; THE CONSEQUENCE DEPENDS ON WHO DECLARED ──────
//
// The rule itself — a payload may only ride a carrier its payload_bin_types
// rows allow — binds every reader and every writer, and nothing here softens
// it. What differs is what a "no" MEANS at the place it is said:
//
//   - DeclaredByPerson is somebody at an admin door typing an assignment
//     before anything physical has happened, or an operator's LOAD at a loader
//     window. Refusing costs nothing and stops the bad row being written at
//     all; at the window the refusal says which carriers the part rides and
//     that this one has to leave, before it waits there as unfetchable stock.
//   - DeclaredByLifecycle is Core's own bookkeeping recording what a cell,
//     a tick or an ingest ALREADY DID. The parts are in the carrier by the
//     time the message arrives. Refusing does not un-fill the bin; it writes
//     down that a thing which happened did not happen, and the count is then
//     wrong in the direction nothing downstream can detect. So the write lands
//     and the carrier carries a finding (owner, 2026-09-20).
//
// ── DERIVED FROM protocol.Declarer AT EVERY DOOR BUT ONE ──────────────────
//
// This started as a private two-valued door type (since removed), collapsed onto
// Declarer because every produce/tick/ingest call site passed lifecycle and the
// one admin door passed person: a second enum kept in lockstep by hand. It is a
// separate value again (carrierNo) because the loader's LOAD made the two
// questions differ on one call: a person declared the load, so the rule refuses
// it, and the announcement still says lifecycle, because the Edge applied the
// LOAD from its own reply and ignores a lifecycle echo of the generation it
// holds, where a person's would rewrite the count and invite a bind. Every
// other door derives it from its Declarer in one place (carrierNoFor), so the
// lockstep is not by hand; protocol/constants.go names this reader.
//
// Sourcing is untouched either way: an undeclared carrier stays unfetchable
// through helpers.BinSourceableSQL, which is precisely why the finding has to
// be visible — see the watcher on /material-flags and the inventory count.
//
// ── WHY IT IS HERE AND NOT AT THE DOORS ───────────────────────────────────
//
// setForProductionTx is the one production statement that writes a payload_code
// onto a bin (bins.SetManifest writes one too, and only the dev seeder and test
// fixtures call it; the emptying statements blank it), and every door that
// loads a bin reaches it: the operator's Load Payload
// (BinService.LoadPayload -> SetFromTemplate), the loader's LOAD (the telemetry
// bin-load, LoadAtLine, a person's declaration) and produce finalize
// (RecordProducedBin, and RecordProducedBinFromTemplate through it). Only the operator door checked
// before 523c6a3b. Produce wrote whatever Edge reported, so a cell finalizing a
// bin of the wrong carrier for its part created exactly the row the sourcing
// readers have refused to source since 493a8062 — a bin that counts as stock
// and can never be fetched. It still creates that row; the difference is that
// the row now says so.
//
// Judging under both doors rather than beside each of them is the same choice
// the sourcing collapse made: one spelling, several doors, and the declarer as
// a parameter rather than two near-copies that can drift apart on the rule
// while differing only on the consequence.
//
// ── ONE QUERY, AND THE SAME TWO HALVES AS THE READ SIDE ───────────────────
//
// has_rules and permitted are computed together so the strict-vs-permissive
// decision costs one round trip inside a transaction that is already open.
// Where payload_bin_types has rows for the payload, only those types pass;
// where it has none, everything does — coverage is a gap in the data, not a
// licence, which is the reading helpers.PayloadBinTypeRuleArm renders as
// `OR NOT EXISTS`, helpers.UndeclaredCarrierRuleSQL as its `EXISTS` half, and
// domain.BinTypeRule carries as nil-vs-empty. A payload with no rows is
// therefore never flagged at all: at Springfield one payload of 128 has rows,
// and flagging the other 127 would bury the one case worth walking to.
//
// An empty payloadCode is a CLEAR, not a load: no rule to check, and no finding
// to carry — the caller's UPDATE clears any standing stamp.
func judgeBinTypeCarriesPayload(tx *sql.Tx, binID int64, payloadCode string, no carrierNo) (bool, error) {
	if payloadCode == "" {
		return false, nil
	}
	var (
		binTypeCode string
		hasRules    bool
		permitted   bool
	)
	err := tx.QueryRow(`
		SELECT COALESCE(bt.code, ''),
		       EXISTS (
		         SELECT 1 FROM payload_bin_types pbt
		         JOIN payloads p ON p.id = pbt.payload_id
		         WHERE p.code = $2
		       ),
		       EXISTS (
		         SELECT 1 FROM payload_bin_types pbt
		         JOIN payloads p ON p.id = pbt.payload_id
		         WHERE p.code = $2 AND pbt.bin_type_id = b.bin_type_id
		       )
		FROM bins b
		LEFT JOIN bin_types bt ON bt.id = b.bin_type_id
		WHERE b.id = $1`, binID, payloadCode).Scan(&binTypeCode, &hasRules, &permitted)
	if err != nil {
		// FAIL CLOSED, including on no-rows, AT BOTH DOORS. A bin that cannot be
		// read is not a bin that passes, and a read failure is not a finding: a
		// finding is a claim about the plant, and this path has learned nothing
		// about the plant. Flagging on a failed read would put a carrier on the
		// operator's list because the database hiccupped.
		return false, fmt.Errorf("check bin %d carrier against payload %q: %w", binID, payloadCode, err)
	}
	if !hasRules || permitted {
		return false, nil
	}
	if no != refuseUndeclared {
		// Core's own bookkeeping, recording something that already happened: the
		// parts are in the bin. Land the write and let the carrier hold the
		// finding; every sourcing reader still refuses it.
		//
		// Written as "not a person" rather than "== lifecycle" so a third
		// Declarer value, if one is ever added, lands on the SAFE side here —
		// flag and keep the record — instead of silently refusing a load that
		// physically happened.
		return true, nil
	}

	allowed, aErr := listPayloadBinTypeCodesTx(tx, payloadCode)
	if aErr != nil {
		allowed = nil
	}
	return false, &CarrierRefusedError{Payload: payloadCode, BinType: binTypeCode, Allowed: allowed}
}

// CarrierRefusedError is the carrier rule's "no" to a person's declaration: the
// payload, the carrier it was declared onto, and the carriers it may ride (nil
// when that list could not be read). Typed so a door can answer it as a
// conflict with its own instructions rather than as a server failure; Error()
// is the sentence both person doors have always returned.
type CarrierRefusedError struct {
	Payload string
	BinType string
	Allowed []string
}

func (e *CarrierRefusedError) Error() string {
	if e.Allowed == nil {
		return fmt.Sprintf("payload %q may not be loaded into bin type %q", e.Payload, e.BinType)
	}
	return fmt.Sprintf("payload %q not compatible with bin type %q (allowed: %v)", e.Payload, e.BinType, e.Allowed)
}

// listPayloadBinTypeCodesTx names the carriers a payload IS allowed in, for the
// refusal message. Separate from the decision so a failure to build the
// sentence can never change the verdict.
func listPayloadBinTypeCodesTx(tx *sql.Tx, payloadCode string) ([]string, error) {
	rows, err := tx.Query(`
		SELECT bt.code
		FROM payload_bin_types pbt
		JOIN payloads p   ON p.id  = pbt.payload_id
		JOIN bin_types bt ON bt.id = pbt.bin_type_id
		WHERE p.code = $1
		ORDER BY bt.code`, payloadCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var codes []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		codes = append(codes, c)
	}
	return codes, rows.Err()
}

// Confirm marks a bin's manifest as confirmed by an operator or automated
// process. Writes a same-tx manifest_confirmed bin_uop_ledger row so the
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
//
// A CARRIER THE PAYLOAD DOES NOT DECLARE LANDS HERE AND IS FLAGGED, never
// refused — the parts are in the bin by the time this message arrives. That is
// the same DeclaredByLifecycle that keeps the announcement from inviting a
// bind, reused: see judgeBinTypeCarriesPayload.
//
// full is a station's declaration that the bin left full (nil: none). When
// the booked part's pack (full.Pack) exceeds uop, the count is booked and then
// stamped up to the pack in the same transaction; see stampDeclaredFullTx.
func (s *BinManifestService) RecordProducedBin(binID int64, manifestJSON, payloadCode string, uop int, producedAt string, full *DeclaredFull) error {
	// A press finishing a carrier. This announcement is the one that routinely
	// lands after a robot has already lifted what it names, so it must never
	// invite a bind.
	_, err := s.recordProduced(binID, manifestJSON, payloadCode, uop, producedAt, flagUndeclared, full)
	return err
}

// DeclaredFull is a station's RELEASE FULL on a produce bin it counted below
// its pack, as the ingest carries it (protocol.OrderIngestRequest.DeclaredQty,
// CalledBy, OrderID), with the pack the caller resolved for the part the bin
// is booked under. The count is booked as counted and stamped up to Pack with
// a row; nothing is stamped when Pack does not exceed the count, so a count is
// never written down.
type DeclaredFull struct {
	// Pack is the catalog pack (uop_capacity, cycles) of the part the bin is
	// booked under: the count a stamp writes. <= 0 is no pack: the count
	// stands and the row says DeclaredFullNoPack.
	Pack int
	// DeclaredQty is what the station declared (the departing order's pack),
	// kept in the row's detail; it is not the count stamped.
	DeclaredQty int
	// DeclaredPart is the part the declaration was made for (the ingest's),
	// kept in the row's detail beside the booked part, which can differ
	// (ApplyIngestManifest keeps the part a bin was counted as).
	DeclaredPart string
	// CalledBy is who declared it: the stamp row's actor.
	CalledBy string
	// OrderUUID is the departing order's Edge UUID; "" names none.
	OrderUUID string
	// Node is the seat the bin was counted at (the ingest's source node).
	Node string
}

// DeclaredFullNoPack is the release_declared_full row's detail reason when
// the booked part has no pack to stamp to (no catalog capacity): the row is
// written from the count to the count, so the declaration is listed with the
// stamps and the bin leaves at what was counted.
const DeclaredFullNoPack = "no_pack"

// recordProduced is the set-and-confirm in one transaction, returning the new
// delta_epoch for the caller that has to hand it back to the Edge. Its
// announcement is always lifecycle; `no` is what the carrier rule does with a
// "no" (see judgeBinTypeCarriesPayload): the press door flags, the loader's
// LOAD refuses. full is the produce ingest's declared-full stamp; the LOAD
// passes nil and never writes one.
func (s *BinManifestService) recordProduced(binID int64, manifestJSON, payloadCode string, uop int, producedAt string, no carrierNo, full *DeclaredFull) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	epoch, err := s.writeProductionTx(tx, binID, manifestJSON, payloadCode, uop, protocol.DeclaredByLifecycle, no)
	if err != nil {
		return 0, err
	}
	if err := stampDeclaredFullTx(tx, binID, payloadCode, uop, full); err != nil {
		return 0, err
	}
	if err := s.confirmTx(tx, binID, producedAt); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return epoch, nil
}

// stampDeclaredFullTx writes a station's declaration over the count just
// booked, on the caller's transaction: when the booked part's pack exceeds the
// count, the bin's uop_remaining to full.Pack and one release_declared_full
// row from counted to full.Pack with the declarer as actor; when there is no
// pack (full.Pack <= 0), the count stands and the row goes from counted to
// counted with reason DeclaredFullNoPack; when the pack is at or below the
// count, nothing. The row is written under the booked part and its detail
// carries the declared part and quantity beside the pack. Its order_id is
// Core's id for full.OrderUUID, read here and only here (one read on
// idx_orders_uuid, only when a row is written and an order is named); an
// order Core cannot find leaves order_id NULL and the detail says so. The
// stamp does not bump the epoch: it is a count inside the binding the set
// just started (TestReleaseDeclaredFullIsNeitherBumpNorUnload).
//
// Cost when it stamps: the UPDATE, the order read when one is named, the
// ledger context read and the INSERT; a no_pack row is the same less the
// UPDATE. Otherwise nothing.
func stampDeclaredFullTx(tx *sql.Tx, binID int64, payloadCode string, counted int, full *DeclaredFull) error {
	if full == nil {
		return nil
	}
	noPack := full.Pack <= 0
	if !noPack && full.Pack <= counted {
		return nil
	}
	after, reason := full.Pack, ""
	if noPack {
		after, reason = counted, DeclaredFullNoPack
	}
	var orderID *int64
	unresolved := false
	if full.OrderUUID != "" {
		var id int64
		switch err := tx.QueryRow(`SELECT id FROM orders WHERE edge_uuid = $1`, full.OrderUUID).Scan(&id); {
		case err == nil:
			orderID = &id
		case errors.Is(err, sql.ErrNoRows):
			unresolved = true
		default:
			return fmt.Errorf("resolve order %q for the declared-full stamp on bin %d: %w", full.OrderUUID, binID, err)
		}
	}
	detail, err := json.Marshal(struct {
		Counted         int    `json:"counted"`
		Pack            int    `json:"pack,omitempty"`
		DeclaredQty     int    `json:"declared_qty"`
		DeclaredPart    string `json:"declared_part,omitempty"`
		Reason          string `json:"reason,omitempty"`
		Node            string `json:"node,omitempty"`
		OrderUUID       string `json:"order_uuid,omitempty"`
		OrderUnresolved bool   `json:"order_unresolved,omitempty"`
	}{counted, max(full.Pack, 0), full.DeclaredQty, full.DeclaredPart, reason, full.Node, full.OrderUUID, unresolved})
	if err != nil {
		return fmt.Errorf("marshal the declared-full stamp on bin %d: %w", binID, err)
	}
	if !noPack {
		if _, err := tx.Exec(`UPDATE bins SET uop_remaining=$2, updated_at=NOW() WHERE id=$1`, binID, after); err != nil {
			return fmt.Errorf("stamp bin %d declared full: %w", binID, err)
		}
	}
	uopCtx, err := resolveBinUOPContext(tx, binID, detail)
	if err != nil {
		return err
	}
	return audit.AppendBinUOP(tx, binID, &counted, after,
		audit.OpReleaseDeclaredFull, "service/bin_manifest.go:recordProduced",
		orderID, payloadCode, full.CalledBy, uopCtx)
}

// ErrUnknownPayload marks a load refused because Core has no template for the
// payload code, so a handler can answer the caller's error rather than 500.
var ErrUnknownPayload = errors.New("unknown payload")

// ErrCountRequired refuses a LOAD that declares no count at a window inside a
// partial home of the part. The standard pack is the default everywhere else;
// here a carrier may hold any count, and one written as full would never be
// called back to be finished.
var ErrCountRequired = errors.New("a load here must say how many it holds")

// LoadAtLine is the bin-load telemetry door's write: an operator's LOAD at a
// loader window, relayed by the Edge, with the parts it named (manifestJSON) and
// the count it declared, if any.
//
// A PERSON DECLARED IT, so the carrier rule refuses rather than flags
// (refuseUndeclared, as BinService.LoadPayload is refused): a LOAD onto a
// carrier the part may not ride is answered with a *CarrierRefusedError and
// nothing is written. It used to flag like the press door, which landed the
// load; every sourcing reader then refused that carrier as a source for good
// while its parts held the reorder level (D33). The press finishing a carrier
// (RecordProducedBin) still flags.
//
// THE ANNOUNCEMENT STAYS LIFECYCLE. The Edge seats the LOAD from this call's
// reply (BindActiveBin with the returned epoch) and ignores a lifecycle echo of
// the generation it already holds (handler_uop_adjustment.go). A person's echo
// would be applied again: a second write on the Pi per LOAD, the count rewritten
// over any tick taken since, and an invitation to bind a carrier the L2 may
// already have lifted. Only the carrier verdict is a person's here.
//
// It is RecordProducedBin with the two questions the door used to answer for
// itself answered here instead:
//
//   - AN UNKNOWN PAYLOAD IS REFUSED, like BinService.LoadPayload refuses it. It
//     used to load at uop 0, which is not "unknown": it is a ledger entry that
//     reads as a measured empty bin.
//   - AN UNDECLARED COUNT IS THE STANDARD PACK (standardPackUOP), the same rule
//     SetFromTemplate applies to a nil override. A declared count, zero
//     included, is written as given.
//
// The set and the confirm commit together (recordProduced). They were two
// transactions with the confirm's failure logged and dropped, which left a
// set-but-unconfirmed bin — invisible to kanban — reported to the Edge as ok.
// Returns the new epoch and the count written.
func (s *BinManifestService) LoadAtLine(binID int64, manifestJSON, payloadCode string, declared *int64) (int64, int, error) {
	p, err := s.db.GetPayloadByCode(payloadCode)
	if err != nil || p == nil {
		return 0, 0, fmt.Errorf("%w: payload template %q not found", ErrUnknownPayload, payloadCode)
	}
	// A partial home holds carriers of any count, so the default cannot stand
	// for one there: the person declares it, and the declaration is the moment
	// the carrier is held from (StampHeldSince, below).
	bin, err := s.db.GetBin(binID)
	if err != nil {
		return 0, 0, err
	}
	atHome := false
	if bin.NodeID != nil {
		if atHome, err = s.db.InPartialHomeOf(*bin.NodeID, payloadCode); err != nil {
			return 0, 0, err
		}
	}
	if atHome && declared == nil {
		return 0, 0, fmt.Errorf("%w: %s is a partial home for %s", ErrCountRequired, bin.NodeName, payloadCode)
	}
	var d *int
	if declared != nil {
		v := int(*declared)
		d = &v
	}
	// A bare cart a person loads parts onto now carries a carrier: the rule
	// judges that, and a refused load puts the marker back.
	marker, err := s.db.StampCarrierForLoad(binID)
	if err != nil {
		return 0, 0, err
	}
	uop := standardPackUOP(p.UOPCapacity, d)
	epoch, err := s.recordProduced(binID, manifestJSON, payloadCode, uop, "", refuseUndeclared, nil)
	if err != nil {
		if marker != nil {
			if rErr := s.db.RestoreBinType(binID, *marker); rErr != nil {
				log.Printf("bin manifest: restore bin %d's cart type after a refused load: %v", binID, rErr)
			}
		}
		return 0, 0, err
	}
	if atHome {
		if err := s.db.StampHeldSince(binID); err != nil {
			log.Printf("bin manifest: held-since for bin %d after its load: %v", binID, err)
		}
	}
	s.notifyBinChanged(binID)
	return epoch, uop, nil
}

// standardPackUOP is the one statement of the standard-pack rule: absence of a
// declared count means a full bin, and a full bin is the payload's
// uop_capacity CYCLES. A declared count is honoured as given, including zero —
// "I counted none" is an answer, not a missing one.
func standardPackUOP(capacity int, declared *int) int {
	if declared != nil {
		return *declared
	}
	return capacity
}

// RecordProducedBinFromTemplate is RecordProducedBin for the no-manifest ingest
// path: it resolves the payload template (like SetFromTemplate) then sets and
// confirms the bin in one transaction. uopOverride nil = template capacity;
// full as RecordProducedBin.
func (s *BinManifestService) RecordProducedBinFromTemplate(binID int64, payloadCode string, uopOverride *int, producedAt string, full *DeclaredFull) error {
	manifestJSON, uop, err := s.resolveTemplateManifest(payloadCode, uopOverride)
	if err != nil {
		return err
	}
	return s.RecordProducedBin(binID, manifestJSON, payloadCode, uop, producedAt, full)
}

// RecordIngestRefused writes the audit row for a produce ingest refused on its
// epoch fence (OpIngestStaleEpochRefused). The bin is unchanged.
func (s *BinManifestService) RecordIngestRefused(binID int64, payloadCode string, uop int, wireEpoch, binEpoch, quantity int64) error {
	meta, err := json.Marshal(struct {
		WireEpoch int64 `json:"wire_epoch"`
		BinEpoch  int64 `json:"bin_epoch"`
		Quantity  int64 `json:"quantity"`
	}{wireEpoch, binEpoch, quantity})
	if err != nil {
		return fmt.Errorf("marshal ingest refusal bin %d: %w", binID, err)
	}
	return audit.AppendBinUOPOverride(s.db, binID, uop, uop, audit.OpIngestStaleEpochRefused,
		"service/bin_manifest.go:RecordIngestRefused", nil, payloadCode, "", meta)
}

// Unconfirm clears a bin's manifest confirmation flag, and writes its
// manifest_unconfirmed ledger row in the same transaction — Confirm's mirror.
//
// It was a silent mutation: Confirm leaves an OpManifestConfirmed row and
// Unconfirm left nothing, so a bin's ledger could say "confirmed" as its last
// word about a bin that had since been taken back out of the drain population.
// after_uop is the unchanged uop_remaining; like confirm, this records a
// lifecycle event, not a count change.
func (s *BinManifestService) Unconfirm(binID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	before, err := readBinUOPInTx(tx, binID)
	if err != nil {
		return fmt.Errorf("unconfirm manifest bin %d: %w", binID, err)
	}
	var uop int
	var payloadCode string
	// updated_at from clock.Now(), as the store writer this replaces stamped it
	// (the rest of this file uses the database's NOW()).
	if err := tx.QueryRow(`UPDATE bins SET manifest_confirmed=false, updated_at=$2
		WHERE id=$1 RETURNING uop_remaining, COALESCE(payload_code, '')`, binID, clock.Now().UTC()).Scan(&uop, &payloadCode); err != nil {
		return fmt.Errorf("unconfirm manifest bin %d: %w", binID, err)
	}
	uopCtx, err := resolveBinUOPContext(tx, binID, nil)
	if err != nil {
		return err
	}
	if err := audit.AppendBinUOP(tx, binID, before, uop,
		audit.OpManifestUnconfirmed, "service/bin_manifest.go:Unconfirm",
		nil, payloadCode, "", uopCtx); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearAndClaim atomically clears manifest and claims the bin for an order.
// Closes the TOCTOU race where ClaimBin + the manifest clear are separate txns.
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
	// mirroring bins.Claim / nodes.ClaimSlotTx.
	// The first row lock in this transaction is this UPDATE's, and its owner
	// test (the liveness CTE) is taken before it (reservations.OwnerLiveSQL).
	var live, named bool
	var n int
	err = tx.QueryRow(`WITH live AS (SELECT `+reservations.OwnerLiveSQL(1)+` AS ok),
		named AS (SELECT `+reservations.NamedBinSQL(1, 2)+` AS ok),
		upd AS (
		  UPDATE bins SET `+emptiedCarrierSET+`,
			claimed_by=$1, updated_at=NOW()
		  WHERE id=$2 AND locked=false AND (claimed_by IS NULL OR claimed_by=$1)
		    AND `+reservations.HeldByOwnerSQL(reservations.KindBin, 1, 2)+`
		    AND (SELECT ok FROM live) AND (SELECT ok FROM named)
		  RETURNING 1)
		SELECT (SELECT ok FROM live), (SELECT ok FROM named), (SELECT count(*) FROM upd)`,
		orderID, binID).Scan(&live, &named, &n)
	if err != nil {
		return fmt.Errorf("clear+claim bin %d: %w", binID, err)
	}
	if !live {
		return reservations.ErrOwnerEnded
	}
	if !named {
		return reservations.ErrNotTheNamedBin
	}
	if n == 0 {
		return fmt.Errorf("bin %d is locked, already claimed, or does not exist", binID)
	}
	// Dispatch claiming a carrier for an order. No person declared a count —
	// the clear is a consequence of the claim — so the station must not bind an
	// empty slot to it.
	if _, err := s.bumpEpoch(tx, binID, protocol.DeclaredByLifecycle); err != nil {
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
// For partial consumption: manifest preserved, uop_remaining set, and the
// bin's generation bumped and announced (see syncUOPAndClaimTx).
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
	// As clearAndClaimTx: the owner test is taken before this UPDATE's row lock,
	// the transaction's first.
	var live, named bool
	var n int
	err = tx.QueryRow(`WITH live AS (SELECT `+reservations.OwnerLiveSQL(2)+` AS ok),
		named AS (SELECT `+reservations.NamedBinSQL(2, 3)+` AS ok),
		upd AS (
		  UPDATE bins SET
			uop_remaining=$1, claimed_by=$2, updated_at=NOW()
		  WHERE id=$3 AND locked=false AND (claimed_by IS NULL OR claimed_by=$2)
		    AND `+reservations.HeldByOwnerSQL(reservations.KindBin, 2, 3)+`
		    AND (SELECT ok FROM live) AND (SELECT ok FROM named)
		  RETURNING 1)
		SELECT (SELECT ok FROM live), (SELECT ok FROM named), (SELECT count(*) FROM upd)`,
		remainingUOP, orderID, binID).Scan(&live, &named, &n)
	if err != nil {
		return fmt.Errorf("sync+claim bin %d: %w", binID, err)
	}
	if !live {
		return reservations.ErrOwnerEnded
	}
	if !named {
		return reservations.ErrNotTheNamedBin
	}
	if n == 0 {
		return fmt.Errorf("bin %d is locked, already claimed, or does not exist", binID)
	}
	// A dispatch claim of a partly consumed carrier is a lifecycle transition,
	// like the partial release: the count is set absolutely from the order's
	// remaining UOP, so a delta the station stamped before it belongs to the
	// generation that just ended. Without the bump that delta was still current
	// and landed on top of the synced number, and without the announcement the
	// station never learned the count Core had set. Nobody declared a count —
	// the sync is a consequence of the claim — so the station must not bind an
	// empty slot to it.
	if _, err := s.bumpEpoch(tx, binID, protocol.DeclaredByLifecycle); err != nil {
		return err
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
// and executes it atomically. It is the FUSED reserve-then-confirm convenience:
// Acquire + claim + Confirm in one call.
//
// NOTE: this fused form hard-claims the bin at the moment it runs, so it is NOT
// used by the dispatch paths — under Rule 1 (soft until complete) every dispatch
// path soft-acquires the bin with ReserveForDispatch while it waits and
// hard-claims it at dispatch with ConfirmClaim, so no order holds a hard bin
// claim before its dispatch step. ClaimForDispatch survives as a primitive for
// tests that want the fused op in one call; production callers must use the
// soft-reserve + confirm-at-dispatch pair instead.
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
//     is what closes the claim/confirm wedge (see the repo-root
//     docs/reservations.md).
//  3. On ANY failure after Acquire → Release (best-effort; Expire is the backstop).
func (s *BinManifestService) ClaimForDispatch(binID, orderID int64, remainingUOP *int) error {
	if err := reservations.Acquire(s.db, orderID, s.laneOwnerOf(orderID), binID, "ClaimForDispatch"); err != nil {
		// ErrReservationConflict, ErrLaneDugByAnother, or a transient DB error —
		// all three surface as codeClaimFailed and all three retry next tick.
		return err
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
	if err := s.ConfirmClaimTx(tx, binID, orderID, remainingUOP); err != nil {
		return err
	}
	return tx.Commit()
}

// ConfirmClaimTx is ConfirmClaim on the caller's transaction: the hard claim (per
// the remainingUOP disposition) and the reservation's pending → confirmed flip,
// committed or rolled back with the rest of the caller's work. dispatch.acquireHand
// uses it so a bin confirmed before a refused lane or slot is not left claimed.
func (s *BinManifestService) ConfirmClaimTx(tx *sql.Tx, binID, orderID int64, remainingUOP *int) error {
	if err := s.claimUnderReservationTx(tx, binID, orderID, remainingUOP); err != nil {
		return err
	}
	return reservations.Confirm(tx, orderID, binID)
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
	return reservations.Acquire(s.db, orderID, s.laneOwnerOf(orderID), binID, "reserveForDispatch")
}

// ReserveForDispatchAndPoint is ReserveForDispatch that also points the order at
// the bin in the same statement (reservations.AcquireAndPoint): a pointer is
// never stamped on a bin the order holds no row on.
func (s *BinManifestService) ReserveForDispatchAndPoint(binID, orderID int64) error {
	return reservations.AcquireAndPoint(s.db, orderID, s.laneOwnerOf(orderID), binID, "reserveForDispatch")
}

// laneOwnerOf resolves the order that would hold a lane mouth row on orderID's
// behalf: its compound parent, or orderID itself when it has none. It mirrors
// dispatch.laneOwnerFor — children never own rows, so a child working inside its
// parent's dug lane is exempted through the parent.
//
// UNREADABLE ANSWERS ITSELF, and the direction matters. Returning orderID for an
// order that DOES have a parent only makes the dig arm stricter: the child loses
// its parent's exemption, fails to claim, and retries next tick. Guessing a
// parent that is not there would hand an unrelated order a dig's exemption, which
// is the direction that lets a foreigner claim inside an excavation — the exact
// collision reservations.Acquire's dig arm exists to stop.
func (s *BinManifestService) laneOwnerOf(orderID int64) int64 {
	var parent sql.NullInt64
	if err := s.db.QueryRow(
		`SELECT parent_order_id FROM orders WHERE id = $1`, orderID).Scan(&parent); err != nil {
		return orderID
	}
	if parent.Valid {
		return parent.Int64
	}
	return orderID
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
	return s.syncOrClearForReleased(binID, orderID, remainingUOP, kind, actor, false, nil)
}

// ReleaseFence is a release's stream fence as Core applies it (Lane H,
// DECISIONS D11 (i)): protocol.ReleaseFence plus the station whose delta
// scope it fences, which is the release envelope's sender, never a field the
// message names.
type ReleaseFence struct {
	Station string
	protocol.ReleaseFence
}

// SyncOrClearForReleasedFenced is SyncOrClearForReleased for a release that
// fences the stream (OrderRelease.AsOf). The declared count is written with
// NO generation bump, and the station's dedup cursor for (bin, epoch) moves
// to the fence in the same transaction, so a delta flushed before the release
// and delivered after it is at or below high water and skipped. The
// generation ends at the lift (BinService.Lift). A fence for another bin or
// another generation than the bin's current one cannot fence anything, and
// the release bumps as an unfenced one does.
func (s *BinManifestService) SyncOrClearForReleasedFenced(binID, orderID int64, remainingUOP *int, kind protocol.UOPDispositionKind, actor string, fence *ReleaseFence) error {
	return s.syncOrClearForReleased(binID, orderID, remainingUOP, kind, actor, false, fence)
}

// SyncOrClearForReleasedNoOwnerFenced is the source-node fallback with a
// fence; see SyncOrClearForReleasedFenced.
func (s *BinManifestService) SyncOrClearForReleasedNoOwnerFenced(binID, orderID int64, remainingUOP *int, actor string, fence *ReleaseFence) error {
	return s.syncOrClearForReleased(binID, orderID, remainingUOP, "", actor, true, fence)
}

// applyReleaseFence moves the station's dedup cursor for (bin, epoch) to the
// fence, inside the release's transaction: last_seq to at least AsOfSeq, and
// applied_net to AsOfNet, the net the declared count is as of, and the applied
// window end to AsOfAt (so a late pre-release window reads as late, not as a
// rollback: atOrBelowHighWater). One statement
// (the release's +1, D11 (i)).
//
// It returns the drift: the net of windows Core ALREADY applied past the
// fence (a delta flushed after the Edge read the fence, delivered ahead of the
// release by the drainer). Those ticks were counted after the declaration, so
// the declared count must take them again (count + drift), and the cursor
// keeps their net so they are not applied a second time. The same arithmetic
// as the Edge's fencedCount, from the other side. 0 when nothing passed the
// fence, or when the scope's row carries no net (an older stream, which no
// fenced release comes from).
func applyReleaseFence(tx *sql.Tx, binID int64, f *ReleaseFence) (int, error) {
	var (
		lastSeq int64
		net     sql.NullInt64
	)
	if err := tx.QueryRow(`
		INSERT INTO inventory_delta_dedup (station, scope_kind, scope_key, epoch, last_seq, applied_net, applied_window_end, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (station, scope_kind, scope_key, epoch)
		DO UPDATE SET
			applied_net = CASE WHEN inventory_delta_dedup.last_seq > EXCLUDED.last_seq
			                    AND inventory_delta_dedup.applied_net IS NOT NULL
			                   THEN inventory_delta_dedup.applied_net ELSE EXCLUDED.applied_net END,
			last_seq = GREATEST(inventory_delta_dedup.last_seq, EXCLUDED.last_seq),
			applied_window_end = GREATEST(inventory_delta_dedup.applied_window_end, EXCLUDED.applied_window_end),
			updated_at = NOW()
		RETURNING last_seq, applied_net`,
		f.Station, protocol.InvDeltaScopeBin, strconv.FormatInt(binID, 10), f.Epoch, f.AsOfSeq, f.AsOfNet,
		sql.NullTime{Time: f.AsOfAt, Valid: !f.AsOfAt.IsZero()},
	).Scan(&lastSeq, &net); err != nil {
		return 0, fmt.Errorf("fence the stream for released bin %d (station %s, epoch %d): %w", binID, f.Station, f.Epoch, err)
	}
	if lastSeq > f.AsOfSeq && net.Valid {
		return int(net.Int64 - f.AsOfNet), nil
	}
	return 0, nil
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
	return s.syncOrClearForReleased(binID, orderID, remainingUOP, "", actor, true, nil)
}

// syncOrClearForReleased is the shared body of the exported variants.
// sourceNodeFallback toggles the claimed_by SQL guard, audit-op constants,
// legacy-tag suffixes, and error-message strings. kind is honored only for
// the owner path (sourceNodeFallback=false); the fallback path passes ""
// and never emits an underpack tag.
//
// fence (Lane H, DECISIONS D11 (i)): a release that fences the stream writes
// the declared count with no bump and moves the dedup cursor; the generation
// ends at the lift. A nil fence, or one naming another bin or generation, is
// a release from an Edge that predates it, and keeps today's bump in the
// release's transaction (mixed-version safety; pin
// TestRelease_UnfencedReleaseStillBumps).
func (s *BinManifestService) syncOrClearForReleased(binID, orderID int64, remainingUOP *int, kind protocol.UOPDispositionKind, actor string, sourceNodeFallback bool, fence *ReleaseFence) error {
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
	// release audit row names the part (the per-bin audit timeline shows it).
	// Best-effort: a missing bin surfaces below via the UPDATE's 0-rows guard.
	// The generation rides the same read (+0): a fence counts only for the
	// generation it was read under.
	var (
		releasedPayloadCode string
		currentEpoch        int64
	)
	_ = tx.QueryRow(`SELECT COALESCE(payload_code, ''), delta_epoch FROM bins WHERE id=$1`, binID).Scan(&releasedPayloadCode, &currentEpoch)
	fenced := fence != nil && fence.Station != "" && fence.BinID == binID && fence.Epoch == currentEpoch
	if fence != nil && !fenced {
		log.Printf("bin_manifest: release of bin %d carries a fence for bin %d epoch %d (station %q); the bin is at epoch %d — "+
			"bumping at the release, as an unfenced release does", binID, fence.BinID, fence.Epoch, fence.Station, currentEpoch)
	}
	drift := 0
	if fenced {
		if drift, err = applyReleaseFence(tx, binID, fence); err != nil {
			return err
		}
	}

	if *remainingUOP == 0 {
		// Clear manifest, preserve claim (if applicable). The claimed_by guard
		// is added only for the owner variant.
		clearSQL := `
			UPDATE bins SET ` + emptiedCarrierSET + `,
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
		// LIFECYCLE, THOUGH THIS FUNCTION HOLDS A PERSON'S NAME. `actor` above
		// is a real operator screen and goes in the audit row, and it is the
		// wrong answer for the wire: a release is a carrier LEAVING the node.
		// Announcing it as a person's declaration would invite the station to
		// bind an empty slot to a carrier on its way out, which is the
		// departed-carrier misattribution the Edge guard exists to prevent.
		// Two questions, two values — see protocol.Declarer.
		//
		// ONLY UNFENCED (Lane H, D11 (i)): a fenced release declares the
		// count inside the generation, which ends at the lift. The drift is
		// not taken here: a carrier released empty is not charged afterwards
		// (R3, the Edge's released_empty flag), so no window passes its fence.
		if !fenced {
			if _, err := s.bumpEpoch(tx, binID, protocol.DeclaredByLifecycle); err != nil {
				return err
			}
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
	// Manifest reconstruction: the bin's manifest is rewritten to the PART
	// NUMBERS the payload template lists. Pre-2026-05 this branch only updated
	// uop_remaining, leaving the manifest carrying the pre-release qty — the
	// SMN_003/ALN_002 stale-manifest bug class. The reconstruction is atomic
	// with the UOP update, reading payload_code from the same row, so there is
	// no read-then-update race.
	//
	// The line no longer carries a qty at all, which retires that bug class
	// rather than re-fixing it: the count is uop_remaining (the $1 this same
	// statement writes) times the template's parts_per_cycle, so there is no
	// second copy to fall out of step. The rewrite still has to happen —
	// the manifest's PART LIST must match the released payload.
	//
	// THE LINES, NOT THE PAYLOAD CODE, and writing the code here was a silent
	// loss. Everything downstream keys a manifest line against
	// payload_manifest.part_number — the CMS quantity derivation, the inventory
	// page's per-part rows — and for a kit the payload code matches no line at
	// all, so every partial-release bin resolved to a ratio of zero and booked
	// NOTHING to the ledger for a real physical move. The single-payload
	// normalization assumption this used to encode was about how many PAYLOADS
	// a bin holds, which was never a statement about how the lines are named.
	//
	// Two CASE guards, both preserving the prior manifest rather than blanking
	// it, and for the same reason: a manifest emptied because we could not
	// determine the part list is indistinguishable from a bin that holds
	// nothing, which is exactly the silent zero above by another door.
	//   - empty payload_code: a malformed state (a partial release should
	//     always have a payload). Erroring would be cleaner but risks
	//     regressing release flows in the field.
	//   - no template lines for the code: an unknown code, or a payload nobody
	//     has given a manifest. We did not have the input to rewrite the list.
	//
	// The HAVING is what makes the second guard work, and it is not optional
	// decoration. jsonb_agg over zero rows returns NULL, but jsonb_build_object
	// wrapped around it does NOT — it returns {"items": null}, a perfectly
	// non-NULL value that sails through COALESCE and writes a manifest parsing
	// to zero items. HAVING count(*) > 0 makes the subquery yield no row at
	// all, which is the NULL the COALESCE is looking for.
	//
	// The ORDER BY inside the aggregate makes the written list deterministic,
	// so re-releasing the same bin does not churn the row.
	syncSQL := `
		UPDATE bins SET
			uop_remaining=$1,
			manifest=CASE
				WHEN COALESCE(payload_code, '') = '' THEN manifest
				ELSE COALESCE((
					SELECT jsonb_build_object('items',
						jsonb_agg(jsonb_build_object('part_number', pm.part_number)
							ORDER BY pm.part_number))
					  FROM payload_manifest pm
					  JOIN payloads p ON p.id = pm.payload_id
					 WHERE p.code = bins.payload_code
					HAVING count(*) > 0
				), manifest)
			END,
			updated_at=NOW()
		WHERE id=$2 AND locked=false`
	// The count written is the declaration plus any windows Core applied
	// past the fence before the release reached it (applyReleaseFence's
	// drift; 0 unfenced and almost always 0 fenced).
	written := *remainingUOP + drift
	var res sql.Result
	if sourceNodeFallback {
		res, err = tx.Exec(syncSQL, written, binID)
	} else {
		res, err = tx.Exec(syncSQL+` AND claimed_by=$3`, written, binID, orderID)
	}
	if err != nil {
		return fmt.Errorf("sync UOP for released bin %d%s: %w", binID, errSuffix, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return notFoundErr
	}
	// The partial-release branch of the same departure; lifecycle for the
	// reason given at the empty branch above, and unfenced only (D11 (i)).
	if !fenced {
		if _, err := s.bumpEpoch(tx, binID, protocol.DeclaredByLifecycle); err != nil {
			return err
		}
	}
	op := audit.OpReleasedPartial
	if sourceNodeFallback {
		op = audit.OpReleasedPartialFallback
	}
	uopCtx, err := resolveBinUOPContext(tx, binID, nil)
	if err != nil {
		return err
	}
	if err := audit.AppendBinUOP(tx, binID, before, written,
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
// (CountSuggested / CapturesSuggested) and writes one bin_uop_ledger
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
