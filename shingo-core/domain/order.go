package domain

import (
	"time"

	"shingo/protocol"
)

// Order is a unit of work produced by the edge-station protocol and
// executed by the fleet. An Order tracks its source and delivery
// nodes, vendor-side identifiers once dispatched, the claim on a bin
// (for simple orders), and a parent/sequence pair for complex orders
// whose child legs are separate Order rows.
//
// StepsJSON is the serialised step list for complex orders; WaitIndex
// marks how many wait segments have been released. BinID is set for
// simple orders once the resolver picks a source bin; complex orders
// use the order_bins junction (OrderBin) to track multiple claimed
// bins, one per step.
type Order struct {
	ID            int64              `json:"id"`
	EdgeUUID      string             `json:"edge_uuid"`
	StationID     string             `json:"station_id"`
	OrderType     protocol.OrderType `json:"order_type"`
	Status        protocol.Status    `json:"status"`
	Quantity      int64              `json:"quantity"`
	SourceNode    string             `json:"source_node"`
	DeliveryNode  string             `json:"delivery_node"`
	ProcessNode   string             `json:"process_node,omitempty"`
	VendorOrderID string             `json:"vendor_order_id"`
	VendorState   string             `json:"vendor_state"`
	RobotID       string             `json:"robot_id"`
	Priority      int                `json:"priority"`
	PayloadDesc   string             `json:"payload_desc"`
	ErrorDetail   string             `json:"error_detail"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
	CompletedAt   *time.Time         `json:"completed_at,omitempty"`
	ParentOrderID *int64             `json:"parent_order_id,omitempty"`
	Sequence      int                `json:"sequence"`
	StepsJSON     string             `json:"steps_json,omitempty"`
	BinID         *int64             `json:"bin_id,omitempty"`
	PayloadCode   string             `json:"payload_code"`
	WaitIndex     int                `json:"wait_index"`
	QueueReason   string             `json:"queue_reason,omitempty"`
	// QueueCode is the structured category behind QueueReason — one of the
	// protocol.QueueCode values (waiting_for_material, waiting_for_slot, …).
	// The sentence is generated FROM this code by the dispatch formatter, so the
	// two never drift; analytics groups by this column, the operator reads the
	// sentence. Empty on non-queued orders and on pre-schema rows (nullable in
	// the DB). See protocol.AllQueueCodes.
	QueueCode string `json:"queue_code,omitempty"`
	// QueueCause is the engineer-only call-site tag naming WHERE in the dispatch
	// pipeline the wait arose (intake-resolve, swap-hold, dropoff-occupied, …).
	// Operators never see it; engineers group by it when a code trends wrong.
	// Stays Core-side — never crosses the wire to Edge. Empty on non-queued rows.
	QueueCause      string `json:"queue_cause,omitempty"`
	SkipAutoConfirm bool   `json:"skip_auto_confirm"`
	// SiblingOrderUUID is the edge UUID of the paired leg in a two-robot
	// swap — the supply's UUID recorded on the evac row (and vice-versa).
	// Written ATOMICALLY in the CreateOrder INSERT so a two-robot evac's
	// link to its supply can never be lost by a failed post-create link
	// step; the swapLegHeld starvation gate depends on it to avoid
	// the ALN_003 line-strand. "" for every non-swap order.
	SiblingOrderUUID string `json:"sibling_order_uuid,omitempty"`
	// SourceIntent classifies how a plain order sources its bin (full / empty /
	// node-local) — the Stage-4 data home replacing the OrderType reads in the
	// source finder + scanner. Set once at intake; "" (full) for retrieves and
	// for orders that don't source through the finder. See dispatch.SourceIntent*.
	SourceIntent string `json:"source_intent,omitempty"`
	// Coordinated is the dispatch-provenance discriminator: true iff the order
	// carries an Edge-authored coordinated (multi-leg) plan and must take the
	// coordinated dispatch tail (role gate + complex reserve/confirm); false for a
	// plain single-transport order. Stamped ONCE at Core intake — complex intake
	// stamps true, every other intake stamps false (the zero value). It REPLACES
	// the StepsJSON != "" heuristic IsCoordinated used, which becomes unsound once
	// simple plans persist to StepsJSON. See dispatch.IsCoordinated.
	Coordinated bool `json:"coordinated"`
	// RemainingUOP is the operator's declared release-correction count, carried
	// from intake to the bin claim. It is NOT a transport property — it is the
	// count an operator declared at a Material-page Release (edge
	// CreateMoveOrderWithUOP → OrderRequest.RemainingUOP) that seeds the bin's
	// manifest sync at claim. The claim-move from intake to the scanner, which has
	// no envelope, means the value rides on the order row. nil = no sync (plain
	// claim); >0 syncs; <=0 clears. In practice only a move carries it (retrieve
	// carries none; an empty carrier forces nil). Bridge field: the unified-create
	// follow-up carries the count in the persisted plan and this retires. See
	// dispatch.planTransport.
	RemainingUOP *int `json:"remaining_uop,omitempty"`
	// OriginID is the demand episode this order was created to serve, and
	// OriginClass says whether it should have had one. STAMPED FORWARD at the
	// create site, never resolved by walking parent_order_id: that column is
	// written in exactly one place in all of shingo-core (dispatch/compound.go),
	// is one level deep, and the synthetic restore parent sets none at all — so a
	// read-time walk dead-ends at exactly the boundary the rule exists to cross.
	//
	// An order created in service of another order inherits its origin AND its
	// class. sibling_order_uuid is the precedent: a durable forward link written
	// with the row rather than reconstructed on read.
	//
	// OriginClass is one of protocol.OriginClass* and is what makes
	// `origin_id IS NULL` answerable — without it that predicate selects every
	// consume-side order, every opportunistic stage and every admin action, with
	// the actual lost origins buried in there. Only `orphan` is a finding.
	OriginID    string `json:"origin_id,omitempty"`
	OriginClass string `json:"origin_class,omitempty"`
	// OpenForChildren says a compound parent may still gain children.
	// SEALED is exactly !OpenForChildren -- "sealed" is the concept's name
	// everywhere else (SealDigGroup, the two-holds work order, the design's
	// join and fold sections), and this is the field that carries it, so a
	// grep for "sealed" arrives here.
	//
	// Two readers decide a reshuffle is FINISHED from "all its children are
	// terminal" -- AdvanceCompoundOrder's success arm and
	// AdvanceStuckReshuffleParents. That inference is sound only while every
	// child exists up front, which is true today and stops being true under the
	// fold, where all-terminal is the ordinary state BETWEEN moves. Everything
	// else that walks the child list is asking a different question ("is
	// anything running right now", "is this one child live") and must not
	// consult this.
	//
	// NAMED FOR THE EXCEPTION ON PURPOSE. The zero value of this struct is
	// false, the column's default is false, and both mean SEALED -- so the safe
	// reading is what you get by forgetting, in Go and in Postgres alike. A
	// `Sealed bool` field would have zero-valued to "open" and disagreed with
	// its own column. Openness is never inherited; it is written.
	OpenForChildren bool `json:"open_for_children,omitempty"`
	// DigTargetNode names the slot holding the bin a service dig exists to
	// uncover, and is empty on every order that is not a service dig's parent.
	//
	// It is what lets a dig's lane lock span the excavation AND the retrieval
	// it was raised for. The lane is released when the TARGET BIN leaves rather
	// than when the last blocker places, which closes the window where a
	// cancelled claim leaves an uncovered bin sitting in an open lane.
	// DigStillOwesItsTarget asks whether a bin is still standing at this slot
	// -- a PHYSICAL fact (law 4), not an inference from any order's status --
	// and that is the deliberate part: the bin leaving by ANY mover ends the
	// hold, including a mover with no connection to the demand that asked for
	// the dig, because what the lock protects is the bin's exposure and nothing
	// else.
	//
	// A LEGITIMATE NON-TERMINAL STATE. A dig still owing its target sits in
	// `reshuffling` with every child terminal, which is the exact shape a
	// completion arm reads as FINISHED.
	//
	// This paragraph used to name three readers that consult it. Two went with
	// the hand-back, under: "the demand is no longer re-parented into its own
	// dig, so nothing resumes and the lane is not held past the compound's
	// completion." §R.91 made that false — a demand does resume — but the reader
	// count did not change, because this column is a FOLDER's record of a debt
	// and a re-parented demand writes none. Its collector is itself, and gate 2's
	// self-handoff takes that branch before this is read. ONE reader survives
	// (dispatch/dig_lock_release.go's handoff). The rule is unchanged —
	// everything else that walks the child list is asking a different question
	// and still must not consult this.
	//
	// Empty is the safe reading in both languages, for the same reason
	// OpenForChildren is named for its exception: a bare orders.Order has no
	// target outstanding and releases the way it did before this field existed,
	// so the longer hold cannot be inherited by omission. It is written on
	// purpose.
	DigTargetNode string `json:"dig_target_node,omitempty"`
}
