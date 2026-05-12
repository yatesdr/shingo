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

// EdgeRegister is sent by an edge on startup.
type EdgeRegister struct {
	StationID string   `json:"station_id"`
	Hostname  string   `json:"hostname"`
	Version   string   `json:"version"`
	LineIDs   []string `json:"line_ids"`
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
	PayloadCode   string `json:"payload_code,omitempty"`
	PayloadDesc   string `json:"payload_desc,omitempty"`
	Quantity      int64  `json:"quantity"`
	DeliveryNode  string `json:"delivery_node,omitempty"`
	SourceNode    string `json:"source_node,omitempty"`
	StagingNode   string `json:"staging_node,omitempty"`
	LoadType      string `json:"load_type,omitempty"`
	Priority      int    `json:"priority,omitempty"`
	RetrieveEmpty bool   `json:"retrieve_empty,omitempty"`
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
}

// OrderCancel cancels an existing order.
type OrderCancel struct {
	OrderUUID string `json:"order_uuid"`
	Reason    string `json:"reason"`
}

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

// OrderStorageWaybill submits a store order.
type OrderStorageWaybill struct {
	OrderUUID   string    `json:"order_uuid"`
	OrderType   OrderType `json:"order_type"`
	PayloadDesc string `json:"payload_desc,omitempty"`
	SourceNode  string `json:"source_node"`
	FinalCount  int64  `json:"final_count"`
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
// per SME (open-items.md Q2'').
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
	Action string `json:"action"`          // "pickup", "dropoff", "wait"
	Node   string `json:"node,omitempty"`  // node or group name (Core auto-resolves groups)
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
	// RemainingUOP: nil = no sync, 0 = clear manifest, >0 = partial consumption.
	RemainingUOP *int `json:"remaining_uop,omitempty"`
}

// UOPDispositionKind names the operator's release-time intent. Phase 0c of
// the UOP bin-as-truth refactor introduces this enum to replace the
// nil/0/N pointer overload on RemainingUOP. Values map 1:1 to the three
// release buttons in the operator UI per the SME process map (see
// shingo-uop-plan-bin-as-truth.md §2.5):
//
//   - DispositionPullParts (button: PULL PARTS LINESIDE, RELEASE) — operator
//     pulled some parts to lineside; bin reduced by sum of captures, lineside
//     buckets increased. NOT the same as "bin is empty" — Phase 1's delta-
//     based handler will treat this as a partial decrement, not a wipe.
//   - DispositionReleasePartial (button: RELEASE PARTIAL) — operator declares
//     the bin still holds Count parts; bin returns to supermarket as-is with
//     manifest preserved.
//   - DispositionReleaseEmpty (button: RELEASE EMPTY) — bin physically empty;
//     manifest cleared.
//
// Wire transition: both this enum and the legacy RemainingUOP pointer ship
// for one release. Edge populates whichever it knows about; Core prefers the
// enum when present and falls back to RemainingUOP otherwise. The pointer
// is removed in Phase 4 cleanup once every Edge in the field is on the new
// shape.
type UOPDispositionKind string

const (
	DispositionPullParts       UOPDispositionKind = "pull_parts"
	DispositionReleasePartial  UOPDispositionKind = "release_partial"
	DispositionReleaseEmpty    UOPDispositionKind = "release_empty"

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
// is keyed by part number with the per-part captured quantity. Phase 1
// will route this through LinesideBucketDelta (capture_fill); for Phase 0c
// the field rides on the wire but Core does not yet act on it.
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
// Disposition (new shape, Phase 0c) carries the same intent as a typed enum,
// disambiguating the capture_lineside overload that today serves both
// "operator pulled parts" and "bin is empty" via the same on-wire value.
// Phase 0c lands the field; Phase 1 wires Edge to populate it and Core to
// prefer it. Both shapes are accepted on the wire for one release.
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

// OrderIngestRequest submits a newly filled bin for storage.
// Core sets the bin manifest and dispatches a store order.
type OrderIngestRequest struct {
	OrderUUID   string               `json:"order_uuid"`
	PayloadCode string               `json:"payload_code"`
	BinLabel    string               `json:"bin_label"`
	SourceNode  string               `json:"source_node"`
	Quantity    int64                `json:"quantity"`
	Manifest    []IngestManifestItem `json:"manifest,omitempty"`
	ProducedAt  string               `json:"produced_at,omitempty"` // RFC3339 timestamp from Edge at cell completion
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
//
// ParentNodeType is the node type of the immediate parent (e.g. "LANE",
// "NGRP", empty for top-level nodes). Edge uses it to validate that
// consume-role style claims land on LANE-parented storage slots — the
// only nodes handleKanbanDemand will actually fire "consume" signals
// for (see shingo-core/engine/wiring_kanban.go's isStorageSlot check).
type NodeInfo struct {
	Name           string `json:"name"`
	NodeType       string `json:"node_type"`
	ParentNodeType string `json:"parent_node_type,omitempty"`
}

// NodeListResponse carries the core's authoritative node list.
type NodeListResponse struct {
	Nodes []NodeInfo `json:"nodes"`
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

// --- Node State ---

// NodeStateRequest is sent by edge to query the occupancy state of specific nodes.
type NodeStateRequest struct {
	Nodes []string `json:"nodes"` // node names to query
}

// NodeStateEntry describes the occupancy state of a single node.
type NodeStateEntry struct {
	Name        string `json:"name"`
	Occupied    bool   `json:"occupied"`     // has at least one bin
	BinCount    int    `json:"bin_count"`     // number of bins at node
	Claimed     bool   `json:"claimed"`       // any bin claimed by an active order
	PayloadCode string `json:"payload_code,omitempty"` // payload of first bin (if occupied)
}

// NodeStateResponse carries the occupancy state for requested nodes.
type NodeStateResponse struct {
	Nodes []NodeStateEntry `json:"nodes"`
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
}

// CatalogPayloadsResponse carries the core's payload catalog.
type CatalogPayloadsResponse struct {
	Payloads []CatalogPayloadInfo `json:"payloads"`
}

// OrderStatusRequest asks Core for the current authoritative status of a set of orders.
type OrderStatusRequest struct {
	OrderUUIDs []string `json:"order_uuids"`
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
}

// OrderStatusResponse carries the authoritative Core-side state for requested orders.
type OrderStatusResponse struct {
	Orders []OrderStatusSnapshot `json:"orders"`
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

// ClaimSyncEntry represents a single manual_swap claim's config for the demand registry.
type ClaimSyncEntry struct {
	CoreNodeName        string    `json:"core_node_name"`
	Role                ClaimRole `json:"role"`
	AllowedPayloadCodes []string  `json:"allowed_payload_codes"` // payloads this node accepts
	OutboundDestination string    `json:"outbound_destination"`
}

// ClaimSync is sent by Edge to Core on startup and claim changes.
// Core uses this to build its demand registry for kanban wiring.
type ClaimSync struct {
	StationID string           `json:"station_id"`
	Claims    []ClaimSyncEntry `json:"claims"`
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
	CorrelationID    string    `json:"corr_id"`             // for matching the eventual CountGroupAck
	Group            string    `json:"group"`               // RDS advanced-group name
	Desired          string    `json:"desired"`             // "on" | "off"
	Robots           []string  `json:"robots"`              // robot IDs in the zone (for audit)
	RobotCount       int       `json:"robot_count"`         // len(Robots) — cheap log without decoding the slice
	FailSafeTriggered bool     `json:"fail_safe_triggered"` // true if this command came from RDS-down fail-safe
	Timestamp        time.Time `json:"ts"`
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
// Both ride the existing HandleData generic dispatch via subject
// discriminator (Decision #1 / B1 fix in plan §2.6) — adding new typed
// methods on MessageHandler would silently no-op through the InboxDedup
// decorator, so we route on Subject from inside HandleData.
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
	WindowStart time.Time         `json:"window_start"`
	WindowEnd   time.Time         `json:"window_end"`
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
type LinesideBucketDelta struct {
	Station     string                    `json:"station"`
	NodeID      int64                     `json:"node_id"`
	PairKey     string                    `json:"pair_key"`
	StyleID     int64                     `json:"style_id"`
	PartNumber  string                    `json:"part_number"`
	Delta       int                       `json:"delta"`
	Reason      LinesideBucketDeltaReason `json:"reason"`
	SequenceID  int64                     `json:"sequence_id"`
	WindowStart time.Time                 `json:"window_start"`
	WindowEnd   time.Time                 `json:"window_end"`
}
