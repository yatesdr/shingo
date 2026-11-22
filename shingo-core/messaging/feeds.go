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
// station when the two differ. One row per feed (coreFeeds) says how it is
// memoised, built and sent.
//
// One feed keeps state per Edge beyond the memo: bins, whose scope is a watch
// list each Edge publishes. bins_feed.go holds those lists in memory and, after
// a known change, pushes on one timer per dirty Edge; the compare here treats
// bins like any per-station feed.

const (
	// feedMemoPlant bounds how stale a plant-wide digest may be. A writer that
	// calls no hook or door is caught by the next heartbeat after the memo
	// expires, so this plus the heartbeat interval (60 s) is the worst-case heal
	// (~70 s).
	feedMemoPlant = 10 * time.Second
	// feedMemoBins bounds one Edge's bins snapshot. Short, as feedMemoPlant is:
	// a bin moved by a writer that calls no hook is in a snapshot ten seconds
	// later, and the Edge's next heartbeat carries it there.
	feedMemoBins = 10 * time.Second
	// feedMemoStation bounds the per-station feeds (nodes, scene). Two minutes is
	// the cadence the node list was re-asked at before feeds. Every known write
	// to what they are built from goes through a change door (FeedsChanged),
	// which forgets the memos and pushes the stations their new copies, and a
	// reply forgets its station's (HandleNodeListRequest). So this is the
	// backstop for a write that reaches no door: it reaches the Edge up to two
	// minutes and a heartbeat later (those writers are named in
	// TestFeedWrites_EveryWriterReachesADoorOrIsNamed).
	feedMemoStation = 2 * time.Minute
)

// feedMemoEntry is one built feed: the value and its digest, kept together so
// what is sent is exactly what was digested.
type feedMemoEntry struct {
	digest string
	value  any
	at     time.Time
}

// feedSendRecord is what Core sent of one key to one station, under the shared
// rule (protocol/resend): the heartbeat compare's sends, which the resend
// guard reads (two minutes, which was the old re-ask's cost for a key that
// never converges), and the sends made outside it (recordPush), which a quote
// may prove too and the guard ignores, so a lost push does not hold back the
// heartbeat send that heals it.
type feedSendRecord struct {
	resend.Record
	flagged bool
}

// feedState is the memo, the send records, the flags and the change doors'
// armed pushes. One mutex: every access is a map lookup or a small write, and
// heartbeats arrive one per station per minute.
type feedState struct {
	mu lockcheck.Mutex[lockorder.MessagingFeeds]
	// memo is each feed's build by slot (coreFeed.slot): feed key and station,
	// "" for a plant-wide feed.
	memo  map[[2]string]feedMemoEntry
	sent  map[[2]string]*feedSendRecord
	flags map[string]map[string]string
	// drops counts the memo drops; dropped is the count at the last drop of
	// one slot, droppedAll at the last drop of every slot of a feed. A build
	// carries the count it started at (dropCount), and store keeps it only
	// when no drop of its slot has come since: a build that read before a
	// known write must not be stored after the drop that write made.
	drops      uint64
	dropped    map[[2]string]uint64
	droppedAll map[string]uint64
	// quoted is the digest each station last quoted for each key, and when:
	// what a change door's push compares with when the quote is newer than
	// the last send (lastKnown).
	quoted map[[2]string]feedQuote
	// pending is each station's push armed by a change door, until it starts;
	// running counts the pushes started and not done (feed_doors.go).
	pending   map[string]*feedPush
	running   int
	pushDelay time.Duration
}

// feedQuote is one heartbeat's quote of one key.
type feedQuote struct {
	digest string
	at     time.Time
}

func newFeedState() *feedState {
	return &feedState{
		memo:       map[[2]string]feedMemoEntry{},
		sent:       map[[2]string]*feedSendRecord{},
		flags:      map[string]map[string]string{},
		dropped:    map[[2]string]uint64{},
		droppedAll: map[string]uint64{},
		quoted:     map[[2]string]feedQuote{},
		pending:    map[string]*feedPush{},
		pushDelay:  feedPushDelay,
	}
}

// coreFeed is everything Core says about one feed it serves, in one place: how
// its build is memoised, how it is built, the subject it is sent on, and the
// send. The facts were a scope table and two switches on the key, one to build
// and one to send; a feed missing from the build switch was silent and one
// missing from the send switch was a log line. TestCoreFeeds_EveryFeedHasARow
// fails on a key of protocol.AllFeeds with no row and on a row with no build,
// carrier or send.
type coreFeed struct {
	key string
	// perStation: built for each station apart, else once for the plant. A
	// per-station feed memoised for the plant would answer station B with
	// station A's build.
	perStation bool
	// ttl is how long a build is memoised; zero is never, and every heartbeat
	// builds the feed afresh.
	ttl time.Duration
	// carrier is the subject the feed is sent on. Feeds that share a carrier
	// share one send per heartbeat: one node list carries the nodes and the
	// scene.
	carrier string
	// build builds the feed's value for one station and digests it. known is
	// false when this Core does not serve the key to that station. Any failed
	// read fails the build: a digest of a partial read would confirm a copy
	// nobody read.
	build func(s *CoreDataService, station string) (digest string, value any, known bool, err error)
	// send sends a due feed's build e to one station, the value exactly as
	// digested. current reads another feed's build as the same answer reads
	// it; held is what the Edge holds, for a send that depends on it.
	send func(s *CoreDataService, station string, f coreFeed, e feedMemoEntry, current feedReader, held map[string]string)
}

// feedReader reads one feed's current build by key, for one station at one
// instant (feedCurrent).
type feedReader func(key string) (feedMemoEntry, bool)

// coreFeeds has one row per feed Core serves and NO DEFAULT: a key without a
// row is neither memoised nor served (feedCurrent leaves it out of the ack, as
// a key this Core does not know). A default once memoised any key plant-wide,
// so a per-station feed left out would have answered station B with station
// A's build. The order is protocol.AllFeeds'.
var coreFeeds = [...]coreFeed{
	{key: protocol.FeedContainment, ttl: feedMemoPlant, carrier: protocol.SubjectContainmentSnapshot,
		build: buildContainmentFeed, send: sendFeedValue},
	{key: protocol.FeedRefusals, ttl: feedMemoPlant, carrier: protocol.SubjectSupplyRefusalSnapshot,
		build: buildRefusalsFeed, send: sendFeedValue},
	{key: protocol.FeedNodes, perStation: true, ttl: feedMemoStation, carrier: protocol.SubjectNodeListResponse,
		build: buildNodesFeed, send: sendNodeFeed},
	{key: protocol.FeedScene, perStation: true, ttl: feedMemoStation, carrier: protocol.SubjectNodeListResponse,
		build: buildSceneFeed, send: sendNodeFeed},
	{key: protocol.FeedCatalog, ttl: feedMemoPlant, carrier: protocol.SubjectCatalogPayloadsResponse,
		build: buildCatalogFeed, send: sendFeedValue},
	// Known per Edge: only for one whose watch list Core holds.
	{key: protocol.FeedBins, perStation: true, ttl: feedMemoBins, carrier: protocol.SubjectBinsSnapshot,
		build: buildBinsFeed, send: sendFeedValue},
	// Never memoised: built from the monitor's in-memory verdicts with no read,
	// so a memo would save nothing and could answer a delta the monitor just
	// pushed with the digest from before it.
	{key: protocol.FeedSourcing, perStation: true, carrier: protocol.SubjectSourcingState,
		build: buildSourcingFeed, send: sendFeedValue},
}

// coreFeedOf is the row for a feed key; ok false for a key this Core does not
// serve.
func coreFeedOf(key string) (coreFeed, bool) {
	for _, f := range coreFeeds {
		if f.key == key {
			return f, true
		}
	}
	return coreFeed{}, false
}

// slot is where the feed's build for one station is memoised: the station's
// own for a per-station feed, the plant's ("") for a plant-wide one.
func (f coreFeed) slot(station string) [2]string {
	if f.perStation {
		return [2]string{f.key, station}
	}
	return [2]string{f.key, ""}
}

// slotOf is the memo slot of key for station, and the key's row (the zero row,
// never memoised, for a key with none).
func slotOf(station, key string) ([2]string, coreFeed) {
	f, ok := coreFeedOf(key)
	if !ok {
		return [2]string{key, ""}, coreFeed{key: key}
	}
	return f.slot(station), f
}

// drop forgets every build of a feed, the plant's or every station's (a
// station's build still running included), so the next read rebuilds it.
// Called after a known write to that feed's data.
func (f *feedState) drop(key string) {
	f.mu.Lock()
	f.drops++
	for slot := range f.memo {
		if slot[0] == key {
			delete(f.memo, slot)
		}
	}
	f.droppedAll[key] = f.drops
	f.mu.Unlock()
}

// dropStation forgets one station's build of a feed: of a plant-wide feed,
// the plant's, which is what that station is answered.
func (f *feedState) dropStation(station, key string) {
	slot, _ := slotOf(station, key)
	f.mu.Lock()
	f.drops++
	delete(f.memo, slot)
	f.dropped[slot] = f.drops
	f.mu.Unlock()
}

// recordPush records a send made outside the heartbeat compare, so a quote of
// it converges. It leaves the compare's digest, time and streak alone, so the
// resend guard holds back only what a heartbeat sent.
func (f *feedState) recordPush(station, key, digest string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rk := [2]string{station, key}
	rec := f.sent[rk]
	if rec == nil {
		rec = &feedSendRecord{}
		f.sent[rk] = rec
	}
	rec.Pushed(digest, now)
}

// lastSent is the digest last sent of one key to one station by either path,
// as recorded, quoted back or not; "" when nothing has been.
func (f *feedState) lastSent(station, key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.sent[[2]string{station, key}]
	if rec == nil {
		return ""
	}
	digest, _ := rec.Last()
	return digest
}

// lookup is the memo's build of one feed for one station, ok false on a miss,
// on a build older than the row's ttl, and always for a feed never memoised.
func (f *feedState) lookup(station, key string, now time.Time) (feedMemoEntry, bool) {
	slot, row := slotOf(station, key)
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.memo[slot]
	if !ok || now.Sub(e.at) >= row.ttl {
		return feedMemoEntry{}, false
	}
	return e, true
}

// dropCount is the number of memo drops so far. A build reads it before its
// first read and carries it to store.
func (f *feedState) dropCount() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drops
}

// store keeps a build that started at drop count drops, unless its slot was
// dropped since, the memo already holds a build started later, or the feed is
// never memoised. A build that read before a known write and finished after
// the write's drop holds the old digest: kept, the next heartbeats would send
// the old value to Edges that have the new one (one that a reply answered, or
// a push sent), until the memo aged out. Pinned by
// TestFeedState_ABuildStartedBeforeADropIsNotKept.
func (f *feedState) store(station, key string, e feedMemoEntry, drops uint64) {
	slot, row := slotOf(station, key)
	if row.ttl <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if drops < f.dropped[slot] || drops < f.droppedAll[key] {
		return
	}
	if cur, ok := f.memo[slot]; !ok || !cur.at.After(e.at) {
		f.memo[slot] = e
	}
}

// compare records one heartbeat's answer for one key and reports whether the
// feed should be sent: the digests differ and the guard does not hold it back.
// The guard does not hold back a key the Edge held and now quotes otherwise:
// a restarted Edge gets it on its first heartbeat. A matching digest ends any
// streak and clears the key's flag.
//
// SO DOES THE EDGE QUOTING A COPY SENT SINCE THE LAST ONE IT PROVED, by the
// compare or by a push (resend.Record.Quoted). Converging means the Edge took
// what it was sent, not that Core's value stood still: a value that moves on
// three heartbeats in a row (a destination added, a bin in, the bin out) is
// sent three times to an Edge that applied every one. And the quote is as old
// as the heartbeat: the copies pushed since it left are newer than it, and
// counting either as a streak flagged a healthy station "not converging". An
// Edge frozen at one digest is believed once and flags on its third send.
// Pinned by TestFeedState_AQuoteOfACopySentSinceTheLastProvedConverges and
// TestFeedState_AFrozenQuoteStillFlags.
func (f *feedState) compare(station, key, current, held string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	rk := [2]string{station, key}
	f.quoted[rk] = feedQuote{digest: held, at: now}
	rec := f.sent[rk]
	converged := current == held
	if rec != nil {
		proved := rec.Quoted(held)
		if converged {
			rec.Converged()
		}
		if (proved || converged) && rec.flagged {
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

// setFlagLocked records a plain-text flag against a station, shown on its row
// of the Edges page. id names the flag so the condition that raised it can
// clear it.
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
// a key sent three times in a row without converging (held in memory, from
// the send records), a process another station reports too (read from the
// report rows, claimsFlags). Read by the Edges page; empty when nothing is
// wrong.
func (s *CoreDataService) EdgeFeedFlags(station string) []string {
	out := append(s.feeds.flagsFor(station), s.claimsFlags(station)...)
	sort.Strings(out)
	return out
}

// answerFeeds answers a heartbeat's Feeds map: Core's current digest for every
// key it could read, and a send of each feed whose digest differs. A key Core
// could not read is left out of the answer and nothing is sent for it.
func (s *CoreDataService) answerFeeds(station string, held map[string]string) map[string]string {
	now := clock.Now()
	current := s.feedReaderAt(station, now)
	answer := make(map[string]string, len(held))
	due := map[string]feedMemoEntry{}
	for key, h := range held {
		e, ok := current(key)
		if !ok {
			continue
		}
		answer[key] = e.digest
		if s.feeds.compare(station, key, e.digest, h, now) {
			due[key] = e
		}
	}
	if len(due) > 0 {
		s.sendFeeds(station, due, current, held)
	}
	return answer
}

// feedReaderAt reads feeds' current builds for one station at one instant.
func (s *CoreDataService) feedReaderAt(station string, now time.Time) feedReader {
	return func(key string) (feedMemoEntry, bool) { return s.feedCurrent(station, key, now) }
}

// feedCurrent returns the memoised build of one feed, building it on a miss.
// ok is false when the key has no row, the row does not serve it to this
// station, or its read failed; a failed read is never memoised, so the next
// heartbeat tries again.
func (s *CoreDataService) feedCurrent(station, key string, now time.Time) (feedMemoEntry, bool) {
	f, ok := coreFeedOf(key)
	if !ok {
		return feedMemoEntry{}, false
	}
	if e, ok := s.feeds.lookup(station, key, now); ok {
		return e, true
	}
	drops := s.feeds.dropCount()
	digest, value, known, err := f.build(s, station)
	if !known {
		return feedMemoEntry{}, false
	}
	if err != nil {
		log.Printf("core_feeds: build %s for %s: %v — left out of the ack", key, station, err)
		return feedMemoEntry{}, false
	}
	e := feedMemoEntry{digest: digest, value: value, at: now}
	s.feeds.store(station, key, e, drops)
	return e, true
}

// The rows' builds. Each is the strict build of its feed, so a digest is never
// of a partial read; a reply's lenient build (HandleNodeListRequest,
// HandleCatalogPayloadsRequest) is never memoised.

func buildContainmentFeed(s *CoreDataService, _ string) (string, any, bool, error) {
	snap, err := s.buildContainmentSnapshot()
	if err != nil {
		return "", nil, true, err
	}
	return snap.Digest, snap, true, nil
}

// buildRefusalsFeed is every refusal Core holds open, digested with the same
// function the Edge digests its own table with. One statement per build.
func buildRefusalsFeed(s *CoreDataService, _ string) (string, any, bool, error) {
	open, err := s.db.ListOpenSupplyRefusals()
	if err != nil {
		return "", nil, true, err
	}
	digest, err := protocol.RefusalsDigest(open)
	if err != nil {
		return "", nil, true, err
	}
	return digest, protocol.SupplyRefusalSnapshot{Open: open}, true, nil
}

func buildNodesFeed(s *CoreDataService, station string) (string, any, bool, error) {
	v, err := s.buildNodeList(station, true)
	if err != nil {
		return "", nil, true, err
	}
	return v.Digest, v, true, nil
}

func buildSceneFeed(s *CoreDataService, _ string) (string, any, bool, error) {
	d, rows, err := s.buildScene()
	return d, rows, true, err
}

func buildCatalogFeed(s *CoreDataService, _ string) (string, any, bool, error) {
	v, err := s.buildCatalog(true)
	if err != nil {
		return "", nil, true, err
	}
	return v.Digest, v, true, nil
}

// buildBinsFeed is unknown for an Edge whose watch list Core does not hold.
func buildBinsFeed(s *CoreDataService, station string) (string, any, bool, error) {
	v, known, err := s.buildNodeBins(station)
	if !known || err != nil {
		return "", nil, known, err
	}
	return v.Digest, v, true, nil
}

// buildSourcingFeed is unknown until the monitor is wired and has made its
// first pass for the station.
func buildSourcingFeed(s *CoreDataService, station string) (string, any, bool, error) {
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

// sendFeedValue is the send of a feed whose build is the message: its value,
// on its carrier.
func sendFeedValue(s *CoreDataService, station string, f coreFeed, e feedMemoEntry, _ feedReader, _ map[string]string) {
	s.resp.sendData(f.carrier, station, e.value)
}

// sendFeeds sends each due feed to one station through its row's send, in key
// order, once per carrier: a feed whose carrier went already this time rode
// that send (the node list carries the nodes and the scene). held is what the
// Edge holds, for a send that depends on it.
func (s *CoreDataService) sendFeeds(station string, due map[string]feedMemoEntry, current feedReader, held map[string]string) {
	keys := make([]string, 0, len(due))
	for key := range due {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sent := map[string]bool{}
	for _, key := range keys {
		f, ok := coreFeedOf(key)
		if !ok || sent[f.carrier] {
			continue
		}
		sent[f.carrier] = true
		f.send(s, station, f, due[key], current, held)
	}
}
