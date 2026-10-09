package engine

import (
	"encoding/json"
	"fmt"

	"shingo/protocol"
	"shingo/protocol/clock"
)

// containment_feed.go — the Edge's copy of Core's containment feed.
//
// Core sends the whole containment state (flags, held bins, what stands at each
// containment destination) on containment.snapshot: broadcast after a known
// containment write, and to this Edge alone when its heartbeat quotes a digest
// Core no longer has. The Edge holds the last snapshot in memory for readers and
// its JSON in feed_copy, so the containment page renders with no call to Core
// and still renders, "as of" its time, after a restart with Core unreachable.

// ApplyContainmentSnapshot takes a snapshot Core sent. The copy readers see is
// replaced, then the digest and body are held — only after the apply, so a
// snapshot that could not be applied leaves the old digest and Core sends
// again. A new digest fires EventContainmentUpdated; an identical re-send only
// moves the received time.
func (e *Engine) ApplyContainmentSnapshot(snap protocol.ContainmentSnapshot) error {
	if snap.Digest == "" {
		return fmt.Errorf("containment snapshot carries no digest")
	}
	body, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("encode containment snapshot: %w", err)
	}
	changed := e.heldDigest(protocol.FeedContainment) != snap.Digest || e.containment.Load() == nil
	e.containment.Store(&snap)
	e.holdFeed(protocol.FeedContainment, snap.Digest, string(body))
	if changed {
		e.Events.Emit(Event{
			Type:      EventContainmentUpdated,
			Timestamp: clock.Now(),
			Payload:   ContainmentUpdatedEvent{Digest: snap.Digest},
		})
	}
	return nil
}

// loadContainmentCopy decodes the containment body loadFeedCopies read from
// feed_copy into the readers' copy. A body that does not decode is dropped
// with its digest, so the first heartbeat quotes nothing held and Core sends
// the feed again; keeping the digest would tell Core this Edge holds a copy
// it cannot show.
func (e *Engine) loadContainmentCopy() {
	e.feeds.mu.Lock()
	h := e.feeds.held[protocol.FeedContainment]
	var body string
	if h != nil {
		body = h.body
	}
	e.feeds.mu.Unlock()
	if body == "" {
		return
	}
	var snap protocol.ContainmentSnapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil || snap.Digest == "" {
		e.logFn("feeds: held containment copy does not decode (%v) — dropped; Core resends it", err)
		e.noteHeld(protocol.FeedContainment, "")
		return
	}
	e.containment.Store(&snap)
}

// LocalContainment is the containment state as this Edge holds it: Core's last
// snapshot with the time it arrived and the time Core last confirmed it. false
// when nothing is held yet. Read-only, and nothing is withheld for age: the
// screens show the time beside the value instead.
func (e *Engine) LocalContainment() (*ContainmentState, bool) {
	snap := e.containment.Load()
	if snap == nil {
		return nil, false
	}
	received, confirmed := e.FeedTimes(protocol.FeedContainment)
	return &ContainmentState{
		Containment:  snap.Flags,
		HeldBins:     snap.HeldBins,
		Destinations: snap.Destinations,
		ReceivedAt:   received,
		ConfirmedAt:  confirmed,
	}, true
}
