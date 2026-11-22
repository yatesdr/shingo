package protocol

// ephemeral.go — the live topic: short-lived, lossy messages on a Kafka topic
// of their own (messaging.ephemeral_topic, default shingo.ephemeral), named for
// the class of data and not for what rides it today (owner ruling, shared-map
// brief step 8; LEAD-NOTES D1–D4).
//
// WHAT MAKES A MESSAGE EPHEMERAL. Its successor carries the same truth a
// moment later, so a lost one costs nothing and a late one is worth less than
// nothing. So it is published straight to the topic: no outbox row, no retry,
// and every one carries an `exp` EphemeralTTL after it was stamped. A reader
// joins no consumer group and starts at the topic's last offset, so a reader
// that (re)starts sees only what is published after it, and it commits nothing.
// What a reader drops — a bad signature, a message past its `exp`, one not
// addressed to it — it drops without a log line: on this topic those are the
// ordinary case, and a line for each would be a line every two seconds.
//
// WHO TOUCHES THE TOPIC. Core creates it (retention.ms and segment.ms 60 s),
// reads its config back and corrects it, and only then names it to the Edges
// (EdgeRegistered.EphemeralTopic, EdgeHeartbeatAck.EphemeralTopic). An Edge
// reads and writes it only while it holds that name. The plant brokers
// auto-create topics, so a reader that came first would bring the topic into
// being with the broker's 7-day default; the order of the two is what stops it.
//
// TYPES, NOT DATA SUBJECTS, and outside AllTypes. These never ride the two
// durable topics and never reach the Ingestor or the durable topics' routers:
// each side reads the topic through router.ReadEphemeral (DecodeEphemeral,
// then a router of its own holding only the types that side reads). A data
// subject would wrap every body in {"subject","data"} (about 40 bytes on a
// message sent every 2 s per watching station) and would put two entries in
// the subject routers' inbound lists that those routers never dispatch. They
// are kept out of AllTypes for the same reason: AllTypes is what both durable
// routers must register, and the docs' type registry is built from it.

import (
	"encoding/json"
	"time"
)

// Message types on the ephemeral topic.
const (
	// TypeRobotWatch — Edge → Core: the Core orders this Edge's operator
	// stations are showing a robot for (RobotWatch). Sent every
	// RobotWatchInterval while any is shown; Core keeps it RobotWatchLease.
	TypeRobotWatch = "robot.watch"
	// TypeRobotPositions — Core → one Edge (dst = its station uid): where the
	// robot of each watched order is and the route it has left
	// (RobotPositions), one per robot poll while the Edge's watch is live.
	TypeRobotPositions = "robot.positions"
)

// EphemeralTypes returns every message type on the ephemeral topic.
func EphemeralTypes() []string {
	return []string{TypeRobotWatch, TypeRobotPositions}
}

// EphemeralTTL is how long an ephemeral message is worth reading: every one
// carries exp = its stamp + EphemeralTTL (owner ruling, 10 s).
//
// IT IS ALSO THE CLOCK SKEW A STATION TOLERATES, as the heartbeat's TTL is
// (expiry.go): exp is stamped on the sender's clock and read on the receiver's,
// so an Edge whose clock runs more than this ahead of Core's drops every
// position as expired, and silently. Core's Edges page already flags an Edge
// whose clock is further than InventoryDeltaFlushWindow (5 s) from its own.
const EphemeralTTL = 10 * time.Second

// RobotWatchInterval is how often an Edge resends its watch while anything is
// shown, and RobotWatchLease how long Core honours one after it arrives: three
// resends, so one or two lost watches do not end the feed, and an Edge that
// stops sending (nothing shown, or the Edge gone) costs Core at most this long.
const (
	RobotWatchInterval = 5 * time.Second
	RobotWatchLease    = 15 * time.Second
)

// RobotPollInterval is Core's robot poll: how often Core reads the fleet's
// robots (engine robotRefreshLoop), and so how often a live watch is answered.
// Every robot.positions says it (RobotPositions.IntervalMS), and so does the
// Edge's {available} frame on a robot panel's stream, so a panel can count
// missed positions before its first position arrives (LEAD-NOTES D13, D17).
const RobotPollInterval = 2 * time.Second

// RobotWatch is one Edge's watch request (TypeRobotWatch). Core keys it on
// the envelope's source station.
type RobotWatch struct {
	// OrderUUIDs are the Core order uuids this Edge's operator stations are
	// showing, together, sorted and each once. Core honours only the ones whose
	// order is this station's. Every watch replaces the one before it.
	OrderUUIDs []string `json:"order_uuids"`
}

// RobotPositions is Core's answer to a live watch (TypeRobotPositions): one
// robot poll, for one station's watched orders.
type RobotPositions struct {
	// T is when Core's robot poll sampled the fleet (Core's clock, UTC,
	// millisecond precision).
	T time.Time `json:"t"`
	// IntervalMS is Core's robot poll interval, so a reader can count the
	// updates it missed.
	IntervalMS int64 `json:"interval_ms"`
	// Orders is one entry per watched order of this station that the fleet
	// holds on a robot (IsVendorTracked) and whose robot this poll listed
	// connected. Any other is left out rather than sent stale; [] when none
	// is.
	Orders []RobotPosition `json:"orders"`
}

// RobotPosition is where one watched order's robot is.
type RobotPosition struct {
	// OrderUUID is the watched order's Core uuid.
	OrderUUID string `json:"order_uuid"`
	// RobotID is the robot Core holds the order on (orders.robot_id), read on
	// this poll: the live robot, not the one an earlier message named.
	RobotID string `json:"robot_id"`
	// X and Y are the robot's position in the scene's frame, metres.
	X float64 `json:"x"`
	Y float64 `json:"y"`
	// Heading is the robot's heading, radians, as the fleet reports it.
	Heading float64 `json:"heading"`
	// Moving is Core's word on whether the robot is driving, decided on its
	// robot poll from every sample (engine robot_motion.go), so a reader knows
	// on its first message. A Core predating it sends false, so deploy Core
	// first.
	Moving bool `json:"moving"`
	// Path is the route the fleet has left the robot to drive, as scene point
	// names, the robot's current point first. Absent when there is none to
	// draw: no plan, an id the scene cannot name, or no synced scene.
	Path []string `json:"path,omitempty"`
}

// DecodeEphemeral verifies and decodes one message read off the ephemeral
// topic. It reports false for every message the reader drops: a bad or
// missing signature (when signingKey is set), an undecodable header or body, a
// message with no exp or past it, and one accept refuses. It logs nothing and
// counts nothing: on this topic each of those is the ordinary case (see the
// file header), and the expired-drop count the health strip reads
// (ExpiredDrops) is about the durable topics.
//
// accept sees the header only, so a message for another station is never
// decoded past it.
func DecodeEphemeral(data, signingKey []byte, accept FilterFunc) (*Envelope, bool) {
	inner, err := VerifyAndUnwrap(data, signingKey)
	if err != nil {
		return nil, false
	}
	var hdr RawHeader
	if err := json.Unmarshal(inner, &hdr); err != nil {
		return nil, false
	}
	// A message with no exp would never expire (IsExpiredHeader), and every
	// ephemeral sender stamps one, so none is a message not meant for here.
	if hdr.ExpiresAt.IsZero() || IsExpiredHeader(&hdr) {
		return nil, false
	}
	if accept != nil && !accept(&hdr) {
		return nil, false
	}
	var env Envelope
	if err := json.Unmarshal(inner, &env); err != nil {
		return nil, false
	}
	return &env, true
}
