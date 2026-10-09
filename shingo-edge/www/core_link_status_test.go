package www

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/engine"
)

// The real engine answers the /status assertion; without this a missing
// method would turn /status into a 503 on a plant with every test green.
var _ statusEngine = (*engine.Engine)(nil)

// stubCoreLink is what stubEngine's Core-link reads answer.
type stubCoreLink struct {
	ackLocal, ackServer time.Time
	received, confirmed map[string]time.Time
	flags               []string
}

func (s *stubEngine) LastCoreAck() (time.Time, time.Time) {
	return s.statusCoreLink.ackLocal, s.statusCoreLink.ackServer
}

func (s *stubEngine) FeedTimes(key string) (time.Time, time.Time) {
	return s.statusCoreLink.received[key], s.statusCoreLink.confirmed[key]
}

func (s *stubEngine) FeedFlags() []string { return s.statusCoreLink.flags }

// TestStatus_CoreLink_BeforeAck: before Core has answered, the keys are
// there and say "never" (null), not zero.
func TestStatus_CoreLink_BeforeAck(t *testing.T) {
	h, r := newTestHandlers(t)
	body := getStatus(t, h, r)
	for _, k := range []string{"core_clock_offset_ms", "seconds_since_last_ack"} {
		v, ok := body[k]
		if !ok || v != nil {
			t.Errorf("%s = %v (present %v), want null before the first ack", k, v, ok)
		}
	}
	if _, ok := body["expired_drops"].(float64); !ok {
		t.Errorf("expired_drops = %v, want a number", body["expired_drops"])
	}
	feeds, ok := body["feed_confirmed_age_seconds"].(map[string]any)
	if !ok {
		t.Fatalf("feed_confirmed_age_seconds = %v, want an object", body["feed_confirmed_age_seconds"])
	}
	for _, key := range coreLinkFeeds {
		if v, ok := feeds[key]; !ok || v != nil {
			t.Errorf("feed %s age = %v (present %v), want null when never confirmed", key, v, ok)
		}
	}
	if flags, ok := body["feed_flags"].([]any); !ok || len(flags) != 0 {
		t.Errorf("feed_flags = %v, want []", body["feed_flags"])
	}
}

// TestStatus_CoreLink_AfterAck: an ack 10 s ago stamped 3 s ahead of this
// Edge's clock, containment confirmed 20 s ago, one flag.
func TestStatus_CoreLink_AfterAck(t *testing.T) {
	h, r := newTestHandlers(t)
	eng := h.orchestration.(*stubEngine)
	now := time.Now()
	ackAt := now.Add(-10 * time.Second)
	eng.statusCoreLink = stubCoreLink{
		ackLocal:  ackAt,
		ackServer: ackAt.Add(3 * time.Second).UTC(),
		confirmed: map[string]time.Time{protocol.FeedContainment: now.Add(-20 * time.Second)},
		flags:     []string{"claims not converging: sent 3 times"},
	}
	body := getStatus(t, h, r)
	if off, _ := body["core_clock_offset_ms"].(float64); off != 3000 {
		t.Errorf("core_clock_offset_ms = %v, want 3000", body["core_clock_offset_ms"])
	}
	if since, _ := body["seconds_since_last_ack"].(float64); since < 10 || since > 12 {
		t.Errorf("seconds_since_last_ack = %v, want about 10", body["seconds_since_last_ack"])
	}
	feeds, _ := body["feed_confirmed_age_seconds"].(map[string]any)
	if age, _ := feeds[protocol.FeedContainment].(float64); age < 20 || age > 22 {
		t.Errorf("containment age = %v, want about 20", feeds[protocol.FeedContainment])
	}
	if v, ok := feeds[protocol.FeedCatalog]; !ok || v != nil {
		t.Errorf("catalog age = %v (present %v), want null (never confirmed)", v, ok)
	}
	if flags, _ := body["feed_flags"].([]any); len(flags) != 1 {
		t.Errorf("feed_flags = %v, want the one flag", body["feed_flags"])
	}
}

// TestCoreLink_OffsetFollowsTheEdgeClock: the offset /status reports is Core's
// stamp on the last ack minus this Edge's stamp on it, read with the Edge's own
// clock — so an Edge clock 2 or 6 minutes ahead reads -120000 / -360000 ms, one
// behind reads positive, and "seconds since the last ack" is on the Edge's
// clock throughout. The Edge's clock is injected: each case is an Edge whose
// clock runs `shift` from Core's (the engine stamps ackLocal from it, see
// engine TestOnCoreAck_StampsTheEdgeClock).
func TestCoreLink_OffsetFollowsTheEdgeClock(t *testing.T) {
	h, _ := newTestHandlers(t)
	eng := h.orchestration.(*stubEngine)
	coreAt := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		shift    time.Duration
		wantMS   int64
		wantNote string
	}{
		{0, 0, "+0.0 s"},
		{2 * time.Minute, -120000, "-120.0 s"},
		{6 * time.Minute, -360000, "-360.0 s"},
		{-2 * time.Minute, 120000, "+120.0 s"},
	} {
		edgeClock := func() time.Time { return coreAt.Add(c.shift) }
		eng.statusCoreLink = stubCoreLink{ackLocal: edgeClock(), ackServer: coreAt}
		got := readCoreLink(eng, edgeClock().Add(5*time.Second))
		if got.CoreClockOffsetMS == nil || *got.CoreClockOffsetMS != c.wantMS {
			t.Errorf("Edge clock %v from Core's: offset %v ms, want %d", c.shift, got.CoreClockOffsetMS, c.wantMS)
		}
		if got.SecondsSinceLastAck == nil || *got.SecondsSinceLastAck != 5 {
			t.Errorf("Edge clock %v from Core's: since last ack %v, want 5", c.shift, got.SecondsSinceLastAck)
		}
		if s := coreLinkSummary(got); !strings.Contains(s, c.wantNote) {
			t.Errorf("Edge clock %v from Core's: Diagnostics line %q, want it to carry %q", c.shift, s, c.wantNote)
		}
	}
}

// TestDiagnostics_CoreLinkNotes: each feed reads "No data from Core yet",
// "current", or "as of HH:MM" in the plant zone once its confirmation is older
// than FeedAsOfAfter. Nothing is hidden for being old.
func TestDiagnostics_CoreLinkNotes(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	orig := plantLocation
	plantLocation = chicago
	defer func() { plantLocation = orig }()

	h, _ := newTestHandlers(t)
	eng := h.orchestration.(*stubEngine)
	now := time.Now()
	old := time.Date(2026, 10, 9, 14, 5, 0, 0, time.UTC) // 09:05 CDT
	eng.statusCoreLink = stubCoreLink{
		ackLocal:  now.Add(-5 * time.Second),
		ackServer: now.Add(-5 * time.Second).Add(-1500 * time.Millisecond),
		received:  map[string]time.Time{protocol.FeedCatalog: old},
		confirmed: map[string]time.Time{protocol.FeedContainment: now.Add(-30 * time.Second), protocol.FeedNodes: old},
	}
	cases := map[string]string{
		protocol.FeedContainment: "current",
		protocol.FeedNodes:       "as of 09:05",
		protocol.FeedCatalog:     "as of 09:05", // received, never confirmed
		protocol.FeedScene:       "No data from Core yet",
		protocol.FeedRefusals:    "No data from Core yet",
	}
	for key, want := range cases {
		if got := coreLinkFeedNote(eng, key, now); got != want {
			t.Errorf("%s note = %q, want %q", key, got, want)
		}
	}

	view := h.diagnosticsCoreLink()
	if view == nil {
		t.Fatal("diagnosticsCoreLink = nil with an engine that answers /status")
	}
	for _, part := range []string{"Core clock offset about -1.5 s", "last ack 5 s ago", "expired drops "} {
		if !strings.Contains(view.Summary, part) {
			t.Errorf("summary %q missing %q", view.Summary, part)
		}
	}

	tmpl := template.Must(template.New("").Funcs(templateFuncs()).
		ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "diagnostics.html", map[string]any{"Page": "logs", "CoreLink": view}); err != nil {
		t.Fatalf("render diagnostics.html: %v", err)
	}
	for _, needle := range []string{"Core clock offset about -1.5 s", "nodes: as of 09:05", "scene: No data from Core yet"} {
		if !strings.Contains(buf.String(), needle) {
			t.Errorf("rendered Diagnostics missing %q", needle)
		}
	}
}

// TestCoreLinkSummary_BeforeAck: no ack yet says so, and still shows drops.
func TestCoreLinkSummary_BeforeAck(t *testing.T) {
	got := coreLinkSummary(coreLinkStatus{ExpiredDrops: 4})
	if got != "No heartbeat ack from Core yet | expired drops 4" {
		t.Errorf("summary = %q", got)
	}
}
