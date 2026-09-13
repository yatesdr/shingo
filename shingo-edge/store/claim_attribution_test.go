package store

import (
	"database/sql"
	"errors"
	"testing"

	"shingo/protocol/testutil"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// claim_attribution_test.go — who wrote each claim row, and what happens to a
// row that changeover history still points at.
//
// The HMI flow composer makes style_node_claims fully open: an operator on
// the floor writes the same rows an engineer writes on the desktop. The
// owner's ruling is that every row says who wrote it and from where, stamped
// by the server, never sent by a client. A new test file for a new concern:
// nothing else in this package is about attribution or retirement.

func seedClaimProcess(t *testing.T, db *DB, name string) (processID, styleID int64) {
	t.Helper()
	pid, err := db.CreateProcess(name, "", "", "", "", false)
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	sid, err := db.CreateStyle("S-"+name, "", pid)
	if err != nil {
		t.Fatalf("CreateStyle: %v", err)
	}
	return pid, sid
}

// TestClaim_ExistingRowsReadAdmin: a row written before attribution existed —
// no source, no called_by, no updated_at — reads as the desktop's, because
// the desktop was the only writer there was. Inserted raw, the way a plant
// database holds it, not through UpsertClaim.
func TestClaim_ExistingRowsReadAdmin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, sid := seedClaimProcess(t, db, "P-Legacy")
	if _, err := db.Exec(`INSERT INTO style_node_claims (style_id, core_node_name, role, swap_mode, payload_code, auto_reorder)
		VALUES (?, 'PLN_01', 'consume', 'two_robot', 'RAW', 0)`, sid); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	claims, err := db.ListStyleNodeClaims(sid)
	if err != nil {
		t.Fatalf("ListStyleNodeClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(claims))
	}
	c := claims[0]
	if c.Source != domain.ClaimSourceAdmin || c.CalledBy != "" || c.UpdatedAt != nil || c.RetiredAt != nil ||
		c.SourcePresetID != nil || c.SourcePresetVersion != nil {
		t.Fatalf("legacy row = source %q called_by %q updated_at %v retired_at %v preset %v/%v; want admin, '', nil, nil, nil, nil",
			c.Source, c.CalledBy, c.UpdatedAt, c.RetiredAt, c.SourcePresetID, c.SourcePresetVersion)
	}
}

// TestClaim_UpsertStampsSourceCalledByAndUpdatedAt: what the writer says it
// is, who it was, and when — on INSERT and again on UPDATE. A second writer
// overwrites the first's attribution: the row now says who last wrote it.
func TestClaim_UpsertStampsSourceCalledByAndUpdatedAt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, sid := seedClaimProcess(t, db, "P-Stamp")
	in := processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "PLN_01", Role: "consume", SwapMode: "two_robot",
		PayloadCode: "RAW", InboundStaging: "STG", OutboundDestination: "DST", Source: domain.ClaimSourceAdmin, CalledBy: "alice",
		SourcePresetID: domain.Ptr(int64(7)), SourcePresetVersion: domain.Ptr(2),
	}
	id, err := db.UpsertStyleNodeClaim(in)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	c, err := db.GetStyleNodeClaim(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if c.Source != "admin" || c.CalledBy != "alice" || c.UpdatedAt == nil {
		t.Fatalf("after insert: source %q called_by %q updated_at %v; want admin/alice/set", c.Source, c.CalledBy, c.UpdatedAt)
	}
	if c.SourcePresetID == nil || *c.SourcePresetID != 7 || c.SourcePresetVersion == nil || *c.SourcePresetVersion != 2 {
		t.Fatalf("after insert: preset provenance %v/%v, want 7/2", c.SourcePresetID, c.SourcePresetVersion)
	}
	first := *c.UpdatedAt

	// The HMI edits the same row: attribution follows the writer. Provenance
	// is pointer-gated like the other absent-means-untouched columns — a
	// writer that says nothing about it leaves it alone.
	in.Source, in.CalledBy = domain.ClaimSourceHMI, "station-press-400"
	in.SourcePresetID, in.SourcePresetVersion = nil, nil
	if _, err := db.UpsertStyleNodeClaim(in); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	c, err = db.GetStyleNodeClaim(id)
	testutil.MustNoErr(t, err, "db.GetStyleNodeClaim")
	if c.Source != "hmi" || c.CalledBy != "station-press-400" {
		t.Fatalf("after update: source %q called_by %q, want hmi/station-press-400", c.Source, c.CalledBy)
	}
	if c.UpdatedAt == nil || c.UpdatedAt.Before(first) {
		t.Fatalf("after update: updated_at %v did not advance from %v", c.UpdatedAt, first)
	}
	if c.SourcePresetID == nil || *c.SourcePresetID != 7 {
		t.Fatalf("after update with no opinion on provenance: preset id %v, want 7 kept", c.SourcePresetID)
	}
	// An unknown source is refused rather than stored.
	in.Source = "browser"
	if _, err := db.UpsertStyleNodeClaim(in); err == nil {
		t.Fatal("source=browser was accepted; the vocabulary is admin/hmi/generated/cloned")
	}
}

// TestClaim_CloneAndGenerateStampTheirOwnSource: a clone's rows say 'cloned'
// and a generated family's rows say 'generated', by the caller who asked;
// preset provenance carries across (the clone IS the same shape), the
// source and called_by do not (the clone sets its own).
func TestClaim_CloneAndGenerateStampTheirOwnSource(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, base := seedClaimProcess(t, db, "P-Clone")
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: base, CoreNodeName: "PLN_01", Role: "consume", SwapMode: "two_robot",
		PayloadCode: "RAW", InboundStaging: "STG", OutboundDestination: "DST", Source: domain.ClaimSourceHMI, CalledBy: "station",
		SourcePresetID: domain.Ptr(int64(7)), SourcePresetVersion: domain.Ptr(2),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cloneID, err := db.CloneStyle(base, "CLONE", "", "alice")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	cl, err := db.GetStyleNodeClaimByNode(cloneID, "PLN_01")
	if err != nil {
		t.Fatalf("get cloned claim: %v", err)
	}
	if cl.Source != domain.ClaimSourceCloned || cl.CalledBy != "alice" || cl.UpdatedAt == nil {
		t.Fatalf("clone = source %q called_by %q updated_at %v; want cloned/alice/set", cl.Source, cl.CalledBy, cl.UpdatedAt)
	}
	if cl.SourcePresetID == nil || *cl.SourcePresetID != 7 || cl.SourcePresetVersion == nil || *cl.SourcePresetVersion != 2 {
		t.Fatalf("clone did not carry preset provenance: %v/%v", cl.SourcePresetID, cl.SourcePresetVersion)
	}

	ids, err := db.GenerateStyles(base, []domain.StyleVariant{{Name: "GEN-A"}, {Name: "GEN-B"}}, "bob")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, id := range ids {
		g, err := db.GetStyleNodeClaimByNode(id, "PLN_01")
		if err != nil {
			t.Fatalf("get generated claim: %v", err)
		}
		if g.Source != domain.ClaimSourceGenerated || g.CalledBy != "bob" {
			t.Fatalf("generated = source %q called_by %q; want generated/bob", g.Source, g.CalledBy)
		}
	}
}

// TestClaim_DeleteRetiresWhenHistoryReferencesIt: a claim that a
// changeover_node_tasks row points at is RETIRED, not deleted — the history
// label resolves it by id and never renders blank — while every list read
// skips it and the routing backfill ignores it. A claim nothing references is
// deleted outright. Re-adding the same (style, node) claim revives the row
// under the same id, so history keeps pointing at a real row.
func TestClaim_DeleteRetiresWhenHistoryReferencesIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, sid := seedClaimProcess(t, db, "P-Retire")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: "PLN_01", Name: "PLN_01", Enabled: true})
	if err != nil {
		t.Fatalf("CreateProcessNode: %v", err)
	}
	mk := func(node, payload, source string) int64 {
		id, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
			StyleID: sid, CoreNodeName: node, Role: "consume", SwapMode: "two_robot",
			PayloadCode: payload, InboundStaging: "STG", InboundSource: source, OutboundDestination: "SMN_DST",
			Source: domain.ClaimSourceAdmin, CalledBy: "alice",
		})
		if err != nil {
			t.Fatalf("upsert %s: %v", node, err)
		}
		return id
	}
	referenced := mk("PLN_01", "PART-OLD", "SMN_OLD")
	unreferenced := mk("PLN_02", "PART-X", "SMN_X")

	// History: one changeover whose node task came FROM the referenced claim.
	res, err := db.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state) VALUES (?, ?, 'completed')`, pid, sid)
	if err != nil {
		t.Fatalf("insert changeover: %v", err)
	}
	coID, err := res.LastInsertId()
	testutil.MustNoErr(t, err, "res.LastInsertId")
	if _, err := db.Exec(`INSERT INTO changeover_node_tasks (process_changeover_id, process_node_id, from_claim_id, situation, state)
		VALUES (?, ?, ?, 'changed', 'completed')`, coID, nodeID, referenced); err != nil {
		t.Fatalf("insert node task: %v", err)
	}

	if err := db.DeleteStyleNodeClaim(referenced); err != nil {
		t.Fatalf("delete referenced: %v", err)
	}
	if err := db.DeleteStyleNodeClaim(unreferenced); err != nil {
		t.Fatalf("delete unreferenced: %v", err)
	}

	// The referenced row is still there, retired; the label still renders.
	c, err := db.GetStyleNodeClaim(referenced)
	if err != nil {
		t.Fatalf("GetStyleNodeClaim(referenced) after delete: %v — the history label would render blank", err)
	}
	if c.RetiredAt == nil || c.PayloadCode != "PART-OLD" {
		t.Fatalf("retired row = retired_at %v payload %q; want retired with its payload intact", c.RetiredAt, c.PayloadCode)
	}
	// The unreferenced row is gone.
	if _, err := db.GetStyleNodeClaim(unreferenced); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetStyleNodeClaim(unreferenced) after delete: err = %v, want ErrNoRows (nothing referenced it)", err)
	}
	// Every list read skips the retired row.
	claims, err := db.ListStyleNodeClaims(sid)
	testutil.MustNoErr(t, err, "db.ListStyleNodeClaims")
	if len(claims) != 0 {
		t.Fatalf("ListStyleNodeClaims after retire = %+v, want none", claims)
	}
	if _, err := db.GetStyleNodeClaimByNode(sid, "PLN_01"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetStyleNodeClaimByNode on a retired claim: err = %v, want ErrNoRows", err)
	}
	var walked int
	if err := processes.WalkClaims(db.DB, processes.WalkOpts{}, func(processes.WalkCtx) bool { walked++; return false }); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if walked != 0 {
		t.Fatalf("WalkClaims visited %d retired claim(s)", walked)
	}
	// The routing backfill ignores it, and the delete guard no longer holds
	// its source.
	rep, err := db.DeriveRoutingNodesForProcess(pid, nil)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	rows, err := db.ListRoutingNodes(pid)
	testutil.MustNoErr(t, err, "db.ListRoutingNodes")
	if len(rows) != 0 || rep.Claims != 0 {
		t.Fatalf("derive after retire: rows %v, claims %d; want nothing derived from a retired claim", routingKeys(rows), rep.Claims)
	}

	// Re-adding the same claim revives the row: same id, live again.
	revived := mk("PLN_01", "PART-NEW", "SMN_NEW")
	if revived != referenced {
		t.Fatalf("re-adding (style, node) minted a new row %d instead of reviving %d — history now points at a tombstone", revived, referenced)
	}
	c, err = db.GetStyleNodeClaim(referenced)
	testutil.MustNoErr(t, err, "db.GetStyleNodeClaim")
	if c.RetiredAt != nil || c.PayloadCode != "PART-NEW" {
		t.Fatalf("revived row = retired_at %v payload %q; want live with the new payload", c.RetiredAt, c.PayloadCode)
	}
	claims, err = db.ListStyleNodeClaims(sid)
	testutil.MustNoErr(t, err, "db.ListStyleNodeClaims")
	if len(claims) != 1 {
		t.Fatalf("ListStyleNodeClaims after revive = %d, want 1", len(claims))
	}
}
