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
type DemandOrigin struct {
	OriginID   string
	Revision   int64
	EpisodeKey string
	Kind       string
	Direction  string
	Trigger    string
	TriggerRef string
	StationID  string

	ProcessID    int64
	CoreNodeName string
	PayloadCode  string

	OpenedAt time.Time

	OpenedTotal int
	Threshold   int

	// ExpectedOrders is NULLABLE BY DESIGN, and the nil is load-bearing.
	//
	// The threshold formula divides by the catalog's UOPCapacity and tryCreateL1
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
