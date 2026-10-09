package engine

import (
	"reflect"
	"testing"

	"shingo/protocol"
)

// The ack hands Claims to the publisher only when Core sent a map: nil is an
// older Core, or one that could not read its table, and nothing is read or
// sent for it. {} is a newer Core holding none, which still runs (a styled
// process this Edge has is then missing and is re-sent).
func TestOnCoreAck_ClaimsReachThePublisherOnlyWhenSent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		ack    *protocol.EdgeHeartbeatAck
		called bool
	}{
		{"older Core: no feeds, no claims", &protocol.EdgeHeartbeatAck{}, false},
		{"newer Core, claims unread", &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}}, false},
		{"newer Core holding none", &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Claims: map[string]string{}}, true},
		{"newer Core holding one", &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Claims: map[string]string{"P1": "d1"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := testEngine(t, testEngineDB(t))
			var got map[string]string
			called := false
			e.SetClaimsAckFunc(func(claims map[string]string) { called, got = true, claims })
			e.OnCoreAck(tc.ack)
			if called != tc.called {
				t.Fatalf("publisher called = %v, want %v", called, tc.called)
			}
			if called && !reflect.DeepEqual(got, tc.ack.Claims) {
				t.Errorf("claims handed over = %v, want %v", got, tc.ack.Claims)
			}
		})
	}
}

// The claims keys share the feeds' resend guard: the same digest inside the
// guard is held back, a new digest goes, the third send in a row flags the
// process on /status, and settling it (Core quoted it back) clears the flag
// while a still-pending process keeps its own.
func TestClaimSendDue_GuardAndFlag(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	steps := []struct {
		process, digest string
		want            bool
	}{
		{"P1", "d1", true},
		{"P1", "d1", false}, // in flight
		{"P1", "d2", true},
		{"P1", "d3", true}, // third send in a row
		{"P2", "e1", true},
		{"P2", "e2", true},
		{"P2", "e3", true},
	}
	for i, s := range steps {
		if got := e.ClaimSendDue(s.process, s.digest, ""); got != s.want {
			t.Fatalf("step %d: ClaimSendDue(%s, %s) = %v, want %v", i, s.process, s.digest, got, s.want)
		}
	}
	want := []string{"claims:P1 not converging: sent 3 times", "claims:P2 not converging: sent 3 times"}
	if got := e.FeedFlags(); !reflect.DeepEqual(got, want) {
		t.Fatalf("flags = %v, want %v", got, want)
	}
	e.ClaimsSettled(map[string]bool{"P2": true})
	if got := e.FeedFlags(); !reflect.DeepEqual(got, want[1:]) {
		t.Errorf("flags after P1 settled = %v, want %v", got, want[1:])
	}
}
