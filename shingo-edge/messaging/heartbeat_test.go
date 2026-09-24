package messaging

import "testing"

// TestHeartbeater_NodeListRequestQuotesTheCachedSceneRevision pins the Edge
// half of the geometry short-circuit: every node-list request names the scene
// revision the Edge already holds, so Core can leave the geometry off. An Edge
// with nothing cached — or one built before the field existed — sends the
// empty string, which Core reads as "send everything".
func TestHeartbeater_NodeListRequestQuotesTheCachedSceneRevision(t *testing.T) {
	t.Parallel()
	h := NewHeartbeater(nil, "edge.test", "v-test", "inst-1", "shingo.orders", nil)

	if got := h.nodeListRequest().SceneRevision; got != "" {
		t.Errorf("no revision source wired, request carries %q — must be empty so Core sends the full scene", got)
	}

	h.SceneRevisionFn = func() string { return "rev-abc" }
	if got := h.nodeListRequest().SceneRevision; got != "rev-abc" {
		t.Errorf("request carries %q, want the cached revision rev-abc", got)
	}

	// The revision is read at SEND time, not captured at construction: a
	// cache replaced between two ticks must show up on the very next request.
	h.SceneRevisionFn = func() string { return "rev-def" }
	if got := h.nodeListRequest().SceneRevision; got != "rev-def" {
		t.Errorf("request carries %q after the cache moved to rev-def", got)
	}
}

// TestHeartbeater_CarriesTickLag: the periodic heartbeat carries the
// production-tick shipper's lag, read at send time, so Core can flag a station
// whose feed is stuck without a message of its own. Unwired, or a failed read,
// leaves the fields off the wire rather than reporting a zero nobody measured.
func TestHeartbeater_CarriesTickLag(t *testing.T) {
	t.Parallel()
	h := NewHeartbeater(nil, "edge.test", "v-test", "inst-1", "shingo.orders", nil)
	if b := h.heartbeatBody(); b.TickPending != nil || b.TickOldestUnsentAgeMS != nil {
		t.Errorf("unwired: lag fields = %v/%v, want absent", b.TickPending, b.TickOldestUnsentAgeMS)
	}
	h.TickLagFn = func() (int64, int64, bool) { return 3, 4200, true }
	b := h.heartbeatBody()
	if b.TickPending == nil || *b.TickPending != 3 || b.TickOldestUnsentAgeMS == nil || *b.TickOldestUnsentAgeMS != 4200 {
		t.Errorf("lag fields = %v/%v, want 3/4200", b.TickPending, b.TickOldestUnsentAgeMS)
	}
	h.TickLagFn = func() (int64, int64, bool) { return 0, 0, false }
	if b := h.heartbeatBody(); b.TickPending != nil {
		t.Errorf("failed read: lag fields present")
	}
}
