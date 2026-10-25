package bins

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"shingo/protocol/clock"
	"shingocore/domain"
)

// ManifestEntry is the bin-manifest line-item domain type — one CatID
// at a given quantity. Lives in shingocore/domain (Stage 2A); aliased
// here so existing references to bins.ManifestEntry compile unchanged.
type ManifestEntry = domain.ManifestEntry

// Manifest is the parsed form of a bin's manifest JSON field. Lives
// in shingocore/domain (Stage 2A) along with Bin.ParseManifest; outer
// store/ re-exports this as store.BinManifest for backward compat.
type Manifest = domain.Manifest

// SetManifest populates a bin's contents from a payload template.
func SetManifest(db *sql.DB, binID int64, manifestJSON string, payloadCode string, uopRemaining int) error {
	_, err := db.Exec(`UPDATE bins SET payload_code=$1, manifest=$2, uop_remaining=$3, manifest_confirmed=false, updated_at=$5 WHERE id=$4`,
		payloadCode, manifestJSON, uopRemaining, binID, clock.Now().UTC())
	return err
}

// Produced is what a produce record writes onto a carrier besides the payload
// code. The zero value is the produce tick's first-delta bind.
type Produced struct {
	// Manifest is the parts list as JSON. nil writes the payload's template
	// manifest (templateManifestSQL), built in the statement.
	Manifest *string
	// Count is the carrier's new count. A record that carries a count starts
	// the carrier's contents over, so it also unconfirms the manifest and
	// stamps updated_at. nil leaves all three alone: the produce tick's bind
	// writes no count (the delta applied after it owns uop_remaining), and
	// un-confirming a carrier on a rebind would change what may source it.
	Count *int
	// Undeclared is the carrier verdict when the caller has already judged
	// it. nil has the statement judge it from the same rule
	// (UndeclaredCarrierRuleSQL), which costs no extra statement on the
	// delta path the produce tick runs under.
	Undeclared *bool
	// FlagAnomaly stamps anomaly_at, keeping a standing stamp: the tick's
	// rebind over units of another payload marks the carrier for a cycle
	// count. It gates nothing.
	FlagAnomaly bool
}

// templateManifestSQL is the manifest the payload named by $1 gives a bin: its
// template lines' part numbers in template order, `{"items":[]}` when it has
// none. It is the shape BinManifestService.resolveTemplateManifest marshals for
// every load and produce write, built in-statement so the produce tick's bind
// stays one UPDATE. The COALESCE matters: jsonb_agg over zero rows is NULL, and
// an empty template must still write an empty list rather than keep the old
// label's parts under the new code.
const templateManifestSQL = `jsonb_build_object('items', COALESCE((
		SELECT jsonb_agg(jsonb_build_object('part_number', pm.part_number) ORDER BY pm.id)
		  FROM payload_manifest pm
		  JOIN payloads p ON p.id = pm.payload_id
		 WHERE p.code = $1), '[]'::jsonb))`

// RecordProducedTx is the produce writer: the one statement that records what
// a station made onto the carrier in hand. Produce finalize
// (service.RecordProducedBin) and the produce tick's bind
// (uop.InventoryDeltaService.ApplyBinUOPDelta) both write through it.
//
// IT IS THE ONE EXEMPTION FROM THE HELD-CARRIER CHECK. Every LOAD door runs
// service.refuseHeldCarrierTx before its write; this statement does not, on
// purpose. The carrier in hand is routinely one an order is carrying away: the
// station's own swap or removal order claims it at dispatch, and the record of
// what the station put in it lands after that, often after the lift. Refusing
// would write down that parts which are in the carrier are not.
// service.TestBinContentsWriters_TheLoadWriteRunsTheCheck holds its callers to
// those two.
//
// payload_code and the manifest are written together, as every payload_code
// writer on bins writes them. undeclared_carrier_at follows the carrier rule
// for the new payload: COALESCE keeps a standing stamp (the question it
// answers is how long the carrier has been wrong), and the ELSE arm clears a
// stale one when the carrier moves onto a payload it may hold.
//
// Never touched: delta_epoch. The tick's bind must not start a new generation
// (the station's next count would arrive stamped with the old one and be
// discarded); finalize bumps it itself, with the announcement a bump owes.
func RecordProducedTx(tx *sql.Tx, binID int64, payloadCode string, p Produced) error {
	_, err := tx.Exec(`UPDATE bins b SET payload_code=$1,
		manifest=COALESCE($2::jsonb, `+templateManifestSQL+`),
		uop_remaining=COALESCE($3::integer, b.uop_remaining),
		manifest_confirmed=CASE WHEN $3::integer IS NULL THEN b.manifest_confirmed ELSE false END,
		updated_at=CASE WHEN $3::integer IS NULL THEN b.updated_at ELSE NOW() END,
		anomaly_at=CASE WHEN $4 THEN COALESCE(b.anomaly_at, NOW()) ELSE b.anomaly_at END,
		undeclared_carrier_at=CASE WHEN COALESCE($5::boolean, `+UndeclaredCarrierRuleSQL("$1", "b.bin_type_id")+`)
			THEN COALESCE(b.undeclared_carrier_at, NOW()) ELSE NULL END
		WHERE b.id=$6`, payloadCode, p.Manifest, p.Count, p.FlagAnomaly, p.Undeclared, binID)
	return err
}

// loadedAtLayouts are the timestamp layouts resolveLoadedAt accepts, tried
// in order. Edge's wire contract is RFC3339 UTC (produce_plan.go builds
// now.UTC().Format(time.RFC3339)), so that's first. The rest cover zoneless
// inputs that legacy data, admin/manual paths, and tests still produce —
// Postgres-style "2006-01-02 15:04:05" and the T-separated zoneless form,
// each with optional fractional seconds. Go's time.Parse reads a zoneless
// layout as UTC, which is exactly the intent: R20-1 was a zoneless value
// being re-localized to the session TimeZone and skewing FIFO ordering.
// Parsing it as UTC here — with the session TZ also pinned to UTC in
// store.go — closes that gap from both directions instead of rejecting the
// value and poisoning loaded_at with server time.
var loadedAtLayouts = []string{
	time.RFC3339Nano,                      // 2006-01-02T15:04:05[.frac]Z07:00 (subsumes RFC3339)
	"2006-01-02T15:04:05.999999999",       // T-separated, zoneless -> UTC
	"2006-01-02 15:04:05.999999999-07:00", // space-separated, with offset
	"2006-01-02 15:04:05.999999999",       // space-separated, zoneless -> UTC
}

// resolveLoadedAt converts Edge's producedAt into the instant to store in
// loaded_at, always as a definite UTC time.Time. Empty is the normal "no
// explicit timestamp" case and falls back to now. A non-empty value is
// matched against loadedAtLayouts; zoneless layouts resolve as UTC so a
// missing offset is never re-localized to the session TimeZone. Only a value
// matching no layout falls back to now, returning an error so the caller can
// surface the broken input rather than silently poisoning loaded_at.
// Returning a time.Time (not a string) lets the driver bind a definite
// instant into TIMESTAMPTZ rather than a zoneless literal the session would
// re-localize and skew FIFO ordering in FindSourceFIFO.
func resolveLoadedAt(producedAt string, now time.Time) (time.Time, error) {
	if producedAt == "" {
		return now, nil
	}
	for _, layout := range loadedAtLayouts {
		if t, err := time.Parse(layout, producedAt); err == nil {
			return t.UTC(), nil
		}
	}
	return now, fmt.Errorf("unparseable producedAt %q (want RFC3339 or 2006-01-02 15:04:05)", producedAt)
}

// ConfirmManifest marks a bin's manifest as confirmed by an operator.
// producedAt is the Edge-side timestamp (RFC3339) of when the operator
// finalized the bin; empty falls back to server time.
func ConfirmManifest(db *sql.DB, binID int64, producedAt string) error {
	loadedAt, err := resolveLoadedAt(producedAt, clock.Now().UTC())
	if err != nil {
		log.Printf("bins: ConfirmManifest bin %d: %v; using server time", binID, err)
	}
	_, err = db.Exec(`UPDATE bins SET manifest_confirmed=true, loaded_at=$1, updated_at=$3 WHERE id=$2`,
		loadedAt, binID, clock.Now().UTC())
	return err
}

// ConfirmManifestTx is ConfirmManifest inside a caller's transaction. Returns
// the resolved loaded_at plus the bin's current uop_remaining and payload_code
// so the caller can write a same-tx bin_uop_ledger row for the confirm event
// (§16 PR 3 — confirm was previously a silent mutation). Reuses resolveLoadedAt
// so producedAt parsing matches ConfirmManifest.
func ConfirmManifestTx(tx *sql.Tx, binID int64, producedAt string) (loadedAt time.Time, uop int, payloadCode string, err error) {
	var perr error
	loadedAt, perr = resolveLoadedAt(producedAt, clock.Now().UTC())
	if perr != nil {
		log.Printf("bins: ConfirmManifestTx bin %d: %v; using server time", binID, perr)
	}
	if err = tx.QueryRow(`UPDATE bins SET manifest_confirmed=true, loaded_at=$1, updated_at=$3
		WHERE id=$2 RETURNING uop_remaining, payload_code`, loadedAt, binID, clock.Now().UTC()).Scan(&uop, &payloadCode); err != nil {
		err = fmt.Errorf("confirm manifest bin %d: %w", binID, err)
	}
	return loadedAt, uop, payloadCode, err
}

// GetManifest fetches a bin and parses its manifest. The Bin type
// owns the JSON-decoding logic (domain.Bin.ParseManifest); this
// helper just stitches a DB read to that pure-data step.
func GetManifest(db *sql.DB, binID int64) (*Manifest, error) {
	bin, err := Get(db, binID)
	if err != nil {
		return nil, err
	}
	return bin.ParseManifest()
}

// FindSourceFIFO finds the best unclaimed bin at an enabled storage node
// matching the given payload code, using FIFO ordering.
// FindSourceFIFO looks for the FIFO-oldest manifest-confirmed bin matching
// payloadCode at an enabled storage node. excludeNodeID > 0 skips bins at
// that node. Pass the order's destination node so a same-node retrieve is
// impossible. See SHINGO_TODO.md "Same-node retrieve" entry.
//
// The whole eligibility question is bins.BinSourceableSQL; this reader adds
// only FIFO order and the same-node exclusion. It has no private spelling of
// status, holds, node liveness or the bin-type rule, and must not grow one. (It
// spelled the quality hold by hand, which no other sourcing reader asked, until
// the hold went into the stock half; TestSourcingReadersSpellNothingThemselves
// now names the column.)
//
// ── THE OFF-SPEC STATUS WARNING NOW APPLIES PLANT-WIDE ────────────────────
//
// This query once carried a REJECT-LIST — `status NOT IN ('staged',
// 'maintenance','flagged','retired', ...)`, plus a status value since retired
// (v138) — which answers TRUE for any value it does not name. The status column carries no CHECK constraint
// (domain.BinStatus says so, and write-time validation is deferred on purpose
// so operators can set off-spec states during incident recovery), so a
// hand-corrected row, or a seventh status added to the enum and not to the
// list, was SOURCEABLE by default. It was converted to the allow-list first,
// and for a while this was the only site where that changed behaviour.
//
// It is not any more. On 2026-09-14 the remaining four hand-spelled
// reject-lists — the sourceability pool, the pool breakdown, the changeover
// preflight and its presence count — were deleted in favour of the same
// allow-list. Off-spec statuses now fail closed everywhere, which is the
// intended direction: fail-closed is right for "should a robot drive to this
// bin". But it means a plant carrying off-spec status values sees those bins
// stop being sourced AND stop being counted, and it presents as a material
// shortage rather than as a rejection. Worth a
// `SELECT status, count(*) FROM bins GROUP BY 1` before deploying to a site.
func FindSourceFIFO(db *sql.DB, payloadCode string, excludeNodeID int64) (*Bin, error) {
	// Empty payloadCode is always a bug here. After the bin-as-truth
	// refactor, unattached bins store payload_code = "" instead of
	// NULL, so `WHERE b.payload_code = $1` with $1 = "" silently
	// matches every unattached bin. Reject at the boundary; mirror the
	// no-match sentinel returned by ScanBin so callers don't need new
	// error handling.
	if payloadCode == "" {
		return nil, sql.ErrNoRows
	}
	row := db.QueryRow(fmt.Sprintf(`%s
		WHERE b.payload_code = $1
		  AND %s
		  AND ($2 = 0 OR b.node_id != $2)
		-- b.id breaks a load-time tie: the seeder and a burst of arrivals stamp
		-- loaded_at to the same instant, and LIMIT 1 over a tie was whatever the
		-- plan returned (B1, TestB1_FIFOTieBreaksByID).
		ORDER BY COALESCE(b.loaded_at, b.created_at) ASC, b.id ASC
		LIMIT 1`, BinJoinQuery, BinSourceableSQL("$1")), payloadCode, excludeNodeID)
	return ScanBin(row)
}
