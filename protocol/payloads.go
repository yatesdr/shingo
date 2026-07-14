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
// Catalog is an ADDITIVE Q-034 field (omitempty): an old core unmarshals and
// ignores it, an old edge simply omits it — absent catalog means "no catalog",
// not an error. No envelope/version bump (see version-skew-research.md).
type EdgeRegister struct {
	StationID string             `json:"station_id"`
	Hostname  string             `json:"hostname"`
	Version   string             `json:"version"`
	LineIDs   []string           `json:"line_ids"`
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
	// Which ROLE that is depends on the mode, so do not read a role into it:
	// two_robot creates the supply first and the evac second, but
	// two_robot_press_index creates the EVAC first (R1 clears the press) and the
	// SUPPLY second (R2 indexes the fresh bin onto it). The pointer says
	// "these two legs are a pair", nothing more; a leg's role comes from its
	// steps (see legTakesLineBin in Core's dispatch package).
	//
	// Core links both order rows on ingest — bidirectionally, via
	// LinkSiblingsByEdgeUUID — so either leg can find the other, and the
	// dispatch hold can see the pairing at intake, before a removal leg's
	// synchronous dispatch claims the line bin.
	SiblingOrderUUID string `json:"sibling_order_uuid,omitempty"`
	// RemainingUOP: nil = no sync, 0 = clear manifest, >0 = partial consumption.
	RemainingUOP *int `json:"remaining_uop,omitempty"`
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
	OrderUUID   string               `json:"order_uuid"`
	PayloadCode string               `json:"payload_code"`
	BinLabel    string               `json:"bin_label"` // optional: blank => resolve the bin by SourceNode
	SourceNode  string               `json:"source_node"`
	Quantity    int64                `json:"quantity"` // operator-measured produced count (UOP); 0 => payload capacity
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
	LoaderKey     string              `json:"loader_key"`
	Role          string              `json:"role"`
	Layout        string              `json:"layout"`
	Replenishment string              `json:"replenishment"`
	OutboundDest  string              `json:"outbound_dest,omitempty"`
	InboundSource string              `json:"inbound_source,omitempty"`
	BufferDest    string              `json:"buffer_dest,omitempty"`
	ConfigGen     int64               `json:"config_gen"`
	Positions     []LoaderPosition    `json:"positions,omitempty"`
	Payloads      []LoaderPayloadInfo `json:"payloads,omitempty"`
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
	UOPThreshold int    `json:"uop_threshold"`
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

// --- Node State ---

// NodeStateRequest is sent by edge to query the occupancy state of specific nodes.
type NodeStateRequest struct {
	Nodes []string `json:"nodes"` // node names to query
}

// NodeStateEntry describes the occupancy state of a single node.
type NodeStateEntry struct {
	Name        string `json:"name"`
	Occupied    bool   `json:"occupied"`               // has at least one bin
	BinCount    int    `json:"bin_count"`              // number of bins at node
	Claimed     bool   `json:"claimed"`                // any bin claimed by an active order
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
	// QueueReason is Core's current blocking signal for a queued order
	// (orders.queue_reason) — e.g. "no bin of requested payload in node
	// group AMR Supermarket". Re-evaluated by the dispatcher/scanner and
	// cleared on dispatch, so Edge refreshes it on each status resync.
	QueueReason string `json:"queue_reason,omitempty"`
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
//
// PayloadThresholds (UOP-threshold replenishment) carries per-payload
// replenish_uop_threshold values for loader produce-role claims.
// Map key is payload_code, value is the threshold UOP count. Entries
// with value 0 are omitted from the wire (legacy bin-count behavior
// preserved). Non-produce claims send an empty/nil map. Core uses this
// to populate demand_registry.replenish_uop_threshold so the threshold
// monitor can compare combined in-loop UOP against the configured
// trigger.
type ClaimSyncEntry struct {
	CoreNodeName        string         `json:"core_node_name"`
	Role                ClaimRole      `json:"role"`
	AllowedPayloadCodes []string       `json:"allowed_payload_codes"` // payloads this node accepts
	OutboundDestination string         `json:"outbound_destination"`
	PayloadThresholds   map[string]int `json:"payload_thresholds,omitempty"` // payload_code → threshold; omit 0
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
type CounterSnapshot struct {
	Station          string    `json:"station"`            // envelope source, from Edge identity
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
// Core deduplicates on (station, edge_event_id).
type DowntimeEvent struct {
	Station     string    `json:"station"`       // envelope source (Edge identity)
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
type LinesideBucketDelta struct {
	Station      string                    `json:"station"`
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

// LoopBelowThresholdSignal is sent by Core to Edge when total in-loop
// UOP for a (loader, payload) pair drops below the configured threshold.
// Edge responds by firing an L1 retrieve_empty order via
// refillLoaderForPayload after the countLoaderInFlightEmptyIn dedup
// guard.
//
// CoreNodeName is the loader-member node the binding is about — a real node
// (the position for dedicated, the first window for shared_window). The loader's
// IDENTITY is LoaderKey; the Edge resolves the loader by that token and uses
// CoreNodeName/MemberNodeName only as the delivery address.
//
// Reason carries either "below_threshold" (normal crossing) or
// "warm_up_startup_sweep" (Core startup observed an existing
// under-threshold state and is firing up to the per-binding warm-up
// cap). The signal is the only path that fires L1 for opted-in pairs;
// the legacy DemandSignal path in HandleDemandSignal explicitly skips
// these pairs to avoid redundant evaluation.
type LoopBelowThresholdSignal struct {
	PayloadCode  string `json:"payload_code"`
	CurrentUOP   int    `json:"current_uop"`
	Threshold    int    `json:"threshold"`
	CoreNodeName string `json:"core_node_name"`
	// MemberNodeName names the specific loader member (a dedicated position, or a
	// shared window) the binding is about — distinct from the loader IDENTITY. The
	// Edge routes the empty to THIS node instead of first-match, fixing the
	// same-payload-on-two-positions bug (O2). Additive (omitempty); for a
	// shared_window loader it is empty/the anchor and the seam funnels to a free
	// window regardless. Today CoreNodeName still doubles as identity+member; step 4
	// splits them (CoreNodeName → identity, MemberNodeName → the address).
	MemberNodeName string `json:"member_node_name,omitempty"`
	// LoaderKey is the loader's opaque identity token ("loader:<id>"). Additive;
	// populated at the step-4 identity cutover (when demand_registry carries
	// loader_id) and becomes the Edge cache key the signal resolves against. Empty
	// before then — the Edge resolves via CoreNodeName until the cutover.
	LoaderKey string `json:"loader_key,omitempty"`
	Reason    string `json:"reason"`
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
	// Epoch carries the moved bin's delta_epoch for the Bound path so the
	// destination seeds active_bin_epoch correctly and subsequent BinUOPDeltas
	// carry the right generation for Core's epoch-aware dedup. Meaningful only
	// when Bound; zero on the Released / legacy-adjustment paths.
	Epoch int64 `json:"epoch,omitempty"`
}
