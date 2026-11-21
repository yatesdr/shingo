package protocol

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"
)

// peer_wire_test.go — the bodies the deployed peers encode, decoded both ways.
//
// The deployed peers are a v0.18.0 Core and a v0.17.2 Edge. Every message here
// keeps the shape it has at both tags (git diff v0.17.2 v0.18.0 changes none of
// these types), so one frozen copy of each type, below, stands for both peers,
// and testdata/peerwire holds the body both tags encode, every field set
// (testdata/peerwire/gen.go, run inside each tag's protocol tree, wrote them).
// They are the peers' bytes: never regenerate them from this tree.
//
// Each case names the keys it lets differ (deletable): the keys proposed for
// deletion because no deployed peer is thought to read them. The list is not
// proof that nothing reads them; the change that deletes one searches both tags
// for a reader. Every other key must survive both directions with the value
// the peer encoded:
//
//   - a peer's body decodes on today's type, and today's encoding of it
//     carries each of the golden's other keys with the golden's value;
//   - today's encoding decodes on the peer's frozen type, and the peer then
//     holds each other key as it held the golden's.
//
// Pattern: plant_claim_legs_test.go (oldCorePlantClaim). The frozen copies use
// the underlying kind (string, int64) where today's type is a named one, so a
// method added to a named type later cannot change what the copy decodes.

type peerWireCase struct {
	name      string   // the type; testdata/peerwire/<name>.json
	sender    string   // the deployed peer that encodes it
	deletable []string // the keys allowed to differ between the peer and today
	current   func() any
	peer      func() any
}

func peerWireCases() []peerWireCase {
	const core, edge = "the v0.18.0 Core", "the v0.17.2 Edge"
	return []peerWireCase{
		{"BinUOPDelta", edge, []string{"station"},
			func() any { return &BinUOPDelta{} }, func() any { return &peerBinUOPDelta{} }},
		{"EdgeRegistered", core, []string{"station_id"},
			func() any { return &EdgeRegistered{} }, func() any { return &peerEdgeRegistered{} }},
		{"EdgeHeartbeatAck", core, []string{"station_id"},
			func() any { return &EdgeHeartbeatAck{} }, func() any { return &peerEdgeHeartbeatAck{} }},
		{"EdgeStale", core, []string{"station_id"},
			func() any { return &EdgeStale{} }, func() any { return &peerEdgeStale{} }},
		{"EdgeRegisterRequest", core, []string{"station_id"},
			func() any { return &EdgeRegisterRequest{} }, func() any { return &peerEdgeRegisterRequest{} }},
		{"OrderProjection", core, []string{"station_id"},
			func() any { return &OrderProjection{} }, func() any { return &peerOrderProjection{} }},
		{"OrderStatusSnapshot", core, []string{"station_id", "source_node", "delivery_node", "vendor_order_id"},
			func() any { return &OrderStatusSnapshot{} }, func() any { return &peerOrderStatusSnapshot{} }},
		{"LinesideLevelReport", edge, []string{"station"},
			func() any { return &LinesideLevelReport{} }, func() any { return &peerLinesideLevelReport{} }},
		{"NodeStructureChanged", core, []string{"node_id", "old_parent_id", "new_parent_id"},
			func() any { return &NodeStructureChanged{} }, func() any { return &peerNodeStructureChanged{} }},
		{"SupplyRefusalSnapshot", core, []string{"digest"},
			func() any { return &SupplyRefusalSnapshot{} }, func() any { return &peerSupplyRefusalSnapshot{} }},
	}
}

// TestPeerWire_TheGoldensAreThePeersWholeShape checks the inputs the two
// direction tests rely on: each golden is exactly what its frozen type encodes
// (the copy is the tag's shape), every field of it is set (an omitempty field
// left zero would drop its key and go unchecked), and it carries every key the
// case lets differ (it is the shape before any deletion).
func TestPeerWire_TheGoldensAreThePeersWholeShape(t *testing.T) {
	t.Parallel()
	for _, c := range peerWireCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			golden := readPeerGolden(t, c.name)
			peer := c.peer()
			if err := json.Unmarshal(golden, peer); err != nil {
				t.Fatalf("the golden does not decode on the frozen copy: %v", err)
			}
			again, err := json.Marshal(peer)
			if err != nil {
				t.Fatalf("marshal the frozen copy: %v", err)
			}
			if !bytes.Equal(again, golden) {
				t.Fatalf("the frozen copy encodes\n%s\nbut %s encoded\n%s\nThe copy has drifted from the tag: copy the "+
					"type from `git show v0.17.2:protocol/` again; do not regenerate the golden from this tree", again, c.sender, golden)
			}
			if path, ok := firstZeroField(reflect.ValueOf(peer).Elem(), c.name); !ok {
				t.Errorf("%s is zero in the golden: set it in testdata/peerwire/gen.go and rerun it at both tags", path)
			}
			keys := peerKeys(t, golden)
			for _, k := range c.deletable {
				if _, ok := keys[k]; !ok {
					t.Errorf("the golden has no %q: it is not the peers' shape", k)
				}
			}
		})
	}
}

// TestPeerWire_APeersBodyDecodesOnTodaysType: what a deployed peer sends
// decodes on today's type, and today's encoding of it keeps every key the peer
// sent, with its value, except the ones the case lets differ.
func TestPeerWire_APeersBodyDecodesOnTodaysType(t *testing.T) {
	t.Parallel()
	for _, c := range peerWireCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			golden := readPeerGolden(t, c.name)
			cur := c.current()
			if err := json.Unmarshal(golden, cur); err != nil {
				t.Fatalf("a body from %s no longer decodes on today's %s: %v\n%s", c.sender, c.name, err, golden)
			}
			now, err := json.Marshal(cur)
			if err != nil {
				t.Fatalf("marshal today's %s: %v", c.name, err)
			}
			assertPeerKeysKept(t, c, golden, now, "today's "+c.name+" re-encoding "+c.sender+"'s body")
		})
	}
}

// TestPeerWire_TodaysBodyDecodesOnAPeersType: what this tree sends decodes on
// the deployed peer's type, and the peer then holds every key as it held its
// own body's, except the ones the case lets differ.
func TestPeerWire_TodaysBodyDecodesOnAPeersType(t *testing.T) {
	t.Parallel()
	for _, c := range peerWireCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			golden := readPeerGolden(t, c.name)
			cur := c.current()
			if err := json.Unmarshal(golden, cur); err != nil {
				t.Fatalf("decode the golden on today's %s: %v", c.name, err)
			}
			now, err := json.Marshal(cur)
			if err != nil {
				t.Fatalf("marshal today's %s: %v", c.name, err)
			}
			peer := c.peer()
			if err := json.Unmarshal(now, peer); err != nil {
				t.Fatalf("today's %s no longer decodes on the deployed peers' type: %v\n%s", c.name, err, now)
			}
			held, err := json.Marshal(peer)
			if err != nil {
				t.Fatalf("marshal the peer's copy: %v", err)
			}
			assertPeerKeysKept(t, c, golden, held, "a deployed peer decoding today's "+c.name)
		})
	}
}

// assertPeerKeysKept fails for every key of golden, outside the case's
// deletable ones, that got does not carry with the same encoded value. It
// counts what it compared, so a golden with nothing to compare is a failure.
func assertPeerKeysKept(t *testing.T, c peerWireCase, golden, got []byte, who string) {
	t.Helper()
	want, have := peerKeys(t, golden), peerKeys(t, got)
	compared := 0
	for _, k := range sortedKeys(want) {
		if slices.Contains(c.deletable, k) {
			continue
		}
		compared++
		v, ok := have[k]
		if !ok {
			t.Errorf("%s: %q is gone (%s sent %s); only %v may go", who, k, c.sender, want[k], c.deletable)
			continue
		}
		if !bytes.Equal(v, want[k]) {
			t.Errorf("%s: %q = %s, want %s as %s sent it", who, k, v, want[k], c.sender)
		}
	}
	if compared == 0 || compared != len(want)-len(c.deletable) {
		t.Errorf("%s: compared %d keys of %d (%d deletable): the golden is not the case's shape", who, compared, len(want), len(c.deletable))
	}
}

func readPeerGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "peerwire", name+".json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return bytes.TrimSuffix(b, []byte("\n"))
}

func peerKeys(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal into a key map: %v\n%s", err, b)
	}
	return m
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// firstZeroField walks v (struct fields, pointer targets, slice elements) and
// returns the path of the first zero value, or ok when there is none.
func firstZeroField(v reflect.Value, path string) (string, bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return path, false
		}
		return firstZeroField(v.Elem(), path)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return path, !v.IsZero()
		}
		for i := 0; i < v.NumField(); i++ {
			if p, ok := firstZeroField(v.Field(i), path+"."+v.Type().Field(i).Name); !ok {
				return p, false
			}
		}
		return path, true
	case reflect.Slice, reflect.Map:
		if v.Len() == 0 {
			return path, false
		}
		if v.Kind() == reflect.Slice {
			for i := 0; i < v.Len(); i++ {
				if p, ok := firstZeroField(v.Index(i), path+"[]"); !ok {
					return p, false
				}
			}
		}
		return path, true
	default:
		return path, !v.IsZero()
	}
}

// ── The deployed peers' types, as v0.17.2 and v0.18.0 declare them ────────

type peerBinUOPDelta struct {
	Station     string    `json:"station"`
	BinID       int64     `json:"bin_id"`
	PayloadCode string    `json:"payload_code"`
	Delta       int       `json:"delta"`
	Reason      string    `json:"reason"`
	SequenceID  int64     `json:"sequence_id"`
	Epoch       int64     `json:"epoch"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	Net         *int64    `json:"net,omitempty"`
}

type peerEdgeRegistered struct {
	StationID      string `json:"station_id"`
	Message        string `json:"message,omitempty"`
	EphemeralTopic string `json:"ephemeral_topic,omitempty"`
}

type peerEdgeHeartbeatAck struct {
	StationID      string            `json:"station_id"`
	ServerTS       time.Time         `json:"server_ts"`
	Timezone       string            `json:"timezone,omitempty"`
	Feeds          map[string]string `json:"feeds"`
	Claims         map[string]string `json:"claims"`
	BinWatch       *string           `json:"bin_watch,omitempty"`
	Asks           []string          `json:"asks,omitempty"`
	EphemeralTopic string            `json:"ephemeral_topic,omitempty"`
}

type peerEdgeStale struct {
	StationID string `json:"station_id"`
	Message   string `json:"message"`
}

type peerEdgeRegisterRequest struct {
	StationID string `json:"station_id"`
	Reason    string `json:"reason"`
}

type peerOrderProjection struct {
	OrderUUID     string `json:"order_uuid"`
	OrderType     string `json:"order_type"`
	Status        string `json:"status"`
	StationID     string `json:"station_id"`
	Quantity      int64  `json:"quantity,omitempty"`
	SourceNode    string `json:"source_node,omitempty"`
	DeliveryNode  string `json:"delivery_node,omitempty"`
	PayloadCode   string `json:"payload_code,omitempty"`
	PayloadDesc   string `json:"payload_desc,omitempty"`
	RetrieveEmpty bool   `json:"retrieve_empty,omitempty"`
	OriginID      string `json:"origin_id,omitempty"`
	OriginClass   string `json:"origin_class,omitempty"`
	QueueReason   string `json:"queue_reason,omitempty"`
	QueueCode     string `json:"queue_code,omitempty"`
	Version       int64  `json:"version,omitempty"`
}

type peerTermRef struct {
	Node       string `json:"node,omitempty"`
	Payload    string `json:"payload,omitempty"`
	Peer       int64  `json:"peer,omitempty"`
	VendorCode int    `json:"vendor_code,omitempty"`
	VendorDesc string `json:"vendor_desc,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

type peerOrderStatusSnapshot struct {
	OrderUUID         string       `json:"order_uuid"`
	Found             bool         `json:"found"`
	Status            string       `json:"status,omitempty"`
	StationID         string       `json:"station_id,omitempty"`
	SourceNode        string       `json:"source_node,omitempty"`
	DeliveryNode      string       `json:"delivery_node,omitempty"`
	VendorOrderID     string       `json:"vendor_order_id,omitempty"`
	ErrorDetail       string       `json:"error_detail,omitempty"`
	QueueReason       string       `json:"queue_reason,omitempty"`
	QueueCode         string       `json:"queue_code,omitempty"`
	FaultSince        *time.Time   `json:"fault_since,omitempty"`
	FaultDeadline     *time.Time   `json:"fault_deadline,omitempty"`
	FaultNotice       bool         `json:"fault_notice,omitempty"`
	FaultNoticeAfterS int          `json:"fault_notice_after_s,omitempty"`
	FaultRef          *peerTermRef `json:"fault_ref,omitempty"`
	Version           int64        `json:"version,omitempty"`
}

type peerLinesideLevelEntry struct {
	CoreNodeName string `json:"core_node_name"`
	PayloadCode  string `json:"payload_code"`
	BinCount     int    `json:"bin_count"`
	BinUOP       int    `json:"bin_uop"`
	BucketQty    int    `json:"bucket_qty"`
	BinID        *int64 `json:"bin_id,omitempty"`
	BinEpoch     int64  `json:"bin_epoch,omitempty"`
	FlushedSeq   int64  `json:"flushed_seq,omitempty"`
}

type peerLinesideLevelReport struct {
	Station    string                   `json:"station"`
	ReportedAt time.Time                `json:"reported_at"`
	Entries    []peerLinesideLevelEntry `json:"entries"`
}

type peerNodeStructureChanged struct {
	NodeID      int64  `json:"node_id"`
	NodeName    string `json:"node_name"`
	OldParentID *int64 `json:"old_parent_id,omitempty"`
	NewParentID *int64 `json:"new_parent_id,omitempty"`
	Action      string `json:"action"`
}

type peerSupplyRefusalState struct {
	Action       string     `json:"action"`
	LoaderNode   string     `json:"loader_node"`
	PayloadCode  string     `json:"payload_code"`
	RefusedAt    time.Time  `json:"refused_at,omitempty"`
	RefusedBy    string     `json:"refused_by,omitempty"`
	AckAt        *time.Time `json:"ack_at,omitempty"`
	AckChoice    string     `json:"ack_choice,omitempty"`
	AckProcessID string     `json:"ack_process_id,omitempty"`
}

type peerSupplyRefusalSnapshot struct {
	Digest string                   `json:"digest"`
	Open   []peerSupplyRefusalState `json:"open"`
}
