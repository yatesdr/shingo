package www

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shingoedge/engine"
)

// The containment feed as the stub engine serves it: the held copy. Whether
// Core speaks feeds is the station catalog stamp's question
// (catalogFeedClock), not the page's.
func (s *stubEngine) LocalContainment() (*engine.ContainmentState, bool) {
	return s.containment, s.containment != nil
}
func (s *stubEngine) CoreSpeaksFeeds() bool { return s.speaksFeeds }

// The page under each state of the feed: nothing held, a held copy Core has
// confirmed recently, one it has not confirmed for longer than FeedAsOfAfter,
// and one never confirmed since it arrived. Nothing blanks for age: the old
// copy renders whole, with the time beside it.
func TestContainmentPage_FeedStates(t *testing.T) {
	t.Parallel()
	old := time.Now().Add(-engine.FeedAsOfAfter - time.Minute)
	oldText := old.In(plantLocation()).Format("15:04")
	cases := []struct {
		name     string
		held     func() *engine.ContainmentState
		contains []string
		absent   []string
	}{
		{"nothing held", nil,
			[]string{containmentNoData, "FP-GRP"}, []string{"as of ", "FP-BIN-9"}},
		{"held, confirmed now", func() *engine.ContainmentState { return containmentPinHeld(t) },
			[]string{"FP-BIN-9", "FP-BIN-11"}, []string{"as of ", containmentNoData}},
		{"held, last confirmed long ago", func() *engine.ContainmentState {
			st := containmentPinHeld(t)
			st.ReceivedAt, st.ConfirmedAt = old.Add(-time.Hour), old
			return st
		},
			[]string{"as of " + oldText, "FP-BIN-9", "FP-BIN-11", `data-payload="FP-PART"`}, []string{containmentNoData}},
		{"held, never confirmed since it arrived (the copy still renders)", func() *engine.ContainmentState {
			st := containmentPinHeld(t)
			st.ReceivedAt, st.ConfirmedAt = old, time.Time{}
			return st
		},
			[]string{"as of " + oldText, "FP-BIN-9"}, []string{containmentNoData}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, core := newContainmentPinHandlers(t)
			stub := h.engine.(*containmentPinEngine).stubEngine
			stub.containment = nil
			if tc.held != nil {
				stub.containment = tc.held()
			}

			rec := httptest.NewRecorder()
			h.handleContainmentPage(rec, httptest.NewRequest(http.MethodGet, "/containment", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /containment = %d (%s)", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			for _, s := range tc.contains {
				if !strings.Contains(body, s) {
					t.Errorf("page lacks %q", s)
				}
			}
			for _, s := range tc.absent {
				if strings.Contains(body, s) {
					t.Errorf("page shows %q", s)
				}
			}
			if n := len(core.snapshot()); n != 0 {
				t.Errorf("page called Core %d kinds of read, want none", n)
			}
		})
	}
}

// The JSON twin carries the same notice and "as of", and only when set, so a
// current copy serves the keys it always did.
func TestContainmentState_NoticeAndAsOfKeys(t *testing.T) {
	t.Parallel()
	h, _ := newContainmentPinHandlers(t)
	stub := h.engine.(*containmentPinEngine).stubEngine

	_, _, raw := getContainmentPinState(t, h)
	if strings.Contains(raw, `"as_of"`) || strings.Contains(raw, `"notice"`) {
		t.Errorf("current copy: body carries as_of or notice: %s", raw)
	}

	stub.containment.ReceivedAt = time.Now().Add(-time.Hour)
	stub.containment.ConfirmedAt = stub.containment.ReceivedAt
	_, _, raw = getContainmentPinState(t, h)
	if !strings.Contains(raw, `"as_of"`) {
		t.Errorf("old copy: body has no as_of: %s", raw)
	}

	stub.containment = nil
	_, st, _ := getContainmentPinState(t, h)
	if st.Notice != containmentNoData {
		t.Errorf("nothing held: notice = %q, want %q", st.Notice, containmentNoData)
	}
}

// A copy loaded with neither time still says it is old rather than showing a
// clock time it does not have.
func TestContainmentAsOf_ZeroTimes(t *testing.T) {
	t.Parallel()
	if got := containmentAsOf(&engine.ContainmentState{}, time.Now()); got != containmentAsOfZero {
		t.Errorf("as of with no times = %q, want %q", got, containmentAsOfZero)
	}
	now := time.Now()
	if got := containmentAsOf(&engine.ContainmentState{ReceivedAt: now}, now); got != "" {
		t.Errorf("as of for a copy received just now = %q, want none", got)
	}
}

// The feed's rows render their times as Core's JSON wrote them and a missing
// time as "", the shape the page served when it decoded Core's body into
// string fields.
func TestContainmentRows_TimeText(t *testing.T) {
	t.Parallel()
	st := containmentPinHeld(t)
	flags := containmentFlagRows(st.Containment)
	if flags[0].ActivatedAt != "2026-10-01T08:00:00Z" || flags[0].DeactivatedAt != "" {
		t.Errorf("flag times = %q / %q, want 2026-10-01T08:00:00Z / \"\"", flags[0].ActivatedAt, flags[0].DeactivatedAt)
	}
	held := containmentHeldRows(st.HeldBins)
	if held[0].HoldAt != "2026-10-01T08:05:00Z" {
		t.Errorf("hold time = %q, want 2026-10-01T08:05:00Z", held[0].HoldAt)
	}
}

// The engine's containment event reaches the page as the SSE `containment`
// event, on the durable queue: a dropped frame would leave the page showing the
// old state with nothing to correct it.
func TestSSE_ContainmentEventIsDurable(t *testing.T) {
	t.Parallel()
	if lossySSETopics["containment"] {
		t.Error("containment is classified lossy")
	}
	hub := NewEventHub()
	eng := &engine.Engine{Events: engine.NewEventBus()}
	hub.SetupEngineListeners(eng)
	eng.Events.Emit(engine.Event{Type: engine.EventContainmentUpdated,
		Payload: engine.ContainmentUpdatedEvent{Digest: "d1"}})
	select {
	case evt := <-hub.broadcast:
		p, ok := evt.Data.(engine.ContainmentUpdatedEvent)
		if evt.Type != "containment" || !ok || p.Digest != "d1" {
			t.Errorf("broadcast %q %+v, want containment with digest d1", evt.Type, evt.Data)
		}
	default:
		t.Error("no SSE event for EventContainmentUpdated")
	}
}
