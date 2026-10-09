package www

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingo/shared/planttime"
	"shingoedge/engine"
)

// core_link_status.go — the Core link as /status and Diagnostics show it: the
// last heartbeat ack, the clock offset it implies, envelopes dropped for
// expiry, and how current each Core feed's copy is. Read through the
// statusEngine assertion, so the page interface does not widen.

// coreLinkFeeds are the Core feeds reported, in display order.
var coreLinkFeeds = []string{
	protocol.FeedContainment, protocol.FeedRefusals, protocol.FeedNodes,
	protocol.FeedScene, protocol.FeedCatalog,
}

// coreLinkStatus is the Core-link part of GET /status. The pointer fields are
// null until the first ack, so "never heard from Core" is not read as "0 s".
type coreLinkStatus struct {
	// CoreClockOffsetMS is Core's ServerTS on the last ack minus this Edge's
	// clock when the ack arrived. Approximate: Core stamps the ack before it
	// goes through Core's outbox and the broker, so the delivery delay is in
	// it and reads as Core being behind by that much.
	CoreClockOffsetMS   *int64 `json:"core_clock_offset_ms"`
	SecondsSinceLastAck *int64 `json:"seconds_since_last_ack"`
	// ExpiredDrops is envelopes this process dropped for expiry since boot
	// (protocol.ExpiredDrops). A climbing count with a large offset is a clock
	// skew eating messages.
	ExpiredDrops int64 `json:"expired_drops"`
	// FeedConfirmedAgeSeconds is, per Core feed, seconds since Core last
	// confirmed this Edge's copy; null where it never has.
	FeedConfirmedAgeSeconds map[string]*int64 `json:"feed_confirmed_age_seconds"`
	// FeedFlags are the keys flagged as not converging. Never null.
	FeedFlags []string `json:"feed_flags"`
}

func readCoreLink(eng statusEngine, now time.Time) coreLinkStatus {
	out := coreLinkStatus{
		ExpiredDrops:            protocol.ExpiredDrops(),
		FeedConfirmedAgeSeconds: make(map[string]*int64, len(coreLinkFeeds)),
		FeedFlags:               eng.FeedFlags(),
	}
	if out.FeedFlags == nil {
		out.FeedFlags = []string{}
	}
	if local, server := eng.LastCoreAck(); !local.IsZero() {
		since := int64(now.Sub(local) / time.Second)
		out.SecondsSinceLastAck = &since
		if !server.IsZero() {
			offset := server.Sub(local.Round(0)).Milliseconds()
			out.CoreClockOffsetMS = &offset
		}
	}
	for _, key := range coreLinkFeeds {
		var age *int64
		if _, confirmed := eng.FeedTimes(key); !confirmed.IsZero() {
			s := int64(now.Sub(confirmed) / time.Second)
			age = &s
		}
		out.FeedConfirmedAgeSeconds[key] = age
	}
	return out
}

// coreLinkFeedLine is one feed's line on Diagnostics.
type coreLinkFeedLine struct{ Key, Note string }

// coreLinkView is the Core link as Diagnostics renders it.
type coreLinkView struct {
	Summary string
	Feeds   []coreLinkFeedLine
	Flags   []string
}

// coreLinkSummary is the one Diagnostics line: offset, seconds since the
// last ack, expired drops.
func coreLinkSummary(s coreLinkStatus) string {
	drops := fmt.Sprintf("expired drops %d", s.ExpiredDrops)
	if s.SecondsSinceLastAck == nil {
		return "No heartbeat ack from Core yet | " + drops
	}
	offset := "unknown"
	if s.CoreClockOffsetMS != nil {
		offset = fmt.Sprintf("%+.1f s", float64(*s.CoreClockOffsetMS)/1000)
	}
	return fmt.Sprintf("Core clock offset about %s (includes Core's outbox delay) | last ack %d s ago | %s",
		offset, *s.SecondsSinceLastAck, drops)
}

// coreLinkFeedNote says how current one feed's copy is. Nothing is hidden for
// being old: an old confirmation shows its time, plant-local.
func coreLinkFeedNote(eng statusEngine, key string, now time.Time) string {
	received, confirmed := eng.FeedTimes(key)
	at := confirmed
	if at.IsZero() {
		at = received
	}
	switch {
	case at.IsZero():
		return "No data from Core yet"
	case now.Sub(at) > engine.FeedAsOfAfter:
		return "as of " + planttime.Clock(at, plantLocation)
	default:
		return "current"
	}
}

// diagnosticsCoreLink builds the Diagnostics view, or nil when the engine
// does not answer /status.
func (h *Handlers) diagnosticsCoreLink() *coreLinkView {
	eng, ok := h.orchestration.(statusEngine)
	if !ok {
		return nil
	}
	now := time.Now()
	s := readCoreLink(eng, now)
	v := &coreLinkView{Summary: coreLinkSummary(s), Flags: s.FeedFlags}
	for _, key := range coreLinkFeeds {
		v.Feeds = append(v.Feeds, coreLinkFeedLine{Key: key, Note: coreLinkFeedNote(eng, key, now)})
	}
	return v
}
