package protocol

// bins_feed.go — the node-bins feed's wire types: the watch list an Edge
// publishes (SubjectBinsWatch) and the snapshot Core answers it with
// (SubjectBinsSnapshot, feed key FeedBins). The digests are in feeds.go with
// every other feed's.
//
// THE ROW CARRIES NO COUNT. A count ticks wherever a cell draws straight from
// a watched node, and a ticking value inside the digest would send the whole
// snapshot on every heartbeat. Stocked (a count above zero) is the one fact
// derived from it. No time and nothing about who asked is in it either, so two
// Edges watching the same nodes are sent the same rows.

// BinWatchList is the sorted, de-duplicated list of Core node names one Edge's
// operator stations draw. Digest is BinWatchDigest(Nodes); Core quotes it back
// on EdgeHeartbeatAck.BinWatch as it arrived and never recomputes it.
type BinWatchList struct {
	Digest string   `json:"digest"`
	Nodes  []string `json:"nodes"`
}

// NodeBinsSnapshot is the bins at one Edge's watched nodes. Digest is
// NodeBinsDigest of Rows and Groups, computed over the value sent.
type NodeBinsSnapshot struct {
	Digest string          `json:"digest,omitempty"`
	Rows   []NodeBinsRow   `json:"rows"`
	Groups []NodeBinsGroup `json:"groups"`
}

// NodeBinsRow is the bin at one watched node, one row per name on the list,
// the name as the Edge sent it. A node Core does not know, and a node with no
// bin, is a row with Occupied false and nothing else set.
//
// TotesOnCart and TotesPerLayer are nil when the number is unknown; 0 is a
// real count (a base: a tote cart with none on it).
type NodeBinsRow struct {
	Node              string  `json:"node"`
	Occupied          bool    `json:"occupied"`
	BinID             int64   `json:"bin_id,omitempty"`
	BinLabel          string  `json:"bin_label,omitempty"`
	BinTypeCode       string  `json:"bin_type_code,omitempty"`
	Bare              bool    `json:"bare,omitempty"`
	PayloadCode       string  `json:"payload_code,omitempty"`
	Manifest          *string `json:"manifest,omitempty"`
	ManifestConfirmed bool    `json:"manifest_confirmed"`
	// Stocked is the bin's count above zero. A count at or below zero is not
	// stocked; a count that moves and stays above zero changes nothing here.
	Stocked       bool `json:"stocked"`
	TotesOnCart   *int `json:"totes_on_cart,omitempty"`
	TotesPerLayer *int `json:"totes_per_layer,omitempty"`
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
