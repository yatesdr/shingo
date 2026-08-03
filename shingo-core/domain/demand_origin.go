// demand_origin.go — the row shape for a demand episode (migration 59).
//
// The queries live in store/demand_origins.go, which explains the state-transfer
// design and the revision guard. The type is here so www handlers can name it
// without importing the store — the same reason sourceability_event.go is here.

package domain

import "time"

// DemandOrigin is one demand episode: a continuous period during which a
// specific place needed material.
//
// It carries only what Edge authors. signal_count, uop_delivered,
// used_edge_reports and parent_origin_id are CORE-OWNED columns and are
// deliberately absent — see store.UpsertDemandOrigin, which must not zero them.
//
// ── uop_delivered HAS NO WRITER. READ THIS BEFORE BUILDING 5.3. ──────────────
//
// "Core-owned" above describes the DESIGN. As of 2026-07-28 it is not the
// state of the code: `uop_delivered` has a migration, a NOT NULL DEFAULT 0, a
// protective clause in the upsert's SET list and a test asserting an Edge
// message cannot zero it — and NOTHING IN THIS REPOSITORY EVER WRITES IT, and
// nothing reads it back. A whole-repo grep returns eight hits: two comments,
// four lines of one test, the migration, and the schema snapshot.
//
// It is the failure ListOrphanFindings' own doc comment names — "a new column
// is not done until something reads it back and asserts on the value" — caught
// one stage earlier than closed_by was. closed_by shipped with every writer and
// no reader. This shipped with a PROTECTOR and no writer, which is worse in one
// specific way: the protecting test seeds the column with raw SQL and asserts
// it survives, so the column looks exercised. It is not.
//
// WHY THAT BLOCKS STAGE 5.3 RATHER THAN MERELY COMPLICATING IT. 5.3 wants
// "uop_delivered vs expected UOP" as a partial-delivery signal. The numerator is
// structurally 0 on every real row, so the surface would report every episode as
// having delivered none of its expected UOP — maximally partial, on every row,
// on every plant, forever, with a green test suite. And because the column is
// NOT NULL DEFAULT 0, the type cannot carry the difference between "measured
// zero" and "never accumulated": the absence was destroyed at the column
// definition, one layer below anything the renderer can fix. That is the
// COALESCE(x, 0) defect the number doctrine is built around, already committed
// in the schema.
//
// The denominator is a separate problem and would still be one after the
// numerator is fixed: there is no expected_uop column, so expected UOP must be
// RECONSTRUCTED as expected_orders x payloads.uop_capacity — where
// expected_orders is nullable by design (below), uop_capacity is catalog-side
// and joined by payload_code, and uop_capacity is itself NOT NULL DEFAULT 0, so
// a payload with no recorded capacity is indistinguishable from one with a
// capacity of zero. That is three more absence cases on a surface whose whole
// discipline is not collapsing them.
//
// So 5.3 is not a scan change. In order: give uop_delivered a writer, decide
// whether "never accumulated" needs to be representable (it does, and that is a
// migration), then reconstruct the denominator. Until the first of those, the
// honest rendering of a partial-bin ratio is no-data for every row, which is a
// column nobody needs.
type DemandOrigin struct {
	OriginID   string
	Revision   int64
	EpisodeKey string
	Kind       string
	Direction  string
	Trigger    string
	TriggerRef string
	StationID  string

	// ProcessID is the Edge process NAME ("SNF2"), the same value
	// process_styles.process_id and style_claims.process_id carry — which is
	// what makes those tables joinable with this one. It was an Edge SQLite row
	// id (BIGINT), unjoinable with either. See migration v63 and
	// protocol.CellEpisodeKey.
	ProcessID    string
	CoreNodeName string
	PayloadCode  string

	OpenedAt time.Time

	OpenedTotal int
	Threshold   int

	// ExpectedOrders is NULLABLE BY DESIGN, and the nil is load-bearing.
	//
	// The threshold formula divides by the catalog's UOPCapacity and fireThresholdL1
	// explicitly guards capacity <= 0, so an unknowable denominator genuinely
	// happens. NOT 0 and NOT 1 — both are lies that render as a real ratio, and
	// a demand whose denominator is unknowable is a DIFFERENT STATE from one
	// whose denominator is 1. The pointer is what carries that difference as far
	// as the renderer; an int would have destroyed it at the scan.
	ExpectedOrders        *int
	ExpectedUnknownReason string

	RerequestCount int
	Discretionary  bool

	ClosedAt    *time.Time
	CloseReason string

	// ClosedBy is "" when the column is NULL, which is a THIRD state beside the
	// two named mechanisms: the sender did not say — an older Edge, or a row
	// written before the column existed. The column has no default precisely so
	// that state survives, and a reader that folds it into either named path
	// defeats the reason it has none.
	ClosedBy string
}

// Close reasons Core assigns on its own. Edge's live in protocol.
const (
	// CloseReasonSuperseded — a NEW episode opened for a place this one still
	// held open, which is proof this one ended: Edge enforces one open episode
	// per episode_key with a PRIMARY KEY, so it could not have minted the new
	// one while this was still open there.
	//
	// It is a PLACEHOLDER, not a verdict. The real close is still in flight or
	// was dead-lettered; if it lands, its higher revision overwrites this with
	// the true reason. And if it never lands, "superseded" still says more than
	// "unattributed" — it says we know this ended because something else started
	// here, we just never heard how.
	CloseReasonSuperseded = "superseded"
)

// DemandEpisode is an episode joined to its child-order count — the read model
// the demand browser renders from.
//
// Children is COUNTED, never stored: cost is COUNT(children), duration is a
// subtraction, ratio is a division. Nothing computed is persisted, because a
// stored rollup starts drifting from what it summarises — the uopCache lesson,
// where a private incremental tally left Springfield reading 139 against a
// truth of 31.
type DemandEpisode struct {
	DemandOrigin

	// Children is the number of orders stamped with this origin_id. A real
	// measured count, and ZERO IS ITS WORST VALUE, not its mildest — zero orders
	// against a real demand is the thing this surface exists to show.
	Children int
}
