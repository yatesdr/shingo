package messaging

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/catalog"
	"shingoedge/store/processes"
)

// countingPublisherDB opens a store whose statements are counted, so a test
// can pin how many queries a publish issues rather than how long it takes.
func countingPublisherDB(t *testing.T) (*store.DB, *store.QueryCounter) {
	t.Helper()
	db, counter, err := store.OpenCounting(filepath.Join(t.TempDir(), "claims.db"))
	if err != nil {
		t.Fatalf("OpenCounting: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, counter
}

// seedClaimsProcess creates a process with `styles` live styles, each carrying
// `claims` produce claims on nodes N-0..N-(claims-1), and returns the process
// id and the style ids in creation (= name) order.
func seedClaimsProcess(t *testing.T, db *store.DB, name string, styles, claims int) (int64, []int64) {
	t.Helper()
	pid, err := db.CreateProcess(name, "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process %s: %v", name, err)
	}
	ids := make([]int64, 0, styles)
	for i := 0; i < styles; i++ {
		sid, err := db.CreateStyle(fmt.Sprintf("%s-S-%03d", name, i), "", pid)
		if err != nil {
			t.Fatalf("create style: %v", err)
		}
		ids = append(ids, sid)
		for j := 0; j < claims; j++ {
			if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
				StyleID:      sid,
				CoreNodeName: fmt.Sprintf("N-%d", j),
				Role:         protocol.ClaimRoleProduce,
				SwapMode:     protocol.SwapModeSequential,
				PayloadCode:  fmt.Sprintf("%s-P-%03d-%d", name, i, j),
				UOPCapacity:  100,
			}); err != nil {
				t.Fatalf("upsert claim: %v", err)
			}
		}
	}
	return pid, ids
}

// decodeReport unwraps one built payload (an encoded envelope) into the
// PlantClaimsReport it carries.
func decodeReport(t *testing.T, payload []byte) protocol.PlantClaimsReport {
	t.Helper()
	var env protocol.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var data protocol.Data
	if err := env.DecodePayload(&data); err != nil {
		t.Fatalf("decode data wrapper: %v", err)
	}
	if data.Subject != protocol.SubjectPlantClaims {
		t.Fatalf("data subject = %q, want %q", data.Subject, protocol.SubjectPlantClaims)
	}
	var report protocol.PlantClaimsReport
	if err := json.Unmarshal(data.Body, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	return report
}

// buildProcess must not scale with the process's style count. Springfield's
// Press 4 and Press 6 will each carry 40-90 styles on a store pinned to ONE
// connection, and a per-style ListClaims inside the loop was ~(1 + styles)
// queries per process — every one of them serialising the operator station
// behind it.
//
// Counted, not timed: the query count is what the single connection feels,
// and it does not vary with the CI runner's load.
func TestPlantClaimsPublisher_BuildProcessQueryCountIsConstant(t *testing.T) {
	t.Parallel()
	db, counter := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "plant-a.line-1")

	pid, _ := seedClaimsProcess(t, db, "SMALL", 2, 2)
	proc, err := processes.Get(db.DB, pid)
	if err != nil {
		t.Fatalf("get process: %v", err)
	}
	counter.Reset()
	if _, err := p.buildProcess(*proc); err != nil {
		t.Fatalf("build small: %v", err)
	}
	small := counter.Count()

	// Six more styles on the same process. Same shape, four times the styles.
	for i := 2; i < 8; i++ {
		sid, err := db.CreateStyle(fmt.Sprintf("SMALL-S-%03d", i), "", pid)
		if err != nil {
			t.Fatalf("create style: %v", err)
		}
		for j := 0; j < 2; j++ {
			if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
				StyleID: sid, CoreNodeName: fmt.Sprintf("N-%d", j),
				Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSequential,
				PayloadCode: fmt.Sprintf("SMALL-P-%03d-%d", i, j), UOPCapacity: 100,
			}); err != nil {
				t.Fatalf("upsert claim: %v", err)
			}
		}
	}
	counter.Reset()
	data, err := p.buildProcess(*proc)
	if err != nil {
		t.Fatalf("build large: %v", err)
	}
	large := counter.Count()

	if large != small {
		t.Errorf("buildProcess queries: 2 styles = %d, 8 styles = %d — the count must not "+
			"grow with the style count (a per-style claim read on a one-connection store "+
			"is a hang at 90 styles)", small, large)
	}
	if got := decodeReport(t, data); len(got.Styles) != 8 {
		t.Errorf("report styles = %d, want 8", len(got.Styles))
	}
}

// The one-query rewrite of buildProcess must produce the report the per-style
// walk produced: styles in name order, each style's claims in (sequence,
// core_node_name) order, manual_swap excluded, the active style flagged, and
// a retired style absent. Core stores the wire order as Seq, so a reordering
// here would churn its mirror on every publish.
func TestPlantClaimsPublisher_BuildProcessReportShape(t *testing.T) {
	t.Parallel()
	db, _ := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "plant-a.line-1")

	pid, ids := seedClaimsProcess(t, db, "SHAPE", 3, 2)
	// A loader claim on style 0 — must be excluded from the wire.
	//
	// SEEDED AS A LEGACY ROW, because manual_swap is retired as a PERSISTED
	// mode: Core owns loader configuration and SynthClaim serves it, so
	// UpsertStyleNodeClaim's allowlist refuses one. The READ paths that
	// tolerate a stored row are still live — this publisher is one of them, and
	// excluding such a row from the wire is what this test is about — so the
	// row goes in the way the engine's own seam puts one in
	// (engine/legacy_claim_seed_test.go's upsertClaimRetiredMode): a
	// configurable placeholder past the allowlist, then the real mode written
	// directly.
	loaderID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: ids[0], CoreNodeName: "LOADER-1", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSequential, PayloadCode: "LOADER-PART",
		PairedCoreNode: "LOADER-B", InboundSource: "IN", OutboundDestination: "OUT",
		AllowedPayloadCodes: []string{"X"},
	})
	if err != nil {
		t.Fatalf("loader claim: %v", err)
	}
	if _, err := db.DB.Exec(`UPDATE style_node_claims SET swap_mode=? WHERE id=?`,
		string(protocol.SwapModeManualSwap), loaderID); err != nil {
		t.Fatalf("store the retired mode: %v", err)
	}
	// Style 1's wire order is (sequence, core_node_name), not insertion order:
	// add A-FIRST last (it takes sequence 3), then push the seeded N-1
	// (sequence 2) out to 9 through the update path. Insertion order would say
	// N-0, N-1, A-FIRST; sequence order says N-0, A-FIRST, N-1.
	// The capacity the wire must carry lives in the catalog, not on the claim:
	// the publisher ships the resolved number, so this row is what makes the
	// assertion below about resolution reaching the wire at all.
	if err := db.UpsertPayloadCatalog(&catalog.CatalogEntry{ID: 1, Name: "First", Code: "FIRST", UOPCapacity: 10}); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: ids[1], CoreNodeName: "A-FIRST", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSequential, PayloadCode: "FIRST",
		// Required at save for sequential since flowspec D4; this test is about
		// the wire's ORDER, so the seed satisfies the mode.
		PairedCoreNode: "A-SECOND", InboundSource: "IN", OutboundDestination: "OUT",
	}); err != nil {
		t.Fatalf("late claim: %v", err)
	}
	nine := 9
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: ids[1], CoreNodeName: "N-1", Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeSequential, PayloadCode: "SHAPE-P-001-1", UOPCapacity: 100,
		Sequence: &nine,
	}); err != nil {
		t.Fatalf("resequence N-1: %v", err)
	}
	if err := db.SetActiveStyle(pid, &ids[1]); err != nil {
		t.Fatalf("set active: %v", err)
	}
	// Retire style 2: it must vanish from the report.
	if err := db.DeleteStyle(ids[2]); err != nil {
		t.Fatalf("retire style: %v", err)
	}

	proc, err := processes.Get(db.DB, pid)
	if err != nil {
		t.Fatalf("get process: %v", err)
	}
	data, err := p.buildProcess(*proc)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got := decodeReport(t, data)

	if got.ProcessID != "SHAPE" {
		t.Errorf("ProcessID = %q, want SHAPE", got.ProcessID)
	}
	if len(got.Styles) != 2 {
		t.Fatalf("styles = %d, want 2 (the retired one must be absent): %+v", len(got.Styles), got.Styles)
	}
	s0, s1 := got.Styles[0], got.Styles[1]
	if s0.StyleID != "SHAPE-S-000" || s1.StyleID != "SHAPE-S-001" {
		t.Errorf("style order = [%s %s], want name order [SHAPE-S-000 SHAPE-S-001]", s0.StyleID, s1.StyleID)
	}
	if s0.Active || !s1.Active {
		t.Errorf("Active flags = [%v %v], want [false true]", s0.Active, s1.Active)
	}
	if len(s0.Claims) != 2 {
		t.Errorf("style 0 claims = %d, want 2 — the manual_swap loader claim must be excluded: %+v", len(s0.Claims), s0.Claims)
	}
	if len(s1.Claims) != 3 || s1.Claims[0].CoreNodeName != "N-0" ||
		s1.Claims[1].CoreNodeName != "A-FIRST" || s1.Claims[2].CoreNodeName != "N-1" {
		t.Fatalf("style 1 claim order = %v, want [N-0 A-FIRST N-1] (sequence, then core_node_name — not insertion order)", claimNodes(s1.Claims))
	}
	late := s1.Claims[1]
	if late.PayloadCode != "FIRST" || late.UOPCapacity != 10 || late.Role != protocol.ClaimRoleConsume ||
		len(late.AllowedPayloadCodes) != 1 || late.AllowedPayloadCodes[0] != "FIRST" {
		t.Errorf("claim fields did not survive the rewrite: %+v", late)
	}
}

func claimNodes(claims []protocol.PlantClaim) []string {
	out := make([]string, len(claims))
	for i, c := range claims {
		out[i] = c.CoreNodeName
	}
	return out
}

// plant_claims_publisher_test.go — the snapshot cadence is a safety net, not a
// delivery mechanism.
//
// At 5 minutes this publisher emitted ~65 messages an hour at Springfield (one
// per process, twelve times an hour) for plant config that changes a few times
// a shift, and it became 66% of everything Core discarded for expiry — 181 a
// day, each carrying config identical to the snapshot before it.
//
// Raising the interval is only safe because the change-driven paths are
// complete: requestSpecChangePublish fires on every style and claim mutation
// (apiCreateStyle and apiUpdateStyle were missing it until 2026-08-22, which
// the 5-minute timer had been masking), and SubjectEdgeRegistered republishes a
// full snapshot on every register — including the re-register Core asks for
// after it restarts. If any of that regresses, this interval is how long a
// plant runs on a stale mirror, so the number is pinned here deliberately.
func TestPlantClaimsPublisher_SnapshotIntervalIsTheSafetyNet(t *testing.T) {
	t.Parallel()

	p := NewPlantClaimsPublisher(nil, "plant-a.line-1")

	if p.snapshotInterval != 60*time.Minute {
		t.Errorf("snapshotInterval = %v, want 60m — this is the LAST-RESORT catch "+
			"for a change whose publish was lost outright, not the way changes "+
			"normally reach Core", p.snapshotInterval)
	}

	// Guard the direction, not just the value: anything at or under the old
	// 5-minute cadence means someone has reverted to broadcasting config on a
	// timer, which is the behaviour that filled Core's expiry bin.
	if p.snapshotInterval <= 5*time.Minute {
		t.Errorf("snapshotInterval is back to timer-driven broadcasting (%v)", p.snapshotInterval)
	}
}

// unsentClaimsPayloads returns the unsent plant.claims outbox rows as
// (id, payload) pairs in id order.
func unsentClaimsPayloads(t *testing.T, db *store.DB) (ids []int64, payloads [][]byte) {
	t.Helper()
	msgs, err := db.ListUnsentOutboxByType([]string{protocol.SubjectPlantClaims})
	if err != nil {
		t.Fatalf("list unsent: %v", err)
	}
	for _, m := range msgs {
		ids = append(ids, m.ID)
		payloads = append(payloads, m.Payload)
	}
	return ids, payloads
}

// A claim edit on process A enqueues exactly ONE plant.claims message, for A,
// and leaves an unsent snapshot row for process B untouched.
//
// The second half is the trap. plant.claims is a coalescable subject:
// EnqueueSnapshot deletes every unsent row of the type before inserting, on
// the argument that each message is a complete snapshot of the plant. A
// single-process message is complete for ONE process, so routing it through
// EnqueueSnapshot would delete the pending snapshot's other processes and
// Core would come up mirroring only the process that was edited.
func TestPlantClaimsPublisher_ChangedPublishesOnlyThatProcess(t *testing.T) {
	t.Parallel()
	db, _ := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "plant-a.line-1")

	pidA, idsA := seedClaimsProcess(t, db, "PRESS-4", 3, 2)
	_, _ = seedClaimsProcess(t, db, "PRESS-6", 2, 2)

	// A pending full snapshot: one unsent row per process, as boot and the
	// periodic timer leave it while Kafka is down.
	if err := p.PublishAll(); err != nil {
		t.Fatalf("PublishAll: %v", err)
	}
	beforeIDs, beforePayloads := unsentClaimsPayloads(t, db)
	if len(beforeIDs) != 2 {
		t.Fatalf("pending rows after PublishAll = %d, want 2 (one per process)", len(beforeIDs))
	}
	maxBefore := beforeIDs[len(beforeIDs)-1]

	// The edit: a new claim on A's first style.
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: idsA[0], CoreNodeName: "N-NEW", Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeSequential, PayloadCode: "NEW-PAYLOAD", UOPCapacity: 5,
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	if err := p.PublishChanged(pidA); err != nil {
		t.Fatalf("PublishChanged: %v", err)
	}

	afterIDs, afterPayloads := unsentClaimsPayloads(t, db)
	var survived int
	var fresh [][]byte
	for i, id := range afterIDs {
		if id > maxBefore {
			fresh = append(fresh, afterPayloads[i])
		} else {
			survived++
		}
	}
	if survived != len(beforeIDs) {
		t.Errorf("pending snapshot rows after PublishChanged = %d, want %d — a single-process "+
			"publish must not supersede the pending full snapshot (it is complete for one "+
			"process, not the plant)", survived, len(beforeIDs))
	}
	if len(fresh) != 1 {
		t.Fatalf("new plant.claims rows = %d, want exactly 1", len(fresh))
	}
	got := decodeReport(t, fresh[0])
	if got.ProcessID != "PRESS-4" {
		t.Errorf("new row is for %q, want PRESS-4", got.ProcessID)
	}
	found := false
	for _, st := range got.Styles {
		for _, c := range st.Claims {
			if st.StyleID == "PRESS-4-S-000" && c.CoreNodeName == "N-NEW" && c.PayloadCode == "NEW-PAYLOAD" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the edited claim is not in the published report: %+v", got.Styles)
	}
	// And the surviving rows still include B's snapshot, intact.
	var sawB bool
	for _, payload := range beforePayloads {
		if decodeReport(t, payload).ProcessID == "PRESS-6" {
			sawB = true
		}
	}
	if !sawB {
		t.Errorf("no pending row for PRESS-6 among the surviving snapshot rows")
	}
}

// PublishChanged(A) and PublishAll must agree on A: they are one builder, so
// the report Core receives for a process is the same whichever path sent it.
// This replaces a test that proved only that the two shared a nil-DB failure;
// this one proves they share the content.
func TestPlantClaimsPublisher_ChangedMatchesAllForThatProcess(t *testing.T) {
	t.Parallel()
	db, _ := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "plant-a.line-1")

	pidA, idsA := seedClaimsProcess(t, db, "PRESS-4", 3, 2)
	if err := db.SetActiveStyle(pidA, &idsA[1]); err != nil {
		t.Fatalf("set active: %v", err)
	}
	_, _ = seedClaimsProcess(t, db, "PRESS-6", 2, 1)

	if err := p.PublishAll(); err != nil {
		t.Fatalf("PublishAll: %v", err)
	}
	_, payloads := unsentClaimsPayloads(t, db)
	var fromAll *protocol.PlantClaimsReport
	for _, payload := range payloads {
		if r := decodeReport(t, payload); r.ProcessID == "PRESS-4" {
			fromAll = &r
		}
	}
	if fromAll == nil {
		t.Fatalf("PublishAll produced no PRESS-4 report")
	}

	if err := p.PublishChanged(pidA); err != nil {
		t.Fatalf("PublishChanged: %v", err)
	}
	_, payloads = unsentClaimsPayloads(t, db)
	fromChanged := decodeReport(t, payloads[len(payloads)-1])

	if !reflect.DeepEqual(*fromAll, fromChanged) {
		t.Fatalf("PublishChanged and PublishAll disagree on PRESS-4:\n all:     %+v\n changed: %+v", *fromAll, fromChanged)
	}
	if !fromChanged.Styles[1].Active {
		t.Errorf("the active style must be flagged on the single-process path: %+v", fromChanged.Styles)
	}
}

// ── THE CLAIM'S LEGS, AND WHETHER THEY REACH CORE ─────────────────────────
//
// An Edge NodeClaim names four nodes besides its own: where inbound material
// is picked up from (InboundSource), where outbound is dropped off to
// (OutboundDestination), and the paired positions of a two-robot cell
// (PairedCoreNode, SecondPairedCoreNode). Together they are the arcs of the
// material loop through the cell — the thing a loop compiler on Core has to
// read to know that this press pulls from that supermarket.
//
// They used to stop here. protocol.PlantClaim carried the sourceability subset
// only, so the four were dropped at the wire and Core's mirror had no column
// for them — nothing broken, the feed answered the question it was built for,
// but it is why the loop compiler had nothing to read. They cross now, and this
// test is what says they arrive with the values the Edge row holds rather than
// with plausible ones.
func TestPlantClaimsPublisher_ClaimLegsOnTheWire(t *testing.T) {
	t.Parallel()
	db, _ := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "plant-a.line-1")

	pid, ids := seedClaimsProcess(t, db, "LEGS", 1, 0)
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: ids[0], CoreNodeName: "PLN_002", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeTwoRobotPressIndex, PayloadCode: "SYN-PART-A",
		UOPCapacity: 120, ReorderPoint: 30,
		// The four legs, all set, which is the heaviest shape a press cell
		// configures: a three-position index that draws from one synthetic
		// supermarket and ships to another.
		InboundSource: "SYN-SMN_001", OutboundDestination: "SYN-SMN_002",
		PairedCoreNode: "PLN_003", SecondPairedCoreNode: "PLN_004",
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}

	proc, err := processes.Get(db.DB, pid)
	if err != nil {
		t.Fatalf("get process: %v", err)
	}
	data, err := p.buildProcess(*proc)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got := decodeReport(t, data)
	if len(got.Styles) != 1 || len(got.Styles[0].Claims) != 1 {
		t.Fatalf("report = %+v, want one style with one claim", got.Styles)
	}
	claim := got.Styles[0].Claims[0]

	// The Edge row is the authority for what the legs are; read it back so the
	// assertion below compares the wire against the SPEC rather than against
	// the literals this test typed.
	stored, err := processes.ListLiveClaimsByProcess(db.DB, pid)
	if err != nil {
		t.Fatalf("read the stored claim back: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored claims = %d, want 1", len(stored))
	}
	src := stored[0]
	if src.InboundSource == "" || src.OutboundDestination == "" ||
		src.PairedCoreNode == "" || src.SecondPairedCoreNode == "" {
		t.Fatalf("the Edge row did not keep the legs, so this test proves nothing about the wire: %+v", src)
	}

	assertPublishedLegs(t, claim, src)
}

// assertPublishedLegs compares the wire claim against the STORED claim, field
// by field, rather than against the literals the test typed. The publisher's
// job is to carry what the spec says; a test that re-typed the expected values
// would still pass if the publisher started reading the wrong column, as long
// as both readings happened to be written by the same seed.
func assertPublishedLegs(t *testing.T, claim protocol.PlantClaim, src processes.NodeClaim) {
	t.Helper()
	if claim.CoreNodeName != src.CoreNodeName || claim.PayloadCode != src.PayloadCode ||
		claim.UOPCapacity != src.UOPCapacity || claim.ReorderPoint != src.ReorderPoint {
		t.Errorf("wire claim = %+v, want the sourceability subset of %+v", claim, src)
	}
	for _, leg := range []struct{ what, got, want string }{
		{"inbound_source", claim.InboundSource, src.InboundSource},
		{"outbound_destination", claim.OutboundDestination, src.OutboundDestination},
		{"paired_core_node", claim.PairedCoreNode, src.PairedCoreNode},
		{"second_paired_core_node", claim.SecondPairedCoreNode, src.SecondPairedCoreNode},
	} {
		if leg.got != leg.want {
			t.Errorf("wire %s = %q, want the stored claim's %q — the legs are the loop "+
				"compiler's only source of the arcs between nodes, so a leg that does not "+
				"cross is an arc Core cannot know about", leg.what, leg.got, leg.want)
		}
	}
}

// A claim with NO legs configured must publish no leg keys at all. This is the
// fleet's case and the old-Edge case in one: omitempty is what keeps the
// message the same size it has always been for the claims — most of them, at
// most plants — that name no source, no destination and no partner.
func TestPlantClaimsPublisher_ClaimWithNoLegsPublishesNoLegKeys(t *testing.T) {
	t.Parallel()
	db, _ := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "plant-a.line-1")

	pid, _ := seedClaimsProcess(t, db, "NOLEGS", 1, 1)
	proc, err := processes.Get(db.DB, pid)
	if err != nil {
		t.Fatalf("get process: %v", err)
	}
	data, err := p.buildProcess(*proc)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got := decodeReport(t, data)
	if len(got.Styles) != 1 || len(got.Styles[0].Claims) != 1 {
		t.Fatalf("report = %+v, want one style with one claim", got.Styles)
	}
	encoded, err := json.Marshal(got.Styles[0].Claims[0])
	if err != nil {
		t.Fatalf("marshal the wire claim: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("unmarshal the wire claim into a key map: %v", err)
	}
	for _, leg := range []string{"inbound_source", "outbound_destination", "paired_core_node", "second_paired_core_node"} {
		if _, present := keys[leg]; present {
			t.Errorf("a claim with no legs configured still publishes %q. Every claim at every "+
				"plant is sent on every spec edit and every hourly snapshot, from a Pi — the "+
				"unconfigured case has to stay free.", leg)
		}
	}
}
