package www

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// handler_status_feedspin_test.go — the exact key set GET /status returns, with
// a publish recorded and no read errors (the *_error keys are omitempty and
// absent).
//
// The five Core-link keys (core_clock_offset_ms, seconds_since_last_ack,
// expired_drops, feed_confirmed_age_seconds, feed_flags) joined in
// "perf(edge): sourcing writes only what changed, status shows the Core link,
// one stream per page"; their values are core_link_status_test.go's.
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
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("/status keys:\n got  %v\n want %v", got, want)
	}
	// Four of the Core-link keys, by name.
	for _, k := range []string{"expired_drops", "core_clock_offset_ms", "seconds_since_last_ack", "feed_confirmed_age_seconds"} {
		if _, ok := body[k]; !ok {
			t.Errorf("/status lacks %q", k)
		}
	}
}
