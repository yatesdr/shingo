//go:build docker

package service

import (
	"database/sql"
	"encoding/json"
	"testing"

	"shingo/protocol"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/audit"
)

// auditRow is the bin_uop_audit row shape needed by these tests. Mirrors
// the schema columns we care about asserting on; defined locally to keep
// the test self-contained (no production read helper exists yet — Phase
// 4's www audit timeline page will introduce one when it lands).
type auditRow struct {
	BinID       int64
	BeforeUOP   sql.NullInt64
	AfterUOP    int
	Op          string
	Source      string
	OrderID     sql.NullInt64
	PayloadCode string
	Actor       string
}

func loadBinUOPAudit(t *testing.T, db *store.DB, binID int64) []auditRow {
	t.Helper()
	rows, err := db.Query(`SELECT bin_id, before_uop, after_uop, op, source, order_id, payload_code, actor
		FROM bin_uop_audit WHERE bin_id=$1 ORDER BY id`, binID)
	if err != nil {
		t.Fatalf("query bin_uop_audit bin=%d: %v", binID, err)
	}
	defer rows.Close()

	var out []auditRow
	for rows.Next() {
		var r auditRow
		testutil.MustNoErr(t, rows.Scan(&r.BinID, &r.BeforeUOP, &r.AfterUOP, &r.Op, &r.Source, &r.OrderID, &r.PayloadCode, &r.Actor), "scan bin_uop_audit row")
		out = append(out, r)
	}
	return out
}

// TestBinUOPAudit_SyncOrClearForReleased_LogsBeforeAndAfter pins Phase 0a:
// every BinManifestService write that touches uop_remaining must append a
// bin_uop_audit row inside the same transaction as the bin update,
// capturing the before/after counts plus the op, order, and actor that
// produced the change. Without this, Phase 1+ regressions devolve into
// guessing which of ~13 write sites stomped a bin.
func TestBinUOPAudit_SyncOrClearForReleased_LogsBeforeAndAfter(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-AUDIT-PARTIAL", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	partial := 47
	testutil.MustNoErr(t, svc.SyncOrClearForReleased(bin.ID, order.ID, &partial, "", "stephen-station"), "SyncOrClearForReleased")

	rows := loadBinUOPAudit(t, db, bin.ID)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if !r.BeforeUOP.Valid || r.BeforeUOP.Int64 != 100 {
		t.Errorf("BeforeUOP = %v, want 100", r.BeforeUOP)
	}
	if r.AfterUOP != partial {
		t.Errorf("AfterUOP = %d, want %d", r.AfterUOP, partial)
	}
	if r.Op != audit.OpReleasedPartial {
		t.Errorf("Op = %q, want %q", r.Op, audit.OpReleasedPartial)
	}
	if r.Source != "service/bin_manifest.go:SyncOrClearForReleased" {
		t.Errorf("Source = %q", r.Source)
	}
	if !r.OrderID.Valid || r.OrderID.Int64 != order.ID {
		t.Errorf("OrderID = %v, want %d", r.OrderID, order.ID)
	}
	if r.Actor != "stephen-station" {
		t.Errorf("Actor = %q, want %q", r.Actor, "stephen-station")
	}
}

// TestBinUOPAudit_ClearForReuse_LogsZeroAfter pins the clear path: even
// when the operation has no associated order (operator-driven cleanup,
// scrap-zero workflow), the audit row must still land with order_id NULL
// and a recognizable op tag.
func TestBinUOPAudit_ClearForReuse_LogsZeroAfter(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-AUDIT-CLEAR", "PART-A", 88)

	if _, err := svc.ClearForReuse(bin.ID, nil); err != nil {
		t.Fatalf("ClearForReuse"+": %v", err)
	}

	rows := loadBinUOPAudit(t, db, bin.ID)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if !r.BeforeUOP.Valid || r.BeforeUOP.Int64 != 88 {
		t.Errorf("BeforeUOP = %v, want 88", r.BeforeUOP)
	}
	if r.AfterUOP != 0 {
		t.Errorf("AfterUOP = %d, want 0", r.AfterUOP)
	}
	if r.Op != audit.OpClearForReuse {
		t.Errorf("Op = %q, want %q", r.Op, audit.OpClearForReuse)
	}
	if r.OrderID.Valid {
		t.Errorf("OrderID = %v, want NULL (no associated order)", r.OrderID.Int64)
	}
}

// TestBinUOPAudit_Atomicity_NoRowOnFailedUpdate pins the transactional
// invariant: if the bin update is rejected (claim mismatch, locked, etc.)
// the audit row must NOT land. Otherwise the audit log lies about what
// actually committed and Phase 1+ forensics get poisoned.
func TestBinUOPAudit_Atomicity_NoRowOnFailedUpdate(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-AUDIT-LOCKED", "PART-A", 100)
	otherOrder := createTestOrder(t, db, sd.LineNode.ID)

	// Claim the bin under a different order, then try to release as
	// otherOrder — the claimed_by guard rejects the UPDATE and rolls back.
	realOrder := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, realOrder.ID)

	zero := 0
	if err := svc.SyncOrClearForReleased(bin.ID, otherOrder.ID, &zero, "", "rogue-caller"); err == nil {
		t.Fatal("expected error from claim mismatch, got nil")
	}

	rows := loadBinUOPAudit(t, db, bin.ID)
	if len(rows) != 0 {
		t.Fatalf("audit rows = %d, want 0 (failed update must not land an audit row); got: %+v", len(rows), rows)
	}

	// And the bin's actual UOP must be unchanged from the rollback.
	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 100 {
		t.Errorf("bin UOP = %d, want 100 (unchanged after failed release)", got.UOPRemaining)
	}
}

// overrideRow extends auditRow with the metadata column so the Phase 0b
// override tests can assert on the JSON context that distinguishes
// override observations from regular write rows.
type overrideRow struct {
	auditRow
	Metadata []byte
}

func loadBinUOPOverrideRows(t *testing.T, db *store.DB, binID int64, op string) []overrideRow {
	t.Helper()
	rows, err := db.Query(`SELECT bin_id, before_uop, after_uop, op, source, order_id, payload_code, actor, metadata
		FROM bin_uop_audit WHERE bin_id=$1 AND op=$2 ORDER BY id`, binID, op)
	if err != nil {
		t.Fatalf("query bin_uop_audit override bin=%d op=%q: %v", binID, op, err)
	}
	defer rows.Close()

	var out []overrideRow
	for rows.Next() {
		var r overrideRow
		var meta sql.NullString
		if err := rows.Scan(&r.BinID, &r.BeforeUOP, &r.AfterUOP, &r.Op, &r.Source,
			&r.OrderID, &r.PayloadCode, &r.Actor, &meta); err != nil {
			t.Fatalf("scan override row: %v", err)
		}
		if meta.Valid {
			r.Metadata = []byte(meta.String)
		}
		out = append(out, r)
	}
	return out
}

// TestRegression_OverrideAuditReleasePartial pins Phase 0b: when the
// operator submits a release_partial Count that diverges from the
// system-suggested baseline, AuditReleaseOverride writes one
// bin_uop_audit row with op=operator_override_release_partial. before
// holds the suggested value, after holds the operator's submission, and
// metadata carries the disposition kind for cross-row context.
//
// SME contract (plan §2.5): operator-entered count is ground truth, but
// every divergence must be observable so management / SCO can review
// override patterns.
func TestRegression_OverrideAuditReleasePartial(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-OVR-PARTIAL", "PART-A", 60)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	suggested := 60
	disp := &protocol.UOPDisposition{
		Kind:           protocol.DispositionReleasePartial,
		Count:          47, // operator typed 47 — bin had been overfilled upstream
		CountSuggested: &suggested,
	}
	testutil.MustNoErr(t, svc.AuditReleaseOverride(bin.ID, order.ID, disp, "stephen-station"), "AuditReleaseOverride")

	rows := loadBinUOPOverrideRows(t, db, bin.ID, audit.OpOperatorOverrideReleasePartial)
	if len(rows) != 1 {
		t.Fatalf("override rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if !r.BeforeUOP.Valid || r.BeforeUOP.Int64 != int64(suggested) {
		t.Errorf("BeforeUOP = %v, want %d (system-suggested)", r.BeforeUOP, suggested)
	}
	if r.AfterUOP != 47 {
		t.Errorf("AfterUOP = %d, want 47 (operator-submitted)", r.AfterUOP)
	}
	if !r.OrderID.Valid || r.OrderID.Int64 != order.ID {
		t.Errorf("OrderID = %v, want %d", r.OrderID, order.ID)
	}
	if r.Actor != "stephen-station" {
		t.Errorf("Actor = %q, want %q", r.Actor, "stephen-station")
	}

	var meta map[string]any
	if err := json.Unmarshal(r.Metadata, &meta); err != nil {
		t.Fatalf("metadata not valid JSON: %v (raw=%q)", err, string(r.Metadata))
	}
	if meta["kind"] != string(protocol.DispositionReleasePartial) {
		t.Errorf("metadata.kind = %v, want %q", meta["kind"], protocol.DispositionReleasePartial)
	}
	if int(meta["auto_count"].(float64)) != suggested {
		t.Errorf("metadata.auto_count = %v, want %d", meta["auto_count"], suggested)
	}
	if int(meta["operator_count"].(float64)) != 47 {
		t.Errorf("metadata.operator_count = %v, want 47", meta["operator_count"])
	}

	// Bin row must be untouched — AuditReleaseOverride is observational.
	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 60 {
		t.Errorf("bin UOP = %d, want 60 (audit must not write to bins)", got.UOPRemaining)
	}
}

// TestRegression_OverrideAuditPullParts pins Phase 0b: pull_parts
// overrides write one audit row per part where the operator's qty
// diverges from the chip's pre-populated value. Parts that match the
// suggestion don't produce rows. payload_code holds the part number so
// forensics can filter by part within a bin.
func TestRegression_OverrideAuditPullParts(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-OVR-PULL", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	disp := &protocol.UOPDisposition{
		Kind: protocol.DispositionPullParts,
		Captures: map[string]int{
			"PART-A": 9, // operator overrode label-claimed 12 → counted 9
			"PART-B": 5, // matches suggestion — no row
			"PART-C": 3, // operator added a part the system didn't list → row
		},
		CapturesSuggested: map[string]int{
			"PART-A": 12,
			"PART-B": 5,
			// PART-C absent → suggested 0, operator 3 → divergence
		},
	}
	testutil.MustNoErr(t, svc.AuditReleaseOverride(bin.ID, order.ID, disp, "stephen-station"), "AuditReleaseOverride")

	rows := loadBinUOPOverrideRows(t, db, bin.ID, audit.OpOperatorOverridePullParts)
	if len(rows) != 2 {
		t.Fatalf("override rows = %d, want 2 (PART-A and PART-C diverge; PART-B matches)", len(rows))
	}

	byPart := map[string]overrideRow{}
	for _, r := range rows {
		byPart[r.PayloadCode] = r
	}
	a, ok := byPart["PART-A"]
	if !ok {
		t.Fatal("no override row for PART-A")
	}
	if !a.BeforeUOP.Valid || a.BeforeUOP.Int64 != 12 || a.AfterUOP != 9 {
		t.Errorf("PART-A: before/after = %v/%d, want 12/9", a.BeforeUOP, a.AfterUOP)
	}
	c, ok := byPart["PART-C"]
	if !ok {
		t.Fatal("no override row for PART-C")
	}
	if !c.BeforeUOP.Valid || c.BeforeUOP.Int64 != 0 || c.AfterUOP != 3 {
		t.Errorf("PART-C: before/after = %v/%d, want 0/3 (suggested absent → 0)", c.BeforeUOP, c.AfterUOP)
	}
	if _, ok := byPart["PART-B"]; ok {
		t.Error("override row for PART-B (matches suggestion) — no row should be written")
	}
}

// TestRegression_NoOverrideNoAudit pins the no-op contract: when
// operator-submitted equals system-suggested across the board,
// AuditReleaseOverride writes no rows. Defends against a refactor that
// turns the audit into "every release writes a row," which would
// flood the table and dilute the signal of an actual override.
func TestRegression_NoOverrideNoAudit(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-NO-OVR", "PART-A", 75)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	suggested := 75
	cases := []struct {
		name string
		disp *protocol.UOPDisposition
	}{
		{
			name: "release_partial_matches",
			disp: &protocol.UOPDisposition{
				Kind:           protocol.DispositionReleasePartial,
				Count:          75,
				CountSuggested: &suggested,
			},
		},
		{
			name: "pull_parts_matches",
			disp: &protocol.UOPDisposition{
				Kind:              protocol.DispositionPullParts,
				Captures:          map[string]int{"PART-A": 12, "PART-B": 5},
				CapturesSuggested: map[string]int{"PART-A": 12, "PART-B": 5},
			},
		},
		{
			name: "release_empty_never_audits",
			disp: &protocol.UOPDisposition{
				Kind: protocol.DispositionReleaseEmpty,
			},
		},
		{
			name: "nil_disposition_legacy_client",
			disp: nil,
		},
		{
			name: "release_partial_no_baseline_legacy_client",
			disp: &protocol.UOPDisposition{
				Kind:  protocol.DispositionReleasePartial,
				Count: 47,
				// CountSuggested nil → no baseline to compare → no audit
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutil.MustNoErr(t, svc.AuditReleaseOverride(bin.ID, order.ID, tc.disp, "stephen-station"), "AuditReleaseOverride")
		})
	}

	for _, op := range []string{audit.OpOperatorOverrideReleasePartial, audit.OpOperatorOverridePullParts} {
		rows := loadBinUOPOverrideRows(t, db, bin.ID, op)
		if len(rows) != 0 {
			t.Errorf("op=%q: %d rows, want 0 (no override → no audit)", op, len(rows))
		}
	}
}
