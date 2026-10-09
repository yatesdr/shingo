package messaging

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
)

// feeds.go — Core's half of the feed digests (protocol/feeds.go has the rule).
//
// No goroutine and no registry. Everything happens inside HandleEdgeHeartbeat:
// for each key the Edge named, Core takes its current digest (from a short memo),
// answers it on the ack, and sends the feed to that station when the two differ.
// A plain switch on the key builds and sends each feed.

const (
	// feedMemoPlant bounds how stale a plant-wide digest may be. A writer that
	// calls no hook is caught by the next heartbeat after the memo expires, so
	// this plus the heartbeat interval (60 s) is the worst-case heal (~70 s).
	feedMemoPlant = 10 * time.Second
	// feedMemoStation bounds the per-station feeds (nodes, scene). Two minutes is
	// the cadence the node list was re-asked at before feeds, and node edits
	// still push through NodeStructureChanged, so it costs no freshness.
	feedMemoStation = 2 * time.Minute
	// feedResendGuard skips a send when the same digest of the same key went to
	// the same station this recently: the copy is in flight, and the next
	// heartbeat may simply predate its arrival. It also bounds a key that never
	// converges at one send per two minutes, which was the old re-ask's cost.
	feedResendGuard = 2 * time.Minute
	// feedFlagAfter is how many sends in a row, with no heartbeat in between
	// quoting the last one back, mark a key as not converging.
	feedFlagAfter = 3
)

// feedMemoEntry is one built feed: the value and its digest, kept together so
// what is sent is exactly what was digested.
type feedMemoEntry struct {
	digest string
	value  any
	at     time.Time
}

// feedSendRecord is the last send of one key to one station.
type feedSendRecord struct {
	digest  string
	at      time.Time
	streak  int
	flagged bool
}

// feedState is the memo, the send records and the flags. One mutex: every
// access is a map lookup or a small write, and heartbeats arrive one per
// station per minute.
type feedState struct {
	mu    sync.Mutex
	memo  map[string]feedMemoEntry
	sent  map[[2]string]*feedSendRecord
	flags map[string]map[string]string
}

func newFeedState() *feedState {
	return &feedState{
		memo:  map[string]feedMemoEntry{},
		sent:  map[[2]string]*feedSendRecord{},
		flags: map[string]map[string]string{},
	}
}

// memoKey scopes the per-station feeds to their station.
func memoKey(station, key string) string {
	switch key {
	case protocol.FeedNodes, protocol.FeedScene:
		return key + "@" + station
	}
	return key
}

func memoTTL(key string) time.Duration {
	switch key {
	case protocol.FeedNodes, protocol.FeedScene:
		return feedMemoStation
	}
	return feedMemoPlant
}

// drop forgets a plant-wide feed's memo, so the next read rebuilds it. Called
// after a known write to that feed's data.
func (f *feedState) drop(key string) {
	f.mu.Lock()
	delete(f.memo, key)
	f.mu.Unlock()
}

func (f *feedState) lookup(station, key string, now time.Time) (feedMemoEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.memo[memoKey(station, key)]
	if !ok || now.Sub(e.at) >= memoTTL(key) {
		return feedMemoEntry{}, false
	}
	return e, true
}

func (f *feedState) store(station, key string, e feedMemoEntry) {
	f.mu.Lock()
	f.memo[memoKey(station, key)] = e
	f.mu.Unlock()
}

// compare records one heartbeat's answer for one key and reports whether the
// feed should be sent: the digests differ and the guard does not hold it back.
// A matching digest ends any streak and clears the key's flag.
//
// SO DOES THE EDGE HOLDING WHAT WAS LAST SENT. Converging means the Edge took
// the copy it was sent, not that Core's value stood still: a value that moves
// on three heartbeats in a row (a destination added, a bin in, the bin out) is
// sent three times to an Edge that applied every one, and counting those as a
// streak flagged a healthy station "not converging".
func (f *feedState) compare(station, key, current, held string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	rk := [2]string{station, key}
	rec := f.sent[rk]
	if current == held {
		if rec != nil {
			rec.streak = 0
			if rec.flagged {
				rec.flagged = false
				f.clearFlagLocked(station, key)
			}
		}
		return false
	}
	if rec != nil && held == rec.digest {
		rec.streak = 0
		if rec.flagged {
			rec.flagged = false
			f.clearFlagLocked(station, key)
		}
	}
	if rec != nil && rec.digest == current && now.Sub(rec.at) < feedResendGuard {
		return false
	}
	if rec == nil {
		rec = &feedSendRecord{}
		f.sent[rk] = rec
	}
	rec.digest, rec.at = current, now
	rec.streak++
	if rec.streak >= feedFlagAfter && !rec.flagged {
		rec.flagged = true
		log.Printf("core_feeds: %s sent to %s %d times in a row without converging (digest %s, edge holds %q)",
			key, station, rec.streak, current, held)
		f.setFlagLocked(station, key, fmt.Sprintf("%s not converging: sent %d times", key, rec.streak))
	}
	return true
}

// setFlag records a plain-text flag against a station, shown on its row of the
// Edges page. id names the flag so the condition that raised it can clear it.
func (f *feedState) setFlag(station, id, text string) {
	f.mu.Lock()
	f.setFlagLocked(station, id, text)
	f.mu.Unlock()
}

func (f *feedState) setFlagLocked(station, id, text string) {
	if f.flags[station] == nil {
		f.flags[station] = map[string]string{}
	}
	f.flags[station][id] = text
}

func (f *feedState) clearFlagLocked(station, id string) {
	delete(f.flags[station], id)
	if len(f.flags[station]) == 0 {
		delete(f.flags, station)
	}
}

func (f *feedState) flagsFor(station string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.flags[station]))
	for _, text := range f.flags[station] {
		out = append(out, text)
	}
	sort.Strings(out)
	return out
}

// EdgeFeedFlags lists the plain-text feed conditions flagged for one station:
// a key sent three times in a row without converging, a process reported by
// two stations. Read by the Edges page; empty when nothing is wrong.
func (s *CoreDataService) EdgeFeedFlags(station string) []string {
	return s.feeds.flagsFor(station)
}

// answerFeeds answers a heartbeat's Feeds map: Core's current digest for every
// key it could read, and a send of each feed whose digest differs. A key Core
// could not read is left out of the answer and nothing is sent for it.
func (s *CoreDataService) answerFeeds(station string, held map[string]string) map[string]string {
	now := clock.Now()
	answer := make(map[string]string, len(held))
	due := map[string]feedMemoEntry{}
	for key, h := range held {
		e, ok := s.feedCurrent(station, key, now)
		if !ok {
			continue
		}
		answer[key] = e.digest
		if s.feeds.compare(station, key, e.digest, h, now) {
			due[key] = e
		}
	}
	if len(due) > 0 {
		s.sendFeeds(station, due, held)
	}
	return answer
}

// feedCurrent returns the memoised build of one feed, building it on a miss.
// ok is false when the key is unknown to this Core or its read failed; a failed
// read is never memoised, so the next heartbeat tries again.
func (s *CoreDataService) feedCurrent(station, key string, now time.Time) (feedMemoEntry, bool) {
	if e, ok := s.feeds.lookup(station, key, now); ok {
		return e, true
	}
	digest, value, known, err := s.buildFeed(station, key)
	if !known {
		return feedMemoEntry{}, false
	}
	if err != nil {
		log.Printf("core_feeds: build %s for %s: %v — left out of the ack", key, station, err)
		return feedMemoEntry{}, false
	}
	e := feedMemoEntry{digest: digest, value: value, at: now}
	s.feeds.store(station, key, e)
	return e, true
}

// buildFeed builds one feed's value and digests it. known is false for a key
// this Core does not serve. On the feed path any read error fails the build:
// a digest of a partial read would confirm a copy nobody read.
func (s *CoreDataService) buildFeed(station, key string) (digest string, value any, known bool, err error) {
	switch key {
	}
	return "", nil, false, nil
}

// sendFeeds sends each due feed to one station, the value exactly as digested.
// held is the heartbeat's map, for feeds whose send depends on what the Edge
// already has.
func (s *CoreDataService) sendFeeds(station string, due map[string]feedMemoEntry, held map[string]string) {
	keys := make([]string, 0, len(due))
	for key := range due {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch key {
		default:
			log.Printf("core_feeds: no sender for %s", key)
		}
	}
}
