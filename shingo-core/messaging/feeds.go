package messaging

import (
	"fmt"
	"log"
	"sort"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/lockcheck"
	"shingo/protocol/resend"
	"shingocore/internal/lockorder"
)

// feeds.go — Core's half of the feed digests (protocol/feeds.go has the rule).
//
// The compare runs no goroutine of its own: it happens inside
// HandleEdgeHeartbeat. For each key the Edge named, Core takes its current
// digest (from a short memo), answers it on the ack, and sends the feed to that
// station when the two differ. A plain switch on the key builds and sends each
// feed.
//
// One feed keeps state per Edge beyond the memo: bins, whose scope is a watch
// list each Edge publishes. bins_feed.go holds those lists in memory and, after
// a known change, pushes on one timer per dirty Edge; the compare here treats
// bins like any per-station feed.

const (
	// feedMemoPlant bounds how stale a plant-wide digest may be. A writer that
	// calls no hook is caught by the next heartbeat after the memo expires, so
	// this plus the heartbeat interval (60 s) is the worst-case heal (~70 s).
	feedMemoPlant = 10 * time.Second
	// feedMemoBins bounds one Edge's bins snapshot. Short, as feedMemoPlant is:
	// a bin moved by a writer that calls no hook is in a snapshot ten seconds
	// later, and the Edge's next heartbeat carries it there.
	feedMemoBins = 10 * time.Second
	// feedMemoStation bounds the per-station feeds (nodes, scene). Two minutes is
	// the cadence the node list was re-asked at before feeds, and node edits
	// still push through NodeStructureChanged, so it costs no freshness.
	feedMemoStation = 2 * time.Minute
)

// feedMemoEntry is one built feed: the value and its digest, kept together so
// what is sent is exactly what was digested.
type feedMemoEntry struct {
	digest string
	value  any
	at     time.Time
}

// feedSendRecord is the last send of one key to one station. The Record is
// the heartbeat compare's send under the shared resend guard (protocol/resend;
// two minutes, which was the old re-ask's cost for a key that never converges);
// pushed and pushedAt are the last send made outside the compare (a bins push),
// kept apart so a lost push does not hold back the heartbeat send that heals
// it.
type feedSendRecord struct {
	resend.Record
	flagged  bool
	pushed   string
	pushedAt time.Time
}

// feedState is the memo, the send records and the flags. One mutex: every
// access is a map lookup or a small write, and heartbeats arrive one per
// station per minute.
type feedState struct {
	mu    lockcheck.Mutex[lockorder.MessagingFeeds]
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

// feedScope is how one feed's build is memoised: for each station apart or
// once for the plant, and for how long, or (noMemo) not at all.
type feedScope struct {
	perStation bool
	ttl        time.Duration
	// noMemo: every heartbeat builds the feed afresh. For a build that reads
	// nothing, where a memo could only answer with a digest older than a push.
	noMemo bool
}

// feedScopes has one row per feed Core serves and NO DEFAULT: a key without a
// row is neither memoised nor served (feedCurrent leaves it out of the ack, as
// a key this Core does not know). The default it replaces memoised any key
// plant-wide, so a per-station feed left out would have answered station B
// with station A's build. TestFeedScopes_CoverAllFeeds pins a row per key.
var feedScopes = map[string]feedScope{
	protocol.FeedContainment: {ttl: feedMemoPlant},
	protocol.FeedRefusals:    {ttl: feedMemoPlant},
	protocol.FeedNodes:       {perStation: true, ttl: feedMemoStation},
	protocol.FeedScene:       {perStation: true, ttl: feedMemoStation},
	protocol.FeedCatalog:     {ttl: feedMemoPlant},
	protocol.FeedBins:        {perStation: true, ttl: feedMemoBins},
	// Built from the monitor's in-memory verdicts with no read, so a memo
	// would save nothing and could answer a delta the monitor just pushed with
	// the digest from before it.
	protocol.FeedSourcing: {perStation: true, noMemo: true},
}

// memoKey scopes the per-station feeds to their station.
func memoKey(station, key string) string {
	if feedScopes[key].perStation {
		return key + "@" + station
	}
	return key
}

// memoTTL is how long a feed's build is memoised: zero, never, for a key
// with no row.
func memoTTL(key string) time.Duration { return feedScopes[key].ttl }

// drop forgets a plant-wide feed's memo, so the next read rebuilds it. Called
// after a known write to that feed's data.
func (f *feedState) drop(key string) {
	f.mu.Lock()
	delete(f.memo, key)
	f.mu.Unlock()
}

// dropStation forgets one station's memo of a per-station feed.
func (f *feedState) dropStation(station, key string) {
	f.mu.Lock()
	delete(f.memo, memoKey(station, key))
	f.mu.Unlock()
}

// recordPush records a send made outside the heartbeat compare. It leaves the
// compare's digest and time alone, so the resend guard holds back only what a
// heartbeat sent.
func (f *feedState) recordPush(station, key, digest string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rk := [2]string{station, key}
	rec := f.sent[rk]
	if rec == nil {
		rec = &feedSendRecord{}
		f.sent[rk] = rec
	}
	rec.pushed, rec.pushedAt = digest, now
}

// lastSent is the digest last sent of one key to one station by either path,
// "" when nothing has been.
func (f *feedState) lastSent(station, key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.sent[[2]string{station, key}]
	if rec == nil {
		return ""
	}
	digest, at := rec.Last()
	if rec.pushedAt.After(at) {
		return rec.pushed
	}
	return digest
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

// store keeps a build unless the memo already holds one started later: a
// heartbeat's build that began before a known write must not overwrite the
// build the write's push made, or the memo would hold the old digest and the
// next heartbeats would resend the old value to Edges that have the new one.
func (f *feedState) store(station, key string, e feedMemoEntry) {
	if feedScopes[key].noMemo {
		return
	}
	f.mu.Lock()
	k := memoKey(station, key)
	if cur, ok := f.memo[k]; !ok || !cur.at.After(e.at) {
		f.memo[k] = e
	}
	f.mu.Unlock()
}

// compare records one heartbeat's answer for one key and reports whether the
// feed should be sent: the digests differ and the guard does not hold it back.
// The guard does not hold back a key the Edge held and now quotes otherwise:
// a restarted Edge gets it on its first heartbeat. A matching digest ends any
// streak and clears the key's flag.
//
// SO DOES THE EDGE HOLDING WHAT WAS LAST SENT, by the compare or by a push. Converging means the Edge took
// the copy it was sent, not that Core's value stood still: a value that moves
// on three heartbeats in a row (a destination added, a bin in, the bin out) is
// sent three times to an Edge that applied every one, and counting those as a
// streak flagged a healthy station "not converging".
func (f *feedState) compare(station, key, current, held string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	rk := [2]string{station, key}
	rec := f.sent[rk]
	converged := current == held
	if rec != nil && (converged || rec.IsLast(held) || (rec.pushed != "" && held == rec.pushed)) {
		rec.Converged()
		if rec.flagged {
			rec.flagged = false
			f.clearFlagLocked(station, key)
		}
	}
	if converged {
		return false
	}
	if rec == nil {
		rec = &feedSendRecord{}
		f.sent[rk] = rec
	}
	if !rec.Send(current, held, now) {
		return false
	}
	if rec.NotConverging() && !rec.flagged {
		rec.flagged = true
		log.Printf("core_feeds: %s sent to %s %d times in a row without converging (digest %s, edge holds %q)",
			key, station, rec.Streak(), current, held)
		f.setFlagLocked(station, key, fmt.Sprintf("%s not converging: sent %d times", key, rec.Streak()))
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

// clearFlagEverywhere clears one flag id from every station that carries it,
// for a condition raised on several stations at once (a process two stations
// report) whose end is seen from only one of them.
func (f *feedState) clearFlagEverywhere(id string) {
	f.mu.Lock()
	for station := range f.flags {
		f.clearFlagLocked(station, id)
	}
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
// ok is false when the key has no scope row, is unknown to this Core, or its
// read failed; a failed read is never memoised, so the next heartbeat tries
// again.
func (s *CoreDataService) feedCurrent(station, key string, now time.Time) (feedMemoEntry, bool) {
	if _, scoped := feedScopes[key]; !scoped {
		return feedMemoEntry{}, false
	}
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
	case protocol.FeedContainment:
		snap, err := s.buildContainmentSnapshot()
		if err != nil {
			return "", nil, true, err
		}
		return snap.Digest, snap, true, nil
	case protocol.FeedRefusals:
		digest, value, err = s.buildRefusalsFeed()
		return digest, value, true, err
	case protocol.FeedNodes:
		v, err := s.buildNodeList(station, true)
		if err != nil {
			return "", nil, true, err
		}
		return v.Digest, v, true, nil
	case protocol.FeedScene:
		d, rows, err := s.buildScene()
		return d, rows, true, err
	case protocol.FeedCatalog:
		v, err := s.buildCatalog(true)
		if err != nil {
			return "", nil, true, err
		}
		return v.Digest, v, true, nil
	case protocol.FeedBins:
		v, known, err := s.buildNodeBins(station)
		if !known || err != nil {
			return "", nil, known, err
		}
		return v.Digest, v, true, nil
	case protocol.FeedSourcing:
		if s.sourcingReport == nil {
			return "", nil, false, nil
		}
		r, ok := s.sourcingReport(station)
		if !ok {
			return "", nil, false, nil
		}
		d, err := protocol.SourcingDigest(r.States)
		return d, r, true, err
	}
	return "", nil, false, nil
}

// buildRefusalsFeed is the FeedRefusals build: every refusal Core holds open,
// digested with the same function the Edge digests its own table with. One
// statement per build.
func (s *CoreDataService) buildRefusalsFeed() (string, any, error) {
	open, err := s.db.ListOpenSupplyRefusals()
	if err != nil {
		return "", nil, err
	}
	digest, err := protocol.RefusalsDigest(open)
	if err != nil {
		return "", nil, err
	}
	return digest, protocol.SupplyRefusalSnapshot{Digest: digest, Open: open}, nil
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
	nodeListSent := false
	for _, key := range keys {
		switch key {
		case protocol.FeedContainment:
			s.resp.sendData(protocol.SubjectContainmentSnapshot, station, due[key].value)
		case protocol.FeedRefusals:
			s.resp.sendData(protocol.SubjectSupplyRefusalSnapshot, station, due[key].value)
		case protocol.FeedNodes, protocol.FeedScene:
			// One node-list message carries both; geometry rides it when the
			// scene differs.
			if !nodeListSent {
				s.sendNodeFeed(station, held)
				nodeListSent = true
			}
		case protocol.FeedCatalog:
			s.resp.sendData(protocol.SubjectCatalogPayloadsResponse, station, due[key].value)
		case protocol.FeedBins:
			s.resp.sendData(protocol.SubjectBinsSnapshot, station, due[key].value)
		case protocol.FeedSourcing:
			s.resp.sendData(protocol.SubjectSourcingState, station, due[key].value)
		default:
			log.Printf("core_feeds: no sender for %s", key)
		}
	}
}
