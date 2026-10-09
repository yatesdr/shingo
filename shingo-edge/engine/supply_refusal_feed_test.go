package engine

import (
	"sort"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
)

// supply_refusal_feed_test.go — ApplySupplyRefusalSnapshot's per-field
// ownership, and the refusals digest on the heartbeat.

// refusalTable renders every open row as "loader/payload by=<by> ack=<choice>/<process>",
// sorted.
func refusalTable(t *testing.T, db *store.DB) []string {
	t.Helper()
	open, err := db.ListOpenSupplyRefusals()
	testutil.MustNoErr(t, err, "list refusals")
	out := []string{}
	for _, r := range open {
		ack := "-"
		if r.Answered() {
			ack = r.AckChoice + "/" + r.AckProcessID
		}
		out = append(out, r.LoaderNode+"/"+r.PayloadCode+" by="+r.RefusedBy+" ack="+ack)
	}
	sort.Strings(out)
	return out
}

// refusalEmits renders the supply.refusal messages on the outbox.
func refusalEmits(t *testing.T, db *store.DB) []string {
	t.Helper()
	out := []string{}
	for _, s := range refusalOutbox(t, db) {
		line := s.Action + " " + s.LoaderNode + "/" + s.PayloadCode
		if s.Action == protocol.SupplyRefusalAcked {
			line += " " + s.AckChoice + " " + s.AckProcessID
		}
		out = append(out, line)
	}
	return out
}

// refusalSeed is one local row: refused by, and an optional ack ("choice/process").
type refusalSeed struct{ loader, payload, by, ack string }

func coreRefusal(loader, payload, by, ack string) protocol.SupplyRefusalState {
	st := protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened, LoaderNode: loader, PayloadCode: payload,
		RefusedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), RefusedBy: by}
	if choice, proc, ok := strings.Cut(ack, "/"); ok {
		at := time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)
		st.AckAt, st.AckChoice, st.AckProcessID = &at, choice, proc
	}
	return st
}

// TestApplySupplyRefusalSnapshot_Ownership drives each ownership rule. The
// Edge is seedLoaderCard's: one loader window (f.core) on process LOAD-PROC.
// LN-OTHER is another Edge's window and OTHER-PROC another Edge's process.
func TestApplySupplyRefusalSnapshot_Ownership(t *testing.T) {
	t.Parallel()
	const own, other, ownProc = "SMN_014", "LN-OTHER", "LOAD-PROC"
	cases := []struct {
		name      string
		local     []refusalSeed
		core      []protocol.SupplyRefusalState
		wantRows  []string
		wantEmits []string
	}{
		{
			name:      "own row Core lacks: kept, Opened re-emitted",
			local:     []refusalSeed{{own, "PART-A", "Bin Loader", ""}},
			wantRows:  []string{own + "/PART-A by=Bin Loader ack=-"},
			wantEmits: []string{"opened " + own + "/PART-A"},
		},
		{
			name:      "own row Core lacks, answered here: kept whole, Opened only (the ack waits for Core's row)",
			local:     []refusalSeed{{own, "PART-A", "Bin Loader", "wait/" + ownProc}},
			wantRows:  []string{own + "/PART-A by=Bin Loader ack=wait/" + ownProc},
			wantEmits: []string{"opened " + own + "/PART-A"},
		},
		{
			name:      "Core row at own window this Edge lacks: stays absent, Closed re-emitted",
			core:      []protocol.SupplyRefusalState{coreRefusal(own, "PART-A", "Bin Loader", "")},
			wantRows:  []string{},
			wantEmits: []string{"closed " + own + "/PART-A"},
		},
		{
			name:      "own ack Core lacks: kept, Acked re-emitted",
			local:     []refusalSeed{{other, "PART-A", "edge.b", "wait/" + ownProc}},
			core:      []protocol.SupplyRefusalState{coreRefusal(other, "PART-A", "edge.b", "")},
			wantRows:  []string{other + "/PART-A by=edge.b ack=wait/" + ownProc},
			wantEmits: []string{"acked " + other + "/PART-A wait " + ownProc},
		},
		{
			name:      "Core's ack on a row this Edge owns is taken",
			local:     []refusalSeed{{own, "PART-A", "Bin Loader", ""}},
			core:      []protocol.SupplyRefusalState{coreRefusal(own, "PART-A", "Bin Loader", "changeover/OTHER-PROC")},
			wantRows:  []string{own + "/PART-A by=Bin Loader ack=changeover/OTHER-PROC"},
			wantEmits: []string{},
		},
		{
			name:      "an ack Core already holds wins over this Edge's own",
			local:     []refusalSeed{{other, "PART-A", "edge.b", "wait/" + ownProc}},
			core:      []protocol.SupplyRefusalState{coreRefusal(other, "PART-A", "edge.b", "changeover/OTHER-PROC")},
			wantRows:  []string{other + "/PART-A by=edge.b ack=changeover/OTHER-PROC"},
			wantEmits: []string{},
		},
		{
			name:      "moot ack: Core closed another Edge's row, the ack goes with it",
			local:     []refusalSeed{{other, "PART-A", "edge.b", "wait/" + ownProc}},
			wantRows:  []string{},
			wantEmits: []string{},
		},
		{
			name:  "another Edge's rows come from Core",
			local: []refusalSeed{{other, "PART-A", "stale", ""}},
			core: []protocol.SupplyRefusalState{
				coreRefusal(other, "PART-A", "edge.b", ""),
				coreRefusal(other, "PART-B", "edge.b", "wait/OTHER-PROC"),
			},
			wantRows: []string{
				other + "/PART-A by=edge.b ack=-",
				other + "/PART-B by=edge.b ack=wait/OTHER-PROC",
			},
			wantEmits: []string{},
		},
		{
			name:      "another Edge's ack Core lacks is not this Edge's to send",
			local:     []refusalSeed{{other, "PART-A", "edge.b", "wait/OTHER-PROC"}},
			core:      []protocol.SupplyRefusalState{coreRefusal(other, "PART-A", "edge.b", "")},
			wantRows:  []string{other + "/PART-A by=edge.b ack=-"},
			wantEmits: []string{},
		},
		{
			name:      "every rule at once, in one apply",
			local:     []refusalSeed{{own, "PART-A", "Bin Loader", ""}, {other, "PART-A", "edge.b", "wait/" + ownProc}, {other, "PART-C", "edge.b", ""}},
			core:      []protocol.SupplyRefusalState{coreRefusal(own, "PART-B", "Bin Loader", ""), coreRefusal(other, "PART-A", "edge.b", "")},
			wantRows:  []string{other + "/PART-A by=edge.b ack=wait/" + ownProc, own + "/PART-A by=Bin Loader ack=-"},
			wantEmits: []string{"opened " + own + "/PART-A", "acked " + other + "/PART-A wait " + ownProc, "closed " + own + "/PART-B"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := seedLoaderCard(t)
			if f.core != own {
				t.Fatalf("fixture window = %q, want %q", f.core, own)
			}
			for _, s := range tc.local {
				testutil.MustNoErr(t, f.db.OpenSupplyRefusal(s.loader, s.payload, s.by), "seed refusal")
				if choice, proc, ok := strings.Cut(s.ack, "/"); ok {
					_, err := f.db.AckSupplyRefusal(s.loader, s.payload, choice, proc)
					testutil.MustNoErr(t, err, "seed ack")
				}
			}
			digest, err := protocol.RefusalsDigest(tc.core)
			testutil.MustNoErr(t, err, "core digest")

			testutil.MustNoErr(t, f.eng.ApplySupplyRefusalSnapshot(protocol.SupplyRefusalSnapshot{Digest: digest, Open: tc.core}), "apply")

			if got := refusalTable(t, f.db); strings.Join(got, "\n") != strings.Join(tc.wantRows, "\n") {
				t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.wantRows, "\n"))
			}
			if got := refusalEmits(t, f.db); strings.Join(got, "\n") != strings.Join(tc.wantEmits, "\n") {
				t.Errorf("re-emitted:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.wantEmits, "\n"))
			}
			// With nothing re-emitted the Edge holds exactly Core's set, so its
			// next heartbeat quotes Core's digest and Core sends nothing more.
			if len(tc.wantEmits) == 0 {
				f.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
				if got := f.eng.FeedDigests()[protocol.FeedRefusals]; got != digest {
					t.Errorf("Edge digest after apply = %q, want Core's %q", got, digest)
				}
			}
		})
	}
}

// TestApplySupplyRefusalSnapshot_ClaimReadFails_FailsClosed: the claim read
// behind this Edge's window set fails, so the apply is refused whole — an error
// back, the table and the outbox exactly as they were — and the next heartbeat
// asks again. Read as "no claim", the failure would drop the window from the
// set, and the Edge's own refusal, which Core lacks, would be deleted.
func TestApplySupplyRefusalSnapshot_ClaimReadFails_FailsClosed(t *testing.T) {
	t.Parallel()
	f := seedLoaderCard(t)
	testutil.MustNoErr(t, f.db.OpenSupplyRefusal(f.core, "PART-A", "Bin Loader"), "seed own refusal")
	snap := protocol.SupplyRefusalSnapshot{Open: []protocol.SupplyRefusalState{coreRefusal("LN-OTHER", "PART-B", "edge.b", "")}}
	var err error
	snap.Digest, err = protocol.RefusalsDigest(snap.Open)
	testutil.MustNoErr(t, err, "core digest")
	rowsBefore, emitsBefore := refusalTable(t, f.db), refusalEmits(t, f.db)

	_, err = f.db.Exec(`ALTER TABLE style_node_claims RENAME TO style_node_claims_hidden`)
	testutil.MustNoErr(t, err, "hide claims")
	if err := f.eng.ApplySupplyRefusalSnapshot(snap); err == nil {
		t.Fatal("apply with the claim read failing returned nil, want the read error")
	}
	if got := refusalTable(t, f.db); strings.Join(got, "\n") != strings.Join(rowsBefore, "\n") {
		t.Errorf("rows after a refused apply:\n%s\nwant unchanged:\n%s", strings.Join(got, "\n"), strings.Join(rowsBefore, "\n"))
	}
	if got := refusalEmits(t, f.db); strings.Join(got, "\n") != strings.Join(emitsBefore, "\n") {
		t.Errorf("outbox after a refused apply:\n%s\nwant unchanged:\n%s", strings.Join(got, "\n"), strings.Join(emitsBefore, "\n"))
	}

	// The read back, the same snapshot applies: the window is this Edge's, so its
	// refusal stays and Core is re-sent the Opened it is missing.
	_, err = f.db.Exec(`ALTER TABLE style_node_claims_hidden RENAME TO style_node_claims`)
	testutil.MustNoErr(t, err, "restore claims")
	testutil.MustNoErr(t, f.eng.ApplySupplyRefusalSnapshot(snap), "apply")
	wantRows := []string{"LN-OTHER/PART-B by=edge.b ack=-", f.core + "/PART-A by=Bin Loader ack=-"}
	if got := refusalTable(t, f.db); strings.Join(got, "\n") != strings.Join(wantRows, "\n") {
		t.Errorf("rows after the retry:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantRows, "\n"))
	}
	if got := refusalEmits(t, f.db); strings.Join(got, ",") != "opened "+f.core+"/PART-A" {
		t.Errorf("outbox after the retry = %v, want the one Opened", got)
	}
}

// TestUndoSupplyRefusal_ClosedOnlyWhenARowWent: Undo emits Closed for the row
// it deleted and nothing for an undo of nothing — a card never refused, or one
// already withdrawn.
func TestUndoSupplyRefusal_ClosedOnlyWhenARowWent(t *testing.T) {
	t.Parallel()
	f := seedLoaderCard(t)

	testutil.MustNoErr(t, f.eng.UndoSupplyRefusal(f.nodeID, "PART-A"), "undo, never refused")
	if got := refusalEmits(t, f.db); len(got) != 0 {
		t.Errorf("undo with no row: outbox = %v, want nothing", got)
	}

	testutil.MustNoErr(t, f.db.OpenSupplyRefusal(f.core, "PART-A", "Bin Loader"), "seed refusal")
	testutil.MustNoErr(t, f.eng.UndoSupplyRefusal(f.nodeID, "PART-A"), "undo")
	testutil.MustNoErr(t, f.eng.UndoSupplyRefusal(f.nodeID, "PART-A"), "undo again")
	if got := refusalEmits(t, f.db); strings.Join(got, ",") != "closed "+f.core+"/PART-A" {
		t.Errorf("undo, undo again: outbox = %v, want one Closed", got)
	}
	if got := refusalTable(t, f.db); len(got) != 0 {
		t.Errorf("rows after undo = %v, want none", got)
	}
}

// TestHandleSupplyRefusalState_OwnWindowExistenceIsLocal: Core's Opened and
// Closed do not change a refusal at this Edge's own window — its table is the
// truth there — while an ack does, and another Edge's window applies as
// before. Found by the versioned-feeds proof (run 6b): an Opened delivered after
// the operator's undo re-created the withdrawn refusal on every side.
func TestHandleSupplyRefusalState_OwnWindowExistenceIsLocal(t *testing.T) {
	t.Parallel()
	f := seedLoaderCard(t)
	const other = "LN-OTHER"
	opened := func(loader string) protocol.SupplyRefusalState {
		return protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened, LoaderNode: loader, PayloadCode: "PART-A", RefusedBy: "edge.b"}
	}
	closed := func(loader string) protocol.SupplyRefusalState {
		return protocol.SupplyRefusalState{Action: protocol.SupplyRefusalClosed, LoaderNode: loader, PayloadCode: "PART-A"}
	}

	f.eng.HandleSupplyRefusalState(opened(f.core))
	if got := refusalTable(t, f.db); len(got) != 0 {
		t.Errorf("a stale Opened at the own window created %v, want nothing", got)
	}
	testutil.MustNoErr(t, f.db.OpenSupplyRefusal(f.core, "PART-A", "Bin Loader"), "own refusal")
	f.eng.HandleSupplyRefusalState(closed(f.core))
	if got := refusalTable(t, f.db); len(got) != 1 {
		t.Errorf("a stale Closed at the own window removed the row: %v", got)
	}
	f.eng.HandleSupplyRefusalState(protocol.SupplyRefusalState{Action: protocol.SupplyRefusalAcked,
		LoaderNode: f.core, PayloadCode: "PART-A", AckChoice: "wait", AckProcessID: "OTHER-PROC"})
	if got := refusalTable(t, f.db); strings.Join(got, ",") != f.core+"/PART-A by=Bin Loader ack=wait/OTHER-PROC" {
		t.Errorf("an ack at the own window: %v, want it answered", got)
	}

	f.eng.HandleSupplyRefusalState(opened(other))
	if got := refusalRow(t, f.db, other, "PART-A"); got != "open by=edge.b ack=-" {
		t.Errorf("another Edge's window: Opened gave %q, want the row", got)
	}
	f.eng.HandleSupplyRefusalState(closed(other))
	if got := refusalRow(t, f.db, other, "PART-A"); got != "none" {
		t.Errorf("another Edge's window: Closed left %q", got)
	}
	if got := refusalEmits(t, f.db); len(got) != 0 {
		t.Errorf("applying broadcasts enqueued %v, want nothing", got)
	}

	// The reorder heals: Core holds the stale row at the own window, which this
	// Edge lacks, so the snapshot merge re-sends Closed.
	testutil.MustNoErr(t, f.eng.UndoSupplyRefusal(f.nodeID, "PART-A"), "undo")
	f.eng.HandleSupplyRefusalState(opened(f.core))
	stale := []protocol.SupplyRefusalState{coreRefusal(f.core, "PART-A", "Bin Loader", "")}
	d, err := protocol.RefusalsDigest(stale)
	testutil.MustNoErr(t, err, "digest")
	testutil.MustNoErr(t, f.eng.ApplySupplyRefusalSnapshot(protocol.SupplyRefusalSnapshot{Digest: d, Open: stale}), "apply")
	if got := refusalEmits(t, f.db); strings.Join(got, ",") != "closed "+f.core+"/PART-A,closed "+f.core+"/PART-A" {
		t.Errorf("after the undo and the snapshot: outbox = %v, want the undo's Closed and the re-sent Closed", got)
	}
	if got := refusalTable(t, f.db); len(got) != 0 {
		t.Errorf("rows after the heal = %v, want none", got)
	}
}

// Mixed versions, Edge side. Against an older Core (an ack with no Feeds map,
// or no ack yet) the heartbeat carries no refusals key and the table is not
// read for it; no snapshot ever arrives, and a refusal travels on its own
// message exactly as before. A failed read leaves the key out rather than
// quoting the digest of nothing.
func TestRefusalsDigest_ByCoreVersionAndReadFailure(t *testing.T) {
	t.Parallel()
	f := seedLoaderCard(t)
	testutil.MustNoErr(t, f.eng.RefuseSupply(f.nodeID, "PART-A", "Bin Loader"), "refuse")

	if _, ok := f.eng.FeedDigests()[protocol.FeedRefusals]; ok {
		t.Error("before any ack: refusals key sent, want it left out")
	}
	f.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{})
	if _, ok := f.eng.FeedDigests()[protocol.FeedRefusals]; ok {
		t.Error("older Core: refusals key sent, want it left out")
	}
	if got := refusalEmits(t, f.db); strings.Join(got, ",") != "opened "+f.core+"/PART-A" {
		t.Errorf("older Core: outbox = %v, want the one Opened", got)
	}

	f.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	open, err := f.db.ListOpenSupplyRefusals()
	testutil.MustNoErr(t, err, "list")
	want, err := protocol.RefusalsDigest(refusalStates(open))
	testutil.MustNoErr(t, err, "digest")
	if got := f.eng.FeedDigests()[protocol.FeedRefusals]; got != want {
		t.Errorf("newer Core: refusals digest = %q, want %q", got, want)
	}
	f.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedRefusals: want}})
	if _, confirmed := f.eng.FeedTimes(protocol.FeedRefusals); confirmed.IsZero() {
		t.Error("an ack quoting the computed digest did not confirm it")
	}

	_, err = f.db.Exec(`ALTER TABLE supply_refusals_open RENAME TO supply_refusals_open_hidden`)
	testutil.MustNoErr(t, err, "hide table")
	if d, ok := f.eng.FeedDigests()[protocol.FeedRefusals]; ok {
		t.Errorf("failed read: refusals key sent (%q), want it left out", d)
	}
}
