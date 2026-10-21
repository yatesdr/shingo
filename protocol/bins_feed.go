package protocol

// bins_feed.go — the node-bins feed's wire types: the watch list an Edge
// publishes (SubjectBinsWatch) and the snapshot Core answers it with
// (SubjectBinsSnapshot, feed key FeedBins). The digests are in feeds.go with
// every other feed's.
//
// THE CONTRACT, AS THE EDGE READS IT. Stated as fact where the code makes it
// so, and as "should" where a later change does.
//
//   - Scope. The Edge publishes the names it watches (BinWatchList), built
//     from its own stations with no Core call. Core answers one row per name
//     and expands a watched group into its members. The list should widen to
//     every place an Edge decides about (process and stationless positions,
//     keep-staged spots, staging, every loader window, dedicated-home buffers,
//     consume-loader outbound members, FG outbound destinations as a static
//     superset) and should be published in the Edge's boot burst. Containment
//     destinations are the containment feed's.
//   - Row. State says what the name is: unknown (no node by that name, a
//     renamed one included), group, empty or occupied; an unknown name and a
//     group are never an empty. Bins is how many bins stand at the node; the
//     resident bin is the one of them with the highest id. Epoch is the
//     resident carrier's generation and moves at generation boundaries only.
//     Stocked and Partial move at crossings only. Inbound is the live orders
//     the dropoff gate counts as on their way to the node. The snapshot names
//     the watch list it answers (ListDigest).
//   - Currency. The Edge holds the copy in memory only. A screen shows a row
//     as current while its confirmation is younger than the display rule
//     (150 s). A decision should act only on a snapshot that is held, answers
//     the Edge's current list (ListDigest), was confirmed by the last
//     heartbeat ack, and whose ack is younger than one heartbeat plus slack;
//     otherwise it should refuse with a sentence, never guess.
//   - Ordering. The snapshot and the order messages leave Core's one outbox,
//     drained in id order onto one topic of one partition. A push follows the
//     change by binsPushDelay and reorders only after a failed publish. A
//     row's order facts are its Inbound: no reader should join its own order
//     rows with the copy.
//   - Who decides. The Edge interprets what its people and its line mean;
//     Core allocates among plant places; a decision is never split across the
//     transport. A choice between places or bins across the plant is Core's.
//   - Writes. An Edge write should carry what it read ({bin_id, epoch,
//     expect}, never "expect empty"), and Core should refuse a mismatch with a
//     sentence and the rows for the nodes it touched.
//   - Transport. Feeds, events, deltas and ticks over Kafka. An ask and its
//     answer should ride the same outboxes (CorID), with no HTTP between the
//     Edge and Core.
//
// THE ROW CARRIES NO COUNT. A count ticks wherever a cell draws straight from
// a watched node, and a ticking value inside the digest would send the whole
// snapshot on every heartbeat; the Edge is the count's source and Core the
// ledger. What a row derives from the count moves only at a crossing: Stocked
// (above zero) and Partial (above zero, below capacity). Its numbers do not
// tick: Bins (carriers at the node), Epoch (a generation), UOPCapacity (the
// part's), HeldUOP (a held partial's declared count, which nothing draws from
// while it waits) and the tote numbers; Inbound changes when an order starts
// or ends. No time that moves and nothing about who asked is in it either (a
// held partial's arrival is fixed), so two Edges watching the same nodes are
// sent the same rows.

// BinWatchList is the sorted, de-duplicated list of Core node names one Edge's
// operator stations draw. Digest is BinWatchDigest(Nodes); Core quotes it back
// on EdgeHeartbeatAck.BinWatch as it arrived and never recomputes it.
type BinWatchList struct {
	Digest string   `json:"digest"`
	Nodes  []string `json:"nodes"`
}

// NodeBinsSnapshot is the bins at one Edge's watched nodes. ListDigest is the
// Digest of the BinWatchList it answers, as that list arrived. Digest is
// NodeBinsDigest of ListDigest, Rows and Groups, computed over the value sent,
// so the heartbeat compare tells a snapshot built for an older list from one
// built for the current list.
type NodeBinsSnapshot struct {
	Digest     string          `json:"digest,omitempty"`
	ListDigest string          `json:"list_digest"`
	Rows       []NodeBinsRow   `json:"rows"`
	Groups     []NodeBinsGroup `json:"groups"`
}

// The values of NodeBinsRow.State.
const (
	// BinsStateUnknown: Core has no node by the name (never made, or renamed).
	BinsStateUnknown = "unknown"
	// BinsStateGroup: the node is a node group (type NGRP).
	BinsStateGroup = "group"
	// BinsStateEmpty: a node with no bin at it.
	BinsStateEmpty = "empty"
	// BinsStateOccupied: a node with a bin at it. Occupied is true exactly here.
	BinsStateOccupied = "occupied"
)

// NodeBinsRow is one place: a name on the watch list as the Edge sent it, or a
// member of a watched group. Rows come in the list's (sorted) order, each
// watched group's member rows directly after it. No name appears twice: a
// member that is also watched keeps its own row at its own place.
//
// Only an occupied row sets the resident bin (BinID through HeldUOP). A group
// row sets Bins and Inbound, and Members when the group is watched (a member
// that is itself a group is not expanded). An unknown row sets its name and
// State alone.
//
// TotesOnCart and TotesPerLayer are nil when the number is unknown; 0 is a
// real count (a base: a tote cart with none on it).
type NodeBinsRow struct {
	Node  string `json:"node"`
	State string `json:"state"`
	// Members is a watched group's members, each named "group.member", in the
	// order Core lists the group's non-synthetic children (by name).
	Members           []string `json:"members,omitempty"`
	Occupied          bool     `json:"occupied"`
	Bins              int      `json:"bins,omitempty"`
	BinID             int64    `json:"bin_id,omitempty"`
	BinLabel          string   `json:"bin_label,omitempty"`
	BinTypeCode       string   `json:"bin_type_code,omitempty"`
	Bare              bool     `json:"bare,omitempty"`
	PayloadCode       string   `json:"payload_code,omitempty"`
	Manifest          *string  `json:"manifest,omitempty"`
	ManifestConfirmed bool     `json:"manifest_confirmed"`
	// Epoch is the resident carrier's generation (bins.delta_epoch).
	Epoch int64 `json:"epoch,omitempty"`
	// Stocked is the bin's count above zero. A count at or below zero is not
	// stocked; a count that moves and stays above zero changes nothing here.
	Stocked bool `json:"stocked"`
	// Partial is the count above zero and below UOPCapacity; false with no
	// capacity.
	Partial       bool `json:"partial"`
	TotesOnCart   *int `json:"totes_on_cart,omitempty"`
	TotesPerLayer *int `json:"totes_per_layer,omitempty"`
	// UOPCapacity is the capacity of the carrier's part, 0 with no part.
	// HeldSince, HeldFor and HeldUOP are set for a HELD partial only, an
	// unfinished carrier waiting in a partial home of its part to be called
	// back: when it entered the home (RFC 3339), the line whose claim keeps it
	// there, and its declared count. None of them ticks: nothing draws from a
	// held partial.
	UOPCapacity int    `json:"uop_capacity,omitempty"`
	HeldSince   string `json:"held_since,omitempty"`
	HeldFor     string `json:"held_for,omitempty"`
	HeldUOP     int    `json:"held_uop,omitempty"`
	// Inbound is the live orders whose delivery resolves at this node, in
	// order id order.
	Inbound []NodeBinsInbound `json:"inbound,omitempty"`
}

// NodeBinsInbound is one live order on its way to a row's node, one the
// dropoff gate counts (it holds a claimed bin, or is a dig leg not yet sent).
// OrderUUID is the order's Edge UUID; From its source node as the order names
// it; Payload its payload code; BinType the bin type of the carrier it names,
// when it names one.
type NodeBinsInbound struct {
	OrderUUID string `json:"order_uuid"`
	BinType   string `json:"bin_type,omitempty"`
	Payload   string `json:"payload"`
	From      string `json:"from"`
}

// NodeBinsGroup is one outbound group, with levels, of a consume loader that
// has a watched window: what the group wants and what each watched cart is
// asked to go back as. A plant with no levels sends no groups.
type NodeBinsGroup struct {
	Group       string               `json:"group"`
	Wants       []NodeBinsWant       `json:"wants"`
	Assignments []NodeBinsAssignment `json:"assignments"`
}

// NodeBinsWant is how many of one thing a group wants: a bin type, and for a
// type with layer wants a height of it (Layers nil: the type, any height).
type NodeBinsWant struct {
	BinType string `json:"bin_type"`
	Layers  *int   `json:"layers,omitempty"`
	Want    int    `json:"want"`
}

// NodeBinsAssignment is what the cart at one watched window is asked to go
// back as.
type NodeBinsAssignment struct {
	Node    string `json:"node"`
	BinType string `json:"bin_type"`
	Layers  *int   `json:"layers,omitempty"`
}
