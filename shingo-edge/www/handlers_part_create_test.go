package www

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/stations"
)

// handlers_part_create_test.go — POST /api/processes/{id}/parts, the station's
// new part (apiCreatePart): the Edge's refusals before anything is sent, and
// Core's three outcomes as the page reads them (done, sent and waiting,
// refused).

// partDoor is one process with a station and HMI setup on, behind the real
// router and engine, whose Core names part.create and has sent one carrier
// type, SYN-CART. other is a station of another process.
type partDoor struct {
	db        *store.DB
	eng       *engine.Engine
	router    *chi.Mux
	processID int64
	station   int64
	other     int64
}

func newPartDoor(t *testing.T) partDoor {
	t.Helper()
	db := testdb.Open(t)
	pid, err := db.CreateProcess("SYN Press", "", "", "", false)
	must(t, err, "process")
	station, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Code: "syn-press", Name: "SYN Press Screen", Enabled: true})
	must(t, err, "station")
	otherPID, err := db.CreateProcess("SYN Other", "", "", "", false)
	must(t, err, "other process")
	other, err := db.CreateOperatorStation(stations.Input{ProcessID: otherPID, Code: "syn-other", Name: "SYN Other Screen", Enabled: true})
	must(t, err, "other station")
	must(t, db.SetFlowComposerEnabled(pid, true), "HMI setup on")
	_, router, eng := realFlowRouterEngine(t, db)
	eng.ApplyNodeList(&protocol.NodeListResponse{Family: map[string]string{"SYN-CART": "SYN-CART"}})
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Asks: []string{protocol.AskPartCreate}})
	return partDoor{db: db, eng: eng, router: router, processID: pid, station: station, other: other}
}

// body is a part the form could send: every field good.
func (f partDoor) body() map[string]any {
	return map[string]any{"station_id": f.station, "part_number": "SYN-NEW-1", "standard_pack": 800,
		"bin_type_code": "SYN-CART", "catid": "", "tote_layers": nil, "uop_per_tote": nil}
}

func (f partDoor) post(t *testing.T, body map[string]any) (int, string) {
	t.Helper()
	resp := doRequest(t, f.router, "POST", fmt.Sprintf("/api/processes/%d/parts", f.processID), body, nil)
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	must(t, err, "read the answer")
	return resp.StatusCode, string(raw)
}

// asksOut is how many part.create asks are on the outbox.
func (f partDoor) asksOut(t *testing.T) int {
	t.Helper()
	msgs, err := f.db.ListPendingOutbox(100)
	must(t, err, "list the outbox")
	n := 0
	for _, m := range msgs {
		if m.MsgType == protocol.SubjectEdgeAsk && strings.Contains(string(m.Payload), protocol.AskPartCreate) {
			n++
		}
	}
	return n
}

// answerParts is a Core that answers every part.create ask on the outbox with
// answer(req) for the test's life, matched to the ask as Core's reply is.
func (f partDoor) answerParts(t *testing.T, answer func(protocol.PartCreateRequest) *protocol.EdgeAnswer) {
	t.Helper()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			if f.eng.PendingAsks() == 0 {
				continue
			}
			msgs, err := f.db.ListPendingOutbox(100)
			if err != nil {
				return
			}
			for _, m := range msgs {
				var env protocol.Envelope
				var data protocol.Data
				var ask protocol.EdgeAsk
				var req protocol.PartCreateRequest
				if m.MsgType != protocol.SubjectEdgeAsk || json.Unmarshal(m.Payload, &env) != nil ||
					json.Unmarshal(env.Payload, &data) != nil || json.Unmarshal(data.Body, &ask) != nil ||
					ask.Kind != protocol.AskPartCreate || json.Unmarshal(ask.Body, &req) != nil ||
					f.db.AckOutbox(m.ID) != nil {
					continue
				}
				f.eng.HandleEdgeAnswer(&protocol.Envelope{Type: protocol.TypeData, CorID: env.ID}, answer(req))
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
}

// TestCreatePart_EdgeRefusalsSendNothing: each request the Edge can see is
// wrong is refused with its sentence, and no ask goes out.
func TestCreatePart_EdgeRefusalsSendNothing(t *testing.T) {
	t.Parallel()
	f := newPartDoor(t)
	with := func(k string, v any) map[string]any { b := f.body(); b[k] = v; return b }
	for _, c := range []struct {
		name   string
		body   map[string]any
		before func()
		want   int
		says   string
	}{
		{name: "a blank part number", body: with("part_number", "   "), want: 400, says: "Type the part number."},
		{name: "a pack of 0", body: with("standard_pack", 0), want: 400, says: "standard pack"},
		{name: "no carrier", body: with("bin_type_code", ""), want: 400, says: "Choose the carrier"},
		{name: "a carrier Core did not send", body: with("bin_type_code", "SYN-NOT-SENT"), want: 400,
			says: "carrier type called SYN-NOT-SENT"},
		{name: "tote layers of 0", body: with("tote_layers", 0), want: 400, says: "Tote layers"},
		{name: "parts per tote of 0", body: with("uop_per_tote", 0), want: 400, says: "Parts per tote"},
		{name: "a station of another process", body: with("station_id", f.other), want: 400, says: "is not a station of this process"},
		{name: "no station", body: with("station_id", 0), want: 400, says: "station_id is required"},
		{name: "a Core that does not name the kind", body: f.body(), want: 400, says: "does not answer the part.create ask",
			before: func() { f.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}}) }},
		{name: "HMI setup off", body: f.body(), want: 403, says: engine.ErrFlowComposerDisabled.Error(),
			before: func() {
				f.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Asks: []string{protocol.AskPartCreate}})
				must(t, f.db.SetFlowComposerEnabled(f.processID, false), "HMI setup off")
			}},
	} {
		if c.before != nil {
			c.before()
		}
		out := f.asksOut(t)
		status, raw := f.post(t, c.body)
		if status != c.want || !strings.Contains(raw, c.says) {
			t.Errorf("%s: %d %s; want %d saying %q", c.name, status, raw, c.want, c.says)
		}
		if n := f.asksOut(t) - out; n != 0 {
			t.Errorf("%s: %d asks went out; want none", c.name, n)
		}
	}
}

// TestCreatePart_DoneIsOnThePicker: Core's yes answers 200 with the part as
// Core made it, and the station's part list has it by the time the page reads
// it.
func TestCreatePart_DoneIsOnThePicker(t *testing.T) {
	t.Parallel()
	f := newPartDoor(t)
	f.answerParts(t, func(req protocol.PartCreateRequest) *protocol.EdgeAnswer {
		body, err := json.Marshal(protocol.PartCreateResponse{ProcessID: req.ProcessID,
			Payload:  protocol.CatalogPayloadInfo{ID: 601, Name: req.PartNumber, Code: req.PartNumber, UOPCapacity: req.StandardPack},
			BinTypes: []protocol.PayloadBinTypeInfo{{PayloadCode: req.PartNumber, BinTypeCode: req.BinTypeCode}}})
		if err != nil {
			t.Errorf("encode the answer: %v", err)
		}
		return &protocol.EdgeAnswer{Kind: protocol.AskPartCreate, Status: protocol.AnswerOK, Body: body}
	})
	b := f.body()
	b["part_number"] = "  SYN-NEW-1 "
	status, raw := f.post(t, b)
	if status != http.StatusOK {
		t.Fatalf("POST = %d %s, want 200", status, raw)
	}
	var made protocol.PartCreateResponse
	must(t, json.Unmarshal([]byte(raw), &made), "decode the answer")
	if made.ProcessID != f.processID || made.Payload.Code != "SYN-NEW-1" || made.Payload.UOPCapacity != 800 {
		t.Errorf("answer = %s, want Core's part SYN-NEW-1 for process %d at 800", raw, f.processID)
	}
	resp := doRequest(t, f.router, "GET", fmt.Sprintf("/api/operator-stations/%d/composer", f.station), nil, nil)
	assertStatus(t, resp, http.StatusOK)
	var read struct {
		PartRows []struct {
			Code string `json:"code"`
			Name string `json:"name"`
		} `json:"part_rows"`
	}
	decodeJSON(t, resp, &read)
	listed := false
	for _, r := range read.PartRows {
		listed = listed || (r.Code == "SYN-NEW-1" && r.Name == "SYN-NEW-1")
	}
	if !listed {
		t.Errorf("the composer read's part rows %+v do not list SYN-NEW-1", read.PartRows)
	}
}

// TestCreatePart_PendingIs202: an ask Core has not answered within the wait
// is sent, not failed: 202 with the pending notice, and the ask is out.
func TestCreatePart_PendingIs202(t *testing.T) {
	t.Parallel()
	f := newPartDoor(t)
	status, raw := f.post(t, f.body())
	if status != http.StatusAccepted || !strings.Contains(raw, `"pending":true`) {
		t.Errorf("POST = %d %s, want 202 pending", status, raw)
	}
	if n := f.asksOut(t); n != 1 {
		t.Errorf("%d asks out, want the one sent", n)
	}
}

// TestCreatePart_CoreRefusalIs409: Core's no is 409 with Core's sentence,
// whatever status Core answered with, and nothing is applied.
func TestCreatePart_CoreRefusalIs409(t *testing.T) {
	t.Parallel()
	for _, coreStatus := range []int{protocol.AnswerRefused, protocol.AnswerExpired} {
		f := newPartDoor(t)
		f.answerParts(t, func(protocol.PartCreateRequest) *protocol.EdgeAnswer {
			return &protocol.EdgeAnswer{Kind: protocol.AskPartCreate, Status: coreStatus, Sentence: "Core already has SYN NEW 1."}
		})
		status, raw := f.post(t, f.body())
		if status != http.StatusConflict || !strings.Contains(raw, "Core already has SYN NEW 1.") {
			t.Errorf("Core answering %d: POST = %d %s, want 409 with Core's sentence", coreStatus, status, raw)
		}
		parts, err := f.db.ListProcessPayloads(f.processID)
		must(t, err, "the part set")
		if len(parts) != 0 {
			t.Errorf("Core answering %d: the part set took %v", coreStatus, parts)
		}
	}
}

// TestCreatePart_TheComposerReadDrawsTheDoor: the engine hands the station's
// composer read what draws the door — Core's carriers, and whether Core names
// the kind — so the read says new_part with SYN-CART while Core does, and
// nothing once an ack stops naming it.
func TestCreatePart_TheComposerReadDrawsTheDoor(t *testing.T) {
	t.Parallel()
	f := newPartDoor(t)
	read := func() (bool, []string) {
		t.Helper()
		resp := doRequest(t, f.router, "GET", fmt.Sprintf("/api/operator-stations/%d/composer", f.station), nil, nil)
		assertStatus(t, resp, http.StatusOK)
		var got struct {
			NewPart      bool     `json:"new_part"`
			CarrierTypes []string `json:"carrier_types"`
		}
		decodeJSON(t, resp, &got)
		return got.NewPart, got.CarrierTypes
	}
	if on, carriers := read(); !on || len(carriers) != 1 || carriers[0] != "SYN-CART" {
		t.Errorf("Core naming part.create: new_part %v, carrier_types %v; want the door with SYN-CART", on, carriers)
	}
	f.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	if on, carriers := read(); on || carriers != nil {
		t.Errorf("an ack without the kind: new_part %v, carrier_types %v; want the door not drawn", on, carriers)
	}
}
