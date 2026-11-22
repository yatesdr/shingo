package messaging

import (
	"sort"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
)

// feed_doors.go — the change doors' one body: what Core does after a known
// write to what the node list, the scene or the catalog is built from
// (service.FeedDoors has the two doors and their callers).
//
// AT ONCE, THE MEMOS GO. The next heartbeat from any Edge compares against a
// build made after the write, whatever else happens. Without the drop a
// heartbeat inside the memo's life compared against the build from before the
// write: an Edge whose digest differed from that build was sent the catalog
// from before the write, and its sync deleted any row the write had added
// there (a floor part its answer had landed). A heartbeat's build that began
// before the write and finishes after the drop is not stored (feedState.store).
//
// A MOMENT LATER, ONE PUSH PER STATION. Each station Core holds a send record
// for (a station Core has sent a feed to, by the heartbeat compare or a push)
// gets one time.AfterFunc, feedPushDelay later, so the writes of one save are
// one rebuild and at most one send per feed. The push builds each feed the
// write changed that the station names on its heartbeats, and sends those
// whose digest differs from the last one sent to that station, or from the one
// it quoted since (it took a reply's copy, or converged with no send): what
// its next heartbeat's compare would send, sooner. It records each send as a
// push, as pushBins does: the compare's resend guard is left alone, so a lost
// push is healed by the next heartbeat.
// A station with no send record is not pushed; its next heartbeat carries the
// change. The memo's lifetime is the backstop for a write that reaches no
// door (feedMemoStation, feedMemoPlant).

// feedPushDelay coalesces the door calls one station sees in a burst (a node
// save writes the node, its stations and its kanban) into one rebuild and at
// most one send per feed. The bins push's delay.
const feedPushDelay = binsPushDelay

// doorFeedsPlantWide are the feeds the plant-wide door forgets and pushes:
// every feed built from what the doors' writers write. Containment, refusals,
// bins and sourcing have pushes of their own.
var doorFeedsPlantWide = []string{protocol.FeedNodes, protocol.FeedScene, protocol.FeedCatalog}

// doorFeedsAtStations are the per-station door's: the node list, which is
// what a station-scoped write changes.
var doorFeedsAtStations = []string{protocol.FeedNodes}

// feedPush is one station's armed push: the feeds the doors named since it was
// armed.
type feedPush struct {
	keys map[string]bool
}

// FeedsChanged is the change doors' body (service.FeedDoors, wired at the
// composition root through Engine.SetFeedsChangedFunc). It is called after a
// write commits: with no stations for a write that may change what any
// station is sent, with the stations whose node lists it changed otherwise.
// It forgets the builds the write made stale at once, then arms a push for
// each of those stations Core holds a send record for. Pinned by
// TestFeedsChanged_PushesWhatTheWriteChanged.
func (s *CoreDataService) FeedsChanged(stations []string) {
	keys := doorFeedsPlantWide
	if len(stations) == 0 {
		for _, key := range keys {
			s.feeds.drop(key)
		}
		stations = s.feeds.recordedStations(nil)
	} else {
		keys = doorFeedsAtStations
		for _, st := range stations {
			for _, key := range keys {
				s.feeds.dropStation(st, key)
			}
		}
		stations = s.feeds.recordedStations(stations)
	}
	s.feeds.armPushes(stations, keys, s.pushFeeds)
}

// pushFeeds builds those of one station's door feeds it names and sends each
// whose digest differs from the one the station is last known to hold
// (lastKnown: the last send, or a newer quote), once per carrier (sendFeeds).
// It records each send as a push. A failed build sends nothing; the heartbeat
// retries.
func (s *CoreDataService) pushFeeds(station string, keys []string) {
	defer s.feeds.pushDone()
	now := clock.Now()
	current := s.feedReaderAt(station, now)
	due := map[string]feedMemoEntry{}
	held := map[string]string{}
	for _, key := range keys {
		known, speaks := s.feeds.lastKnown(station, key)
		held[key] = known
		if !speaks {
			continue
		}
		if e, ok := current(key); ok && e.digest != known {
			due[key] = e
		}
	}
	if len(due) == 0 {
		return
	}
	// The node list carries the scene: the revision the station is known to
	// hold decides whether the geometry rides it, as the heartbeat's held
	// revision does there.
	if _, ok := held[protocol.FeedScene]; !ok {
		held[protocol.FeedScene], _ = s.feeds.lastKnown(station, protocol.FeedScene)
	}
	s.sendFeeds(station, due, current, held)
	for key, e := range due {
		s.feeds.recordPush(station, key, e.digest, now)
	}
}

// lastKnown is the digest of one key a station is last known to hold or to
// have on its way: the last one sent to it, by the compare or a push, unless
// a heartbeat has quoted a digest since, which it then holds. A heartbeat that
// quotes a digest in the same instant as a send is the one that send answered,
// so the send wins. "" when neither is known. speaks is whether the station
// has named the key on a heartbeat or been sent it: a push sends only what
// the heartbeat compare would, so a key the Edge never names is not pushed.
func (f *feedState) lastKnown(station, key string) (digest string, speaks bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rk := [2]string{station, key}
	var at time.Time
	rec := f.sent[rk]
	if rec != nil {
		digest, at = rec.Last()
	}
	q, quoted := f.quoted[rk]
	if quoted && q.at.After(at) {
		digest = q.digest
	}
	return digest, quoted || rec != nil
}

// recordedStations is the stations Core holds a send record for, sorted: all
// of them, or those of want.
func (f *feedState) recordedStations(want []string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keep := map[string]bool{}
	for rk := range f.sent {
		keep[rk[0]] = true
	}
	var out []string
	if want == nil {
		for st := range keep {
			out = append(out, st)
		}
	} else {
		for _, st := range want {
			if keep[st] {
				out = append(out, st)
				keep[st] = false
			}
		}
	}
	sort.Strings(out)
	return out
}

// armPushes adds keys to each station's armed push, arming one (pushDelay
// later, on its own goroutine) for a station with none. push runs on the
// timer's goroutine with the keys named since the push was armed.
func (f *feedState) armPushes(stations, keys []string, push func(station string, keys []string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, st := range stations {
		p := f.pending[st]
		if p == nil {
			p = &feedPush{keys: map[string]bool{}}
			f.pending[st] = p
			station := st
			time.AfterFunc(f.pushDelay, func() { push(station, f.pushStarted(station)) })
		}
		for _, key := range keys {
			p.keys[key] = true
		}
	}
}

// pushStarted takes a station's armed push as it begins, so a door from here
// on arms another, counts it as running, and returns its keys, sorted.
func (f *feedState) pushStarted(station string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pending[station]
	delete(f.pending, station)
	f.running++
	var keys []string
	if p != nil {
		for key := range p.keys {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// pushDone ends a push pushStarted began, whether or not it sent.
func (f *feedState) pushDone() {
	f.mu.Lock()
	f.running--
	f.mu.Unlock()
}

// pushesSettled reports whether no push is armed or running.
func (f *feedState) pushesSettled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending) == 0 && f.running == 0
}
