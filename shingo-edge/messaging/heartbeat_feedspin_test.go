package messaging

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
	"time"
)

// heartbeat_feedspin_test.go — what the heartbeater sends and how often,
// pinned before the versioned feeds change it. Each case asserts today's value;
// the after/label fields say what the named change is predicted to move it to.

// TestFeedsPin_HeartbeatBodyBytes pins the heartbeat payload byte for byte.
// startTime is set to now so uptime_s reads 0; the field is whole seconds.
func TestFeedsPin_HeartbeatBodyBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		wire  func(h *Heartbeater)
		want  string
		after string // predicted once Uptime/Orders go and Feeds joins (key order follows struct order)
		label string
	}{
		{
			name:  "unwired",
			wire:  func(h *Heartbeater) {},
			want:  `{"station_id":"edge.test","uptime_s":0,"active_orders":0}`,
			after: `{"station_id":"edge.test","feeds":null}`,
			label: "X5, F1",
		},
		{
			name: "orders, zone and tick lag wired",
			wire: func(h *Heartbeater) {
				h.orderCountFn = func() int { return 3 }
				h.TimezoneFn = func() string { return "America/Chicago" }
				h.TickLagFn = func() (int64, int64, bool) { return 2, 1500, true }
			},
			want:  `{"station_id":"edge.test","uptime_s":0,"active_orders":3,"timezone":"America/Chicago","tick_pending":2,"tick_oldest_unsent_age_ms":1500}`,
			after: `{"station_id":"edge.test","timezone":"America/Chicago","tick_pending":2,"tick_oldest_unsent_age_ms":1500,"feeds":{"catalog":"…","containment":"…","nodes":"…","refusals":"…","scene":"…"}}`,
			label: "X5, F1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewHeartbeater(nil, "edge.test", "v-test", "inst-1", "shingo.orders", nil)
			tc.wire(h)
			h.startTime = time.Now()
			got, err := json.Marshal(h.heartbeatBody())
			if err != nil {
				t.Fatalf("marshal heartbeat: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("heartbeat bytes:\n got  %s\n want %s\n(after %s: %s)", got, tc.want, tc.label, tc.after)
			}
		})
	}
}

// TestFeedsPin_HeartbeatCountsActiveOrdersEveryBeat pins the per-beat order
// count: one orderCountFn call per heartbeat body, on a 60 s interval. The
// count is a store read on the Edge's single connection, once a minute.
func TestFeedsPin_HeartbeatCountsActiveOrdersEveryBeat(t *testing.T) {
	t.Parallel()
	calls := 0
	h := NewHeartbeater(nil, "edge.test", "v-test", "inst-1", "shingo.orders", func() int {
		calls++
		return 0
	})
	cases := []struct {
		name  string
		got   func() any
		want  any
		after any
		label string
	}{
		{"heartbeat interval", func() any { return h.interval }, 60 * time.Second, 60 * time.Second, "same"},
		{"order counts per heartbeat body", func() any {
			calls = 0
			h.heartbeatBody()
			return calls
		}, 1, 0, "X5 (the func and the NewHeartbeater parameter are removed)"},
	}
	for _, tc := range cases {
		if got := tc.got(); got != tc.want {
			t.Errorf("%s = %v, want %v (after %s: %v)", tc.name, got, tc.want, tc.label, tc.after)
		}
	}
}

// TestFeedsPin_NodeAndCatalogRequestBodies pins the two request bodies the
// re-ask sends: the node-list request quotes the held scene revision, the
// catalog request is empty.
func TestFeedsPin_NodeAndCatalogRequestBodies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		rev   func() string
		want  string
		after string
		label string
	}{
		{"no revision source", nil, `{}`, `{}`, "same"},
		{"revision held", func() string { return "rev-1" }, `{"scene_revision":"rev-1"}`, `{"scene_revision":"rev-1"}`, "same"},
		{"nothing held", func() string { return "" }, `{}`, `{}`, "same"},
	}
	for _, tc := range cases {
		h := NewHeartbeater(nil, "edge.test", "v-test", "inst-1", "shingo.orders", nil)
		h.SceneRevisionFn = tc.rev
		got, err := json.Marshal(h.nodeListRequest())
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s: node-list request = %s, want %s (after %s: %s)", tc.name, got, tc.want, tc.label, tc.after)
		}
	}
}

// TestFeedsPin_HeartbeatLoopReasksEverySecondTick is a source-shape pin: the
// loop is a goroutine on a real ticker with no seam, so the branch is pinned
// as written. Every second tick (~2 min at the 60 s interval) re-asks for the
// node list and the catalog, unconditionally.
//
// after (F4): the same two calls run only while !coreSpeaksFeeds; the regexp
// below stops matching and is replaced by one that requires the guard.
func TestFeedsPin_HeartbeatLoopReasksEverySecondTick(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("heartbeat.go")
	if err != nil {
		t.Fatalf("read heartbeat.go: %v", err)
	}
	cases := []struct {
		name  string
		re    *regexp.Regexp
		want  bool
		after bool
		label string
	}{
		{
			name:  "unguarded tick%2 re-ask of node list and catalog",
			re:    regexp.MustCompile(`if tick%2 == 0 \{\s*h\.sendNodeListRequest\(\)\s*h\.sendCatalogRequest\(\)\s*\}`),
			want:  true,
			after: false,
			label: "F4",
		},
		{
			name:  "a heartbeat on every tick before the re-ask",
			re:    regexp.MustCompile(`case <-ticker\.C:\s*h\.sendHeartbeat\(\)\s*tick\+\+`),
			want:  true,
			after: true,
			label: "same",
		},
	}
	for _, tc := range cases {
		if got := tc.re.Match(src); got != tc.want {
			t.Errorf("%s: matched = %v, want %v (after %s: %v)", tc.name, got, tc.want, tc.label, tc.after)
		}
	}
}
