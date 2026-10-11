//go:build docker

package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/internal/helpers"
)

// demand_tense_docker_test.go — the demand record's tense, pinned on a
// synthetic forecast row.
//
// A forecast is a demand_origins row with opened_at NULL and closed_at NULL:
// the record before its demand opens. No production path writes one in this
// phase (TestDemandTense_NoProductionWriterWritesAForecast), so every pin here
// writes it with raw SQL, the one way a test can put the row a later forecaster
// will write in front of today's readers and doors.

// insertForecast writes a forecast row at key and returns its origin id.
func insertForecast(t *testing.T, db *store.DB, key, kind, station string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`
		INSERT INTO demand_origins (origin_id, revision, episode_key, kind, station_id, opened_at)
		VALUES ($1, 0, $2, $3, $4, NULL)`, id, key, kind, station)
	testutil.MustNoErr(t, err, "insert synthetic forecast")
	return id
}

// TestDemandTense_ForecastRowIsLegal: after v153 a row may carry no opened_at,
// and the column says so.
func TestDemandTense_ForecastRowIsLegal(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	id := insertForecast(t, db, "cell|SNF1|PANEL-F|consume", "cell", "PLANT.LINE1")

	var openedNull, closedNull bool
	testutil.MustNoErr(t, db.QueryRow(
		`SELECT opened_at IS NULL, closed_at IS NULL FROM demand_origins WHERE origin_id = $1`, id).
		Scan(&openedNull, &closedNull), "read forecast back")
	if !openedNull || !closedNull {
		t.Errorf("forecast read back opened_at NULL=%v closed_at NULL=%v, want both NULL", openedNull, closedNull)
	}

	var nullable string
	testutil.MustNoErr(t, db.QueryRow(`
		SELECT is_nullable FROM information_schema.columns
		 WHERE table_name = 'demand_origins' AND column_name = 'opened_at'`).Scan(&nullable), "read column")
	if nullable != "YES" {
		t.Errorf("demand_origins.opened_at is_nullable = %q, want YES", nullable)
	}
}

// TestOpenReaders_AForecastIsNotOpen: every reader that asks "which episode is
// open" answers as if the forecast were not there, and none of them fails on
// its missing opened_at. An open reader that returned a forecast would let a
// threshold fire stamp an order on it, or let the keeper mint asks under it.
func TestOpenReaders_AForecastIsNotOpen(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	thrKey := protocol.ThresholdEpisodeKey("SLN_FC", "PANEL-FC")
	thr := insertForecast(t, db, thrKey, protocol.EpisodeKindThreshold, "PLANT.FC")
	mnt := insertForecast(t, db, protocol.MaintainEpisodeKey("MNT-FC", "MNT-FC-T"), protocol.EpisodeKindMaintain, "test-core")

	id, err := db.OpenOriginForKey(thrKey)
	testutil.MustNoErr(t, err, "OpenOriginForKey")
	if id != "" {
		t.Errorf("OpenOriginForKey returned the forecast %s as the open episode", id)
	}

	for _, kind := range []string{protocol.EpisodeKindThreshold, protocol.EpisodeKindMaintain} {
		open, err := db.ListOpenEpisodesOfKind(kind)
		if err != nil {
			t.Fatalf("ListOpenEpisodesOfKind(%s) failed on a forecast row: %v", kind, err)
		}
		for _, o := range open {
			if o.OriginID == thr || o.OriginID == mnt {
				t.Errorf("ListOpenEpisodesOfKind(%s) lists the forecast %s", kind, o.OriginID)
			}
		}
	}

	states, err := db.ListOpenEpisodeStates()
	if err != nil {
		t.Fatalf("ListOpenEpisodeStates failed on a forecast row: %v", err)
	}
	for _, s := range states {
		if s.OriginID == thr || s.OriginID == mnt {
			t.Errorf("ListOpenEpisodeStates lists the forecast %s", s.OriginID)
		}
	}

	group, bt, err := db.MaintainedEpisodeForOrigin(mnt)
	testutil.MustNoErr(t, err, "MaintainedEpisodeForOrigin")
	if group != "" || bt != "" {
		t.Errorf("MaintainedEpisodeForOrigin(forecast) = %q/%q, want blank: a forecast is not an open maintain episode", group, bt)
	}
}

// TestListDemandEpisodes_AForecastIsNotListed: the demand browser lists open
// and recently settled episodes. A forecast is neither, and its NULL opened_at
// (first under ORDER BY opened_at DESC) must not fail the page.
func TestListDemandEpisodes_AForecastIsNotListed(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	open := originAt("cell|SNF1|PANEL-LS|consume", "aaaaaaaa-0000-0000-0000-0000000000f1", 1, now.Add(-time.Minute))
	testutil.MustNoErr(t, db.UpsertDemandOrigin(open), "open episode")
	fc := insertForecast(t, db, "cell|SNF1|PANEL-LS2|consume", protocol.EpisodeKindCell, "PLANT.LINE1")

	eps, truncated, err := db.ListDemandEpisodes(now.Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("ListDemandEpisodes failed on a forecast row: %v", err)
	}
	if truncated {
		t.Error("truncated with two rows under a limit of 50")
	}
	if len(eps) != 1 || eps[0].OriginID != open.OriginID {
		ids := []string{}
		for _, e := range eps {
			ids = append(ids, e.OriginID)
		}
		t.Errorf("listed %v, want only the open episode %s (forecast %s is not an episode yet)", ids, open.OriginID, fc)
	}
}

// TestGetDemandOrigin_ReadsAForecast: the by-id read (the detail page) reads a
// forecast back rather than failing, with a zero OpenedAt for the NULL and no
// ClosedAt.
func TestGetDemandOrigin_ReadsAForecast(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	id := insertForecast(t, db, "cell|SNF1|PANEL-GD|consume", protocol.EpisodeKindCell, "PLANT.LINE1")
	got, err := db.GetDemandOrigin(id)
	if err != nil {
		t.Fatalf("GetDemandOrigin failed on a forecast row: %v", err)
	}
	if got == nil {
		t.Fatal("GetDemandOrigin returned nil for a forecast that exists")
	}
	if !got.OpenedAt.IsZero() || got.ClosedAt != nil {
		t.Errorf("forecast read back OpenedAt=%v ClosedAt=%v, want zero and nil", got.OpenedAt, got.ClosedAt)
	}
}

// mustOpenCore mints a Core episode and fails the test on any error. The id
// OpenCoreEpisode returns is the row's (a promoted forecast keeps its own);
// callers here plant no forecast, so it is the one they proposed.
func mustOpenCore(t *testing.T, db *store.DB, o store.DemandOrigin, what string) {
	t.Helper()
	_, err := db.OpenCoreEpisode(o)
	testutil.MustNoErr(t, err, what)
}

// TestDemandTense_GoSQLAgree: the SQL fragments and domain.DemandTenseOf
// classify every shape of row the same way, the closed-unfired forecast
// included, so a reader in SQL and a page in Go can never disagree about a
// row's tense.
func TestDemandTense_GoSQLAgree(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	rows := map[string]string{} // origin -> name
	add := func(name string, opened, closed *time.Time) {
		id := uuid.NewString()
		_, err := db.Exec(`
			INSERT INTO demand_origins (origin_id, episode_key, kind, opened_at, closed_at)
			VALUES ($1, $2, 'cell', $3, $4)`, id, "cell|SNF-AG|"+name+"|consume", opened, closed)
		testutil.MustNoErr(t, err, "insert "+name)
		rows[id] = name
	}
	closed := now.Add(time.Minute)
	add("forecast", nil, nil)
	add("open", &now, nil)
	add("settled", &now, &closed)
	add("settled-unfired", nil, &closed)

	for id, name := range rows {
		var sqlTense string
		testutil.MustNoErr(t, db.QueryRow(`
			SELECT CASE WHEN `+helpers.DemandForecastSQL("")+` THEN 'forecast'
			            WHEN `+helpers.DemandOpenSQL("")+` THEN 'open'
			            WHEN `+helpers.DemandSettledSQL("")+` THEN 'settled' END
			  FROM demand_origins WHERE origin_id = $1`, id).Scan(&sqlTense), "classify "+name)
		got, err := db.GetDemandOrigin(id)
		testutil.MustNoErr(t, err, "read "+name)
		if string(got.Tense()) != sqlTense {
			t.Errorf("%s: Go says %q, SQL says %q", name, got.Tense(), sqlTense)
		}
	}
}
