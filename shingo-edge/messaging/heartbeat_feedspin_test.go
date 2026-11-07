package messaging

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// heartbeat_feedspin_test.go — what the heartbeater sends and how often: the
// heartbeat body byte for byte, the 60 s interval, the node-list request body,
// and the loop's re-ask shape. Written as predictions before the versioned
// feeds landed; each case now asserts the landed value.

// TestFeedsPin_HeartbeatBodyBytes pins the heartbeat payload byte for byte
// (key order follows the struct). feeds is null while FeedsFn is unwired (the
// wired map is TestHeartbeater_FeedsAndTheReAsk's), and there is no uptime_s
// or active_orders: "fix(core,edge): ack on a registry failure, no plant-wide
// list on a read error, reconcile after an ack gap, uptime and order count off
// the heartbeat" removed both (no Core reader; the count was a store read
// every minute).
func TestFeedsPin_HeartbeatBodyBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		wire func(h *Heartbeater)
		want string
	}{
		{
			name: "unwired",
			wire: func(h *Heartbeater) {},
			want: `{"station_id":"edge.test","feeds":null}`,
		},
		{
			// The name predates the order count's removal: only the zone and
			// the tick lag are wired.
			name: "orders, zone and tick lag wired",
			wire: func(h *Heartbeater) {
				h.TimezoneFn = func() string { return "America/Chicago" }
				h.TickLagFn = func() (int64, int64, bool) { return 2, 1500, true }
			},
			want: `{"station_id":"edge.test","timezone":"America/Chicago","tick_pending":2,"tick_oldest_unsent_age_ms":1500,"feeds":null}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewHeartbeater(nil, "edge.test", "v-test", "inst-1", "shingo.orders")
			tc.wire(h)
			got, err := json.Marshal(h.heartbeatBody())
			if err != nil {
				t.Fatalf("marshal heartbeat: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("heartbeat bytes:\n got  %s\n want %s", got, tc.want)
			}
		})
	}
}

// TestFeedsPin_HeartbeatCountsActiveOrdersEveryBeat pins the heartbeat
// interval (60 s) and that the body carries no active_orders key. The name is
// the per-beat order count it was written to pin (one orderCountFn call per
// body); the func and its NewHeartbeater parameter went with the key, in the
// change TestFeedsPin_HeartbeatBodyBytes names.
func TestFeedsPin_HeartbeatCountsActiveOrdersEveryBeat(t *testing.T) {
	t.Parallel()
	h := NewHeartbeater(nil, "edge.test", "v-test", "inst-1", "shingo.orders")
	cases := []struct {
		name string
		got  func() any
		want any
	}{
		{"heartbeat interval", func() any { return h.interval }, 60 * time.Second},
		{"active_orders on the heartbeat body", func() any {
			b, err := json.Marshal(h.heartbeatBody())
			if err != nil {
				t.Fatalf("marshal heartbeat body: %v", err)
			}
			return strings.Contains(string(b), "active_orders")
		}, false},
	}
	for _, tc := range cases {
		if got := tc.got(); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestFeedsPin_NodeAndCatalogRequestBodies pins the node-list request body the
// re-ask and the boot burst send: the held scene revision under
// scene_revision, and {} when none is held. Despite the name the catalog
// request is not pinned here, and never was (sendCatalogRequest sends an empty
// CatalogPayloadsRequest).
func TestFeedsPin_NodeAndCatalogRequestBodies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rev  func() string
		want string
	}{
		{"no revision source", nil, `{}`},
		{"revision held", func() string { return "rev-1" }, `{"scene_revision":"rev-1"}`},
		{"nothing held", func() string { return "" }, `{}`},
	}
	for _, tc := range cases {
		h := NewHeartbeater(nil, "edge.test", "v-test", "inst-1", "shingo.orders")
		h.SceneRevisionFn = tc.rev
		got, err := json.Marshal(h.nodeListRequest())
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s: node-list request = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestFeedsPin_HeartbeatLoopReasksEverySecondTick is a source-shape pin: the
// loop is a goroutine on a real ticker with no seam, so the branch is pinned
// as written. Every tick sends a heartbeat first. The every-second-tick
// (~2 min at the 60 s interval) re-ask of the node list and the catalog is not
// the unconditional form: since "perf(core,edge): node list, scene and catalog
// sent when the digest differs, not every two minutes" it runs only while
// !coreSpeaksFeeds (the predicate is TestHeartbeater_FeedsAndTheReAsk's). The
// guard is pinned only by the unconditional pattern's absence; the name is
// the unconditional re-ask the pin was written against.
func TestFeedsPin_HeartbeatLoopReasksEverySecondTick(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("heartbeat.go")
	if err != nil {
		t.Fatalf("read heartbeat.go: %v", err)
	}
	cases := []struct {
		name string
		re   *regexp.Regexp
		want bool
	}{
		{
			name: "unguarded tick%2 re-ask of node list and catalog",
			re:   regexp.MustCompile(`if tick%2 == 0 \{\s*h\.sendNodeListRequest\(\)\s*h\.sendCatalogRequest\(\)\s*\}`),
			want: false,
		},
		{
			name: "a heartbeat on every tick before the re-ask",
			re:   regexp.MustCompile(`case <-ticker\.C:\s*h\.sendHeartbeat\(\)\s*tick\+\+`),
			want: true,
		},
	}
	for _, tc := range cases {
		if got := tc.re.Match(src); got != tc.want {
			t.Errorf("%s: matched = %v, want %v", tc.name, got, tc.want)
		}
	}
}
