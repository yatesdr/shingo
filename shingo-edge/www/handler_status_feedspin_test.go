package www

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// handler_status_feedspin_test.go — the key set GET /status returns today, with
// a publish recorded and no read errors (the *_error keys are omitempty and
// absent).
//
// after (X7): the same keys plus the approximate Core clock offset from the
// last ack's ServerTS, seconds since the last ack, the expired-drop count
// (protocol.ExpiredDrops), each feed's confirmedAt age, and the flagged feed
// keys. The exact key names are X7's to choose; the pin flips to the new list
// in X7's commit.
func TestStatus_Keys_FeedsPin(t *testing.T) {
	h, r := newTestHandlers(t)
	eng := h.orchestration.(*stubEngine)
	eng.statusLastPublishEver = true
	eng.statusLastPublishOK = true
	eng.statusLastPublishAt = time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)

	body := getStatus(t, h, r)
	got := make([]string, 0, len(body))
	for k := range body {
		got = append(got, k)
	}
	sort.Strings(got)

	// X7: the five Core-link keys joined the base list.
	want := []string{
		"core_clock_offset_ms",
		"dead_letters",
		"expired_drops",
		"feed_confirmed_age_seconds",
		"feed_flags",
		"kafka_connected",
		"kafka_last_publish_at",
		"kafka_last_publish_ok",
		"outbox_depth",
		"process_start_time",
		"production_ticks_oldest_unsent_age_ms",
		"production_ticks_pending",
		"seconds_since_last_ack",
		"station_id",
		"subscribers_wired",
		"uptime_seconds",
	}
	// after: want + {core clock offset, seconds since last ack, expired drops,
	// per-feed confirmedAt age, flagged keys}   (label X7)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("/status keys:\n got  %v\n want %v\n(after: plus clock offset, seconds since last ack, expired drops, "+
			"per-feed confirmedAt age, flagged keys — X7)", got, want)
	}
	// X7: the per-feed ages are under "feed_confirmed_age_seconds" (the pin
	// guessed "feeds"); the other three names held.
	for _, k := range []string{"expired_drops", "core_clock_offset_ms", "seconds_since_last_ack", "feed_confirmed_age_seconds"} {
		if _, ok := body[k]; !ok {
			t.Errorf("/status lacks %q (base: absent; X7: present)", k)
		}
	}
}
