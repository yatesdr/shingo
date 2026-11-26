//go:build docker

package messaging

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
)

// Mixed versions on the heartbeat, Core side. An older Edge (no Feeds) gets
// the ack it always got plus an empty feeds map and nil claims, and nothing is
// sent to it. A newer Edge naming a key this Core does not serve gets that key
// left out of the answer, nothing sent, and its plant-claims digests — {} when
// Core holds none.
func TestHandleEdgeHeartbeat_FeedsByEdgeVersion(t *testing.T) {
	t.Parallel()
	const st = "edge.test"
	cases := []struct {
		name  string
		feeds map[string]string
	}{
		{"older Edge, no feeds", nil},
		{"newer Edge, a key this Core does not serve", map[string]string{"no-such-feed": "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			_, err := db.EnrollEdge(st, "")
			testutil.MustNoErr(t, err, "enroll")
			resp := &feedsPinResponder{}
			svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
			svc.HandleEdgeHeartbeat(feedsPinEnv(st), &protocol.EdgeHeartbeat{Feeds: tc.feeds})

			if got := resp.trace(); len(got) != 1 || got[0] != "reply "+protocol.SubjectEdgeHeartbeatAck {
				t.Fatalf("trace = %v, want the ack alone", got)
			}
			ack := resp.events[0].payload.(*protocol.EdgeHeartbeatAck)
			if ack.Feeds == nil || len(ack.Feeds) != 0 {
				t.Errorf("ack feeds = %#v, want an empty, non-nil map", ack.Feeds)
			}
		})
	}
}
