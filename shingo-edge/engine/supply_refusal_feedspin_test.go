package engine

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// supply_refusal_feedspin_test.go — what the Edge does with supply refusals
// today: the three apply arms of Core's broadcast, the message each operator
// verb puts on the outbox, clear-on-LOAD, and the lost-ack sequence nothing
// heals. Also GetContainment's handling of a non-200.
//
// Each case asserts the base value; `after` is the predicted value and `label`
// the brief label that moves it ("same" where the label keeps it).

// refusalOutbox decodes every supply.refusal message on the outbox, in enqueue
// order.
func refusalOutbox(t *testing.T, db *store.DB) []protocol.SupplyRefusalState {
	t.Helper()
	msgs, err := db.ListUnsentOutboxByType([]string{protocol.SubjectSupplyRefusal})
	testutil.MustNoErr(t, err, "read outbox")
	var out []protocol.SupplyRefusalState
	for _, m := range msgs {
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "decode envelope")
		var d protocol.Data
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &d), "decode data wrapper")
		if d.Subject != protocol.SubjectSupplyRefusal {
			t.Fatalf("outbox row typed %s carries subject %q", protocol.SubjectSupplyRefusal, d.Subject)
		}
		var st protocol.SupplyRefusalState
		testutil.MustNoErr(t, json.Unmarshal(d.Body, &st), "decode state")
		out = append(out, st)
	}
	return out
}

// refusalRow renders the open row for a card as one comparable string:
// "none", or "open by=<refused_by> ack=<choice>/<process>".
func refusalRow(t *testing.T, db *store.DB, loader, payload string) string {
	t.Helper()
	r, err := db.GetSupplyRefusal(loader, payload)
	if errors.Is(err, store.ErrNoOpenRefusal) {
		return "none"
	}
	testutil.MustNoErr(t, err, "get refusal")
	ack := "-"
	if r.Answered() {
		ack = r.AckChoice + "/" + r.AckProcessID
	}
	return "open by=" + r.RefusedBy + " ack=" + ack
}

// TestHandleSupplyRefusalState_Arms_FeedsPin pins the three apply arms of
// HandleSupplyRefusalState and its two ignores. Every arm is idempotent and
// keyed (loader, payload); Opened never writes ack fields even when the message
// carries them, Acked answers only an open unanswered row, Closed deletes.
//
// after: every arm the same; F3 adds a separate snapshot apply beside it.
func TestHandleSupplyRefusalState_Arms_FeedsPin(t *testing.T) {
	t.Parallel()
	const loader, payload = "FP-SMN-1", "FP-PART"
	opened := protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened, LoaderNode: loader, PayloadCode: payload, RefusedBy: "Edge A"}
	cases := []struct {
		name  string
		seed  []protocol.SupplyRefusalState
		apply protocol.SupplyRefusalState
		want  string
		after string
		label string
	}{
		{
			name:  "Opened writes the row",
			apply: opened,
			want:  "open by=Edge A ack=-", after: "same", label: "F3",
		},
		{
			name: "Opened ignores ack fields it carries",
			apply: protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened, LoaderNode: loader, PayloadCode: payload,
				RefusedBy: "Edge A", AckChoice: "wait", AckProcessID: "PROC-B"},
			want: "open by=Edge A ack=-", after: "same", label: "F3",
		},
		{
			name:  "Opened over an open row leaves it (first refused_by stands)",
			seed:  []protocol.SupplyRefusalState{opened},
			apply: protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened, LoaderNode: loader, PayloadCode: payload, RefusedBy: "Edge Z"},
			want:  "open by=Edge A ack=-", after: "same", label: "F3",
		},
		{
			name:  "Acked answers the open row",
			seed:  []protocol.SupplyRefusalState{opened},
			apply: protocol.SupplyRefusalState{Action: protocol.SupplyRefusalAcked, LoaderNode: loader, PayloadCode: payload, AckChoice: "wait", AckProcessID: "PROC-B"},
			want:  "open by=Edge A ack=wait/PROC-B", after: "same", label: "F3",
		},
		{
			name: "Acked on an answered row keeps the first answer",
			seed: []protocol.SupplyRefusalState{opened,
				{Action: protocol.SupplyRefusalAcked, LoaderNode: loader, PayloadCode: payload, AckChoice: "wait", AckProcessID: "PROC-B"}},
			apply: protocol.SupplyRefusalState{Action: protocol.SupplyRefusalAcked, LoaderNode: loader, PayloadCode: payload, AckChoice: "changeover", AckProcessID: "PROC-C"},
			want:  "open by=Edge A ack=wait/PROC-B", after: "same", label: "F3",
		},
		{
			name:  "Acked with no open row writes nothing",
			apply: protocol.SupplyRefusalState{Action: protocol.SupplyRefusalAcked, LoaderNode: loader, PayloadCode: payload, AckChoice: "wait", AckProcessID: "PROC-B"},
			want:  "none", after: "same", label: "F3",
		},
		{
			name:  "Closed deletes the row and its ack",
			seed:  []protocol.SupplyRefusalState{opened, {Action: protocol.SupplyRefusalAcked, LoaderNode: loader, PayloadCode: payload, AckChoice: "wait", AckProcessID: "PROC-B"}},
			apply: protocol.SupplyRefusalState{Action: protocol.SupplyRefusalClosed, LoaderNode: loader, PayloadCode: payload},
			want:  "none", after: "same", label: "F3",
		},
		{
			name:  "Closed with no row is a no-op",
			apply: protocol.SupplyRefusalState{Action: protocol.SupplyRefusalClosed, LoaderNode: loader, PayloadCode: payload},
			want:  "none", after: "same", label: "F3",
		},
		{
			name:  "unknown action is ignored",
			seed:  []protocol.SupplyRefusalState{opened},
			apply: protocol.SupplyRefusalState{Action: "snapshot", LoaderNode: loader, PayloadCode: payload},
			want:  "open by=Edge A ack=-", after: "same", label: "F3",
		},
		{
			name:  "a message with no payload is ignored",
			seed:  []protocol.SupplyRefusalState{opened},
			apply: protocol.SupplyRefusalState{Action: protocol.SupplyRefusalClosed, LoaderNode: loader},
			want:  "open by=Edge A ack=-", after: "same", label: "F3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			for _, s := range tc.seed {
				eng.HandleSupplyRefusalState(s)
			}
			eng.HandleSupplyRefusalState(tc.apply)
			if got := refusalRow(t, db, loader, payload); got != tc.want {
				t.Errorf("row = %q, want %q (after: %s, %s)", got, tc.want, tc.after, tc.label)
			}
			// An apply never writes to the outbox: the Edge does not answer Core.
			// after: same for these arms (F3's re-emits come from the snapshot
			// apply, not from this function)
			if out := refusalOutbox(t, db); len(out) != 0 {
				t.Errorf("apply enqueued %d supply.refusal messages, want 0 (after: same, F3)", len(out))
			}
		})
	}
}

// TestSupplyRefusal_Emit_FeedsPin pins the message each operator verb puts on
// the outbox: one supply.refusal row per verb, carrying the action and the
// fields that action owns. A repeated refuse emits again (the write is
// idempotent, the emit is not guarded); a repeated ack emits nothing.
//
// X1 (flipped): an undo that deleted no row emits nothing — Closed only when a
// row went; at the base the second undo emitted Closed again. F3 keeps the
// per-verb messages; healing rides the heartbeat.
func TestSupplyRefusal_Emit_FeedsPin(t *testing.T) {
	t.Parallel()
	f := seedLoaderCard(t)
	procName := ""
	if n, err := f.db.GetProcessNode(f.nodeID); err == nil && n != nil {
		procName = f.eng.processName(n.ProcessID)
	}

	testutil.MustNoErr(t, f.eng.RefuseSupply(f.nodeID, "PART-A", "Bin Loader"), "refuse")
	testutil.MustNoErr(t, f.eng.RefuseSupply(f.nodeID, "PART-A", "Bin Loader"), "refuse again")
	testutil.MustNoErr(t, f.eng.AckSupplyRefusal(f.nodeID, f.core, "PART-A", protocol.SupplyRefusalChoiceWait), "ack")
	testutil.MustNoErr(t, f.eng.AckSupplyRefusal(f.nodeID, f.core, "PART-A", protocol.SupplyRefusalChoiceChangeover), "ack again")
	testutil.MustNoErr(t, f.eng.UndoSupplyRefusal(f.nodeID, "PART-A"), "undo")
	// Undo with no row emits nothing (X1; at the base it emitted Closed again).
	testutil.MustNoErr(t, f.eng.UndoSupplyRefusal(f.nodeID, "PART-A"), "undo again")

	out := refusalOutbox(t, f.db)
	render := func(s protocol.SupplyRefusalState) string {
		parts := []string{s.Action, s.LoaderNode, s.PayloadCode}
		switch s.Action {
		case protocol.SupplyRefusalOpened:
			parts = append(parts, "by="+s.RefusedBy, "at="+feedsPinBool(!s.RefusedAt.IsZero()))
		case protocol.SupplyRefusalAcked:
			parts = append(parts, s.AckChoice, s.AckProcessID, "at="+feedsPinBool(s.AckAt != nil))
		}
		return strings.Join(parts, " ")
	}
	want := []string{
		"opened " + f.core + " PART-A by=Bin Loader at=true",
		"opened " + f.core + " PART-A by=Bin Loader at=true",
		"acked " + f.core + " PART-A wait " + procName + " at=true",
		"closed " + f.core + " PART-A",
	} // X1: the second closed is gone
	var got []string
	for _, s := range out {
		got = append(got, render(s))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("outbox supply.refusal messages:\n%s\nwant:\n%s\n(after: same, X1)",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestClearOnLoad_FeedsPin pins clear-on-LOAD (operator_bin_ops.go, the
// DeleteSupplyRefusal after the fallback outbound): a LOAD with no L1 in
// flight deletes the card's refusal and, since X1, emits Closed so every other
// Edge and Core close it too (before X1 it emitted nothing). A LOAD at an
// unrefused card emits nothing.
//
// The normal side cycle — LOAD confirming a delivered L1 — returned before that
// line at the base, so the refusal survived the LOAD and nothing was emitted.
// X1 (flipped) runs the clear on both paths.
func TestClearOnLoad_FeedsPin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		refused   bool
		l1        bool
		wantRow   string
		wantMsgs  string
		afterRow  string
		afterMsgs string
		label     string
	}{
		{
			name: "fallback LOAD at a refused card", refused: true,
			wantRow: "none", wantMsgs: "closed", // X1
			afterRow: "none", afterMsgs: "closed", label: "X1",
		},
		{
			name: "fallback LOAD at an unrefused card", refused: false,
			wantRow: "none", wantMsgs: "",
			afterRow: "none", afterMsgs: "", label: "X1",
		},
		{
			name: "LOAD confirming a delivered L1 at a refused card", refused: true, l1: true,
			wantRow: "none", wantMsgs: "closed", // X1
			afterRow: "none", afterMsgs: "closed", label: "X1",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			window := "SC-FPCL-W" + itoa(int64(i))
			eng, db, core, _, nodeID := scLoaderEngine(t, window)
			if tc.l1 {
				core.set(window, false, "")
				l1, err := eng.RequestEmptyBin(nodeID, "")
				testutil.MustNoErr(t, err, "RequestEmptyBin")
				scEcho(t, eng, l1)
				core.set(window, true, "")
				scDeliver(t, eng, db, l1)
			} else {
				core.set(window, true, "") // an empty carrier stands at the window
			}
			if tc.refused {
				testutil.MustNoErr(t, db.OpenSupplyRefusal(window, "PART-SC", "FP Loader"), "seed refusal")
			}

			testutil.MustNoErr(t, eng.LoadBin(nodeID, "PART-SC", nil, scManifest), "LoadBin")

			if got := refusalRow(t, db, window, "PART-SC"); got != tc.wantRow {
				t.Errorf("row after LOAD = %q, want %q (after: %s, %s)", got, tc.wantRow, tc.afterRow, tc.label)
			}
			var actions []string
			for _, s := range refusalOutbox(t, db) {
				actions = append(actions, s.Action)
			}
			if got := strings.Join(actions, ","); got != tc.wantMsgs {
				t.Errorf("supply.refusal messages = %q, want %q (after: %s, %s)", got, tc.wantMsgs, tc.afterMsgs, tc.label)
			}
		})
	}
}

// TestSupplyRefusal_LostAckNeverReachesLoaderEdge_FeedsPin pins the sequence
// "Edge B's ack is lost, then the loader Edge receives Core's state".
//
// Edge A owns the loader window and refuses. Core relays A's Opened to Edge B
// (and back to A). An operator on B answers; B's Acked is enqueued on B's
// outbox and never delivered. At the base, what Core then sent A was all it
// had: the Opened again, so A's row never showed the answer and nothing
// re-sent it.
//
// F3 (flipped): B's heartbeat carries its refusals digest; Core's differs, B
// re-emits Acked (it owns the ack fields while Core has the row open), and the
// next supply.refusal_snapshot to A carries the ack, so A's row reads answered
// and every digest converges.
func TestSupplyRefusal_LostAckNeverReachesLoaderEdge_FeedsPin(t *testing.T) {
	t.Parallel()
	a := seedLoaderCard(t) // the loader Edge

	// Edge B: its own database and a cell process node to answer from.
	dbB := testEngineDB(t)
	engB := testEngine(t, dbB)
	procB, err := dbB.CreateProcess("FP-CELL-B", "", "", "", false)
	testutil.MustNoErr(t, err, "create B process")
	nodeB, err := dbB.CreateProcessNode(processes.NodeInput{
		ProcessID: procB, CoreNodeName: "FP-CELL-B-N1", Code: "C1", Name: "Cell", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create B node")

	// A refuses; Core relays the Opened to B and back to A.
	testutil.MustNoErr(t, a.eng.RefuseSupply(a.nodeID, "PART-A", "Bin Loader"), "A refuses")
	sent := refusalOutbox(t, a.db)
	if len(sent) != 1 || sent[0].Action != protocol.SupplyRefusalOpened {
		t.Fatalf("A's outbox = %+v, want one Opened", sent)
	}
	engB.HandleSupplyRefusalState(sent[0])
	a.eng.HandleSupplyRefusalState(sent[0])

	// B answers. Its Acked sits on B's outbox and is lost.
	testutil.MustNoErr(t, engB.AckSupplyRefusal(nodeB, a.core, "PART-A", protocol.SupplyRefusalChoiceWait), "B answers")
	lost := refusalOutbox(t, dbB)

	// F3: Core holds A's refusal, unanswered. B's heartbeat quotes the digest of
	// its own table, which carries the answer, so it differs from Core's and
	// Core sends B its open set; B owns the ack while Core holds the row open
	// and re-emits Acked. Core applies it, and A's digest now differs, so Core
	// sends A its set, which carries the answer.
	coreOpen := []protocol.SupplyRefusalState{sent[0]}
	engB.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	a.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	coreDigest := func() string {
		d, err := protocol.RefusalsDigest(coreOpen)
		testutil.MustNoErr(t, err, "core digest")
		return d
	}
	if engB.FeedDigests()[protocol.FeedRefusals] == coreDigest() {
		t.Fatal("B's refusals digest equals Core's although Core lacks B's answer")
	}
	testutil.MustNoErr(t, engB.ApplySupplyRefusalSnapshot(protocol.SupplyRefusalSnapshot{Digest: coreDigest(), Open: coreOpen}), "B applies")
	healed := refusalOutbox(t, dbB)
	if len(healed) != 2 || healed[1].Action != protocol.SupplyRefusalAcked {
		t.Fatalf("B's outbox after the snapshot = %+v, want the lost Acked re-emitted", healed)
	}
	coreOpen[0].AckAt, coreOpen[0].AckChoice, coreOpen[0].AckProcessID = healed[1].AckAt, healed[1].AckChoice, healed[1].AckProcessID
	testutil.MustNoErr(t, a.eng.ApplySupplyRefusalSnapshot(protocol.SupplyRefusalSnapshot{Digest: coreDigest(), Open: coreOpen}), "A applies")

	cases := []struct {
		name, got, want, after, label string
	}{
		{"B's row", refusalRow(t, dbB, a.core, "PART-A"), "open by=Bin Loader ack=wait/FP-CELL-B", "same", "F3"},
		{"B's Acked enqueued once", itoa(int64(len(lost))), "1", "same; re-emitted on a refusals digest mismatch", "F3"},
		{"A's row after Core's state", refusalRow(t, a.db, a.core, "PART-A"), "open by=Bin Loader ack=wait/FP-CELL-B", "open by=Bin Loader ack=wait/FP-CELL-B (from supply.refusal_snapshot)", "F3"}, // F3
		{"A emits nothing on apply", itoa(int64(len(refusalOutbox(t, a.db)) - 1)), "0", "same", "F3"},
		{"A's digest equals Core's", feedsPinBool(a.eng.FeedDigests()[protocol.FeedRefusals] == coreDigest()), "true", "same", "F3"},
		{"B's digest equals Core's", feedsPinBool(engB.FeedDigests()[protocol.FeedRefusals] == coreDigest()), "true", "same", "F3"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q (after: %s, %s)", c.name, c.got, c.want, c.after, c.label)
		}
	}
}

// TestGetContainment_Non200_FeedsPin pins CoreClient.GetContainment on each
// status. At the base it never read the status, so a non-200 whose body
// decoded was returned as state with no error, and only an undecodable body
// errored. X3 flipped it: any non-200 is an error naming the status. `base` is
// the value before X3.
func TestGetContainment_Non200_FeedsPin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		status    int
		body      string
		wantErr   bool
		wantFlags int
		base      string
		label     string
	}{
		{"200 with state", http.StatusOK, `{"containment":[{"payload_code":"FP-PART","active":true}],"held_bins":[]}`, false, 1, "same", "X3"},
		{"500 with a jsonError body", http.StatusInternalServerError, `{"error":"database unavailable"}`, true, 0, "no error, 0 flags", "X3"},                             // X3
		{"503 with a state-shaped body", http.StatusServiceUnavailable, `{"containment":[{"payload_code":"FP-STALE","active":true}]}`, true, 0, "no error, 1 flag", "X3"}, // X3
		{"404 with a text body", http.StatusNotFound, "404 page not found\n", true, 0, "error (a decode error, not naming the status)", "X3"},                             // X3
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/containment" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(tc.status)
				if _, err := w.Write([]byte(tc.body)); err != nil {
					t.Errorf("stub Core: write body: %v", err)
				}
			}))
			t.Cleanup(srv.Close)

			st, err := stubCoreClient(srv.URL).GetContainment()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v (base: %s, %s)", err, tc.wantErr, tc.base, tc.label)
			}
			if err != nil && !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("err = %v, want it to name status %d (base: %s, %s)", err, tc.status, tc.base, tc.label)
			}
			if err == nil && len(st.Containment) != tc.wantFlags {
				t.Errorf("flags = %d, want %d (base: %s, %s)", len(st.Containment), tc.wantFlags, tc.base, tc.label)
			}
		})
	}
}

func feedsPinBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
