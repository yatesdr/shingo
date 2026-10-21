//go:build docker

package www

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingo/protocol/testutil"

	"shingocore/asks"
	"shingocore/internal/testdb"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// door_ask_agreement_test.go — the ask and the HTTP call answer through one
// body (doors.go). Every arm the door pins drive through an HTTP adapter
// (runDoorArms) is driven again, on a world of its own, through the ask
// server with a recording reply, and checked against the same expected
// answer and the same writes: the same status, the same sentence and rows,
// and the body the HTTP adapter writes byte for byte. TestHeldPartialsDoor's
// arms, which are a GET and do not ride runDoorArms, are
// TestDoorAsk_HeldPartialsAgrees.

// doorAsk is the ask an HTTP door answers for: its kind, its ask adapter, the
// request field that names the asking station (the envelope's source carries
// it on the ask; "" when the door reads none), and whether it replays as
// cmd/shingocore registers it.
type doorAsk struct {
	kind    string
	door    func(*Doors) asks.Door
	station string
	replay  bool
}

// doorAsks is keyed by the HTTP door's path, as runDoorArms names it.
var doorAsks = map[string]doorAsk{
	"/api/telemetry/bin-load":         {protocol.AskBinLoad, func(d *Doors) asks.Door { return d.AskBinLoad }, "", false},
	"/api/telemetry/bin-count":        {protocol.AskBinCount, func(d *Doors) asks.Door { return d.AskBinCount }, "", false},
	"/api/telemetry/bin-clear":        {protocol.AskBinClear, func(d *Doors) asks.Door { return d.AskBinClear }, "", false},
	"/api/telemetry/bin-quality-hold": {protocol.AskQualityHold, func(d *Doors) asks.Door { return d.AskQualityHold }, "", false},
	"/api/telemetry/clear-home":       {protocol.AskClearHome, func(d *Doors) asks.Door { return d.AskClearHome }, "station", true},
	"/api/inventory/preflight":        {protocol.AskPreflight, func(d *Doors) asks.Door { return d.AskPreflight }, "station", false},
	"/api/release/points":             {protocol.AskReleasePoints, func(d *Doors) asks.Door { return d.AskReleasePoints }, "station_id", false},
}

// askThrough sends one ask as an Edge at station would and returns Core's
// answer: the server built as the composition root builds it for this kind
// (the world's store for the replay), its reply recorded.
func askThrough(t *testing.T, f *doorFix, a doorAsk, station string, body []byte) *protocol.EdgeAnswer {
	t.Helper()
	var got []*protocol.EdgeAnswer
	srv := asks.New(f.db, func(env *protocol.Envelope, ans *protocol.EdgeAnswer) (int64, error) {
		got = append(got, ans)
		reply, err := protocol.NewDataReply(protocol.SubjectEdgeAnswer, protocol.Address{Role: protocol.RoleCore, Station: "core"},
			protocol.Address{Role: protocol.RoleEdge, Station: env.Src.Station}, env.ID, ans)
		testutil.MustNoErr(t, err, "the answer's envelope")
		wire, err := reply.Encode()
		testutil.MustNoErr(t, err, "encode the answer's envelope")
		t.Logf("%s answer %d: %d bytes on the wire (%s)", ans.Kind, ans.Status, len(wire), t.Name())
		return int64(len(got)), nil
	})
	srv.Register(a.kind, a.door(f.h.doors()), a.replay)
	env := &protocol.Envelope{ID: uuid.NewString(), Type: protocol.TypeData,
		Src: protocol.Address{Role: protocol.RoleEdge, Station: station}}
	srv.Handle(env, &protocol.EdgeAsk{Kind: a.kind, Body: body})
	if len(got) != 1 {
		t.Fatalf("%d answers to one ask", len(got))
	}
	return got[0]
}

// assertAnswerAgrees checks an ask's answer against the HTTP door's expected
// status and body: the same status; the body the adapter writes from the
// answer (its own body, else the sentence and rows as doorRefusal) is the
// expected bytes; a refusal's sentence and rows are the expected body's, and
// an accept carries neither.
func assertAnswerAgrees(t *testing.T, got *protocol.EdgeAnswer, code int, want string) {
	t.Helper()
	written := doorJSON(t, doorRefusal{Error: got.Sentence, Rows: got.Rows})
	if got.Body != nil {
		written = string(got.Body) + "\n"
	}
	if got.Status != code || written != want {
		t.Errorf("the ask's answer:\n got %d %s\nwant %d %s", got.Status, written, code, want)
	}
	if code == http.StatusOK {
		if got.Sentence != "" || len(got.Rows) != 0 {
			t.Errorf("an accept carries sentence %q and %d rows", got.Sentence, len(got.Rows))
		}
		return
	}
	var refusal struct {
		Error string                 `json:"error"`
		Rows  []protocol.NodeBinsRow `json:"rows"`
	}
	testutil.MustNoErr(t, json.Unmarshal([]byte(want), &refusal), "the expected refusal")
	if got.Sentence != refusal.Error {
		t.Errorf("the ask's sentence %q, the door's %q", got.Sentence, refusal.Error)
	}
	if g, w := doorJSON(t, got.Rows), doorJSON(t, refusal.Rows); g != w {
		t.Errorf("the ask's rows %s, the door's %s", g, w)
	}
}

// runAskArm is one door arm through the ask: the arm's world, its request as
// the ask's body (a string sent raw, else encoded as the HTTP leg encodes it),
// the arm's expected answer and writes.
func runAskArm(t *testing.T, a doorAsk, arm doorArm) {
	t.Helper()
	f := newDoorFix(t)
	body, watch := arm.arrange(t, f)
	snap := snapDoor(t, f.db, watch...)
	raw, isRaw := body.(string)
	encoded := []byte(raw)
	if !isRaw {
		var buf bytes.Buffer
		testutil.MustNoErr(t, json.NewEncoder(&buf).Encode(body), "encode the ask's body")
		encoded = buf.Bytes()
	}
	// The Edge's station rides the envelope; it is the one the request names.
	station := ""
	if fields, ok := body.(map[string]any); ok && a.station != "" {
		station, _ = fields[a.station].(string)
	}
	got := askThrough(t, f, a, station, encoded)
	code, want := arm.answer(t, f)
	assertAnswerAgrees(t, got, code, want)
	if arm.after != nil {
		arm.after(t, f)
	}
	if arm.wrote == nil {
		snap.assertNothingWritten(t, f.db)
		return
	}
	cols, added := arm.wrote(f)
	snap.assertWrote(t, f.db, cols, added)
}

// TestDoorAsk_HeldPartialsAgrees is TestHeldPartialsDoor_Arms' refusal and
// list through the ask: the payload the GET takes as ?payload= is the
// request's payload_code.
func TestDoorAsk_HeldPartialsAgrees(t *testing.T) {
	t.Parallel()
	a := doorAsk{kind: protocol.AskHeldPartials, door: func(d *Doors) asks.Door { return d.AskHeldPartials }}
	held := func(code string) []byte {
		b, err := json.Marshal(protocol.HeldPartialsRequest{PayloadCode: code})
		testutil.MustNoErr(t, err, "marshal held-partials request")
		return b
	}
	t.Run("no payload is refused", func(t *testing.T) {
		t.Parallel()
		f := newDoorFix(t)
		assertAnswerAgrees(t, askThrough(t, f, a, "edge.hp", held("")), http.StatusBadRequest, doorErr("payload is required"))
		assertAnswerAgrees(t, askThrough(t, f, a, "edge.hp", held(" ")), http.StatusBadRequest, doorErr("payload is required"))
		assertAnswerAgrees(t, askThrough(t, f, a, "edge.hp", []byte("{")), http.StatusBadRequest, doorErr("unexpected EOF"))
	})
	t.Run("a part with no held partial answers an empty list", func(t *testing.T) {
		t.Parallel()
		f := newDoorFix(t)
		ppHome(t, f.db, f.sd.StorageNode, "HP-NONE")
		testdb.CreateBinAtNode(t, f.db, "HP-NONE", f.sd.LineNode.ID, "HP-AT-LINE")
		assertAnswerAgrees(t, askThrough(t, f, a, "edge.hp", held("HP-NONE")), http.StatusOK, "[]\n")
	})
	t.Run("the held partials of a part, oldest held first", func(t *testing.T) {
		t.Parallel()
		f := newDoorFix(t)
		ppHome(t, f.db, f.sd.StorageNode, "HP-X")
		var want []protocol.HeldPartial
		for i, at := range []string{"2026-01-02T03:04:05Z", "2026-01-01T03:04:05Z"} {
			label := fmt.Sprintf("HP-X-%d", i+1)
			b := testdb.CreateBinAtNode(t, f.db, "HP-X", f.sd.StorageNode.ID, label)
			testutil.MustNoErr(t, f.db.SetBinManifest(b.ID, `{"items":[]}`, "HP-X", 5+i), "a partial")
			_, err := f.db.DB.Exec(`UPDATE bins SET held_since=$1 WHERE id=$2`, at, b.ID)
			testutil.MustNoErr(t, err, "held since")
			want = append([]protocol.HeldPartial{{BinID: b.ID, BinLabel: label, NodeName: f.sd.StorageNode.Name,
				UOPRemaining: 5 + i, HeldSince: at}}, want...)
		}
		snap := snapDoor(t, f.db, want[0].BinID, want[1].BinID)
		assertAnswerAgrees(t, askThrough(t, f, a, "edge.hp", held("HP-X")), http.StatusOK, doorJSON(t, want))
		snap.assertNothingWritten(t, f.db)
	})
}

// TestDoorAsk_Reseat is the reseat's answers through the ask (it has no HTTP
// door): a request naming no node and one Core cannot read are refused, a node
// Core has no node for is refused not found (never answered as empty), and a
// node with no carrier answers not landed. The landing a carrier gets is
// service's TestReseat_ReAnnouncesTheCarrierToTheAsker.
func TestDoorAsk_Reseat(t *testing.T) {
	t.Parallel()
	a := doorAsk{kind: protocol.AskReseat, door: func(d *Doors) asks.Door { return d.AskReseat }}
	reseat := func(node string) []byte {
		b, err := json.Marshal(protocol.ReseatRequest{Node: node})
		testutil.MustNoErr(t, err, "marshal reseat request")
		return b
	}
	f := newDoorFix(t)
	assertAnswerAgrees(t, askThrough(t, f, a, "edge.rs", reseat("")), http.StatusBadRequest, doorErr("node is required"))
	assertAnswerAgrees(t, askThrough(t, f, a, "edge.rs", []byte("{")), http.StatusBadRequest, doorErr("unexpected EOF"))
	assertAnswerAgrees(t, askThrough(t, f, a, "edge.rs", reseat("NO-SUCH-NODE")), http.StatusNotFound,
		doorErr("Core has no node NO-SUCH-NODE"))
	assertAnswerAgrees(t, askThrough(t, f, a, "edge.rs", reseat(f.sd.StorageNode.Name)), http.StatusOK,
		"{\"node\":\""+f.sd.StorageNode.Name+"\",\"landed\":false}\n")
}

// TestDoorAsk_LoaderRetrieve: an ask for carriers at a loader is answered by
// Core's own sizing and window choice (ReplenishLoader). A key that is not a
// loader, a direction or an origin it does not name (a threshold is Core's own,
// never asked), and a count below one are refused before any read; a loader
// Core does not have is 404. A person's ask for three empties at an
// operator-driven loader with two windows makes two, named by their UUIDs, and
// the same ask again makes none and names both windows as held. A release's
// full at an unloader comes from the source the ask names, carries its part,
// and a second release of the part makes none.
func TestDoorAsk_LoaderRetrieve(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	d := h.doors()
	ask := func(req protocol.LoaderRetrieveRequest) (asks.Result, protocol.LoaderRetrieveResponse) {
		t.Helper()
		raw, err := json.Marshal(req)
		testutil.MustNoErr(t, err, "marshal")
		res := d.AskLoaderRetrieve("edge.line1", raw)
		var out protocol.LoaderRetrieveResponse
		if b, ok := res.Body.(protocol.LoaderRetrieveResponse); ok {
			out = b
		}
		return res, out
	}
	empty := func(key string, count int) protocol.LoaderRetrieveRequest {
		return protocol.LoaderRetrieveRequest{LoaderKey: key, Direction: protocol.LoaderRetrieveEmpty,
			Origin: protocol.LoaderAskOperator, Count: count}
	}
	for name, c := range map[string]struct {
		req  protocol.LoaderRetrieveRequest
		want int
	}{
		"a key that is not a loader":  {empty("SMN_A1", 1), http.StatusBadRequest},
		"a count of none":             {empty("loader:1", 0), http.StatusBadRequest},
		"no direction":                {protocol.LoaderRetrieveRequest{LoaderKey: "loader:1", Origin: protocol.LoaderAskOperator, Count: 1}, http.StatusBadRequest},
		"no origin":                   {protocol.LoaderRetrieveRequest{LoaderKey: "loader:1", Direction: protocol.LoaderRetrieveFull, Count: 1}, http.StatusBadRequest},
		"a threshold over the wire":   {protocol.LoaderRetrieveRequest{LoaderKey: "loader:1", Direction: protocol.LoaderRetrieveEmpty, Origin: "threshold", Count: 1}, http.StatusBadRequest},
		"a loader Core does not have": {empty("loader:999999", 1), http.StatusNotFound},
	} {
		if res, _ := ask(c.req); res.Status != c.want {
			t.Errorf("%s: %d %q, want %d", name, res.Status, res.Sentence, c.want)
		}
	}

	sd := testdb.SetupStandardData(t, db)
	market := &nodes.Node{Name: "LE-MARKET", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(market), "market")
	id, err := db.CreateLoader(loaders.Loader{Name: "LE-LDR", Role: loaders.RoleProduce,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator, InboundSource: market.Name})
	testutil.MustNoErr(t, err, "loader")
	for _, name := range []string{"LE-W1", "LE-W2"} {
		w := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(w), name)
		testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: w.ID}), "window "+name)
	}
	testutil.MustNoErr(t, db.UpsertLoaderPayload(loaders.Payload{LoaderID: id, PayloadCode: sd.Payload.Code}), "payload")

	key := loaders.Key(id)
	req := empty(key, 3)
	req.PayloadCode, req.OriginClass = sd.Payload.Code, protocol.OriginClassNoDemand
	res, out := ask(req)
	if res.Status != http.StatusOK || out.Want != 3 || len(out.Created) != 2 {
		t.Fatalf("an ask for three at two windows: %d %q %+v", res.Status, res.Sentence, out)
	}
	// The class the asker stated is the orders' own: a push that serves no
	// demand is no_demand, not an orphan.
	for _, uuid := range out.Created {
		o, err := db.GetOrderByUUID(uuid)
		testutil.MustNoErr(t, err, "read "+uuid)
		if o.OriginClass != protocol.OriginClassNoDemand || o.OrderType != protocol.OrderTypeRetrieveEmpty {
			t.Errorf("order %s: %s class %q, want an empty, %q", uuid, o.OrderType, o.OriginClass, protocol.OriginClassNoDemand)
		}
	}
	req.OriginClass = ""
	res, out = ask(req)
	if res.Status != http.StatusOK || len(out.Created) != 0 || len(out.HeldBy) != 2 {
		t.Errorf("the same ask again: %d %+v, want none made and both windows held", res.Status, out)
	}

	// An unloader fed directly, asked for a full by a release that names its
	// source.
	fg := &nodes.Node{Name: "LE-FG", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(fg), "FG market")
	uid, err := db.CreateLoader(loaders.Loader{Name: "LE-ULD", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator})
	testutil.MustNoErr(t, err, "unloader")
	for _, name := range []string{"LE-U1", "LE-U2"} {
		w := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(w), name)
		testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: uid, PositionNodeID: w.ID}), "window "+name)
	}
	testutil.MustNoErr(t, db.UpsertLoaderPayload(loaders.Payload{LoaderID: uid, PayloadCode: sd.Payload.Code}), "unloader payload")
	full := protocol.LoaderRetrieveRequest{LoaderKey: loaders.Key(uid), Direction: protocol.LoaderRetrieveFull,
		Origin: protocol.LoaderAskSideCycle, PayloadCode: sd.Payload.Code, Count: 1, SourceNode: fg.Name,
		OriginClass: protocol.OriginClassNoDemand, SkipAutoConfirm: true}
	res, out = ask(full)
	if res.Status != http.StatusOK || len(out.Created) != 1 {
		t.Fatalf("a release's full: %d %q %+v", res.Status, res.Sentence, out)
	}
	o, err := db.GetOrderByUUID(out.Created[0])
	testutil.MustNoErr(t, err, "read the full")
	if o.OrderType != protocol.OrderTypeRetrieve || o.SourceNode != fg.Name || o.PayloadCode != sd.Payload.Code || o.DeliveryNode != "LE-U1" {
		t.Errorf("the full: %s from %q of %q to %q, want a retrieve from %q of %q to LE-U1",
			o.OrderType, o.SourceNode, o.PayloadCode, o.DeliveryNode, fg.Name, sd.Payload.Code)
	}
	if !o.SkipAutoConfirm {
		t.Errorf("the full asked with skip_auto_confirm: the sweep would confirm it (skip_auto_confirm false)")
	}
	res, out = ask(full)
	if res.Status != http.StatusOK || len(out.Created) != 0 || out.Skipped == "" {
		t.Errorf("a second release of the part: %d %+v, want none made and why", res.Status, out)
	}

	// A person's full at the same unloader, fed directly, naming no source: with
	// any_source Core searches the plant (the order has no source), as for an
	// order request that names none; without it, a loader fed directly is given
	// nothing. Neither is kept off the sweep unless it says so.
	person := protocol.LoaderRetrieveRequest{LoaderKey: loaders.Key(uid), Direction: protocol.LoaderRetrieveFull,
		Origin: protocol.LoaderAskOperator, PayloadCode: sd.Payload.Code, Count: 1,
		OriginClass: protocol.OriginClassNoDemand}
	res, out = ask(person)
	if res.Status != http.StatusOK || len(out.Created) != 0 || out.Skipped == "" {
		t.Errorf("a person's full at an unloader fed directly, no source: %d %+v, want none made and why", res.Status, out)
	}
	person.AnySource = true
	res, out = ask(person)
	if res.Status != http.StatusOK || len(out.Created) != 1 {
		t.Fatalf("a person's full, any source: %d %q %+v, want one", res.Status, res.Sentence, out)
	}
	o, err = db.GetOrderByUUID(out.Created[0])
	testutil.MustNoErr(t, err, "read the person's full")
	if o.SourceNode != "" || o.DeliveryNode != "LE-U2" || o.SkipAutoConfirm {
		t.Errorf("a person's full, any source: from %q to %q skip_auto_confirm %v, want no source, LE-U2, false",
			o.SourceNode, o.DeliveryNode, o.SkipAutoConfirm)
	}
}
