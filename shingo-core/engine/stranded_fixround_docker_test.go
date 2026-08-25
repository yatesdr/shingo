//go:build docker

package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// stranded_fixround_docker_test.go — the four holes the round-2 dev review found
// in the freeze's lifecycle and in what an operator is told afterwards.
//
// Each of these was RED against the ten-commit branch. They are here as a set
// because they are one story: a frozen observation is only worth acting on
// while it describes something this process actually watched and has not been
// overtaken by time, and every sentence written about it has to be the same
// sentence next tick or the dedup that keeps it out of the journal cannot work.

// logSink captures the engine's log lines so a test can assert that a line was
// PRINTED, not merely that the database changed.
//
// The stranded-note map is a silencer keyed on the note's text: it exists so a
// sweep repeating the same sentence every pass says it once. That makes "was it
// logged" a real question with a wrong answer — a bin re-stranded after an
// operator recovery matches its own stale entry and says nothing at all — and
// the only way to ask it is to read the log.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) log(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

func (s *logSink) countContaining(sub string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// newLoggingEngine is newUnstartedEngine with the log captured instead of
// forwarded to t.Logf.
func newLoggingEngine(t *testing.T, db *store.DB, sink *logSink) *Engine {
	t.Helper()
	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-core"
	cfg.Messaging.DispatchTopic = "shingo.dispatch"
	return New(Config{
		AppConfig: cfg,
		DB:        db,
		Fleet:     simulator.New(),
		MsgClient: nil,
		LogFunc:   sink.log,
	})
}

// appendOrderHistory adds a history row WITHOUT removing the ones already
// there — pickupOrderAt replaces, and a replan is a second arrival at the same
// status rather than a correction of the first.
func appendOrderHistory(t *testing.T, db *store.DB, ord *orders.Order, status protocol.Status, at time.Time) {
	t.Helper()
	_, err := db.DB.Exec(
		`INSERT INTO order_history (order_id, status, detail, created_at) VALUES ($1,$2,$3,$4)`,
		ord.ID, string(status), "test replan", at)
	testutil.MustNoErr(t, err, "append order history")
}

// ── P1: the observation expires; the answer is not re-taken ────────────────

// AN EXPIRED OBSERVATION DECLINES. It does not get replaced by a fresh one.
//
// The freeze holds the one reading that describes where the bin was set down.
// When the placement cannot be made — an occupied slot, a point that does not
// resolve — that reading is retried, and after the inference window it stops
// being worth acting on: hours later an operator may have moved the bin by
// hand. Declining is the honest answer.
//
// What must NOT happen is the answer being re-taken. The robot has gone back to
// work by then and is standing somewhere Core CAN name, so a re-taken reading
// resolves cleanly and places the bin at a station it was never set down at —
// the exact pin drift the freeze exists to prevent, arriving one window late.
func TestCarriedBin_AnExpiredObservationDeclinesAndIsNeverRetaken(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	// Where the deck actually empties: a park point, which never resolves.
	seedScenePoint(t, db, "Area-01", "PP41", "ParkPoint", "")
	// Where the robot is standing three hours later: a real station, empty and
	// resolvable — everything a placement needs except a reason to believe it.
	later := &nodes.Node{Name: "SMN_041", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(later), "create the later station")
	seedScenePoint(t, db, "Area-01", "SMN_041", "GeneralLocation", "AP241")

	bin, ord := seedStranded(t, db, "AMR-EXPIRE")
	cacheRobot(eng, loadedDeck("AMR-EXPIRE"))
	eng.inferStrandedTransitBin(ord.ID)

	cacheRobot(eng, atPoint("AMR-EXPIRE", "PP41", 0.91, 24.84))
	eng.sweepCarriedBins()
	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-EXPIRE" {
		t.Fatalf("setup: bin is at %q, want the carrier node — a park point must not place", got)
	}

	// A night passes. ONLY THE OBSERVATION AGES: the bin never left the deck
	// and this process never stopped running, which is what makes this a test
	// of the expiry rather than of the restart rule.
	eng.dropObsMu.Lock()
	aged := eng.dropObs[bin.ID]
	aged.At = aged.At.Add(-3 * time.Hour)
	eng.dropObs[bin.ID] = aged
	eng.dropObsMu.Unlock()

	// The robot has long since gone back to work and is parked at a station.
	cacheRobot(eng, atPoint("AMR-EXPIRE", "AP241", -7.2, 3.4))
	eng.sweepCarriedBins()
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-EXPIRE" {
		t.Errorf("bin was placed at %q from a reading taken three hours after the deck "+
			"emptied — the observation expired and the watch took a fresh one, which is "+
			"the pin drift the freeze exists to prevent, delayed by one window", got)
	}
	note := binNote(t, db, bin.ID)
	if strings.Contains(note, "AP241") || strings.Contains(note, "SMN_041") {
		t.Errorf("note %q names where the robot went afterwards, not where the deck emptied", note)
	}
	if !strings.Contains(note, "observed more than") {
		t.Errorf("note = %q, want the age of the observation named as the reason", note)
	}
	if !strings.Contains(note, "x=0.91") {
		t.Errorf("note = %q, want the FROZEN coordinates — the drop was watched, so where "+
			"it happened is still evidence even once it is too old to act on", note)
	}
}

// ── P1: a telemetry gap is not a witnessed drop ────────────────────────────

// A DECK LAST SEEN LOADED AN HOUR AGO IS NOT A TRANSITION THIS PROCESS WATCHED.
//
// The restart rule refuses a drop Core did not see because it was down. Core
// staying UP is not the same as Core watching: the fleet can go unreachable, or
// one AMR can roam off the WiFi, and the robot cache is never pruned — so a
// deck last read loaded keeps re-arming the witness from a stale entry while
// the robot is somewhere Core cannot see. When it comes back with an empty
// deck, the gap is where the bin could have been taken off by hand, and the
// reading on the far side of it describes the robot rather than the bin.
func TestCarriedBin_ATelemetryGapIsNotAWitnessedDrop(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_042", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	seedScenePoint(t, db, "Area-01", "SMN_042", "GeneralLocation", "AP242")

	bin, ord := seedStranded(t, db, "AMR-GAP")
	cacheRobot(eng, loadedDeck("AMR-GAP"))
	eng.inferStrandedTransitBin(ord.ID)
	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-GAP" {
		t.Fatalf("setup: bin is at %q, want the carrier node", got)
	}

	// The fleet goes away for an hour. Core is up the whole time; it simply
	// hears nothing about this robot, so the last thing it knows is a deck that
	// was loaded an hour ago.
	eng.dropObsMu.Lock()
	eng.deckSeenLoaded[bin.ID] = eng.deckSeenLoaded[bin.ID].Add(-time.Hour)
	eng.dropObsMu.Unlock()

	// It comes back parked at a station, deck empty. Somewhere in that hour it
	// set the bin down — or somebody lifted it off.
	cacheRobot(eng, atPoint("AMR-GAP", "AP242", 3.3, -4.4))
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-GAP" {
		t.Errorf("bin was placed at %q from a drop nobody watched — the deck was last read "+
			"loaded an hour earlier, and everything that could have happened to the bin "+
			"happened inside that gap", got)
	}
	note := binNote(t, db, bin.ID)
	if !strings.Contains(note, "last read loaded") {
		t.Errorf("note = %q, want the gap named", note)
	}
	if strings.Contains(note, "restarted") {
		t.Errorf("note = %q reuses the restart sentence — Core did not restart, it went "+
			"deaf, and telling an operator the wrong thing about why is worse than telling "+
			"them nothing", note)
	}
	if strings.Contains(note, "x=") {
		t.Errorf("note %q offers coordinates the same sentence says are meaningless", note)
	}
}

// ── P2: the _TRANSIT decline says the same thing twice ─────────────────────

// A DECLINE THAT REPEATS MUST REPEAT THE SAME BYTES. The reconciliation sweep
// re-runs the inference over every stranded bin on every pass, and both dedups
// — the log's and the database's — are keyed on the note being unchanged. A
// note carrying the time the reading was TAKEN is unchanged only on the carried
// path, where the reading is frozen; on the _TRANSIT path the reading is taken
// fresh each pass, so the timestamp moves and neither dedup can fire.
func TestStrandedBin_TransitDeclineIsByteStableAcrossPasses(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	seedScenePoint(t, db, "Area-01", "CP44", "ChargePoint", "")

	bin, ord := seedStranded(t, db, "AMR-CHURN2")
	// PARKED, at a point that will not resolve. Nothing about this robot
	// changes between passes, so anything that moves in the note came from the
	// note's own construction.
	cacheRobot(eng, atPoint("AMR-CHURN2", "CP44", 2.0, 3.0))

	eng.inferStrandedTransitBin(ord.ID)
	if got := binNodeName(t, db, bin.ID); got != "_TRANSIT" {
		t.Fatalf("setup: bin is at %q, want _TRANSIT — a charge point must not place", got)
	}
	first, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if first.AnomalyNote == "" {
		t.Fatal("setup: no note was written")
	}
	// THE STRUCTURAL HALF, and it needs no clock: the instant a reading was
	// taken belongs on the line that records a placement, which is written
	// once. In a note the sweep rewrites, it is the one field guaranteed to
	// differ next pass.
	if strings.Contains(first.AnomalyNote, "deck read empty") {
		t.Errorf("note %q carries the instant the reading was taken; on this path that "+
			"reading is taken fresh every pass, so the note can never be twice the same",
			first.AnomalyNote)
	}

	// THE BEHAVIOURAL HALF. Straddle a second boundary, because the timestamp
	// this is about has one-second resolution and three passes inside one
	// second would agree by accident.
	time.Sleep(1100 * time.Millisecond)
	eng.sweepStrandedBins()
	time.Sleep(1100 * time.Millisecond)
	eng.sweepStrandedBins()

	after, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin again")
	if after.AnomalyNote != first.AnomalyNote {
		t.Errorf("the note changed across passes with nothing about the bin or the robot "+
			"changing:\n  %q\n  %q", first.AnomalyNote, after.AnomalyNote)
	}
	if !after.UpdatedAt.Equal(first.UpdatedAt) {
		t.Errorf("updated_at moved from %s to %s across two identical passes — the write "+
			"dedup cannot fire on a note that is never twice the same",
			first.UpdatedAt, after.UpdatedAt)
	}
}

// ── P3: what an operator leaves behind, and what the next stranding says ────

// THE OPERATOR FOUND IT, SO NOTHING IS LOST ANY MORE — and if it is lost again
// the log has to say so.
//
// Two halves, and they are the same defect from two sides. The database side:
// RecoverToNode clears the stamp and used to leave the NOTE, so a bin at its
// correct home still carried a sentence naming a robot's coordinates. The
// engine side: the log's silencer is keyed on that note's text and nothing
// cleared it either, so the same bin stranded the same way a second time was
// re-flagged in the database and never mentioned.
func TestStrandedBin_ReStrandAfterAnOperatorRecoveryIsStampedAndLogged(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sink := &logSink{}
	eng := newLoggingEngine(t, db, sink)

	home := &nodes.Node{Name: "SMN_045", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(home), "create the node the operator found it at")
	seedScenePoint(t, db, "Area-01", "CP45", "ChargePoint", "")

	bin, ord := seedStranded(t, db, "AMR-REFIND")
	cacheRobot(eng, atPoint("AMR-REFIND", "CP45", 5.0, 6.0))

	eng.inferStrandedTransitBin(ord.ID)
	stranded, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if stranded.AnomalyAt == nil || stranded.AnomalyNote == "" {
		t.Fatalf("setup: bin was not flagged (at=%v note=%q)", stranded.AnomalyAt, stranded.AnomalyNote)
	}
	firstNote := stranded.AnomalyNote
	if n := sink.countContaining("left at _TRANSIT"); n != 1 {
		t.Fatalf("setup: %d log lines for the first stranding, want 1", n)
	}

	// THE OPERATOR DOOR, both halves of it: the service call, and the
	// bookkeeping the handler does around it.
	testutil.MustNoErr(t, eng.BinService().RecoverTransitAnomaly(bin.ID, home.ID, "operator:test", ""),
		"operator recovery")
	eng.forgetStrandedNote(bin.ID)

	recovered, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin after recovery")
	if recovered.AnomalyAt != nil {
		t.Errorf("anomaly_at survived the recovery")
	}
	if recovered.AnomalyNote != "" {
		t.Errorf("anomaly_note = %q after an operator found the bin and said where it is — "+
			"the sentence means nobody knows where this bin is, and somebody just did",
			recovered.AnomalyNote)
	}

	// THE `OR anomaly_at IS NULL` CLAUSE, pinned where it is still load-bearing.
	// A cycle count clears the stamp and keeps the note (bins.RecordCount, and
	// ClearAnomaly below), so a bin stranded again in exactly the same way would
	// match its own leftover text; without that half of the guard the UPDATE is
	// skipped and the bin ends up lost and unflagged.
	testutil.MustNoErr(t, db.MarkBinAnomalyWithNote(bin.ID, firstNote), "re-note the bin")
	testutil.MustNoErr(t, db.ClearBinAnomaly(bin.ID), "clear the stamp, keep the note")
	testutil.MustNoErr(t, db.MarkBinAnomalyWithNote(bin.ID, firstNote), "re-mark with the same note")
	reStamped, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin after the same-note re-mark")
	if reStamped.AnomalyAt == nil {
		t.Error("an unchanged note with a cleared stamp did not re-stamp — the bin is lost " +
			"and not flagged")
	}
	testutil.MustNoErr(t, db.ClearBinAnomaly(bin.ID), "reset for the re-strand")
	_, err = db.DB.Exec(`UPDATE bins SET anomaly_note='' WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "reset the note for the re-strand")

	// STRANDED AGAIN, THE SAME WAY. Same robot, same point, same sentence.
	transit, err := db.GetNodeByName("_TRANSIT")
	testutil.MustNoErr(t, err, "get _TRANSIT")
	testutil.MustNoErr(t, db.MoveBinToTransit(bin.ID, transit.ID), "re-strand the bin")
	eng.inferStrandedTransitBin(ord.ID)

	again, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin after the second stranding")
	if again.AnomalyAt == nil {
		t.Error("the second stranding did not stamp the bin")
	}
	if again.AnomalyNote != firstNote {
		t.Errorf("second note = %q, want the same sentence as the first (%q)", again.AnomalyNote, firstNote)
	}
	if n := sink.countContaining("left at _TRANSIT"); n != 2 {
		t.Errorf("%d log lines across two separate strandings, want 2 — the silencer that "+
			"suppresses a repeat within one episode must not carry across a recovery, or a "+
			"bin lost twice the same way is announced once", n)
	}
}

// ── P4: a replan is not a pickup ───────────────────────────────────────────

// THE PICKUP IS THE FIRST TIME THE ORDER WENT IN_TRANSIT, NOT THE LAST.
//
// `faulted -> in_transit` is a legal transition (dispatch/lifecycle.go), so an
// order that faulted and replanned carries two in_transit rows — and the bin
// was picked up once, at the first. Reading the latest row lets a twenty-hour
// -old pickup wear a five-minute-old timestamp, which is exactly the guard
// E-prime exists to be.
func TestBranchA_AReplanDoesNotLaunderAStalePickup(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_047", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	seedScenePoint(t, db, "Area-01", "SMN_047", "GeneralLocation", "AP247")

	bin, ord := seedStranded(t, db, "AMR-REPLAN")
	// The pickup: twenty-one hours ago.
	pickupOrderAt(t, db, ord, clock.Now().UTC().Add(-21*time.Hour))
	// The replan: five minutes ago, and no bin moved — the robot was already
	// carrying it.
	appendOrderHistory(t, db, ord, protocol.StatusInTransit, clock.Now().UTC().Add(-5*time.Minute))
	strandOrderAt(t, db, ord, clock.Now().UTC())

	cacheRobot(eng, atPoint("AMR-REPLAN", "AP247", -1.1, -2.2))
	eng.inferStrandedTransitBin(ord.ID)

	if got := binNodeName(t, db, bin.ID); got != "_TRANSIT" {
		t.Errorf("bin was placed at %q — the bin left the floor twenty-one hours ago and "+
			"only the REPLAN is recent, so where this robot is standing now says nothing "+
			"about where it put the bin", got)
	}
	if note := binNote(t, db, bin.ID); !strings.Contains(note, "picked up longer ago") {
		t.Errorf("note = %q, want the pickup age named", note)
	}
}
