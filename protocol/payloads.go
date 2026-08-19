package protocol

import (
	"encoding/json"
	"time"
)

// Data is the payload for TypeData messages.
// Subject selects the sub-schema; Body carries the subject-specific data.
type Data struct {
	Subject string          `json:"subject"`
	Body    json.RawMessage `json:"data"`
}

// --- Edge lifecycle data schemas ---

// CellProcessBinding is one reporting point inside a cell-catalog entry — the
// (process, style, PLC tag) tuple an edge scores. Part of the Q-034 cell
// catalog; additive only.
type CellProcessBinding struct {
	ProcessID int64  `json:"process_id"`
	StyleID   int64  `json:"style_id"`
	PLCName   string `json:"plc_name"`
	TagName   string `json:"tag_name"`
}

// CellCatalogEntry groups an edge's reporting points by PLC into a "cell" Core
// can offer in the cell picker (Q-034). CellLabel is the grouping PLCName.
type CellCatalogEntry struct {
	CellLabel string               `json:"cell_label"`
	Processes []CellProcessBinding `json:"processes"`
}

// EdgeRegister is sent by an edge on startup.
//
// StationID CARRIES THE STATION UID, and the field is deliberately NOT
// renamed. Address.Station is a routing selector whose VALUE is the identity
// Core minted at enrollment, so the identity change is a change in what the
// string means and who mints it — not in the field's name. Renaming would have
// broken the one direction the rollout actually creates: Edge deploys before
// Core, so for a window a NEW edge talks to an OLD core, which would read
// StationID as "".
//
// Instance is a random id the edge draws ONCE PER PROCESS at boot. It exists
// for the case the hostname check is blind to: two Pis flashed from one SD
// image share a hostname AND a station_uid, and only the instance distinguishes
// them. Additive and omitempty — an old core ignores it and an old edge omits
// it, which Core reads as "cannot judge", the same as an empty hostname.
//
// Catalog is an ADDITIVE Q-034 field (omitempty): an old core unmarshals and
// ignores it, an old edge simply omits it — absent catalog means "no catalog",
// not an error. No envelope/version bump (see version-skew-research.md).
//
// LineIDs is RETIRED. It shipped []string{cfg.LineID} regardless of any station
// override — always ["line-1"] — and its only consumer composed
// station + "." + line into 'plant-a.line-1.line-1', a dashboard scope no row
// from ListOrderStations() can match. A field that carries one constant into
// one wrong answer is not vestigial, it is a defect with a schema.
type EdgeRegister struct {
	StationID string             `json:"station_id"`
	Hostname  string             `json:"hostname"`
	Instance  string             `json:"instance,omitempty"`
	Version   string             `json:"version"`
	Catalog   []CellCatalogEntry `json:"catalog,omitempty"`
}

// EdgeHeartbeat is sent periodically by an edge.
type EdgeHeartbeat struct {
	StationID string `json:"station_id"`
	Uptime    int64  `json:"uptime_s"`
	Orders    int    `json:"active_orders"`
}

// EdgeRegistered acknowledges edge registration.
type EdgeRegistered struct {
	StationID string `json:"station_id"`
	Message   string `json:"message,omitempty"`
}

// EdgeHeartbeatAck acknowledges a heartbeat.
type EdgeHeartbeatAck struct {
	StationID string    `json:"station_id"`
	ServerTS  time.Time `json:"server_ts"`
}

// --- Order payloads: Edge -> Core ---

// OrderRequest is a new transport order from edge.
type OrderRequest struct {
	OrderUUID     string    `json:"order_uuid"`
	OrderType     OrderType `json:"order_type"`
	PayloadCode   string    `json:"payload_code,omitempty"`
	PayloadDesc   string    `json:"payload_desc,omitempty"`
	Quantity      int64     `json:"quantity"`
	DeliveryNode  string    `json:"delivery_node,omitempty"`
	SourceNode    string    `json:"source_node,omitempty"`
	StagingNode   string    `json:"staging_node,omitempty"`
	LoadType      string    `json:"load_type,omitempty"`
	Priority      int       `json:"priority,omitempty"`
	RetrieveEmpty bool      `json:"retrieve_empty,omitempty"`
	// RemainingUOP: nil = no sync, 0 = clear manifest, >0 = partial consumption.
	RemainingUOP *int `json:"remaining_uop,omitempty"`
	// SkipAutoConfirm prevents Core's reconciliation sweep from auto-confirming
	// this order when it is stuck at "delivered". Edge sets this for side-cycle
	// orders (L1 loader empty-in, U1 unloader full-in) where a human operator
	// must explicitly confirm after performing a physical action (loading or
	// unloading the bin). Without this, Core auto-confirms the moment the bin
	// arrives, bypassing the operator and immediately triggering the outbound
	// leg (L2/U2) while the bin is still empty/full.
	SkipAutoConfirm bool `json:"skip_auto_confirm,omitempty"`
	// OriginID / OriginClass attribute this order to the demand episode that
	// asked for it. SEAM 2, Edge -> Core. Additive omitempty; SiblingOrderUUID
	// on ComplexOrderRequest is the exact template.
	//
	// An order arriving with no OriginID from an Edge that predates this lands
	// as an ORPHAN, not an error — see OriginClass.
	OriginID    string `json:"origin_id,omitempty"`
	OriginClass string `json:"origin_class,omitempty"`
}

// Order origin classes. THREE VALUES, AND AGING DOES NOT ADD A FOURTH.
//
// Without the enum, `origin_id IS NULL` selects every consume-side order, every
// opportunistic stage, every admin action, and — buried in there — the actual
// lost origins. A haystack with the needle in it.
//
// An `orphan_aged` value existed here briefly and is gone: an aged orphan is an
// `orphan` whose orders.orphan_aged_at is set (Core migration 61), and
// aged-ness is DERIVED from that timestamp.
//
// origin_class answers HOW DID THIS ORDER RELATE TO A DEMAND. That is a
// create-time fact and it is true forever. Letting a clock mutate it means the
// row can no longer answer what its class was at creation — a fact overwritten
// by a derivation, which is the uopCache mistake in a fourth costume. It also
// matches the shape the schema already uses everywhere for exactly this:
// closed_at beside close_reason, anomaly_at beside the bin. A nullable
// timestamp NEXT TO the fact, never a state value inside it.
//
// AN AGED ORPHAN IS STILL A FINDING. Aging changes which lane it sits in and
// who is expected to act on it, not whether it is a problem. There is no
// deferred attach — an orphan that later matches an open episode stays orphaned
// and reconciles by a human — so the expiry exists only because an alarm that
// never clears is indistinguishable from a broken one. Never deleted, never
// auto-attached.
//
// Edge stamps only attached and no_demand; orphan is assigned on Core, at
// intake, when an order arrives with no origin on it.
const (
	// OriginClassAttached: the order has an episode.
	OriginClassAttached = "attached"
	// OriginClassNoDemand: structurally originless, and stamped as such AT THE
	// CREATE SITE, where it is known. Opportunistic loader staging, unloader
	// full-ins, Core direct orders, Core spot orders. Not a finding.
	OriginClassNoDemand = "no_demand"
	// OriginClassOrphan: it should have had an episode and didn't. THE ONLY ONE
	// THAT IS A FINDING.
	OriginClassOrphan = "orphan"
)

// OrderCancel cancels an existing order.
type OrderCancel struct {
	OrderUUID string `json:"order_uuid"`
	Reason    string `json:"reason"`
}

// CancelReasonAcceptHalfSwap marks a supply-leg cancel where the operator
// explicitly ACCEPTED the half-swap: the fleet-committed partner evac must NOT
// be cancelled in response (axiom: Core cannot recall a robot mid-drive, so
// cancelling a committed evac is not an option — accepting the half-swap is).
// Core's swap-peer unwind switches on this exact reason; every other cancel
// reason keeps the fail-closed peer-cancel behaviour.
const CancelReasonAcceptHalfSwap = "accept_half_swap"

// OrderReceipt confirms delivery acceptance.
type OrderReceipt struct {
	OrderUUID   string `json:"order_uuid"`
	ReceiptType string `json:"receipt_type"`
	FinalCount  int64  `json:"final_count"`
}

// OrderRedirect changes the delivery destination.
type OrderRedirect struct {
	OrderUUID       string `json:"order_uuid"`
	NewDeliveryNode string `json:"new_delivery_node"`
}

// --- Order payloads: Core -> Edge ---

// OrderAck confirms order acceptance.
type OrderAck struct {
	OrderUUID     string `json:"order_uuid"`
	ShingoOrderID int64  `json:"shingo_order_id"`
	SourceNode    string `json:"source_node,omitempty"`
}

// OrderWaybill assigns a robot.
type OrderWaybill struct {
	OrderUUID string `json:"order_uuid"`
	WaybillID string `json:"waybill_id"`
	RobotID   string `json:"robot_id,omitempty"`
	ETA       string `json:"eta,omitempty"`
}

// OrderUpdate provides a status change.
type OrderUpdate struct {
	OrderUUID string `json:"order_uuid"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	ETA       string `json:"eta,omitempty"`
	// QueueReason carries the blocking signal from Core's orders.queue_reason when
	// Status is "queued" — e.g. "no bin of requested payload in node group AMR
	// Supermarket". Omitted (not cleared) for non-queued updates; omitempty means
	// an absent field signals "leave unchanged", not "clear". Core emits this from
	// the EventOrderQueued handler after the fulfillment scanner completes its pass.
	QueueReason string `json:"queue_reason,omitempty"`
	// QueueCode is the structured category behind QueueReason (protocol.QueueCode).
	// Additive: old Edge ignores it; new Edge with old Core sees "" and falls back
	// to the sentence. Ships alongside QueueReason so Edge can persist the code
	// for future branching without a schema change.
	QueueCode string `json:"queue_code,omitempty"`
}

// OrderDelivered signals fleet delivery complete.
//
// Item 8 of the bin-as-truth refactor dropped the BinUOPRemaining
// snapshot field. Pre-Item-8 the snapshot rode through this envelope
// so Edge could reset the lineside counter from the bin's actual
// contents on partial returns. Post-Item-8 the runtime cache is the
// source of truth for "what's in the bin right now"; Edge always
// resets to claim.UOPCapacity on delivery and the reconciler heals
// to Core's authoritative value within the next 60s pass. The
// trade-off — a brief "looks like full bin" UI on partial-back
// returns until the heal — is SME-accepted.
type OrderDelivered struct {
	OrderUUID      string     `json:"order_uuid"`
	DeliveredAt    time.Time  `json:"delivered_at"`
	StagedExpireAt *time.Time `json:"staged_expire_at,omitempty"`
	// BinID carries the bin's Core-side ID so Edge can attribute PLC
	// tick deltas to the right bin in the Phase 1 bin-as-truth flow.
	// Nil for multi-bin orders; Phase 1 emits bucket deltas only in
	// that case and bin attribution waits for the multi-bin handling
	// refinement. Older Core/Edge builds tolerate either side being
	// nil.
	BinID *int64 `json:"bin_id,omitempty"`

	// UOPRemaining and DeltaEpoch are the bin's authoritative count and
	// load-lifecycle epoch as of delivery, snapshotted by Core right
	// after the bin lands at the destination (handleOrderDelivered, after
	// applyBinArrivalForOrder). Edge seeds its runtime cache and stamps
	// outgoing BinUOPDeltas from these — so the seed and the epoch ride
	// the same Kafka message as the delivery itself, with no separate
	// HTTP pull. UOPRemaining is a pointer so an older Core that doesn't
	// send it (nil) is distinguishable from a genuinely-empty bin (0);
	// Edge falls back to its role default in the nil case. DeltaEpoch 0
	// is the pre-migration / unknown sentinel. Both are only meaningful
	// for single-bin orders (BinID != nil).
	UOPRemaining *int  `json:"uop_remaining,omitempty"`
	DeltaEpoch   int64 `json:"delta_epoch,omitempty"`
	// DeliveryNode is the Core dot-name of the destination. Populated for all
	// orders so the Edge can bind the runtime cache even when the order was
	// created on Core directly (no Edge order row). Broadcast to all edges when
	// the order has no station (stationID==""); omitempty so older Edge builds
	// ignore the field silently.
	DeliveryNode string `json:"delivery_node,omitempty"`

	// BinDestNode is the Core dot-name of the node the carried bin came to rest
	// at — set ONLY for multi-tote deliveries (F1b), where BinID names the one
	// order_bin destined for the consuming process node. It is the per-bin
	// landing node, which for a swap is authoritative and unambiguous, unlike
	// DeliveryNode (the last dropoff — the supermarket for the evac leg) or a
	// steps-derived finalDropoff. Edge binds when BinDestNode == its process
	// node's CoreNodeName. Empty for single-bin orders, whose existing
	// delivery-gate (steps finalDropoff / DeliveryNode) is unchanged.
	BinDestNode string `json:"bin_dest_node,omitempty"`
}

// BinPickedUp notifies Edge that a robot has physically picked up a
// bin from the source location. Item 11 of the bin-as-truth refactor:
// the SEND PARTIAL BACK flow leaves a partial bin at the line while
// the cell keeps cycling. PLC ticks during that pickup window must
// keep attributing to the released bin until the robot actually
// grabs it. Once picked up, Edge flushes the released bin's delta
// accumulator and advances the active claim to the next bin.
//
// Sent on subject SubjectBinPickedUp. Routed via Core's HandleData.
// Edge crash during the pickup window is handled by the reconciler:
// the released bin's count may be biased by a tick or two, accepted
// per SME (open-items.md Q2”).
type BinPickedUp struct {
	OrderUUID  string    `json:"order_uuid"`
	BinID      int64     `json:"bin_id"`
	Location   string    `json:"location"`
	PickedUpAt time.Time `json:"picked_up_at"`
}

// OrderError signals order failure.
type OrderError struct {
	OrderUUID string `json:"order_uuid"`
	ErrorCode string `json:"error_code"`
	Detail    string `json:"detail"`
}

// OrderSkipped signals an order reached a terminal "skipped" state — the
// work was never needed, distinct from a failure. Today the sole producer
// is the complex-order dispatcher detecting "no bins at any pickup node"
// (the source was emptied externally before the order dispatched). Wire
// shape mirrors OrderError so handlers stay parallel.
type OrderSkipped struct {
	OrderUUID string `json:"order_uuid"`
	ErrorCode string `json:"error_code"`
	Detail    string `json:"detail"`
}

// OrderCancelled confirms order cancellation.
type OrderCancelled struct {
	OrderUUID string `json:"order_uuid"`
	Reason    string `json:"reason"`
}

// --- Complex order payloads ---

// ComplexOrderStep describes a single step in a complex (multi-leg) order.
// Node can be a concrete node name or a group node name — Core auto-detects
// and resolves groups via the group resolver.
type ComplexOrderStep struct {
	Action string `json:"action"`         // "pickup", "dropoff", "wait"
	Node   string `json:"node,omitempty"` // node or group name (Core auto-resolves groups)
	// Empty marks a pickup leg that must fetch an EMPTY carrier rather than a
	// payload-matching full — the produce-node "bring an empty to fill" leg
	// (the store dual of a consume node's full retrieve). When true on a
	// pickup, Core resolves an NGRP source to a slot holding an empty bin and
	// claims an empty carrier, instead of the default full-retrieve semantics.
	// Pickup-only; ignored on dropoff/wait. Backward-compatible: absent/false
	// preserves the prior always-full behavior, so an older Core that ignores
	// the field behaves exactly as today.
	Empty bool `json:"empty,omitempty"`
	// WaitKind declares WHO MAY ADVANCE a wait step, carried across the wire so
	// the far side does not have to guess.
	//
	// ── WHY IT IS ON THE WIRE AT ALL ──────────────────────────────────────
	//
	// Core has always known: a wait it splices for a lane is stamped, and its
	// release fence keys on that stamp. The Edge never received it. So a station
	// holding a plan could not tell a wait it owns — "hold at staging until the
	// line clears", which an operator ends — from one only Core's lane evaluator
	// can advance, and the board either offered a button that could not work or
	// offered nothing and explained nothing. The sim operator guessed with a
	// three-strike retry cap and guessed wrong; a human at an HMI has strictly
	// less to go on.
	//
	// Values are dispatch.WaitKindLane ("lane", Core-owned) and
	// dispatch.WaitKindStation ("station", station-owned). They are declared in
	// Core because Core is where the fence that reads them lives.
	//
	// EMPTY IS NOT A THIRD KIND. It means "authored before this field", and it
	// is read as station-owned for exactly as long as pre-ruling orders are
	// draining — the historical default, so nothing needs migrating. After the
	// drain window an untagged wait is a defect, and the drift tests on both
	// sides say so.
	WaitKind string `json:"wait_kind,omitempty"`
	// ExclusiveSlot declares that this DROPOFF lands on a node that holds ONE
	// bin at a time and must therefore be reserved before the robot is sent.
	//
	// ── WHY THE SENDER HAS TO SAY IT ──────────────────────────────────────
	//
	// Core gates its destination checks on node ROLE: a dropoff is reserved and
	// capacity-checked when the node is a storage slot (a child of a LANE or
	// NGRP), and skipped otherwise. The skip is deliberate and load-bearing — a
	// two-robot SUPPLY leg delivers to a LINE node that a sibling EVAC clears,
	// and gating that re-creates the deadlock 2b05dce fixed.
	//
	// A STAGING node is neither. It holds one bin like a slot, but it is seeded
	// as a station with no parent, so Core's role test rejects it at the
	// parent-nil guard and BOTH destination gates decline to act. Nothing
	// reserves it and nothing checks it is free.
	//
	// Core cannot repair that by looking harder. Every station — line, press,
	// weld, loader, unloader, staging, dest — carries the one STATION node type;
	// the plantspec's Kind field is advisory and never persisted; and the
	// staging designation lives in the EDGE cell config, which Core does not
	// have. The sender is the only party that knows.
	//
	// ── THE INCIDENT THIS WAS ATTRIBUTED TO WAS NOT THIS BUG (§R.112) ─────
	//
	// This field and its fix are UNCHANGED and keep their standing. What is
	// struck is the causal claim, which stood here and at fourteen other sites:
	//
	//	"Springfield, 2026-08-12: AMR-04 held a bin for 48 minutes unable to
	//	place at SLN_003, with the fleet reporting the robot RUNNING and no
	//	error. Order 4580 was cancelled by an admin after 2h05m. Nothing was
	//	broken — nothing had ever asked whether SLN_003 was free."
	//
	// The plant queries say otherwise. Order 4580's DESTINATION was ALN_004;
	// SLN_003 was a mid-route waypoint, not the node it could not place at. The
	// sibling order ran the identical route on the same robot and completed
	// twelve minutes earlier. The fleet wedged. Whatever held AMR-04 for 48
	// minutes, an unreserved staging node was not it.
	//
	// It is quoted once, here, rather than at each of the fifteen sites that
	// carried it: a false sentence reproduced fifteen times to mark its own
	// deletion is the disease this round is treating.
	//
	// THE GAP IS STILL REAL AND STILL REACHABLE, which is why nothing else
	// moves. A declared staging dropoff is reserved by nothing and checked by
	// nothing; two orders can take the same node and the second robot arrives to
	// find it full. That argument stands on the code above without an incident
	// under it, and it is the argument the fix should always have carried —
	// TestDeclaredStagingDropoffIsReserved and the two invariant walks are what
	// hold it, not a story.
	//
	// This is WaitKind's mirror, and the same rule: carried across the wire so
	// the far side does not have to guess. There it was Core knowing something
	// the Edge could not infer; here it is the Edge knowing something Core
	// cannot.
	//
	// DROPOFF-ONLY; ignored on pickup/wait. Backward-compatible: absent/false is
	// exactly today's behaviour, so an older sender — and an older Core that
	// ignores the field — behave as they do now. Setting it on a LINE node would
	// re-create the 2b05dce deadlock, so senders must not.
	ExclusiveSlot bool `json:"exclusive_slot,omitempty"`
}

// ComplexOrderRequest is a multi-step transport order from edge.
//
// ProcessNode names the production node the order belongs to — the line
// node where the operator releases / confirms and where the "active bin"
// for manifest sync lives. Distinct from SourceNode (first pickup step,
// fleet routing) and DeliveryNode (last dropoff). For swap orders that
// pick up at InboundSource and drop at the line, SourceNode is the
// supermarket but ProcessNode is the line; Core uses ProcessNode (when
// non-empty) to pick the bin claimed at the line for order.BinID and for
// the late-bind manifest fallback at release. Empty for orders without a
// distinct line node — Core falls back to SourceNode behavior.
type ComplexOrderRequest struct {
	OrderUUID   string             `json:"order_uuid"`
	PayloadCode string             `json:"payload_code,omitempty"`
	PayloadDesc string             `json:"payload_desc,omitempty"`
	Quantity    int64              `json:"quantity"`
	Priority    int                `json:"priority,omitempty"`
	ProcessNode string             `json:"process_node,omitempty"`
	Steps       []ComplexOrderStep `json:"steps"`
	// SiblingOrderUUID is the edge UUID of the paired leg in a two-robot swap.
	// It rides the SECOND-created leg — the only one that can know the other's
	// UUID — and is empty for non-swap orders and for the first-created leg.
	//
	// Which ROLE the pointer-carrying (second-created) leg is is NOT fixed —
	// it varies by mode and by the creating path, so do not read a role into
	// it. two_robot creates the supply leg first and the evac leg second; a
	// press-index CHANGEOVER does the same (supply first, evac second), but a
	// steady-state press-index swap creates the evac leg (R1, which clears the
	// press) first. The pointer says "these two legs are a pair", nothing more;
	// a leg's role comes from its steps (see legTakesLineBin in Core's dispatch
	// package).
	//
	// Core links both order rows on ingest — bidirectionally, via
	// LinkSiblingsByEdgeUUID — so either leg can find the other, and the
	// dispatch hold can see the pairing at intake, before a removal leg's
	// synchronous dispatch claims the line bin.
	SiblingOrderUUID string `json:"sibling_order_uuid,omitempty"`
	// RemainingUOP: nil = no sync, 0 = clear manifest, >0 = partial consumption.
	RemainingUOP *int `json:"remaining_uop,omitempty"`
	// OriginID / OriginClass attribute this order to the demand episode that
	// asked for it. SEAM 2, Edge -> Core, same shape as OrderRequest's.
	//
	// BOTH legs of a swap pair carry the SAME OriginID: one fire of
	// applyConsumePlan is one demand served by two order rows, and counting it
	// as two demands would read every swap-mode episode 2x high.
	OriginID    string `json:"origin_id,omitempty"`
	OriginClass string `json:"origin_class,omitempty"`
}

// UOPDispositionKind names the operator's release-time intent. Values map
// 1:1 to the release buttons in the operator UI:
//
//   - DispositionPullParts      — operator pulled some parts to lineside;
//     bin reduced by sum of captures, lineside buckets increased.
//   - DispositionReleasePartial — operator declares the bin still holds Count
//     parts; bin returns to supermarket as-is with manifest preserved.
//   - DispositionReleaseEmpty   — bin physically empty; manifest cleared.
//
// Both this enum and the legacy RemainingUOP pointer ship on the wire. Edge
// populates whichever it knows about; Core prefers the enum when present and
// falls back to RemainingUOP otherwise.
type UOPDispositionKind string

const (
	DispositionPullParts      UOPDispositionKind = "pull_parts"
	DispositionReleasePartial UOPDispositionKind = "release_partial"
	DispositionReleaseEmpty   UOPDispositionKind = "release_empty"

	// DispositionReleaseUnderpack — operator declares the bin is
	// physically empty even though the system's tracked count is
	// still positive (bin labeled 1200 actually held 1190; cell
	// starves at runtime=10). Wire-shape is the same as
	// DispositionReleaseEmpty (RemainingUOP = &0; manifest cleared
	// at Core), but the audit row tags as released_underpack so
	// forensics can trend the missing-inventory pattern. The
	// before_uop on the audit row carries the system's expected
	// count at click time; suggested_uop - after_uop = the missing
	// delta.
	DispositionReleaseUnderpack UOPDispositionKind = "release_underpack"
)

// UOPDisposition is the structured release-time disposition that supersedes
// OrderRelease.RemainingUOP. Count is meaningful only when Kind ==
// DispositionReleasePartial (the operator-entered "this bin has N left"
// value); for the other kinds Count is ignored.
//
// Captures is meaningful only when Kind == DispositionPullParts. The map
// is keyed by part number with the per-part captured quantity.
//
// CountSuggested and CapturesSuggested carry the values the system would
// have shipped without operator intervention — the snapshot from the
// runtime / manifest at the moment the release modal opened. Core
// compares them against Count / Captures at release time and writes a
// bin_uop_audit row whenever they differ, surfacing every operator
// override (mislabelled bin, upstream overfill, miscount) as forensic
// evidence. Both fields are populated by UI-aware Edge clients only;
// legacy clients leave them nil/empty and no override audit is recorded.
type UOPDisposition struct {
	Kind              UOPDispositionKind `json:"kind"`
	Count             int                `json:"count,omitempty"`
	Captures          map[string]int     `json:"captures,omitempty"`
	CountSuggested    *int               `json:"count_suggested,omitempty"`
	CapturesSuggested map[string]int     `json:"captures_suggested,omitempty"`
}

// OrderRelease signals that a staged (dwelling) order should resume.
//
// RemainingUOP (legacy shape) late-binds the bin's manifest at the operator's
// release click:
//
//   - nil = no manifest change (legacy/unspecified — preserves pre-release behavior)
//   - 0   = clear manifest (bin is empty, e.g. NOTHING PULLED disposition)
//   - >0  = sync UOP, preserve manifest (bin returns as partial, e.g. SEND PARTIAL BACK)
//
// Disposition carries the same intent as a typed enum, disambiguating the
// capture_lineside overload that serves both "operator pulled parts" and
// "bin is empty" via the same on-wire value. Both shapes are accepted on
// the wire.
//
// Routing on Core mirrors ClaimForDispatch but operates on the already-claimed
// bin via BinManifestService.SyncOrClearForReleased. See docs on that method.
//
// CalledBy carries the operator identity (station name, badge id, etc.) from
// the HTTP body all the way through to Core's bin audit. Empty when the
// caller is a system-internal path (wiring completion fallbacks, restore,
// etc.); Core defaults to "system" in that case.
type OrderRelease struct {
	OrderUUID    string          `json:"order_uuid"`
	RemainingUOP *int            `json:"remaining_uop,omitempty"`
	Disposition  *UOPDisposition `json:"disposition,omitempty"`
	CalledBy     string          `json:"called_by,omitempty"`
}

// OrderStaged notifies edge that an order is dwelling at a staging node.
type OrderStaged struct {
	OrderUUID string `json:"order_uuid"`
	Detail    string `json:"detail,omitempty"`
}

// --- Origination payloads: Edge -> Core ---

// OrderIngestRequest reports a produced (filled) bin so Core records its
// manifest. It is a manifest-only inventory write: Core sets AND confirms the
// bin's payload + count and dispatches nothing — there is no store order (that
// leg went with the retired simple-produce mode).
//
// Bin identity resolves two ways: a non-empty BinLabel is looked up directly
// (manual/HTTP ingest — an operator scanned a real tote); a blank BinLabel
// falls back to SourceNode, and Core resolves the bin from what is parked at
// that node (headless produce-finalize, which tracks the bin by id, not label).
type OrderIngestRequest struct {
	OrderUUID   string `json:"order_uuid"`
	PayloadCode string `json:"payload_code"`
	BinLabel    string `json:"bin_label"` // optional: blank => resolve the bin by SourceNode
	// BinID pins the exact Core bin (0 = absent → BinLabel/SourceNode
	// resolution). Exists for the release-time produce manifest: by the time
	// Core processes a deferred ingest, a press-index R2 may already have
	// indexed the fresh tote onto the position the manifest's tote occupied,
	// so resolve-by-node would credit the wrong bin. Edge passes the runtime's
	// active bin id, which Core itself seeded at delivery.
	BinID      int64                `json:"bin_id,omitempty"`
	SourceNode string               `json:"source_node"`
	Quantity   int64                `json:"quantity"` // operator-measured produced count (UOP); 0 => payload capacity
	Manifest   []IngestManifestItem `json:"manifest,omitempty"`
	ProducedAt string               `json:"produced_at,omitempty"` // RFC3339 timestamp from Edge at cell completion
}

// IngestManifestItem describes a single item in an ingest manifest.
type IngestManifestItem struct {
	PartNumber  string `json:"part_number"`
	Quantity    int64  `json:"quantity"`
	Description string `json:"description,omitempty"`
}

// --- Node list data schemas ---

// NodeListRequest is sent by edge to request the core's node list.
type NodeListRequest struct{}

// NodeInfo describes a single node in the core's node list.
type NodeInfo struct {
	Name     string `json:"name"`
	NodeType string `json:"node_type"`
}

// PayloadBinTypeInfo maps one payload code to one bin-type code.
// One row per (payload, bin_type) pair in payload_bin_types.
// Carried as a sibling slice on NodeListResponse so Edge can derive
// the dunnage picker options from a node's allowed payloads without
// a per-node query.
type PayloadBinTypeInfo struct {
	PayloadCode string `json:"payload_code"`
	BinTypeCode string `json:"bin_type_code"`
}

// NodeListResponse carries the core's authoritative node list, plus (loader
// refactor cutover) the Core-owned loader config as a sibling slice so a loader
// and its member positions arrive atomically with the topology. Loaders is
// omitted until Core authors loaders, so this is additive — a pre-cutover Edge
// ignores the unknown field.
type NodeListResponse struct {
	Nodes           []NodeInfo           `json:"nodes"`
	Loaders         []LoaderInfo         `json:"loaders,omitempty"`
	PayloadBinTypes []PayloadBinTypeInfo `json:"payload_bin_types,omitempty"`
}

// LoaderInfo describes one Core-owned bin loader (produce) or unloader (consume)
// for the downward config sync. Carried as a sibling slice on NodeListResponse —
// NOT folded into NodeInfo — so the loader and its positions/payloads arrive
// together. The loader's identity is LoaderKey (the surrogate token); it has no
// node id of its own. Edge keys on node NAMES, so Positions carry core_node_name
// (Core resolves its position_node_id → name when building this). ConfigGen rides
// every config write so Edge can detect stale config. Names per D4: layout =
// shared_window | dedicated_positions; replenishment = auto | operator.
type LoaderInfo struct {
	Name string `json:"name"`
	// LoaderKey is the loader's IDENTITY — the opaque token Core mints from
	// bin_loaders.id as "loader:<id>". It is the Edge cache key that groups a loader's
	// windows for the never-2N budget and what the pooled threshold signal names. The
	// loader has no node id of its own (a multi-window loader spans many nodes); its
	// delivery targets are the explicit member nodes in Positions. domain.LoaderID
	// stays a string newtype so a future UUID swap is invisible.
	LoaderKey     string `json:"loader_key"`
	Role          string `json:"role"`
	Layout        string `json:"layout"`
	Replenishment string `json:"replenishment"`
	OutboundDest  string `json:"outbound_dest,omitempty"`
	InboundSource string `json:"inbound_source,omitempty"`
	ConfigGen     int64  `json:"config_gen"`
	// FunnelWindows restricts a shared_window loader to ONE window at a time:
	// empties funnel to its first window on a budget of 1 instead of spreading one
	// bin per window. Stated as the restriction so the zero value — which is also
	// what a Core predating this field sends — means "spread", the behaviour every
	// loader has today. Ignored for dedicated_positions loaders.
	FunnelWindows bool                `json:"funnel_windows,omitempty"`
	Positions     []LoaderPosition    `json:"positions,omitempty"`
	Payloads      []LoaderPayloadInfo `json:"payloads,omitempty"`
	// Quota is the declared carrier mix — how many of each bin type this loader
	// wants on hand. Empty means none declared, which is today's behaviour.
	Quota []LoaderQuota `json:"quota,omitempty"`
}

// LoaderPosition is one home of a loader. For a dedicated_positions loader it is
// a position node bound to exactly one payload; for a shared_window loader it is
// one window of the shared cluster, carrying no per-position payload (the shared
// set rides LoaderInfo.Payloads). Kind makes that distinction EXPLICIT on the
// wire so a consumer reading a single position need not re-derive it from the
// payload being empty — the empty-payload-means-window convention that already
// mis-wires the Edge. Kind is set by Core from the parent loader's Layout (the
// single authoritative discriminator); see LoaderPositionKind* below.
type LoaderPosition struct {
	CoreNodeName string `json:"core_node_name"`
	PayloadCode  string `json:"payload_code"`
	Kind         string `json:"kind,omitempty"`
	// HomeKind separates the two DIFFERENT things a blank PayloadCode can mean
	// on a dedicated loader: a kept-partial BUFFER slot, and a HOME position the
	// operator dragged in but has not assigned a payload to yet.
	//
	// Core has always distinguished them — bin_loader_homes.home_kind, which
	// InSourcePool reads to keep an unassigned home OUT of the loader's source
	// pool while a buffer stays in. It just never said so on the wire, so the
	// Edge inferred "buffer" from an empty payload and reached the opposite
	// answer for an unpinned home.
	//
	// That is the same re-derivation Kind above was added to stop, one level
	// down. Kind resolved window-vs-dedicated; this resolves home-vs-buffer.
	//
	// Additive and unsentinelled, like Ordinal and BinTypes: a Core that
	// predates the field sends nothing, every position decodes "", and the
	// reader falls back to the empty-payload inference — exactly what it did
	// before. See LoaderHomeKind* below.
	HomeKind     string `json:"home_kind,omitempty"`
	UOPThreshold int    `json:"uop_threshold"`
	// Ordinal is where the operator put this window. An admin screen lets them
	// drag a loader's windows into the order they want filled; Core persists
	// that and this is how it reaches the plant.
	//
	// It used to go no further than Core. There was no field here and no column
	// in the Edge's cache, so the arrangement was accepted, stored, synced
	// downward, and discarded on arrival — the Edge re-sorted by name. Since
	// the funnel case delivers to "the first window" and spreading fills free
	// windows in order, that decided which window a carrier physically went to.
	//
	// Additive and unsentinelled: a Core that predates the field sends nothing,
	// every position decodes 0, every comparison ties, and the reader falls
	// through to its name ordering — which is exactly what it did before. See
	// shingo/shared/windoworder for the rule both sides share.
	Ordinal int `json:"ordinal,omitempty"`
	// BinTypes is what this window can PHYSICALLY take, as bin-type codes.
	// EMPTY MEANS ANYTHING, which is what every window does today and keeps
	// doing until somebody says otherwise — so an older Core that sends nothing
	// reads as "no constraint", the same shape as the ordinal above.
	//
	// A set, not one type: a slot that fits a 45x48 may also fit a tote of the
	// same footprint. Per window rather than per loader because it is a fact
	// about the floor, not about intent — intent is the loader's quota.
	BinTypes []string `json:"bin_types,omitempty"`
}

// LoaderQuota is one line of a loader's declared carrier mix: how many carriers
// of a bin type it wants on hand.
//
// A PREFERENCE, NOT A CAP. The never-2N budget still bounds how many carriers
// exist at a loader; this only decides which type to fetch next inside that
// bound. An absent quota means no declared mix, which is today's behaviour.
type LoaderQuota struct {
	BinTypeCode string `json:"bin_type_code"`
	Want        int    `json:"want"`
}

// LoaderPositionKind values for LoaderPosition.Kind. A window belongs to a
// shared_window loader's shared budget and carries no payload; a dedicated
// position carries one payload (possibly unassigned == empty). Empty Kind on the
// wire means "from a Core that predates this field" — the reader falls back to
// the parent loader's Layout, which remains authoritative.
const (
	LoaderPositionKindWindow    = "window"
	LoaderPositionKindDedicated = "dedicated"
)

// LoaderHomeKind values for LoaderPosition.HomeKind, mirroring Core's
// bin_loader_homes.home_kind. A HOME is a payload-pinned position (or one
// waiting for its payload); a BUFFER holds kept partials and pins nothing.
//
// Empty means "from a Core that predates this field" — the reader falls back to
// classifying by empty payload, which is what it did before. Blank must NOT be
// read as "home": an older Core sends blank for buffer slots too.
const (
	LoaderHomeKindHome   = "home"
	LoaderHomeKindBuffer = "buffer"
)

// LoaderPayloadInfo is one entry in a shared_window loader's allowed payload set.
type LoaderPayloadInfo struct {
	PayloadCode  string `json:"payload_code"`
	UOPThreshold int    `json:"uop_threshold"`
}

// --- Production data schemas ---

// ProductionReportEntry is a single cat_id production count.
type ProductionReportEntry struct {
	CatID string `json:"cat_id"`
	Count int64  `json:"count"`
}

// ProductionReport carries production counts from an edge station.
type ProductionReport struct {
	StationID string                  `json:"station_id"`
	Reports   []ProductionReportEntry `json:"reports"`
}

// ProductionReportAck acknowledges processing of a production report.
type ProductionReportAck struct {
	StationID string `json:"station_id"`
	Accepted  int    `json:"accepted"`
}

// EdgeStale is sent by core to notify an edge that it has been marked stale.
type EdgeStale struct {
	StationID string `json:"station_id"`
	Message   string `json:"message"`
}

// EdgeRegisterRequest is sent by core to ask an edge to re-register.
type EdgeRegisterRequest struct {
	StationID string `json:"station_id"`
	Reason    string `json:"reason"`
}

// --- QR Tag Verification ---

// TagVerifyRequest is sent by edge to verify a scanned QR tag against an order's payload bin.
type TagVerifyRequest struct {
	OrderUUID string `json:"order_uuid"`
	TagID     string `json:"tag_id"`
	Location  string `json:"location,omitempty"`
}

// TagVerifyResponse is the core's response to a tag verification request.
type TagVerifyResponse struct {
	OrderUUID string `json:"order_uuid"`
	Match     bool   `json:"match"`
	Expected  string `json:"expected,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// --- Payload Catalog ---

// CatalogPayloadsRequest is sent by edge to request the payload catalog.
type CatalogPayloadsRequest struct{}

// CatalogPayloadInfo describes a single payload template in the catalog.
type CatalogPayloadInfo struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Code        string `json:"code"`
	Description string `json:"description"`
	UOPCapacity int    `json:"uop_capacity"`
	// CATID is the payload's part identity (from its Core manifest) when it maps
	// to exactly one part number, else empty. The edge uses it to auto-fill a
	// style's expected_catid when the payload is chosen for a node claim, so the
	// PLC part-identity guard configures itself. Empty when the payload's manifest
	// carries zero or several distinct part numbers — never guess.
	CATID string `json:"catid,omitempty"`
}

// CatalogPayloadsResponse carries the core's payload catalog.
type CatalogPayloadsResponse struct {
	Payloads []CatalogPayloadInfo `json:"payloads"`
}

// OrderStatusRequest asks Core for the current authoritative status of a set of orders.
type OrderStatusRequest struct {
	OrderUUIDs []string `json:"order_uuids"`
}

// OrderProjection is the Core → Edge push of a whole order row
// (SubjectOrderProjected).
//
// It exists because Core is about to author orders itself. An order the Edge
// never asked for has no row on the Edge, so it is invisible on the operator
// board, and the delivery handler has to paper over the miss with a bind-only
// fallback. The projection gives the Edge the row.
//
// IDEMPOTENT BY UUID. Core re-sends the same projection freely — on creation,
// and again for anything the reconcile finds the Edge is missing — so the
// applier must upsert, never insert. A duplicate must be a no-op, not a
// conflict and not a second row.
//
// NO process_node_id. That is an Edge-local foreign key to the Edge's own
// process_nodes table, and Core has never had it. The Edge resolves it from
// DeliveryNode, and a legitimately unresolvable one (a Core node with no Edge
// process node) leaves it null — which is a supported shape, not an error.
//
// Additive-only: an older Edge that does not register the subject logs and
// drops the whole message, so the feed is a no-op in both mixed-version
// directions.
type OrderProjection struct {
	OrderUUID    string    `json:"order_uuid"`
	OrderType    OrderType `json:"order_type"`
	Status       string    `json:"status"`
	StationID    string    `json:"station_id"`
	Quantity     int64     `json:"quantity,omitempty"`
	SourceNode   string    `json:"source_node,omitempty"`
	DeliveryNode string    `json:"delivery_node,omitempty"`
	PayloadCode  string    `json:"payload_code,omitempty"`
	PayloadDesc  string    `json:"payload_desc,omitempty"`
	// RetrieveEmpty distinguishes a pull of an empty carrier from a pull of a
	// full one. Carried explicitly rather than derived from OrderType, because
	// the Edge column is separate and the two have drifted before.
	RetrieveEmpty bool `json:"retrieve_empty,omitempty"`
	// OriginID / OriginClass are the demand-episode attribution, passed through
	// so a projected row answers "why does this exist" the same way a locally
	// created one does.
	OriginID    string `json:"origin_id,omitempty"`
	OriginClass string `json:"origin_class,omitempty"`
	// QueueReason / QueueCode mirror OrderStatusSnapshot's, so a projection that
	// arrives for a queued order explains the wait without a second round trip.
	QueueReason string `json:"queue_reason,omitempty"`
	QueueCode   string `json:"queue_code,omitempty"`
}

// OrderStatusSnapshot is the current Core-side view of an order.
type OrderStatusSnapshot struct {
	OrderUUID     string `json:"order_uuid"`
	Found         bool   `json:"found"`
	Status        string `json:"status,omitempty"`
	StationID     string `json:"station_id,omitempty"`
	SourceNode    string `json:"source_node,omitempty"`
	DeliveryNode  string `json:"delivery_node,omitempty"`
	VendorOrderID string `json:"vendor_order_id,omitempty"`
	ErrorDetail   string `json:"error_detail,omitempty"`
	// QueueReason is Core's current blocking signal for a queued order
	// (orders.queue_reason) — e.g. "no bin of requested payload in node
	// group AMR Supermarket". Re-evaluated by the dispatcher/scanner and
	// cleared on dispatch, so Edge refreshes it on each status resync.
	QueueReason string `json:"queue_reason,omitempty"`
	// QueueCode is the structured category behind QueueReason
	// (protocol.QueueCode). Carried on the snapshot (additive) so an Edge
	// resync doesn't lose the code; old Edge ignores it.
	QueueCode string `json:"queue_code,omitempty"`
}

// OrderStatusResponse carries the authoritative Core-side state for requested orders.
type OrderStatusResponse struct {
	Orders []OrderStatusSnapshot `json:"orders"`
	// Unlisted carries orders for the asking station that the Edge did NOT name
	// — Core-authored ones it has no row for. This is the healing half of the
	// reconcile, and it is why the reconcile is load-bearing rather than a
	// backstop: the Core → Edge outbox drops a message permanently once it is
	// past its retry limit, so a lost projection is a normal event and the only
	// thing that repairs it is the Edge finding out here.
	//
	// Additive: an older Edge decodes the snapshots and ignores this field, which
	// leaves it exactly where it is today.
	Unlisted []OrderProjection `json:"unlisted,omitempty"`
}

// NodeStructureChanged is sent Core→Edge when a node group's structure changes
// (reparent or group deletion). Edge uses this to refresh its node cache.
type NodeStructureChanged struct {
	NodeID      int64  `json:"node_id"`
	NodeName    string `json:"node_name"`
	OldParentID *int64 `json:"old_parent_id,omitempty"`
	NewParentID *int64 `json:"new_parent_id,omitempty"`
	Action      string `json:"action"` // "reparented" or "group_deleted"
}

// DemandSignal is sent by Core to Edge when a kanban event fires.
// Edge creates an order for the specified payload at the specified node.
type DemandSignal struct {
	CoreNodeName string    `json:"core_node_name"` // delivery node for the order
	PayloadCode  string    `json:"payload_code"`   // which payload to request
	Role         ClaimRole `json:"role"`           // determines order type
	Reason       string    `json:"reason"`         // human-readable trigger (e.g., "empty bin returned to storage")
}

// CountGroupCommand is sent by Core to Edge when an advanced zone's occupancy state changes.
// Edge translates this into a request/ack handshake against a PLC tag via WarLink.
//
// Subject: protocol.SubjectCountGroupCommand.
type CountGroupCommand struct {
	CorrelationID     string    `json:"corr_id"`             // for matching the eventual CountGroupAck
	Group             string    `json:"group"`               // RDS advanced-group name
	Desired           string    `json:"desired"`             // "on" | "off"
	Robots            []string  `json:"robots"`              // robot IDs in the zone (for audit)
	RobotCount        int       `json:"robot_count"`         // len(Robots) — cheap log without decoding the slice
	FailSafeTriggered bool      `json:"fail_safe_triggered"` // true if this command came from RDS-down fail-safe
	Timestamp         time.Time `json:"ts"`
}

// CountGroupAck is sent by Edge to Core after a CountGroupCommand has been
// processed (or abandoned) by the PLC.
//
// Subject: protocol.SubjectCountGroupAck. Outcome ∈ {AckOutcomeAcked,
// AckOutcomeTimeout, AckOutcomeWarlinkErr}.
type CountGroupAck struct {
	CorrelationID string     `json:"corr_id"`
	Group         string     `json:"group"`
	Outcome       AckOutcome `json:"outcome"`
	AckLatencyMs  int64      `json:"ack_latency_ms"`
	Timestamp     time.Time  `json:"ts"`
}

// ─── Phase 1 — inventory delta envelopes ─────────────────────────────────
//
// Two envelopes carry every count change in the bin-as-truth refactor.
// Both ride TypeData with a Subject discriminator (Decision #1 / B1 fix
// in plan §2.6), dispatched through the SubjectRouter to the
// CoreDataService methods rather than via standalone envelope types.
// The original rationale (pre-router: a new MessageHandler method
// would have silently no-opped through the InboxDedup decorator)
// no longer applies — the router is explicit — but the subject-based
// shape stayed because the wire format and consumer code already
// committed to it.
//
// Dedup is at the message level via a (station, scope_kind, scope_key,
// last_seq) table on Core — distinct from inbox dedup which gates
// at-most-once order processing. SequenceID is monotonically increasing
// per (station, scope_key); Core ignores any envelope whose SequenceID
// is ≤ last_seen for its scope.

// BinUOPDeltaReason names the cause of a BinUOPDelta. Stable strings —
// Core dedup and audit rows reference them, so renames must come with a
// migration. The set covers every site that mutates bins.uop_remaining
// in Phase 1+ (cycle count, operator load, partial-back-at-release stay
// on the legacy direct-write path because they are explicit operator
// actions, not deltas).
type BinUOPDeltaReason string

const (
	// ReasonConsumeTick — a PLC consume tick drained the bin past the
	// active lineside bucket (or no bucket existed). Always negative
	// delta. Emitted by drainLinesideFirst's bin-side return.
	ReasonConsumeTick BinUOPDeltaReason = "consume_tick"
	// ReasonProduceTick — a PLC produce tick filled the bin. Positive
	// delta. Emitted by handleProduceTick.
	ReasonProduceTick BinUOPDeltaReason = "produce_tick"
	// ReasonABFallthrough — A/B-cycling consume tick attributed to the
	// inactive node's bin via the runtime-flip path. Always negative.
	ReasonABFallthrough BinUOPDeltaReason = "ab_fallthrough"
	// ReasonCaptureReduction — operator pulled parts to lineside on
	// release. Negative delta equal to the sum of captures. Emitted
	// by ReleaseOrderWithLineside's capture path.
	ReasonCaptureReduction BinUOPDeltaReason = "capture_reduction"
	// ReasonOperatorCorrection — explicit operator-driven correction
	// path (e.g. cycle-count diff applied as a delta rather than a
	// direct overwrite). Reserved for Phase 3+; included now so the
	// schema doesn't churn.
	ReasonOperatorCorrection BinUOPDeltaReason = "operator_correction"
)

// LinesideBucketDeltaReason names the cause of a LinesideBucketDelta.
// Note: NO changeover_deactivate — buckets are location-only (Option C),
// activation is computed at query time from the active claim, no state
// to flip. NEVER emitted by manual_swap nodes (no PLC).
type LinesideBucketDeltaReason string

const (
	// ReasonCaptureFill — operator pulled parts to lineside on release.
	// Positive delta. Emitted by ReleaseOrderWithLineside's capture
	// path, one per (style, part_number) captured.
	ReasonCaptureFill LinesideBucketDeltaReason = "capture_fill"
	// ReasonConsumeDrain — PLC consume tick drained the bucket before
	// reaching the bin. Always negative. Emitted by drainLinesideFirst's
	// bucket-side return.
	ReasonConsumeDrain LinesideBucketDeltaReason = "consume_drain"
	// ReasonOperatorCorrectionBucket — engineer/team-leader override
	// from the edge "Lineside Buckets" admin page (clear or edit qty).
	// Sign matches the delta (negative for clears / qty reductions,
	// positive for upward adjustments). Mirrors the bin-side
	// ReasonOperatorCorrection in intent: a deliberate human correction,
	// not automated state.
	ReasonOperatorCorrectionBucket LinesideBucketDeltaReason = "operator_correction"
)

// BinUOPDelta carries a count change against a specific physical bin.
// Sent on subject SubjectBinUOPDelta. Core's HandleData routes on
// subject and applies the delta after dedup against
// inventory_delta_dedup(station, "bin", strconv(BinID)).
//
// PayloadCode lets Core reject mismatched envelopes (a bin's payload
// shouldn't change underfoot). WindowStart/WindowEnd bracket the
// accumulator window the delta covers — telemetry and forensics can
// align deltas to PLC-tick timestamps.
type BinUOPDelta struct {
	Station     string            `json:"station"`
	BinID       int64             `json:"bin_id"`
	PayloadCode string            `json:"payload_code"`
	Delta       int               `json:"delta"`
	Reason      BinUOPDeltaReason `json:"reason"`
	SequenceID  int64             `json:"sequence_id"`
	// Epoch is the bin's delta_epoch as known to Edge at emit time. Bumps
	// on every Core-controlled lifecycle boundary (set_for_production,
	// clear). The dedup PK on Core is (station, scope_kind, scope_key,
	// epoch); extending the key with epoch means a stale Edge seq counter
	// (deploy reset, backup restore, cache loss) can't poison the new
	// bin life's delta stream. Pre-epoch envelopes deserialize with
	// Epoch=0, which Core treats as the pre-migration cohort (matches
	// the backfilled epoch=1 on existing rows after the migration runs).
	Epoch       int64     `json:"epoch"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
}

// CounterSnapshot is the production.tick payload (plan §12): one PLC counter
// tick observed at an Edge reporting point, captured upstream of the
// inventory hold-and-replay/accumulator logic so per-tick timing survives bin
// swaps. Sent on SubjectProductionTick. Core dedups on
// (Station, EdgeSnapshotID) — never bare EdgeSnapshotID, since Edge-local
// SQLite autoincrements collide across stations (§8 #22, Round-3 Obs 8).
//
// RecordedAt MUST be stamped in Go with millisecond precision
// (time.Now().UTC()) at insert time, NOT pulled from SQLite's
// datetime('now') default, whose second granularity injects ~5%
// quantization noise on 22.5s cycle math (§8 #21).
//
// Anomaly carries "" or "jump"; Edge emits even for "jump" ticks (the
// heartbeat needs to know the cell physically fired even when inventory
// attribution is operator-gated) and downstream decides whether jumps count
// toward MTBF/cycle math (§8 #20).
// THE PAYLOAD COPY OF THE STATION IS GONE (identity change). Every one of
// these envelopes carried the station twice — once in Envelope.Src.Station,
// where the transport put it, and once in the body, where the Edge put it —
// and every handler resolved the disagreement with
// `if station == "" { station = env.Src.Station }`. That is a rule with two
// possible answers, and the only reason it never produced a wrong one is that
// a plant has exactly one station, so the two copies could not differ. Distinct
// per-edge identity is precisely the change that makes them able to. Deleting
// the body copy leaves ONE source of the station, and it is the one the
// transport authenticated rather than the one the sender asserted.
//
// It is also the safe direction across the deploy skew: a NEW edge omits the
// field, an OLD core reads "" and takes env.Src.Station — the fallback this
// change deletes is what makes deleting it survivable.
type CounterSnapshot struct {
	ReportingPointID int64     `json:"reporting_point_id"` // Edge-local; provenance only, not a Core join key
	EdgeSnapshotID   int64     `json:"edge_snapshot_id"`   // counter_snapshots.id — composite with Station for dedup
	ProcessID        int64     `json:"process_id"`         // enriched at emit time from rp.ProcessID
	StyleID          int64     `json:"style_id"`           // enriched at emit time from rp.StyleID
	CountValue       int64     `json:"count_value"`        // absolute counter value (rollover detection on Core)
	Delta            int64     `json:"delta"`              // count change (typically 1)
	Anomaly          string    `json:"anomaly"`            // "" or "jump"
	RecordedAt       time.Time `json:"recorded_at"`        // Edge wall-clock at insert, ms precision (NOT SQLite default)
}

// DowntimeEvent carries a persisted downtime start or end event (G9).
// Emitted by the sim's downtime model on readiness-gate transitions and
// projected into core's downtime_events table for OEE availability.
// Two events per outage: one "down" (started) and one "up" (ended).
// The end event carries duration_ms for convenience; the start event
// sets duration_ms = 0 and ended_at to the zero value.
// Core deduplicates on (station, edge_event_id), taking the station from the
// envelope — see CounterSnapshot for why the payload copy was deleted.
type DowntimeEvent struct {
	PLCName     string    `json:"plc_name"`      // the machine that went down
	Reason      string    `json:"reason"`        // "breakdown" (sim); extensible for real plant
	IsDown      bool      `json:"is_down"`       // true = machine went down, false = machine came back up
	StartedAt   time.Time `json:"started_at"`    // when the downtime began
	EndedAt     time.Time `json:"ended_at"`      // when it ended (zero for start events)
	DurationMS  int64     `json:"duration_ms"`   // 0 for start events; repair duration for end events
	EdgeEventID int64     `json:"edge_event_id"` // monotonic counter for dedup (Edge-local)
}

// LinesideBucketDelta carries a count change against a specific
// lineside bucket. Sent on subject SubjectLinesideBucketDelta. Core
// routes on subject, dedups against
// inventory_delta_dedup(station, "bucket",
// "<NodeID>|<PairKey>|<StyleID>|<PartNumber>"), and applies via UPSERT
// to the lineside_buckets row keyed on
// (station, node_id, pair_key, style_id, part_number). When qty hits
// zero Core deletes the row — Option C: active/inactive is computed
// at query time, so empty buckets carry no useful information.
//
// PayloadCode (UOP-threshold replenishment) lets Core associate a
// bucket with the payload its parts came from so SystemUOPForPayload
// can sum bins + buckets for the same payload. Edge populates this at
// capture time from the order context. Empty string means "unknown"
// (orphan bucket whose claim was deleted before the capture event
// could resolve a payload). Orphans are excluded from
// SystemUOPForPayload — conservative undercount, never overcount.
// Round-3 Obs 8 (2026-05-21): NodeID dropped, CoreNodeName added.
// The Edge-local int64 process_nodes.id and Core-side nodes.id share
// a namespace but mean different things at each end. Pre-fix
// LinesideBucketDelta sent Edge's process_nodes.id; Core's applier
// then UPSERT'd against that integer under the assumption it
// referenced Core's nodes table, producing the cross-plant bucket
// drift that surfaced as the Springfield 6883 stuck-bucket and the
// Hopkinsville-vs-plant-a Core-only orphan. Switching to CoreNodeName
// is translation-free at the wire: Edge populates from
// process_nodes.core_node_name; Core's applier resolves to nodes.id
// via GetNodeByName before insert, and drops the delta with a loud
// log if the name doesn't resolve. The precedent is the
// NodeStructureChanged sibling at protocol/payloads.go:484 (still
// Core's authoritative ID — safe direction Core→Edge) and the
// earlier Item 14 (D6) drop of NodeID on another envelope.
//
// The payload copy of the station is deleted — see CounterSnapshot. This one
// carried it TWICE in one envelope, and Core's dedup scope key is Edge-local
// (`claimDeltaSequence`), so which copy answers "whose sequence counter space
// is this" is not a cosmetic question.
type LinesideBucketDelta struct {
	CoreNodeName string                    `json:"core_node_name"`
	PairKey      string                    `json:"pair_key"`
	StyleID      int64                     `json:"style_id"`
	PartNumber   string                    `json:"part_number"`
	PayloadCode  string                    `json:"payload_code,omitempty"`
	Delta        int                       `json:"delta"`
	Reason       LinesideBucketDeltaReason `json:"reason"`
	SequenceID   int64                     `json:"sequence_id"`
	WindowStart  time.Time                 `json:"window_start"`
	WindowEnd    time.Time                 `json:"window_end"`
}

// UOPAdjustment carries an absolute UOP value set by an admin via Core's
// Bins record-count action. Core validates the value is within [0,
// payload.UOPCapacity] before propagating. Edge writes the value
// directly to process_node_runtime_states.remaining_uop_cached and
// emits EventUOPAdjusted so the operator screen refreshes via the
// existing counter-update SSE channel. PLC ticks accumulate from the
// new value with no accumulator involvement.
//
// CoreNodeName allows Edge to look up the target process node without
// scanning — it is the canonical cross-system identifier carried on
// every other protocol envelope.
type UOPAdjustment struct {
	BinID        int64     `json:"bin_id"`
	CoreNodeName string    `json:"core_node_name"`
	NewRemaining int       `json:"new_remaining"`
	Actor        string    `json:"actor"`
	AdjustedAt   time.Time `json:"adjusted_at"`
	// Released, when true, means the bin was MOVED off CoreNodeName in Core
	// (admin Move). Edge clears that node's active_bin_id so its PLC ticks stop
	// attributing consumption to a bin that has left, instead of applying
	// NewRemaining (which is ignored when Released). Reuses this Core→Edge
	// channel rather than a separate subject. Older Edges ignore the field.
	Released bool `json:"released,omitempty"`
	// Bound, when true, means the bin was MOVED onto CoreNodeName in Core (admin
	// Move mirroring a physical relocation the robot-delivery path never recorded
	// — manual fork-truck recovery, a failed delivery that left the bin
	// unregistered). Edge binds that node's runtime to the bin: active_bin_id =
	// BinID, active_bin_epoch = Epoch, remaining_uop_cached = NewRemaining — so
	// its PLC ticks resume counting the arrived bin. The dual of Released; Core
	// sets exactly one of the two per message. Core's Move guarantees the
	// destination held no other bin, so Edge binds ahead of its active-bin guard
	// and overwrites any stale pointer. Older Edges ignore the field.
	Bound bool `json:"bound,omitempty"`
	// Epoch carries the bin's delta_epoch so a bind seeds active_bin_epoch
	// correctly and subsequent BinUOPDeltas carry the right generation for
	// Core's epoch-aware dedup. Set on the Bound path (admin Move onto a node)
	// and on a plain count correction (record_count): when the correction lands
	// on a staged-but-unbound bin, Edge binds it with this epoch (P2-C5), so the
	// resumed delta stream is accepted instead of dropped as epoch-0. Zero on
	// the Released path and from older Cores.
	Epoch int64 `json:"epoch,omitempty"`
}

// BinEpochRefresh is the body of a SubjectBinEpochRefresh message: the
// carrier at CoreNodeName has started a new generation, and Epoch is it.
//
// Three fields, and the shortness is the contract. There is no count here
// because nobody declared one — Core sends this when it discards a count for
// carrying a generation that has ended, which proves the Edge is behind and
// says nothing at all about how many parts are in the carrier. The Edge's own
// number stays the Edge's. It writes the stamp, reports its next count under
// it, and that count is what corrects Core's ledger.
//
// The Edge ignores a refresh naming a carrier it is not holding: Core sends to
// the station it believes has the carrier, and if that slot holds something
// else the message is about a carrier that is not there.
type BinEpochRefresh struct {
	BinID        int64  `json:"bin_id"`
	CoreNodeName string `json:"core_node_name"`
	Epoch        int64  `json:"epoch"`
}

// PlantClaimsReport is the body of a SubjectPlantClaims data message
// (Edge → Core). It carries the FULL claim set for one process: every style
// the process can run and, per style, the (node, payload) assignments the
// sourceability computation reads. Edge stays the source of truth for the
// plant spec; this feed plumbs it onto Core so recomputes never depend on
// Edge being up.
//
// One message = one process's complete picture. A full snapshot is a series
// of these (one per process); a single-process change is one. Core replaces
// its mirror for ProcessID on every message (the message is authoritative
// for that process), so a periodic full snapshot rebuilds late joiners.
//
// Loaders/unloaders are EXCLUDED here — a manual_swap claim never appears in
// Claims. They enter the computation as pool supply/demand via the loader
// aggregate, not as style claims.
//
// Value schema is ADDITIVE-only. Future fields append with omitempty; an
// older Core ignores unknown fields (forward-compatible), and an older Core
// that does not register SubjectPlantClaims logs-and-drops the whole message
// (mixed-version no-op).
type PlantClaimsReport struct {
	// ProcessID is the Edge plant-spec process identifier (the process's
	// Name on Edge — e.g. "SNF2"). Core mirrors it verbatim as the cache
	// key; it is the logical partition for the sourceability computation.
	ProcessID string `json:"process_id"`
	// Styles is the full set of styles this process can run, each with its
	// claim list. A style with no sourceability-relevant claims still
	// appears (empty Claims) so Core knows the style exists for an
	// all-styles recompute.
	Styles []PlantClaimsStyle `json:"styles"`
	// ConfigGen is bumped by Edge on every plant-spec write. Core records
	// it so a stale snapshot (an older ConfigGen arriving after a newer
	// one) can be detected and ignored. Optional; zero means "not tracked"
	// (Core accepts the message regardless).
	ConfigGen int64 `json:"config_gen,omitempty"`
}

// PlantClaimsStyle is one style of a process in a PlantClaimsReport.
type PlantClaimsStyle struct {
	// StyleID is the Edge plant-spec style identifier (the style's Name on
	// Edge within ProcessID). Together (ProcessID, StyleID) is the
	// sourceability computation key.
	StyleID string `json:"style_id"`
	// Claims is the set of sourceability-relevant node claims for this
	// style. Manual_swap (loader/unloader) claims are excluded at the
	// publisher; only the material-flow claims the netting reads appear.
	Claims []PlantClaim `json:"claims"`
	// Active marks the style the process is currently running — Edge's
	// processes.active_style_id, which is the same field Edge already
	// resolves node claims through (findActiveClaim keys on it). At most one
	// style per process carries it.
	//
	// Additive: an older Core simply never sets its mirror column, and the
	// sourcing page keeps saying no style is marked running. Absent on a
	// report from an older Edge, which is indistinguishable from "this
	// process has no active style set" — both mean Core must not claim to
	// know what is running.
	Active bool `json:"active,omitempty"`
}

// PlantClaim is one (node, payload) assignment under a style — the
// sourceability subset of an Edge NodeClaim. Fields mirror ONLY what the
// computation reads; everything else (staging, pairing, flags, reorder
// source) stays on Edge and never crosses.
type PlantClaim struct {
	// CoreNodeName is the node the claim binds (Core's node-name key
	// space). The netting nets bin/order/reservation state against this.
	CoreNodeName string `json:"core_node_name"`
	// Role is the material-flow direction: "consume" (node pulls material
	// from upstream) or "produce" (node pushes material downstream).
	Role ClaimRole `json:"role"`
	// SwapMode is the changeover/dispatch mode for the claim. Carried so
	// the computation can distinguish the flow modes; manual_swap never
	// appears (excluded at the publisher).
	SwapMode SwapMode `json:"swap_mode"`
	// PayloadCode is the binding payload for non-manual_swap claims. For a
	// single-payload claim this is the value the netting matches against.
	PayloadCode string `json:"payload_code"`
	// AllowedPayloadCodes is the effective payload set this claim accepts
	// (ClaimAllowedPayloads on Edge). For most claims this is just
	// [PayloadCode]; carried as the canonical set the netting reads.
	AllowedPayloadCodes []string `json:"allowed_payload_codes"`
	// UOPCapacity is the bin capacity (UOP) for the claim's payload — the
	// denominator context for time-to-empty. Read by the at-risk tier.
	UOPCapacity int `json:"uop_capacity"`
	// ReorderPoint is the role-dependent replenishment trigger (consume:
	// UOP threshold; produce: bin-count floor). Carried so the
	// computation's fill-priority signal has the configured trigger.
	ReorderPoint int `json:"reorder_point"`
}

// SourcingStateReport is the Core → Edge sourceability feed (SubjectSourcingState).
// It carries the verdict for one or more (process, style) pairs. Snapshot marks a
// full replace (every style Core knows, so Edge can drop stale rows); a change
// delta (Snapshot=false) carries only the styles whose verdict just changed.
// Value schema is ADDITIVE-only — an older Edge ignores unknown fields, and one
// that does not register the subject logs-and-drops the whole message.
type SourcingStateReport struct {
	States   []SourcingState `json:"states"`
	Snapshot bool            `json:"snapshot,omitempty"`
}

// SourcingState is one (process, style)'s sourceability verdict on the wire.
// Status is the GATED result: when the owner has not enabled the at-risk tier a
// yellow-computed style arrives here as "green" with AtRisk empty, so no screen
// ever shows an unvalidated yellow.
type SourcingState struct {
	ProcessID string `json:"process_id"`
	StyleID   string `json:"style_id"`
	// Status is "green", "yellow", "red", or "not_configured".
	//
	// "not_configured" means the style has no sourceability claims, so there is
	// no verdict — NOT that it is fine. It is never selectable. An older Edge
	// that does not know the value must treat it as not-selectable rather than
	// falling back to a capable state; the picker's default arm does that.
	Status string `json:"status"`
	// Missing lists the payloads no available bin could satisfy (RED only).
	Missing []string `json:"missing,omitempty"`
	// AtRisk lists lines projecting empty within the horizon (yellow tier only).
	AtRisk []SourcingAtRisk `json:"at_risk,omitempty"`
	// Reason is the operator-facing generated sentence (Core owns the wording;
	// the HMI displays it verbatim and never invents text).
	Reason     string    `json:"reason,omitempty"`
	ComputedAt time.Time `json:"computed_at"`
}

// SourcingAtRisk is one line's time-to-empty projection on the wire.
type SourcingAtRisk struct {
	PayloadCode        string  `json:"payload_code"`
	Node               string  `json:"node,omitempty"`
	TimeToEmptySeconds float64 `json:"time_to_empty_seconds"`
}

// DemandOriginState is the WHOLE episode row, sent on every change.
//
// STATE TRANSFER, NOT EVENTS, and the reason is a lesson this repo already
// learned once. The threshold monitor used to keep a private incremental UOP
// tally that drifted from the database — Springfield stuck at 139 while truth
// was 31 — and the fix was to stop accumulating deltas and read the
// authoritative value every time. Rebuilding episode state on Core by replaying
// opened/closed events is that same mistake in a new place: a second copy,
// maintained by replay, that can diverge from the one Edge holds.
//
// Core upserts on origin_id, guarded by Revision. What that buys is
// STRUCTURAL rather than handled:
//
//	duplicate delivery   same revision, no-op. No conflict error to train
//	                     somebody to ignore.
//	out-of-order arrival the older message loses the comparison. No parking
//	                     queue, no reconcile branch.
//	lost message         the next state change carries current truth.
//	the LAST message     is sufficient on its own — lose everything except the
//	                     close and Core still converges. With events, a lost
//	                     "opened" is unrecoverable.
//
// Test the reversed and repeated cases explicitly. Both happen during a network
// event, which is when nobody is reading logs.
type DemandOriginState struct {
	OriginID string `json:"origin_id"`
	// Revision is monotonic per origin, stamped by Edge on every change. It is
	// the ONLY thing that decides whether an arriving message is newer than
	// what Core holds — not a timestamp, which two services cannot agree on.
	Revision int64 `json:"revision"`
	// EpisodeKey is the computed identity. Core keys its partial unique index
	// on it, so both services build it with the constructors in episode_key.go
	// rather than formatting their own.
	EpisodeKey string `json:"episode_key"`
	Kind       string `json:"kind"`
	// Direction carries the cell's ROLE — produce or consume. Typed, and the two
	// values are the claim's own; "supply"/"evacuate" were a second vocabulary
	// for the same fact and are retired (see protocol/episode_key.go). The JSON
	// key stays `direction` so the wire shape is unchanged; only the value
	// domain moved, which migration 87 carries for stored rows.
	Direction ClaimRole `json:"direction,omitempty"`
	Trigger   string    `json:"trigger,omitempty"`
	// TriggerRef is the claim key or ProcessChangeoverID behind the mint —
	// forensic, not identity.
	TriggerRef string `json:"trigger_ref,omitempty"`
	// ProcessID is THE GRAIN for a cell episode: choreography spans nodes, the
	// process does not.
	//
	// THE EDGE PROCESS NAME ("SNF2"), NOT ITS SQLITE ROW ID. It was an int64 row
	// id, which made Core hold two unjoinable identity systems for one set of
	// processes — this field against process_styles.process_id and
	// PlantClaimsReport.ProcessID, both of which are the name. See
	// CellEpisodeKey in episode_key.go for the full argument and for the one
	// exposure the change adds.
	ProcessID string `json:"process_id,omitempty"`
	// CoreNodeName is the head node. Forensic, not the key.
	CoreNodeName string    `json:"core_node_name,omitempty"`
	PayloadCode  string    `json:"payload_code,omitempty"`
	OpenedAt     time.Time `json:"opened_at"`
	// OpenedTotal is the reading at the falling edge — the number the decision
	// was made from, whatever its quality.
	OpenedTotal int `json:"opened_total"`
	Threshold   int `json:"threshold"`
	// ExpectedOrders is the system's own stated intent, STAMPED ONCE at the
	// falling edge and never recomputed or accumulated.
	//
	// NULLABLE, and that is deliberate. The threshold kind's formula divides by
	// the payload catalog's UOPCapacity, and fireThresholdL1 explicitly guards
	// `entry.UOPCapacity <= 0` — which means somebody has hit it. Neither 0 nor
	// 1 is honest there: both render as a real ratio and invite a conclusion
	// from a denominator that does not exist. A demand whose denominator is
	// UNKNOWABLE is a different state from one whose denominator is 1, and the
	// surface shows it as "—".
	//
	// For a cell episode it is len(plan orders) — what BuildConsumePlan said it
	// would create — NOT the literal 1 that RequestNodeMaterial takes as a BIN
	// count. Accumulating it per re-fire is the failure mode most likely to
	// look correct in review: it would render 2026-07-21 as ratio 1.0.
	ExpectedOrders *int `json:"expected_orders,omitempty"`
	// ExpectedUnknownReason says WHY the denominator is unknowable, when it is.
	// A NULL with no reason is indistinguishable from a bug.
	ExpectedUnknownReason string `json:"expected_unknown_reason,omitempty"`
	// RerequestCount is operator pushes that JOINED this episode.
	RerequestCount int `json:"rerequest_count,omitempty"`
	// Discretionary marks an operator request with no open episode on a node
	// the system reads as fine. Either the ledger is wrong, or the reorder
	// point is, or the operator knows something the count does not. FLAG, DO
	// NOT CONCLUDE.
	Discretionary bool `json:"discretionary,omitempty"`
	// ClosedAt is nil while the episode is open. Its presence IS the close —
	// there is no separate close message to lose.
	ClosedAt *time.Time `json:"closed_at,omitempty"`
	// CloseReason is one of the CloseReason* constants. Empty while open.
	CloseReason string `json:"close_reason,omitempty"`
	// ClosedBy is "notification" or "sweep" — WHICH MECHANISM ended the
	// episode. Empty means the sender did not say, which is a different fact
	// from "a notification path closed it": an older Edge sends nothing here.
	// It exists because the reconciling sweep uses the same close_reason codes
	// as the notification paths deliberately, so without this a total silent
	// failure of every notification path is indistinguishable from health.
	ClosedBy string `json:"closed_by,omitempty"`
}

// Episode close reasons.
const (
	// CloseReasonRecovered — the level came back above its threshold plus the
	// hysteresis margin. The ordinary ending.
	CloseReasonRecovered = "recovered"
	// CloseReasonChangeoverComplete / CloseReasonCancelled — the changeover
	// kind's two endings.
	CloseReasonChangeoverComplete = "changeover_complete"
	CloseReasonCancelled          = "cancelled"
	// CloseReasonThresholdChanged — the denominator moved, so the episode ends
	// and a new one opens. Continuing would make cost_ratio a division by a
	// number that was never in force.
	CloseReasonThresholdChanged = "threshold_changed"
	// CloseReasonThresholdRemoved — the binding went away underneath it.
	CloseReasonThresholdRemoved = "threshold_removed"
	// CloseReasonClaimRemoved — the cell-side mirror of threshold_removed: the
	// claim that was below its level is gone, or the process swapped to a style
	// that does not claim that payload there. The need did not recover; it
	// stopped being asked. Reachable only from the reconciler, because nothing
	// fires when a claim quietly stops existing — which is the whole reason the
	// reconciler exists.
	CloseReasonClaimRemoved = "claim_removed"
	// CloseReasonUnattributed — a childless episode aged out. NOT OPTIONAL:
	// childless episodes are reachable even at full version parity, because
	// Edge already silently drops threshold signals it cannot resolve. An
	// alarm that never clears is indistinguishable from a broken one.
	CloseReasonUnattributed = "unattributed"
)

// Which mechanism ended an episode. Values for DemandOriginState.ClosedBy and
// demand_origins.closed_by.
//
// The sweep uses the SAME close_reason codes as the notification paths, on
// purpose — one vocabulary for one set of facts about the plant. This is how
// you tell which machinery is actually doing the work, and therefore the only
// way a total silent failure of the notification paths is visible at all.
const (
	ClosedByNotification = "notification"
	ClosedBySweep        = "sweep"
)

// ─── Supply refusal ──────────────────────────────────────────────────────

// SupplyRefusalAction discriminates what happened to a refusal. Each action
// writes a DISJOINT set of fields, which is what lets a row with two authors on
// two different edges — the loader opens it, the cell answers it — merge without
// a revision counter or a last-writer-wins race.
const (
	// SupplyRefusalOpened — the loader operator said they cannot fill the call.
	SupplyRefusalOpened = "opened"
	// SupplyRefusalAcked — the cell answered: wait, or change over.
	SupplyRefusalAcked = "acked"
	// SupplyRefusalClosed — the refusal ended. A LOAD at that window (the normal
	// path, the parts arrived) or UNDO (the mis-tap path). Both delete.
	SupplyRefusalClosed = "closed"
)

// SupplyRefusalChoice is the cell operator's answer.
const (
	// SupplyRefusalChoiceWait — "I know, and I am holding for this part." The
	// window stays open, the order stays queued, nothing is cancelled.
	SupplyRefusalChoiceWait = "wait"
	// SupplyRefusalChoiceChangeover — the demand is abandoned; the cell is going
	// a different direction. Routes through StartProcessChangeover, which cancels
	// the pre-dispatch orders as a general property.
	SupplyRefusalChoiceChangeover = "changeover"
)

// SupplyRefusalState is one refusal, keyed on the CARD the loader operator is
// standing at: (LoaderNode, PayloadCode). Both board layouts reduce to that
// pair — a shared window renders one card per payload, a dedicated home one card
// per position — so the key needs no layout branch anywhere it travels.
//
// Sent Edge → Core on SubjectSupplyRefusal, and Core → every edge on
// SubjectSupplyRefusalState. The receiving edge filters locally: show it to a
// cell that has an outstanding call for that part, which is the same predicate
// the supplier's endpoint enforces before accepting the refusal.
type SupplyRefusalState struct {
	// Action is one of SupplyRefusalOpened / Acked / Closed and decides which of
	// the fields below are authoritative in this message.
	Action string `json:"action"`

	LoaderNode  string `json:"loader_node"`
	PayloadCode string `json:"payload_code"`

	// Opened fields.
	RefusedAt time.Time `json:"refused_at,omitempty"`
	// RefusedBy is STATION-level. The loader board carries no operator identity,
	// so this is the station name and not a person — named honestly rather than
	// dressed up as attribution it cannot make.
	RefusedBy string `json:"refused_by,omitempty"`

	// Acked fields.
	AckAt     *time.Time `json:"ack_at,omitempty"`
	AckChoice string     `json:"ack_choice,omitempty"`
	// AckProcessID is the process NAME ("SNF2") of the cell that ANSWERED,
	// matching the demand grain. Note it is who answered, not who was told —
	// with a broadcast there is no single addressee to record.
	AckProcessID string `json:"ack_process_id,omitempty"`
}
