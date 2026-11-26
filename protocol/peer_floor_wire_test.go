package protocol

import "time"

// peer_floor_wire_test.go: the bodies a v0.19.0 Core and a v0.19.0 Edge encode,
// for every message that carries a key that release keeps for an older peer.
// testdata/peerwire/v0.19.0 holds the release's bytes (gen.go there, run inside
// the tag's tree); the TestPeerWire_* tests (peer_wire_test.go) run them both
// ways.
//
// deletable is the rule: a key listed may differ, because no v0.19.0 reader
// uses it when the rest of the body is that release's (the semantic half is
// TestFloorWire_* in shingo-edge/engine and shingo-core/messaging). A key not
// listed stays. Three of those are kept only for a v0.19.0 peer:
//
//   - EdgeRegistered.station_id and EdgeHeartbeatAck.station_id: a v0.19.0
//     Edge logs them.
//   - OrderDelivered.landing_sent: without it a v0.19.0 Edge binds from
//     order.delivered.

// peerFloorWireCases are the v0.19.0 cases.
func peerFloorWireCases() []peerWireCase {
	const core, edge = "the v0.19.0 Core", "the v0.19.0 Edge"
	return []peerWireCase{
		{"v0.19.0/EdgeRegister", edge, []string{"station_id"},
			func() any { return &EdgeRegister{} }, func() any { return &floorEdgeRegister{} }, nil},
		{"v0.19.0/EdgeHeartbeat", edge, []string{"station_id"},
			func() any { return &EdgeHeartbeat{} }, func() any { return &floorEdgeHeartbeat{} }, nil},
		{"v0.19.0/RobotWatch", edge, []string{"station_id"},
			func() any { return &RobotWatch{} }, func() any { return &floorRobotWatch{} }, nil},
		{"v0.19.0/LinesideLevelReport", edge, []string{"station"},
			func() any { return &LinesideLevelReport{} }, func() any { return &peerLinesideLevelReport{} }, nil},
		{"v0.19.0/EdgeRegistered", core, nil,
			func() any { return &EdgeRegistered{} }, func() any { return &peerEdgeRegistered{} }, nil},
		{"v0.19.0/EdgeHeartbeatAck", core, []string{"claims", "bin_watch"},
			func() any { return &EdgeHeartbeatAck{} }, func() any { return &floorEdgeHeartbeatAck{} }, nil},
		{"v0.19.0/OrderDelivered", core, []string{"staged_expire_at", "uop_remaining", "delta_epoch", "bin_payload_code"},
			func() any { return &OrderDelivered{} }, func() any { return &floorOrderDelivered{} }, nil},
		{"v0.19.0/UOPAdjustment", core, []string{"bound"},
			func() any { return &UOPAdjustment{} }, func() any { return &floorUOPAdjustment{} }, nil},
	}
}

// ── The types as v0.19.0 declares them ────────────────────────────────────

type floorCellProcessBinding struct {
	ProcessID int64  `json:"process_id"`
	StyleID   int64  `json:"style_id"`
	PLCName   string `json:"plc_name"`
	TagName   string `json:"tag_name"`
}

type floorCellCatalogEntry struct {
	CellLabel string                    `json:"cell_label"`
	Processes []floorCellProcessBinding `json:"processes"`
}

type floorEdgeRegister struct {
	StationID     string                  `json:"station_id"`
	Hostname      string                  `json:"hostname"`
	Instance      string                  `json:"instance,omitempty"`
	Version       string                  `json:"version"`
	Timezone      string                  `json:"timezone,omitempty"`
	SchemaVersion int                     `json:"schema_version,omitempty"`
	Catalog       []floorCellCatalogEntry `json:"catalog,omitempty"`
}

type floorEdgeHeartbeat struct {
	StationID             string            `json:"station_id"`
	Timezone              string            `json:"timezone,omitempty"`
	TickPending           *int64            `json:"tick_pending,omitempty"`
	TickOldestUnsentAgeMS *int64            `json:"tick_oldest_unsent_age_ms,omitempty"`
	Feeds                 map[string]string `json:"feeds"`
	Owned                 map[string]string `json:"owned"`
}

type floorOwnedAnswer struct {
	Digest  string            `json:"digest"`
	Members map[string]string `json:"members,omitempty"`
}

type floorEdgeHeartbeatAck struct {
	StationID      string                      `json:"station_id"`
	ServerTS       time.Time                   `json:"server_ts"`
	Timezone       string                      `json:"timezone,omitempty"`
	DisplayName    *string                     `json:"display_name,omitempty"`
	Feeds          map[string]string           `json:"feeds"`
	Owned          map[string]floorOwnedAnswer `json:"owned"`
	Claims         map[string]string           `json:"claims"`
	BinWatch       *string                     `json:"bin_watch,omitempty"`
	Asks           []string                    `json:"asks,omitempty"`
	EphemeralTopic string                      `json:"ephemeral_topic,omitempty"`
}

type floorRobotWatch struct {
	StationID  string   `json:"station_id"`
	OrderUUIDs []string `json:"order_uuids"`
}

type floorOrderDelivered struct {
	OrderUUID      string     `json:"order_uuid"`
	DeliveredAt    time.Time  `json:"delivered_at"`
	StagedExpireAt *time.Time `json:"staged_expire_at,omitempty"`
	BinID          *int64     `json:"bin_id,omitempty"`
	UOPRemaining   *int       `json:"uop_remaining,omitempty"`
	DeltaEpoch     int64      `json:"delta_epoch,omitempty"`
	BinPayloadCode *string    `json:"bin_payload_code,omitempty"`
	DeliveryNode   string     `json:"delivery_node,omitempty"`
	BinDestNode    string     `json:"bin_dest_node,omitempty"`
	LandingSent    bool       `json:"landing_sent,omitempty"`
}

type floorUOPAdjustment struct {
	BinID        int64     `json:"bin_id"`
	CoreNodeName string    `json:"core_node_name"`
	NewRemaining int       `json:"new_remaining"`
	Actor        string    `json:"actor"`
	AdjustedAt   time.Time `json:"adjusted_at"`
	Released     bool      `json:"released,omitempty"`
	PlacedSeq    int64     `json:"placed_seq,omitempty"`
	Bound        bool      `json:"bound,omitempty"`
	Epoch        int64     `json:"epoch,omitempty"`
	AsOfNet      *int64    `json:"as_of_net,omitempty"`
	AsOfSeq      *int64    `json:"as_of_seq,omitempty"`
	AsOfStation  string    `json:"as_of_station,omitempty"`
	PayloadCode  string    `json:"payload_code,omitempty"`
	PayloadKnown bool      `json:"payload_known,omitempty"`
}
