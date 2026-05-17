package service

import (
	"context"
	"errors"
	"testing"

	"shingo/protocol"
	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// fakeCorePoster is the in-memory PreflightCorePoster the unit tests wire
// into PreflightChecker.
type fakeCorePoster struct {
	available       bool
	queriedStation  string
	queriedPayloads []string
	respond         func(payloads []string) (*PreflightCoreResult, error)
}

func (f *fakeCorePoster) Available() bool { return f.available }
func (f *fakeCorePoster) PreflightInventory(station string, payloads []string) (*PreflightCoreResult, error) {
	f.queriedStation = station
	f.queriedPayloads = append([]string(nil), payloads...)
	if f.respond != nil {
		return f.respond(payloads)
	}
	return &PreflightCoreResult{}, nil
}

// seedPreflightStyle creates a process + style with three claims:
// PART-A (consume), PART-Z (consume), and an __empty__ produce sentinel.
// Returns the to-style ID.
func seedPreflightStyle(t *testing.T, db *store.DB) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO processes (name, description) VALUES ('PREFLIGHT-LINE', '')`)
	if err != nil {
		t.Fatalf("insert process: %v", err)
	}
	processID, _ := res.LastInsertId()
	res, err = db.Exec(`INSERT INTO styles (process_id, name, description) VALUES (?, 'STYLE-TO', '')`, processID)
	if err != nil {
		t.Fatalf("insert style: %v", err)
	}
	styleID, _ := res.LastInsertId()
	for _, c := range []processes.NodeClaimInput{
		{StyleID: styleID, CoreNodeName: "NODE-1", PayloadCode: "PART-A", Role: protocol.ClaimRoleConsume, SwapMode: "single_robot", InboundStaging: "ISTG-1", OutboundStaging: "OSTG-1"},
		{StyleID: styleID, CoreNodeName: "NODE-2", PayloadCode: "PART-Z", Role: protocol.ClaimRoleConsume, SwapMode: "single_robot", InboundStaging: "ISTG-2", OutboundStaging: "OSTG-2"},
		{StyleID: styleID, CoreNodeName: "NODE-3", PayloadCode: "__empty__", Role: protocol.ClaimRoleProduce, SwapMode: "single_robot", InboundStaging: "ISTG-3", OutboundStaging: "OSTG-3"},
	} {
		if _, err := db.UpsertStyleNodeClaim(c); err != nil {
			t.Fatalf("upsert claim %s: %v", c.CoreNodeName, err)
		}
	}
	return styleID
}

// TestChangeoverPreflight_BuildsPayloadListFromToStyle: when the to-style
// has claims for several payloads (with one __empty__ sentinel), the
// preflight call carries exactly the unique non-empty payload codes.
func TestChangeoverPreflight_BuildsPayloadListFromToStyle(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	toStyleID := seedPreflightStyle(t, db)

	core := &fakeCorePoster{available: true}
	checker := NewPreflightChecker(db, core, "station-1")

	missing, err := checker.PreflightInventoryCheck(context.Background(), toStyleID)
	if err != nil {
		t.Fatalf("PreflightInventoryCheck: %v", err)
	}
	if missing != nil {
		t.Errorf("expected nil missing, got %v", missing)
	}
	if core.queriedStation != "station-1" {
		t.Errorf("queriedStation = %q, want station-1", core.queriedStation)
	}
	gotSet := map[string]bool{}
	for _, p := range core.queriedPayloads {
		gotSet[p] = true
	}
	if !gotSet["PART-A"] || !gotSet["PART-Z"] {
		t.Errorf("queriedPayloads = %v, want both PART-A and PART-Z", core.queriedPayloads)
	}
	if gotSet["__empty__"] {
		t.Error("__empty__ sentinel must be filtered from preflight payloads")
	}
	if got := len(core.queriedPayloads); got != 2 {
		t.Errorf("queriedPayloads length = %d, want 2 (PART-A + PART-Z, no __empty__)", got)
	}
}

// TestChangeoverPreflight_FailsWhenNoBinAvailable: Core returns missing
// payloads → checker returns the same list unchanged.
func TestChangeoverPreflight_FailsWhenNoBinAvailable(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	toStyleID := seedPreflightStyle(t, db)

	core := &fakeCorePoster{
		available: true,
		respond: func(payloads []string) (*PreflightCoreResult, error) {
			return &PreflightCoreResult{Missing: []string{"PART-A"}}, nil
		},
	}
	checker := NewPreflightChecker(db, core, "station-1")

	missing, err := checker.PreflightInventoryCheck(context.Background(), toStyleID)
	if err != nil {
		t.Fatalf("PreflightInventoryCheck: %v", err)
	}
	if len(missing) != 1 || missing[0] != "PART-A" {
		t.Errorf("missing = %v, want [PART-A]", missing)
	}
}

// TestChangeoverPreflight_PropagatesCoreError: a Core HTTP failure surfaces
// as an error rather than collapsing silently to "all available".
func TestChangeoverPreflight_PropagatesCoreError(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	toStyleID := seedPreflightStyle(t, db)

	core := &fakeCorePoster{
		available: true,
		respond: func(payloads []string) (*PreflightCoreResult, error) {
			return nil, errors.New("core: HTTP 500")
		},
	}
	checker := NewPreflightChecker(db, core, "station-1")

	_, err := checker.PreflightInventoryCheck(context.Background(), toStyleID)
	if err == nil {
		t.Fatal("expected error when Core returns failure, got nil")
	}
}

// TestChangeoverPreflight_NoCoreClient: a nil-or-unavailable core client
// surfaces as an error — refuse the changeover rather than passing
// silently when the source of truth is offline.
func TestChangeoverPreflight_NoCoreClient(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	toStyleID := seedPreflightStyle(t, db)

	core := &fakeCorePoster{available: false}
	checker := NewPreflightChecker(db, core, "station-1")

	_, err := checker.PreflightInventoryCheck(context.Background(), toStyleID)
	if err == nil {
		t.Fatal("expected error when core client is unavailable")
	}
}
