//go:build docker

package sourceability_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/sourceability"
)

// This proves ScoreTTE's SQL against the real schema: both join arms, the
// "last sample BEFORE the episode opened" rule, and that an episode with no
// prior projection survives as a blind spot rather than vanishing. The pure
// aggregate lives in tte_score_test.go.

var scoreBase = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

func insertSample(t *testing.T, db *store.DB, at time.Time, process, node, payload string, tte any) {
	t.Helper()
	_, err := db.DB.Exec(`INSERT INTO tte_samples
		(computed_at, process_id, style_id, core_node_name, payload_code,
		 uop_remaining, rate_per_sec, tte_seconds, style_status, reorder_point)
		VALUES ($1,$2,'A',$3,$4,100,1.0,$5,'green',0)`,
		at, process, node, payload, tte)
	if err != nil {
		t.Fatalf("insert sample: %v", err)
	}
}

func insertEpisode(t *testing.T, db *store.DB, originID, key, kind, process, node, payload string, openedAt time.Time, trigger string) {
	t.Helper()
	_, err := db.DB.Exec(`INSERT INTO demand_origins
		(origin_id, episode_key, kind, trigger_kind, process_id, core_node_name,
		 payload_code, opened_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		originID, key, kind, trigger, process, node, payload, openedAt)
	if err != nil {
		t.Fatalf("insert episode: %v", err)
	}
}

func scoreFor(t *testing.T, scores []sourceability.TTEScore, key string) sourceability.TTEScore {
	t.Helper()
	for _, s := range scores {
		if s.EpisodeKey == key {
			return s
		}
	}
	t.Fatalf("no score for episode %q in %+v", key, scores)
	return sourceability.TTEScore{}
}

// THE BRIEF'S THREE EPISODES: one the forecast called late, one it called
// early, and one it never covered at all.
func TestScoreTTE_HitLateHitEarlyAndBlind(t *testing.T) {
	db := testdb.Open(t)

	// LATE: at 11:00 the line was projected to run dry in 3600s (12:00). It
	// actually opened a demand at 11:50 — the forecast still had 600s on the
	// clock, so the error is +600.
	insertSample(t, db, scoreBase.Add(-60*time.Minute), "PRESS-1", "NODE-LATE", "BIN-A", 3600.0)
	insertEpisode(t, db, "11111111-1111-1111-1111-111111111111",
		"thr|NODE-LATE|BIN-A", "threshold", "PRESS-1", "NODE-LATE", "BIN-A",
		scoreBase.Add(-10*time.Minute), "autoreorder")

	// EARLY: at 11:00 projected dry in 600s (11:10), but the demand did not
	// open until 11:40 — called dry 1800s early.
	insertSample(t, db, scoreBase.Add(-60*time.Minute), "PRESS-2", "", "BIN-B", 600.0)
	insertEpisode(t, db, "22222222-2222-2222-2222-222222222222",
		"cell|PRESS-2|BIN-B|consume", "cell", "PRESS-2", "", "BIN-B",
		scoreBase.Add(-20*time.Minute), "operator")

	// BLIND: an episode with no sample for its place at all.
	insertEpisode(t, db, "33333333-3333-3333-3333-333333333333",
		"thr|NODE-BLIND|BIN-C", "threshold", "PRESS-3", "NODE-BLIND", "BIN-C",
		scoreBase.Add(-5*time.Minute), "autoreorder")

	scores, err := sourceability.ScoreTTE(db.DB, scoreBase.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ScoreTTE: %v", err)
	}
	if len(scores) != 3 {
		t.Fatalf("scores = %d, want 3: %+v", len(scores), scores)
	}

	late := scoreFor(t, scores, "thr|NODE-LATE|BIN-A")
	if !late.HasSample {
		t.Fatal("late episode has no sample; the threshold arm did not join")
	}
	if late.ErrorSeconds != 600 {
		t.Errorf("late error = %v, want +600 (guess still had time on the clock)", late.ErrorSeconds)
	}
	if late.TriggerKind != "autoreorder" {
		t.Errorf("late trigger = %q, want autoreorder", late.TriggerKind)
	}

	// THE CELL ARM JOINS ON THE PROCESS, and this episode carries no node at
	// all — so a join that reached for core_node_name would find nothing here.
	early := scoreFor(t, scores, "cell|PRESS-2|BIN-B|consume")
	if !early.HasSample {
		t.Fatal("cell episode has no sample; the process arm did not join")
	}
	if early.ErrorSeconds != -1800 {
		t.Errorf("early error = %v, want -1800 (called dry early)", early.ErrorSeconds)
	}
	if early.TriggerKind != "operator" {
		t.Errorf("early trigger = %q, want operator", early.TriggerKind)
	}

	blind := scoreFor(t, scores, "thr|NODE-BLIND|BIN-C")
	if blind.HasSample {
		t.Errorf("blind episode reported a sample: %+v", blind)
	}

	aggs := sourceability.AggregateTTEScores(scores)
	if len(aggs) != 3 {
		t.Fatalf("aggregates = %d, want 3 (three distinct places): %+v", len(aggs), aggs)
	}
}

// ONLY A SAMPLE THAT PREDATES THE EPISODE COUNTS. A projection made after the
// demand opened knows the answer; scoring against it would grade the forecast
// on hindsight.
func TestScoreTTE_IgnoresSamplesAfterTheEpisodeOpened(t *testing.T) {
	db := testdb.Open(t)

	insertSample(t, db, scoreBase.Add(-30*time.Minute), "P", "NODE-X", "BIN-A", 1800.0)  // before: the one to use
	insertSample(t, db, scoreBase.Add(+30*time.Minute), "P", "NODE-X", "BIN-A", 99999.0) // after: must be ignored
	insertEpisode(t, db, "44444444-4444-4444-4444-444444444444",
		"thr|NODE-X|BIN-A", "threshold", "P", "NODE-X", "BIN-A", scoreBase, "autoreorder")

	scores, err := sourceability.ScoreTTE(db.DB, scoreBase.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ScoreTTE: %v", err)
	}
	s := scoreFor(t, scores, "thr|NODE-X|BIN-A")
	// The 11:30 sample projected dry at 12:00, exactly when the demand opened.
	if s.ErrorSeconds != 0 {
		t.Errorf("error = %v, want 0 — the pre-episode sample is the one that counts", s.ErrorSeconds)
	}
}

// THE LAST one before, not the first. A place is sampled every two minutes; the
// forecast being graded is the most recent one standing when demand opened.
func TestScoreTTE_TakesTheLastSampleBefore(t *testing.T) {
	db := testdb.Open(t)

	insertSample(t, db, scoreBase.Add(-60*time.Minute), "P", "NODE-Y", "BIN-A", 60.0)
	insertSample(t, db, scoreBase.Add(-10*time.Minute), "P", "NODE-Y", "BIN-A", 600.0) // the last one
	insertEpisode(t, db, "55555555-5555-5555-5555-555555555555",
		"thr|NODE-Y|BIN-A", "threshold", "P", "NODE-Y", "BIN-A", scoreBase, "autoreorder")

	scores, err := sourceability.ScoreTTE(db.DB, scoreBase.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ScoreTTE: %v", err)
	}
	s := scoreFor(t, scores, "thr|NODE-Y|BIN-A")
	// 11:50 + 600s = 12:00, the moment demand opened.
	if s.ErrorSeconds != 0 {
		t.Errorf("error = %v, want 0 — the 11:50 sample, not the 11:00 one", s.ErrorSeconds)
	}
}

// A SAMPLE WITH NO PROJECTION IS NOT A FORECAST. tte_seconds IS NULL means the
// line had nothing staged or the payload had no consumption in the window;
// scoring it as zero would manufacture a wildly early call out of an absence.
func TestScoreTTE_NullProjectionIsNotAForecast(t *testing.T) {
	db := testdb.Open(t)

	insertSample(t, db, scoreBase.Add(-10*time.Minute), "P", "NODE-Z", "BIN-A", nil)
	insertEpisode(t, db, "66666666-6666-6666-6666-666666666666",
		"thr|NODE-Z|BIN-A", "threshold", "P", "NODE-Z", "BIN-A", scoreBase, "autoreorder")

	scores, err := sourceability.ScoreTTE(db.DB, scoreBase.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ScoreTTE: %v", err)
	}
	s := scoreFor(t, scores, "thr|NODE-Z|BIN-A")
	if s.HasSample {
		t.Errorf("a NULL tte_seconds row was scored as a forecast: %+v", s)
	}
}

// The window is honoured: an episode older than `since` is not scored.
func TestScoreTTE_RespectsSince(t *testing.T) {
	db := testdb.Open(t)

	insertEpisode(t, db, "77777777-7777-7777-7777-777777777777",
		"thr|NODE-OLD|BIN-A", "threshold", "P", "NODE-OLD", "BIN-A",
		scoreBase.Add(-48*time.Hour), "autoreorder")
	insertEpisode(t, db, "88888888-8888-8888-8888-888888888888",
		"thr|NODE-NEW|BIN-A", "threshold", "P", "NODE-NEW", "BIN-A",
		scoreBase.Add(-1*time.Hour), "autoreorder")

	scores, err := sourceability.ScoreTTE(db.DB, scoreBase.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ScoreTTE: %v", err)
	}
	if len(scores) != 1 {
		t.Fatalf("scores = %d, want 1 (the 48h-old episode is outside the window): %+v", len(scores), scores)
	}
	if scores[0].EpisodeKey != "thr|NODE-NEW|BIN-A" {
		t.Errorf("scored %q, want the in-window episode", scores[0].EpisodeKey)
	}
}

// Kinds Core does not forecast — changeover, maintain — are not in the
// population at all. A changeover is not a line running dry.
func TestScoreTTE_OnlyCellAndThresholdKinds(t *testing.T) {
	db := testdb.Open(t)

	insertEpisode(t, db, "99999999-9999-9999-9999-999999999999",
		"co|PLANT.LINE1|7", "changeover", "P", "", "", scoreBase.Add(-time.Hour), "operator")
	insertEpisode(t, db, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"mnt|GROUP-1|BT-1", "maintain", "P", "GROUP-1", "", scoreBase.Add(-time.Hour), "autoreorder")

	scores, err := sourceability.ScoreTTE(db.DB, scoreBase.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ScoreTTE: %v", err)
	}
	if len(scores) != 0 {
		t.Errorf("scores = %+v, want none — changeover and maintain are not forecast kinds", scores)
	}
}

// The writer's round trip: RecordTTESamples stores what ComputeWithSamples
// produced, including the NULL for an unknown projection, and ScoreTTE reads it
// back.
func TestRecordTTESamples_RoundTrip(t *testing.T) {
	db := testdb.Open(t)

	samples := []sourceability.TTESample{
		{
			ProcessID: "PRESS-1", StyleID: "A", StyleStatus: "red", ReorderPoint: 12,
			Line: sourceability.LineTTE{
				NodeName: "NODE-RT", PayloadCode: "BIN-A", UOPRemaining: 50,
				RatePerSec: 2, TimeToEmpty: 25 * time.Second, Known: true,
			},
		},
		{
			ProcessID: "PRESS-1", StyleID: "A", StyleStatus: "red",
			Line: sourceability.LineTTE{
				NodeName: "NODE-UNK", PayloadCode: "BIN-B", // Known false
			},
		},
	}
	if err := sourceability.RecordTTESamples(db.DB, samples, sourceability.TTERetention); err != nil {
		t.Fatalf("RecordTTESamples: %v", err)
	}

	var known, null int
	if err := db.DB.QueryRow(
		`SELECT COUNT(*) FILTER (WHERE tte_seconds IS NOT NULL),
		        COUNT(*) FILTER (WHERE tte_seconds IS NULL)
		 FROM tte_samples`).Scan(&known, &null); err != nil {
		t.Fatalf("count: %v", err)
	}
	if known != 1 || null != 1 {
		t.Errorf("known/null = %d/%d, want 1/1 — an unknown projection stores as NULL", known, null)
	}

	var status string
	var reorder int
	var tte float64
	if err := db.DB.QueryRow(
		`SELECT style_status, reorder_point, tte_seconds FROM tte_samples
		 WHERE core_node_name = 'NODE-RT'`).Scan(&status, &reorder, &tte); err != nil {
		t.Fatalf("read back: %v", err)
	}
	// THE RED SAMPLE SURVIVED THE WRITE. This is the row the whole change
	// exists to keep.
	if status != "red" {
		t.Errorf("style_status = %q, want red", status)
	}
	if reorder != 12 {
		t.Errorf("reorder_point = %d, want 12", reorder)
	}
	if tte != 25 {
		t.Errorf("tte_seconds = %v, want 25", tte)
	}
}

// Retention removes what is past the horizon and keeps what is inside it.
func TestRecordTTESamples_PrunesPastRetention(t *testing.T) {
	db := testdb.Open(t)

	// Written directly with explicit ages; RecordTTESamples stamps NOW().
	insertSample(t, db, time.Now().Add(-100*24*time.Hour), "P", "NODE-OLD", "BIN-A", 10.0)
	insertSample(t, db, time.Now().Add(-1*24*time.Hour), "P", "NODE-NEW", "BIN-A", 10.0)

	if err := sourceability.RecordTTESamples(db.DB, nil, sourceability.TTERetention); err != nil {
		t.Fatalf("RecordTTESamples: %v", err)
	}

	var nodes []string
	rows, err := db.DB.Query(`SELECT core_node_name FROM tte_samples ORDER BY core_node_name`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		nodes = append(nodes, n)
	}
	if len(nodes) != 1 || nodes[0] != "NODE-NEW" {
		t.Errorf("surviving rows = %v, want [NODE-NEW] — 100 days is past the 45-day horizon", nodes)
	}
}
