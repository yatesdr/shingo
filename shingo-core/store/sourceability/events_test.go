//go:build docker

package sourceability_test

import (
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/sourceability"
)

// The monitor recomputes every two minutes and diffs; only the DIFF is
// recorded. These pin that contract at the persistence layer — the
// engine-level "one row per transition, not per tick" behaviour is asserted in
// engine/sourceability_events_test.go.

func TestRecordChange_WritesTheVerdictAndTheMissingPayload(t *testing.T) {
	db := testdb.Open(t)

	st := sourceability.StyleState{
		ProcessID: "SNF2",
		StyleID:   "STYLE-A",
		Status:    sourceability.StatusRed,
		Missing:   []string{"74577-6SA0B.06", "74577-6SA0C.06"},
	}
	testutil.MustNoErr(t, sourceability.RecordChange(db.DB, "SNF2", "STYLE-A", st, "green"), "record change")

	got, err := sourceability.ListEvents(db.DB, time.Now().UTC().Add(-time.Hour), "", "", 50)
	testutil.MustNoErr(t, err, "list events")
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	e := got[0]
	if e.ProcessKey != "SNF2" || e.StyleID != "STYLE-A" {
		t.Errorf("identity wrong: %+v", e)
	}
	if e.Sourceable {
		t.Error("a RED verdict is not sourceable")
	}
	// The FIRST missing payload is denormalised into its own indexed column —
	// that is the one an operator is told to go stock.
	if e.MissingPayload != "74577-6SA0B.06" {
		t.Errorf("missing_payload = %q, want the first missing payload", e.MissingPayload)
	}
	// JSONB round-trips with its own whitespace, so match on the parsed value
	// rather than the literal encoding.
	var meta struct {
		Missing    []string `json:"missing"`
		PrevStatus string   `json:"prev_status"`
	}
	testutil.MustNoErr(t, json.Unmarshal([]byte(e.Metadata), &meta), "decode metadata")
	if meta.PrevStatus != "green" {
		t.Errorf("metadata should record what it changed FROM, got %+v", meta)
	}
	if len(meta.Missing) != 2 {
		t.Errorf("metadata should carry the whole missing list, got %+v", meta.Missing)
	}
	// bin_uop_ledger's vocabulary, not a new one.
	if e.Op != "sourceability_change" || e.Actor != "system" || e.Source == "" {
		t.Errorf("op/source/actor not populated: %+v", e)
	}
}

// The recovery half of "SNF2 went unsourceable at 09:14, recovered 09:41".
func TestRecordChange_RecoveryIsSourceableWithNoMissing(t *testing.T) {
	db := testdb.Open(t)

	testutil.MustNoErr(t, sourceability.RecordChange(db.DB, "SNF2", "STYLE-A",
		sourceability.StyleState{Status: sourceability.StatusGreen}, "red"), "record recovery")

	got, err := sourceability.ListEvents(db.DB, time.Now().UTC().Add(-time.Hour), "", "", 50)
	testutil.MustNoErr(t, err, "list events")
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if !got[0].Sourceable {
		t.Error("a GREEN verdict is sourceable")
	}
	if got[0].MissingPayload != "" || got[0].Reason != "" {
		t.Errorf("recovery has nothing missing: %+v", got[0])
	}
}

// Filtering by payload is what makes "when was this part last unsourceable"
// a query rather than a scan.
func TestListEvents_FiltersByProcessAndPayload(t *testing.T) {
	db := testdb.Open(t)

	red := func(missing string) sourceability.StyleState {
		return sourceability.StyleState{Status: sourceability.StatusRed, Missing: []string{missing}}
	}
	testutil.MustNoErr(t, sourceability.RecordChange(db.DB, "SNF2", "A", red("PART-1"), ""), "1")
	testutil.MustNoErr(t, sourceability.RecordChange(db.DB, "SNF3", "A", red("PART-2"), ""), "2")
	testutil.MustNoErr(t, sourceability.RecordChange(db.DB, "SNF2", "B", red("PART-2"), ""), "3")

	since := time.Now().UTC().Add(-time.Hour)

	byProcess, err := sourceability.ListEvents(db.DB, since, "SNF2", "", 50)
	testutil.MustNoErr(t, err, "by process")
	if len(byProcess) != 2 {
		t.Errorf("SNF2 has 2 events, got %d", len(byProcess))
	}

	byPayload, err := sourceability.ListEvents(db.DB, since, "", "PART-2", 50)
	testutil.MustNoErr(t, err, "by payload")
	if len(byPayload) != 2 {
		t.Errorf("PART-2 appears in 2 events, got %d", len(byPayload))
	}

	both, err := sourceability.ListEvents(db.DB, since, "SNF2", "PART-2", 50)
	testutil.MustNoErr(t, err, "by both")
	if len(both) != 1 {
		t.Errorf("SNF2 + PART-2 is one event, got %d", len(both))
	}
}

// A window that predates every row returns nothing — the read is bounded, so
// the table growing does not make the query slower for a recent question.
func TestListEvents_WindowBounds(t *testing.T) {
	db := testdb.Open(t)
	testutil.MustNoErr(t, sourceability.RecordChange(db.DB, "SNF2", "A",
		sourceability.StyleState{Status: sourceability.StatusRed, Missing: []string{"P"}}, ""), "seed")

	got, err := sourceability.ListEvents(db.DB, time.Now().UTC().Add(time.Hour), "", "", 50)
	testutil.MustNoErr(t, err, "future window")
	if len(got) != 0 {
		t.Errorf("a future window matches nothing, got %d", len(got))
	}
}
