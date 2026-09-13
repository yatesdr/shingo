package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/domain/flowspec"
	"shingoedge/internal/testdb"
	"shingoedge/store"
)

// flow_preset_service_test.go — the Presets tab's whole read, against a whole
// plant rather than a hand-built process.
//
// The three things it has to get right are the three the round argued about:
// MEMBERS are the styles whose claims point at the preset (provenance), DRIFT
// is computed by collapsing each member and comparing shapes (never from the
// stored version), and CANDIDATES are the migration offer — one entry per
// distinct shape that matches no active preset, so an engineer naming them all
// ends with a named shape per flow the press actually runs.

func presetFixture(t *testing.T) (*ProcessService, testdb.SeededPlant, *store.DB) {
	t.Helper()
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	testdb.SeedRoutingSet(t, db, seeded.ProcessID, "test")
	return NewProcessService(db), seeded, db
}

// TestPresets_CandidatesAreOneEntryPerDistinctShape is the migration offer.
//
// Plant A's first press runs two shapes across its styles — a press index
// on PLN_01/PLN_04 and a two-robot swap on PLN_03/PLN_06 — and a dozen styles
// spread across them. The offer has to be TWO rows, not a dozen: the part is
// not part of a shape, so every style that runs one shape collapses into one
// candidate. That property is the difference between an offer and a list.
func TestPresets_CandidatesAreOneEntryPerDistinctShape(t *testing.T) {
	t.Parallel()
	svc, seeded, _ := presetFixture(t)
	view, err := svc.PresetsFor(seeded.ProcessID)
	if err != nil {
		t.Fatalf("PresetsFor: %v", err)
	}
	if len(view.Presets) != 0 {
		t.Errorf("a press with no presets listed %d", len(view.Presets))
	}
	if len(view.Candidates) == 0 {
		t.Fatal("no candidate shapes at all — the migration offer would never appear on a press full of flows")
	}
	total := 0
	for _, c := range view.Candidates {
		if len(c.Members) == 0 {
			t.Errorf("candidate %q has no members; a shape with no style behind it is not a candidate", c.SuggestedName)
		}
		if c.ShapeKey == "" {
			t.Errorf("candidate %q carries no shape key, so naming it cannot match its members", c.SuggestedName)
		}
		if len(c.Shape) == 0 {
			t.Errorf("candidate %q carries no cells, so the modal has nothing to save", c.SuggestedName)
		}
		for _, cell := range c.Shape {
			if cell.PayloadCode != "" {
				t.Errorf("candidate %q carries a part (%s on %s) — a preset is a shape",
					c.SuggestedName, cell.PayloadCode, cell.CoreNodeName)
			}
		}
		total += len(c.Members)
	}
	if total < len(view.Candidates) {
		t.Errorf("%d members across %d candidates", total, len(view.Candidates))
	}
	// FEWER CANDIDATES THAN STYLES WITH A FLOW is the whole point.
	withFlow := view.StylesWithFlow
	if withFlow == 0 {
		t.Fatal("no style on the press has a flow; the fixture is not the HK pull")
	}
	if len(view.Candidates) >= withFlow && withFlow > 1 {
		t.Errorf("%d candidates for %d styles with a flow — shapes are not being grouped", len(view.Candidates), withFlow)
	}
	t.Logf("HK P400: %d candidate shapes over %d styles with a flow", len(view.Candidates), withFlow)
	for _, c := range view.Candidates {
		t.Logf("  %-44s used by %d", c.SuggestedName, len(c.Members))
	}
}

// TestPresets_NamingACandidateStampsExactlyItsMembers: the two-column write,
// and every other column byte-equal before and after (the U3 carry-through
// pin's shape).
func TestPresets_NamingACandidateStampsExactlyItsMembers(t *testing.T) {
	t.Parallel()
	svc, seeded, db := presetFixture(t)
	view, err := svc.PresetsFor(seeded.ProcessID)
	if err != nil {
		t.Fatalf("PresetsFor: %v", err)
	}
	cand := view.Candidates[0]

	// BACKDATE FIRST, so updated_at CAN differ.
	//
	// The blobs below hold updated_at, and SQLite's datetime('now') has second
	// resolution — so with the seed and the stamp inside one second a stamp
	// that moved it would compare equal and this pin would pass on the defect
	// it exists to catch. This used to be time.Sleep(1100ms) to cross a whole
	// second; putting the seeded rows an hour into the past buys the same
	// separation for nothing (store_test.go:818's pattern). Verified: with
	// `updated_at = datetime('now')` put back in StampClaimPresetProvenance
	// this test is red, and green without it.
	backdateClaims(t, db, seeded.ProcessID)

	before := map[int64][]byte{}
	for _, sid := range allStyleIDs(t, db, seeded.ProcessID) {
		before[sid] = claimsBlob(t, db, sid)
	}

	id, err := svc.CreateFromCandidate(seeded.ProcessID, "Named by the test", cand.ShapeKey, "s.brown")
	if err != nil {
		t.Fatalf("CreateFromCandidate: %v", err)
	}

	p, err := db.GetFlowPreset(id)
	if err != nil {
		t.Fatalf("GetFlowPreset: %v", err)
	}
	if p.Version != 1 || p.CreatedBy != "s.brown" {
		t.Errorf("preset = v%d by %q, want v1 by s.brown", p.Version, p.CreatedBy)
	}
	if strings.Contains(p.FlowJSON, "payload_code\":\"P") {
		t.Errorf("the stored shape carries a part: %s", p.FlowJSON)
	}

	stamped := map[int64]bool{}
	for _, sid := range allStyleIDs(t, db, seeded.ProcessID) {
		claims, err := db.ListStyleNodeClaims(sid)
		if err != nil {
			t.Fatalf("claims for %d: %v", sid, err)
		}
		any := false
		for _, c := range claims {
			if c.SourcePresetID != nil && *c.SourcePresetID == id {
				any = true
				if c.SourcePresetVersion == nil || *c.SourcePresetVersion != 1 {
					t.Errorf("style %d claim %s: version = %v, want 1", sid, c.CoreNodeName, c.SourcePresetVersion)
				}
			}
		}
		stamped[sid] = any
	}
	for _, sid := range cand.Members {
		if !stamped[sid] {
			t.Errorf("member style %d was not stamped", sid)
		}
	}
	for sid, was := range stamped {
		if was && !containsID(cand.Members, sid) {
			t.Errorf("style %d was stamped and is not a member of the named shape", sid)
		}
	}

	// AND NOTHING ELSE MOVED. Two columns, a dedicated narrow update: the
	// 18-unconditional-column write of updateClaim is not the tool, and this
	// is the pin that says so.
	for _, sid := range allStyleIDs(t, db, seeded.ProcessID) {
		if got := claimsBlob(t, db, sid); string(got) != string(before[sid]) {
			t.Errorf("style %d: a column outside source_preset_id/version changed.\n before %s\n after  %s",
				sid, before[sid], got)
		}
	}
}

// TestPresets_DriftIsComputedFromTruth: a member whose flow is edited after
// the preset was named reads as drifted, and the field the table shows is the
// one that changed. The stored version is not consulted — the claim rows are.
func TestPresets_DriftIsComputedFromTruth(t *testing.T) {
	t.Parallel()
	svc, seeded, db := presetFixture(t)
	view, err := svc.PresetsFor(seeded.ProcessID)
	testutil.MustNoErr(t, err, "svc.PresetsFor")
	cand := view.Candidates[0]
	id, err := svc.CreateFromCandidate(seeded.ProcessID, "Drifty", cand.ShapeKey, "s.brown")
	if err != nil {
		t.Fatalf("CreateFromCandidate: %v", err)
	}

	view, err = svc.PresetsFor(seeded.ProcessID)
	testutil.MustNoErr(t, err, "svc.PresetsFor")
	if len(view.Presets) != 1 {
		t.Fatalf("%d presets after naming one", len(view.Presets))
	}
	if n := len(view.Presets[0].Drifted); n != 0 {
		t.Fatalf("%d members drifted the moment the preset was named: %v", n, view.Presets[0].Drifted)
	}
	if len(view.Presets[0].Members) != len(cand.Members) {
		t.Errorf("members = %d, want the candidate's %d", len(view.Presets[0].Members), len(cand.Members))
	}

	// Change one field on one member, through the store, and ask again.
	victim := cand.Members[0]
	claims, err := db.ListStyleNodeClaims(victim)
	testutil.MustNoErr(t, err, "db.ListStyleNodeClaims")
	in := domain.InputFromClaim(claims[0])
	in.OutboundDestination = "Empty Tote Return"
	if _, err := db.UpsertStyleNodeClaim(in); err != nil {
		t.Fatalf("edit the member: %v", err)
	}

	view, err = svc.PresetsFor(seeded.ProcessID)
	testutil.MustNoErr(t, err, "svc.PresetsFor")
	p := view.Presets[0]
	if len(p.Drifted) != 1 || p.Drifted[0].StyleID != victim {
		t.Fatalf("drifted = %+v, want exactly style %d", p.Drifted, victim)
	}
	// THE WORD IS flowspec's, not this package's. A field is called what the
	// claim calls it (owner ruling F3) and there is one table of those words,
	// so the sentence here is the same one a refusal about the same field
	// would use.
	want := flowspec.Label(flowspec.OutboundDestination)
	if len(p.Drifted[0].Fields) == 0 || !strings.Contains(p.Drifted[0].Fields[0], want) {
		t.Errorf("the first differing field reads %v, want the position and %q", p.Drifted[0].Fields, want)
	}
	// The provenance still points at the preset — drift is not a broken link.
	after, err := db.ListStyleNodeClaims(victim)
	testutil.MustNoErr(t, err, "db.ListStyleNodeClaims")
	if after[0].SourcePresetID == nil || *after[0].SourcePresetID != id {
		t.Error("editing a member dropped its provenance; drift is computed, not recorded")
	}

	// AND THE DRIFTED MEMBER IS NOT BACK ON OFFER. Its shape now matches no
	// active preset, so the letter of §1a would list it under FOUND IN YOUR
	// FLOWS — with a suggested name built from the mode and the positions,
	// which is to say the name the preset it drifted from already has, because
	// `outbound destination` is not a field a suggested name is built from.
	//
	// Naming that row would write version 2 of the preset holding a DIFFERENT
	// shape under a name that means the first one. The offer is for parts
	// nobody has migrated; this one has been migrated and has since drifted,
	// and its control is the preset's own `Apply to parts…`.
	for _, c := range view.Candidates {
		for _, m := range c.Members {
			if m == victim {
				t.Errorf("the drifted member is on offer as %q — the same name as the preset it drifted from",
					c.SuggestedName)
			}
		}
	}
}

// TestPresets_ArchiveHidesItAndKeepsProvenance.
func TestPresets_ArchiveHidesItAndKeepsProvenance(t *testing.T) {
	t.Parallel()
	svc, seeded, db := presetFixture(t)
	view, err := svc.PresetsFor(seeded.ProcessID)
	testutil.MustNoErr(t, err, "svc.PresetsFor")
	id, err := svc.CreateFromCandidate(seeded.ProcessID, "Doomed", view.Candidates[0].ShapeKey, "s.brown")
	if err != nil {
		t.Fatalf("CreateFromCandidate: %v", err)
	}
	members := view.Candidates[0].Members
	if err := svc.Archive(id); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	view, err = svc.PresetsFor(seeded.ProcessID)
	testutil.MustNoErr(t, err, "svc.PresetsFor")
	for _, p := range view.Presets {
		if p.ID == id {
			t.Error("an archived preset is still listed")
		}
	}
	claims, err := db.ListStyleNodeClaims(members[0])
	testutil.MustNoErr(t, err, "db.ListStyleNodeClaims")
	kept := false
	for _, c := range claims {
		if c.SourcePresetID != nil && *c.SourcePresetID == id {
			kept = true
		}
	}
	if !kept {
		t.Error("archiving dropped a member's provenance; the row stays because a claim points at it")
	}
	// The shape is a candidate again, because no ACTIVE preset matches it.
	if len(view.Candidates) == 0 {
		t.Error("after archiving the only preset, its shape is not offered again")
	}
}

// TestPresets_CreateFromStyleStripsThePart: the other door into create.
func TestPresets_CreateFromStyleStripsThePart(t *testing.T) {
	t.Parallel()
	svc, seeded, db := presetFixture(t)
	sid := allStyleIDs(t, db, seeded.ProcessID)[0]
	id, err := svc.CreateFromStyle(seeded.ProcessID, sid, "From a style", "s.brown")
	if err != nil {
		t.Fatalf("CreateFromStyle: %v", err)
	}
	p, err := db.GetFlowPreset(id)
	testutil.MustNoErr(t, err, "db.GetFlowPreset")
	cells, err := domain.ParsePresetShape(p.FlowJSON)
	if err != nil {
		t.Fatalf("the stored preset does not parse as a preset: %v", err)
	}
	for _, c := range cells {
		if c.PayloadCode != "" {
			t.Errorf("%s kept its part %q", c.CoreNodeName, c.PayloadCode)
		}
	}
	// A second preset of the same name is version 2.
	id2, err := svc.CreateFromStyle(seeded.ProcessID, sid, "From a style", "s.brown")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	p2, err := db.GetFlowPreset(id2)
	testutil.MustNoErr(t, err, "db.GetFlowPreset")
	if p2.Version != 2 {
		t.Errorf("second preset of the same name is v%d, want v2", p2.Version)
	}
}

// backdateClaims puts every claim of a process an hour into the past.
//
// SQLite's datetime('now') has second resolution, so a test that seeds rows and
// then asserts a write did NOT move updated_at cannot tell "unchanged" from
// "changed within the same second". Separating them by an hour costs nothing;
// the alternative is sleeping past a second boundary, which costs a second of
// every gate run for the life of the test.
func backdateClaims(t *testing.T, db *store.DB, processID int64) {
	t.Helper()
	old := time.Now().UTC().Add(-1 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.Exec(`UPDATE style_node_claims SET updated_at=? WHERE style_id IN (
		SELECT id FROM styles WHERE process_id=?)`, old, processID); err != nil {
		t.Fatalf("backdate claims: %v", err)
	}
}

func allStyleIDs(t *testing.T, db *store.DB, processID int64) []int64 {
	t.Helper()
	styles, err := db.ListStylesByProcess(processID)
	if err != nil {
		t.Fatalf("list styles: %v", err)
	}
	out := make([]int64, 0, len(styles))
	for _, s := range styles {
		out = append(out, s.ID)
	}
	return out
}

// claimsBlob is every column of every claim of one style, as bytes, with the
// two provenance columns blanked — so a diff of it before and after a stamp is
// "did anything ELSE move".
//
// updated_at IS COMPARED, and that is the point of the change that made it
// possible. The stamp used to move it, the way every other write here does, and
// this helper blanked it to get on with comparing the rest. But updated_at has
// one reader — flowProvenance, which turns it into the operator's set-up card
// sentence `Flow saved 09-12 from the desktop` — so naming a candidate on a
// press with ninety parts moved ninety of those dates to today and told the
// floor an engineer had changed ninety flows. The stamp leaves it alone now
// (store/processes/claims.go), so this helper can hold it like any other column
// and a stamp that starts moving it again is red here.
func claimsBlob(t *testing.T, db *store.DB, styleID int64) []byte {
	t.Helper()
	claims, err := db.ListStyleNodeClaims(styleID)
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	var b strings.Builder
	for _, c := range claims {
		c.SourcePresetID, c.SourcePresetVersion = nil, nil
		blob, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal claim: %v", err)
		}
		b.Write(blob)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
