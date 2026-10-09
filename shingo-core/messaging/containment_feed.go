package messaging

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingo/protocol/clock"
)

// containment_feed.go — the FeedContainment builder and the push after a known
// containment write (protocol/feeds.go has the rule).
//
// THREE STATEMENTS PER BUILD, whatever the number of destinations: the flags,
// the held bins, and one read of every containment destination with its
// children and the bins standing there. The Edge's containment page used to
// make one HTTP read for the state, one per group destination for its children
// and one per destination for its bins, on every render and every 5 s poll.

// buildContainmentSnapshot reads the whole containment state and digests it.
// Any read error fails the build: a digest of a partial read would confirm a
// copy nobody read. Empty lists are sent as [] so the Edge's copy and Core's
// GET /api/containment read alike.
func (s *CoreDataService) buildContainmentSnapshot() (*protocol.ContainmentSnapshot, error) {
	flags, err := s.db.ListPayloadContainment()
	if err != nil {
		return nil, fmt.Errorf("containment flags: %w", err)
	}
	held, err := s.db.ListHeldBins()
	if err != nil {
		return nil, fmt.Errorf("held bins: %w", err)
	}
	dests, err := s.db.ListContainmentDestinations()
	if err != nil {
		return nil, fmt.Errorf("containment destinations: %w", err)
	}
	snap := &protocol.ContainmentSnapshot{Flags: flags, HeldBins: held, Destinations: dests}
	if snap.Flags == nil {
		snap.Flags = []protocol.PayloadContainmentRow{}
	}
	if snap.HeldBins == nil {
		snap.HeldBins = []protocol.HeldBinRow{}
	}
	digest, err := protocol.ContainmentDigest(*snap)
	if err != nil {
		return nil, fmt.Errorf("containment digest: %w", err)
	}
	snap.Digest = digest
	return snap, nil
}

// ContainmentChanged is called after a known write to containment state (a
// flag set or cleared, a bin held or unheld, a diverted bin stamped on
// arrival). It drops the memo, rebuilds once and broadcasts the snapshot to
// every Edge, so a flag set on Core reaches the Edge screens in seconds rather
// than at the next heartbeat. The rebuilt value is the memo's, so the next
// heartbeats compare against exactly what was broadcast.
//
// Writers with no hook (a bin moved into a destination by an ordinary order)
// are caught by the heartbeat once the memo expires. An Edge older than this
// subject logs "no handler" once per broadcast and is otherwise unaffected.
func (s *CoreDataService) ContainmentChanged() {
	s.feeds.drop(protocol.FeedContainment)
	e, ok := s.feedCurrent("", protocol.FeedContainment, clock.Now())
	if !ok {
		log.Printf("core_feeds: containment changed but the snapshot did not build — nothing broadcast; the next heartbeats retry")
		return
	}
	s.resp.sendData(protocol.SubjectContainmentSnapshot, protocol.StationBroadcast, e.value)
}
