// demand_origin.go — the row shape for a demand episode (migration 59).
//
// The queries live in store/demand_origins.go, which explains the state-transfer
// design and the revision guard. The type is here so www handlers can name it
// without importing the store — the same reason sourceability_event.go is here.

package domain

import (
	"time"

	"shingo/protocol"
)

// DemandOrigin is one demand episode: a continuous period during which a
// specific place needed material.
//
// It carries only what Edge authors. signal_count is a CORE-OWNED column and is
// deliberately absent — see store.UpsertDemandOrigin, which must not zero it.
//
// NO PARTIAL-DELIVERY SIGNAL EXISTS. READ THIS BEFORE BUILDING 5.3. Stage 5.3
// wanted "UOP delivered vs expected UOP" per episode. uop_delivered was planned
// as its Core-owned numerator; like parent_origin_id it never had a writer or a
// reader (2026-07-28, and the A9 census of 2026-11-20), and v184 drops both.
// A 5.3 needs, in order: a numerator with a writer that can say "never
// accumulated" apart from a measured zero (a NOT NULL DEFAULT 0 cannot), and a
// denominator, which no column holds: expected_orders (nullable by design,
// below) times payloads.uop_capacity, itself NOT NULL DEFAULT 0, so a payload
// with no recorded capacity reads as one with a capacity of zero. Until then the
// honest rendering of a partial-delivery ratio is no-data for every row.
type DemandOrigin struct {
	OriginID   string
	Revision   int64
	EpisodeKey string
	Kind       string
	// Direction holds the cell's ROLE — produce or consume, the claim's own two
	// values. It held "supply"/"evacuate", a second vocabulary for the same fact,
	// and migration 87 rewrote the stored rows. Typed so a reader cannot compare
	// it against a word that no longer exists. Empty for the kinds with no cell
	// behind them. The COLUMN keeps the name `direction`; renaming it on both
	// services is a cosmetic follow-on, deliberately not bundled here.
	Direction  protocol.ClaimRole
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
	// The threshold formula divides by the catalog's UOPCapacity and the
	// sizing entry point (dispatch.BinsToReachThreshold) explicitly refuses
	// capacity <= 0, so an unknowable denominator genuinely
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

// Close reasons only the forecaster writes (D2, DESIGN-D2 §4), on a forecast
// that will not open. Core-only: they never cross the wire. A forecast that
// opens is not closed at all; it changes tense and goes on to settle.
const (
	// CloseReasonExpired: the place's projected need passed, plus grace, and it
	// never opened. A miss.
	CloseReasonExpired = "expired"
	// CloseReasonRecoveredUnattributed: the place's stock rose with no demand
	// open, so its projected need moved later past the band. A hit by another
	// hand.
	CloseReasonRecoveredUnattributed = "recovered_unattributed"
	// CloseReasonWithdrawn: the place left the population (claim retired, style
	// changed, binding removed or zeroed). Neither hit nor miss.
	CloseReasonWithdrawn = "withdrawn"
)

// ForecastRecord is an unsettled threshold or cell record as the forecaster
// reads it: forecast or open, with its plan and projection.
type ForecastRecord struct {
	OriginID        string
	EpisodeKey      string
	Kind            string
	Open            bool
	Plan            *PlannedPick
	PlanVerdict     string
	NeedAt          *time.Time
	PredictedNeedAt *time.Time
}

// ForecastBirth is a place's first forecast record.
type ForecastBirth struct {
	OriginID     string
	EpisodeKey   string
	Kind         string
	Direction    protocol.ClaimRole
	StationID    string
	ProcessID    string
	CoreNodeName string
	PayloadCode  string
	Threshold    int
	Plan         *PlannedPick
	PlanVerdict  string
	NeedAt       *time.Time
}

// ForecastView is one forecast as the ahead view shows it: the place, the bin
// the firing door would take (nil when none), why not when none, and when the
// place will need it.
type ForecastView struct {
	EpisodeKey      string
	Kind            string
	StationID       string
	CoreNodeName    string
	PayloadCode     string
	Plan            *PlannedPick
	PlanVerdict     string
	NeedAt          *time.Time
	PredictedNeedAt *time.Time
}

// ForecastScore is the forecasts' record since a moment (D2.3): opened (a hit;
// OpenedProjected of them carried a projection, MedianLateness is opened −
// predicted over those, positive = opened after the forecast said), expired
// (a miss), recovered by another hand, and withdrawn (neither).
//
// Born is every record ever born a forecast and Forecasting those still
// forecasts now; neither is limited by the moment.
type ForecastScore struct {
	Born, Forecasting                                      int
	Opened, OpenedProjected, Expired, Recovered, Withdrawn int
	MedianLateness                                         *time.Duration
}
