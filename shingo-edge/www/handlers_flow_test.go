package www

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/internal/testdb"
	"shingoedge/store"

	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/engine/changeover"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// handlers_flow_test.go — the flow composer's endpoints as status mappings
// over the engine's named refusals, the station resolution, the fingerprint
// check in front of start, and the one publish a save fires.

func flowBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func flowStation(t *testing.T, db *store.DB, processID int64) int64 {
	t.Helper()
	id, err := db.CreateOperatorStation(stations.Input{ProcessID: processID, Code: "flow-" + flowItoa(int(processID)), Name: "Press 400", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	return id
}

func flowItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestFlowPreview_WritesTheEngineResultVerbatim: a 200 is the engine's
// FlowPreview, byte for byte — the parity pin in engine compares that struct's
// JSON, so the handler must add and remove nothing.
func TestFlowPreview_WritesTheEngineResultVerbatim(t *testing.T) {
	t.Parallel()
	h, router := newOperatorStationsRouter(t)
	stub := h.orchestration.(*stubEngine)
	stub.flowPreview = &engine.FlowPreview{
		Actions: []changeover.PreviewAction{{NodeID: 3, NodeName: "Press front", CoreNodeName: "PLN_01", Situation: "swap",
			SupplyOrder: &changeover.PreviewSpec{Kind: "retrieve", DeliveryNode: "PLN_01", PayloadCode: "P"}}},
		OrderCount:  ptr(1),
		Findings:    []domain.NodeFinding{{CoreNodeName: "PLN_04", Side: "to", Field: "inbound_staging", Severity: "error", Message: "needs a staging slot"}},
		Unresolved:  []string{"PLN_05"},
		Preflight:   engine.FlowPreflight{State: engine.FlowPreflightMissing, Missing: []string{"P"}},
		Fingerprint: "abc",
	}
	want, err := json.Marshal(stub.flowPreview)
	testutil.MustNoErr(t, err, "json.Marshal")

	resp := doRequest(t, router, "POST", "/api/processes/1/flow/preview", map[string]any{"to_style_id": 7, "cells": []any{}}, nil)
	assertStatus(t, resp, http.StatusOK)
	var got json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	gotC, err := json.Marshal(json.RawMessage(got))
	testutil.MustNoErr(t, err, "json.Marshal")
	if string(gotC) != string(want) {
		t.Errorf("handler body:\n got  %s\n want %s", gotC, want)
	}
}

// TestFlowPreview_StepsReachTheEngineAndTheNoOrdersBody: ?steps=1 is the
// engine request's Steps, beside ?preflight=1 and independent of it, and off
// without it. A preview that fires nothing carries its steady state on the
// hand-built 400 exactly as the engine built it, and without one the key is
// absent, as on a 200 (omitempty).
func TestFlowPreview_StepsReachTheEngineAndTheNoOrdersBody(t *testing.T) {
	t.Parallel()
	h, router := newOperatorStationsRouter(t)
	stub := h.orchestration.(*stubEngine)
	stub.flowPreview = &engine.FlowPreview{OrderCount: ptr(1), Findings: []domain.NodeFinding{}, Unresolved: []string{},
		Preflight: engine.FlowPreflight{State: "unchecked", Missing: []string{}}, Fingerprint: "f"}
	for _, c := range []struct {
		query            string
		steps, preflight bool
	}{{"", false, false}, {"?steps=1", true, false}, {"?steps=1&preflight=1", true, true}, {"?preflight=1", false, true}, {"?steps=0", false, false}} {
		resp := doRequest(t, router, "POST", "/api/processes/1/flow/preview"+c.query, map[string]any{"to_style_id": 7, "cells": []any{}}, nil)
		assertStatus(t, resp, http.StatusOK)
		got := stub.flowPreviewCalls[len(stub.flowPreviewCalls)-1]
		if got.Steps != c.steps || got.Preflight != c.preflight {
			t.Errorf("%q: the engine was asked steps %v preflight %v, want %v %v", c.query, got.Steps, got.Preflight, c.steps, c.preflight)
		}
	}

	steady := []engine.SteadyCell{{CoreNodeName: "PLN_01", Role: "consume", SwapMode: "two_robot", Legs: []engine.SteadyLeg{
		{Leg: engine.SteadyLegA, Kind: engine.SteadyKindComplex, Steps: []protocol.ComplexOrderStep{{Action: "pickup", Node: "PLN_01"}}},
	}}, {CoreNodeName: "PLN_02", Role: "produce", SwapMode: "two_robot", Error: "needs a destination", Legs: []engine.SteadyLeg{}}}
	want, err := json.Marshal(steady)
	testutil.MustNoErr(t, err, "json.Marshal")
	stub.flowPreview = &engine.FlowPreview{OrderCount: ptr(0), Findings: []domain.NodeFinding{}, Unresolved: []string{},
		Preflight: engine.FlowPreflight{State: "unchecked", Missing: []string{}}, Fingerprint: "f", Steady: steady}
	stub.flowPreviewErr = &engine.FlowNoOrdersError{Preview: stub.flowPreview}
	for _, with := range []bool{true, false} {
		if !with {
			stub.flowPreview.Steady = nil
		}
		resp := doRequest(t, router, "POST", "/api/processes/1/flow/preview?steps=1", map[string]any{"to_style_id": 7}, nil)
		assertStatus(t, resp, http.StatusBadRequest)
		var body map[string]json.RawMessage
		testutil.MustNoErr(t, json.NewDecoder(resp.Body).Decode(&body), "decode")
		raw, ok := body["steady"]
		switch {
		case with && string(raw) != string(want):
			t.Errorf("no-orders steady:\n got  %s\n want %s", raw, want)
		case !with && ok:
			t.Errorf("no-orders body carries steady %s with none asked for", raw)
		}
	}
}

// TestFlowPreview_StatusMapping: no orders is 400 with the reason and the
// preview's findings/unresolved; the seam's gates are 409; anything else 400.
func TestFlowPreview_StatusMapping(t *testing.T) {
	t.Parallel()
	h, router := newOperatorStationsRouter(t)
	stub := h.orchestration.(*stubEngine)

	stub.flowPreview = &engine.FlowPreview{OrderCount: ptr(0), Unresolved: []string{"PLN_09"}, Findings: []domain.NodeFinding{}, Preflight: engine.FlowPreflight{State: "unchecked", Missing: []string{}}, Fingerprint: "f"}
	stub.flowPreviewErr = &engine.FlowNoOrdersError{Preview: stub.flowPreview}
	resp := doRequest(t, router, "POST", "/api/processes/1/flow/preview", map[string]any{"to_style_id": 7}, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	body := flowBody(t, resp)
	if body["order_count"] != float64(0) || body["error"] == "" || body["unresolved"] == nil || body["fingerprint"] != "f" {
		t.Errorf("no-orders body = %v, want error, order_count 0, unresolved, fingerprint", body)
	}

	// ErrStyleAlreadyRunning is still on this list and no longer reachable from
	// the composer's own doors: owner ruling R3 (2026-09-12) let a save and a
	// preview of the running style through, and planChangeover keeps raising
	// it for a changeover TO the running style. ErrRunningPositionMove is the
	// refusal the composer can now hit on the running style.
	for _, refusal := range []error{engine.ErrStyleAlreadyRunning, engine.ErrChangeoverActive, engine.ErrRunningPositionMove} {
		stub.flowPreview, stub.flowPreviewErr = nil, refusal
		resp = doRequest(t, router, "POST", "/api/processes/1/flow/preview", map[string]any{"to_style_id": 7}, nil)
		assertStatus(t, resp, http.StatusConflict)
		if body := flowBody(t, resp); body["error"] != refusal.Error() {
			t.Errorf("409 body = %v, want the seam's words %q", body, refusal.Error())
		}
	}

	stub.flowPreviewErr = errors.New("target style 7 does not belong to process 1")
	resp = doRequest(t, router, "POST", "/api/processes/1/flow/preview", map[string]any{"to_style_id": 7}, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// TestFlowSave_StatusMappingAndOnePublish: gate off is 403 with the named
// message; the seam's gates 409; stale 409 with stale:true; a validation
// refusal 422 with findings; success 200 with the engine's result, the
// session's person stamped as called_by, and EXACTLY ONE per-process publish —
// none on any refusal.
func TestFlowSave_StatusMappingAndOnePublish(t *testing.T) {
	t.Parallel()
	h, router := newOperatorStationsRouter(t)
	db := dbOf(h)
	stub := h.orchestration.(*stubEngine)
	rec := recordSpecChangePublishes(t, h)
	stationID := flowStation(t, db, 1)
	body := func(extra map[string]any) map[string]any {
		out := map[string]any{"to_style_id": 7, "cells": []any{}, "fingerprint": "abc", "station_id": stationID}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	stub.flowSaveErr = engine.ErrFlowComposerDisabled
	resp := doRequest(t, router, "POST", "/api/processes/1/flow/save", body(nil), nil)
	assertStatus(t, resp, http.StatusForbidden)
	assertJSONPath(t, resp, "error", "HMI setup is off for this process: an engineer sets up its styles on the desktop")

	stub.flowSaveErr = engine.ErrChangeoverActive
	resp = doRequest(t, router, "POST", "/api/processes/1/flow/save", body(nil), nil)
	assertStatus(t, resp, http.StatusConflict)

	stub.flowSaveErr = engine.ErrFlowStale
	resp = doRequest(t, router, "POST", "/api/processes/1/flow/save", body(nil), nil)
	assertStatus(t, resp, http.StatusConflict)
	if b := flowBody(t, resp); b["stale"] != true {
		t.Errorf("stale 409 body = %v, want stale:true", b)
	}

	stub.flowSaveErr = &engine.FlowValidationError{Findings: []domain.NodeFinding{{CoreNodeName: "PLN_01", Side: "to", Field: "payload_code", Severity: "error", Message: "which part?"}}}
	resp = doRequest(t, router, "POST", "/api/processes/1/flow/save", body(nil), nil)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	if b := flowBody(t, resp); b["findings"] == nil {
		t.Errorf("422 body = %v, want findings", b)
	}

	// A station the body names must belong to the process. A body naming
	// none is a process with no screen, and saves
	// (TestFlowSave_AProcessWithNoScreenSavesItsFirstStyle).
	stub.flowSaveErr = nil
	stub.flowSaveResult = &engine.FlowSaveResult{Fingerprint: "def", Written: 2, Deleted: 1}
	resp = doRequest(t, router, "POST", "/api/processes/2/flow/save", body(nil), nil)
	assertStatus(t, resp, http.StatusBadRequest)

	if n := len(stub.flowSaveCalls); n != 4 {
		t.Errorf("SaveFlow was called %d times across the refusals, want 4 (the engine refused each)", n)
	}
	if got := rec.calls(); len(got) != 0 {
		t.Fatalf("a refused save published %v; a refusal has no side effects", got)
	}

	resp = doRequest(t, router, "POST", "/api/processes/1/flow/save", body(nil), authCookie(t, h))
	assertStatus(t, resp, http.StatusOK)
	if b := flowBody(t, resp); b["fingerprint"] != "def" || b["written"] != float64(2) || b["deleted"] != float64(1) {
		t.Errorf("200 body = %v, want the engine's result", b)
	}
	last := stub.flowSaveCalls[len(stub.flowSaveCalls)-1]
	if last.CalledBy != "testadmin" || last.Fingerprint != "abc" || last.ToStyleID != 7 {
		t.Errorf("SaveFlow request = %+v, want the session's person as called_by and the body's fields", last)
	}
	rec.waitFor(t, []int64{1})
}

// TestFlowSave_TheHandlerChoosesTheSourceFromTheCaller (owner ruling R1): the
// SERVER decides authorship, and nothing in the body declares it. The route is
// behind login (TestRouter_FlowSaveIsBehindLogin) — only the desktop saves a
// flow — so the author is the session's person, stamped admin.
func TestFlowSave_TheHandlerChoosesTheSourceFromTheCaller(t *testing.T) {
	t.Parallel()
	h, router := newOperatorStationsRouter(t)
	db := dbOf(h)
	stub := h.orchestration.(*stubEngine)
	stub.flowSaveResult = &engine.FlowSaveResult{Fingerprint: "def", Written: 1}
	// A station on process 1 made here, in this test's own database
	// (newTestHandlers): no other test's station is in it.
	stationID, err := db.CreateOperatorStation(stations.Input{
		ProcessID: 1, Code: "flow-source", Name: "Press 400", Sequence: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	body := map[string]any{"to_style_id": 7, "cells": []any{}, "fingerprint": "abc", "station_id": stationID}

	// A person on the desktop saved it.
	cookie := authCookie(t, h)
	resp := doRequest(t, router, "POST", "/api/processes/1/flow/save", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	last := stub.flowSaveCalls[len(stub.flowSaveCalls)-1]
	if last.Source != domain.ClaimSourceAdmin {
		t.Errorf("an authenticated save stamped source %q, want %q — that is the desktop", last.Source, domain.ClaimSourceAdmin)
	}
	if last.CalledBy != "testadmin" {
		t.Errorf("called_by = %q, want the session user — who did it, not which screen they did it on", last.CalledBy)
	}

	// AND THE BODY CANNOT SAY OTHERWISE. A page declaring its own source is a
	// page deciding authorship, which is the thing the server stamp exists to
	// prevent — the same rule the routing origin is held to.
	lie := map[string]any{}
	for k, v := range body {
		lie[k] = v
	}
	lie["source"] = domain.ClaimSourceHMI
	lie["called_by"] = "not me"
	resp = doRequest(t, router, "POST", "/api/processes/1/flow/save", lie, cookie)
	assertStatus(t, resp, http.StatusOK)
	last = stub.flowSaveCalls[len(stub.flowSaveCalls)-1]
	if last.Source != domain.ClaimSourceAdmin || last.CalledBy != "testadmin" {
		t.Errorf("the body changed the authorship: source %q called_by %q", last.Source, last.CalledBy)
	}
}

// TestFlowSave_CarriesPresetProvenanceWhenTheBodyHasIt (U10): applying a
// preset is a normal save that also records where the shape came from.
//
// Pointer-gated, so the two halves of the pin are one sentence apart: a save
// that names a preset writes the two columns, and a save that does not LEAVES
// THEM UNTOUCHED — which is what lets an engineer edit a preset-applied flow
// by hand without the row forgetting it was ever applied, and what makes drift
// computable at all.
func TestFlowSave_CarriesPresetProvenanceWhenTheBodyHasIt(t *testing.T) {
	t.Parallel()
	h, router := newOperatorStationsRouter(t)
	db := dbOf(h)
	stub := h.orchestration.(*stubEngine)
	stub.flowSaveResult = &engine.FlowSaveResult{Fingerprint: "def", Written: 1}
	stationID, err := db.CreateOperatorStation(stations.Input{
		ProcessID: 1, Code: "flow-preset", Name: "Press 400", Sequence: 3, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	body := func(extra map[string]any) map[string]any {
		out := map[string]any{"to_style_id": 7, "cells": []any{}, "fingerprint": "abc",
			"station_id": stationID}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	// With provenance.
	resp := doRequest(t, router, "POST", "/api/processes/1/flow/save",
		body(map[string]any{"source_preset_id": 42, "source_preset_version": 3}), nil)
	assertStatus(t, resp, http.StatusOK)
	last := stub.flowSaveCalls[len(stub.flowSaveCalls)-1]
	if last.SourcePresetID == nil || *last.SourcePresetID != 42 {
		t.Errorf("SourcePresetID = %v, want 42", last.SourcePresetID)
	}
	if last.SourcePresetVersion == nil || *last.SourcePresetVersion != 3 {
		t.Errorf("SourcePresetVersion = %v, want 3", last.SourcePresetVersion)
	}

	// Without: nil, so the update does not touch the columns.
	resp = doRequest(t, router, "POST", "/api/processes/1/flow/save", body(nil), nil)
	assertStatus(t, resp, http.StatusOK)
	last = stub.flowSaveCalls[len(stub.flowSaveCalls)-1]
	if last.SourcePresetID != nil || last.SourcePresetVersion != nil {
		t.Errorf("a plain save spoke the provenance columns (%v, %v); it must leave them alone",
			last.SourcePresetID, last.SourcePresetVersion)
	}

	// A preset id with no version is refused rather than half-written: a row
	// that says "preset 42, version nothing" is provenance nobody can read.
	resp = doRequest(t, router, "POST", "/api/processes/1/flow/save",
		body(map[string]any{"source_preset_id": 42}), nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// TestFlowSave_AProcessWithNoScreenSavesItsFirstStyle: a style's flow is
// saved on the desktop by a person, and nothing in the save reads a station —
// so a process that has no operator screen yet saves one. The press is set up
// before anybody hangs a screen on it, and refusing the save until a screen
// exists made the screen a prerequisite of the setup it is part of.
//
// Through the real engine, so the claim is on the style afterwards and not
// merely handed to a stub. A station that IS named still has to be this
// process's: a body naming another process's screen is refused before the
// engine runs, and the style is left as it was.
func TestFlowSave_AProcessWithNoScreenSavesItsFirstStyle(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	pid, err := db.CreateProcess("NOSCREEN-PROC", "no operator screen", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	for i, name := range []string{"NOSCREEN-A", "NOSCREEN-B"} {
		id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: name, Name: name, Sequence: i + 1, Enabled: true})
		testutil.MustNoErr(t, err, "position "+name)
		_, err = db.EnsureProcessNodeRuntime(id)
		testutil.MustNoErr(t, err, "runtime "+name)
	}
	styleID, err := db.CreateStyle("NOSCREEN-STYLE", "", pid)
	testutil.MustNoErr(t, err, "create style")
	if screens, err := db.ListOperatorStationsByProcess(pid); err != nil || len(screens) != 0 {
		t.Fatalf("the fixture's process has screens %v (%v); this test is about one with none", screens, err)
	}
	otherPID, err := db.CreateProcess("NOSCREEN-OTHER", "", "", "", false)
	testutil.MustNoErr(t, err, "create other process")
	otherStation, err := db.CreateOperatorStation(stations.Input{ProcessID: otherPID, Code: "other-screen", Name: "Other Screen", Enabled: true})
	testutil.MustNoErr(t, err, "create other station")

	h, router := realFlowRouter(t, db)
	cookie := authCookie(t, h)
	cells := []domain.FlowCell{{
		CoreNodeName: "NOSCREEN-A", Role: "consume", SwapMode: "two_robot", PayloadCode: "PART-NOSCREEN",
		InboundStaging: "NOSCREEN-STG", InboundSource: "NOSCREEN-SRC", OutboundDestination: "NOSCREEN-DST",
	}}
	pv := doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/flow/preview",
		map[string]any{"to_style_id": styleID, "cells": cells}, cookie)
	fingerprint, _ := flowBody(t, pv)["fingerprint"].(string)
	if fingerprint == "" {
		t.Fatal("the preview returned no fingerprint; the save below would be refused as stale")
	}
	body := map[string]any{"to_style_id": styleID, "cells": cells, "fingerprint": fingerprint}

	// A station of ANOTHER process is still refused, and nothing is written.
	wrong := map[string]any{"station_id": otherStation}
	for k, v := range body {
		wrong[k] = v
	}
	resp := doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/flow/save", wrong, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	if msg, _ := flowBody(t, resp)["error"].(string); msg != "station Other Screen is not a station of this process" {
		t.Errorf("the refusal reads %q, want it to name the other process's screen", msg)
	}
	if got, err := db.ListStyleNodeClaims(styleID); err != nil || len(got) != 0 {
		t.Fatalf("a refused save left claims %v (%v)", got, err)
	}

	// No station at all: the save goes through.
	resp = doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/flow/save", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	if b := flowBody(t, resp); b["written"] != float64(1) {
		t.Errorf("200 body = %v, want one cell written", b)
	}
	saved, err := db.ListStyleNodeClaims(styleID)
	testutil.MustNoErr(t, err, "reload claims")
	if len(saved) != 1 || saved[0].CoreNodeName != "NOSCREEN-A" || saved[0].PayloadCode != "PART-NOSCREEN" {
		t.Fatalf("the style holds %+v, want the one cell the save sent", saved)
	}
	if saved[0].Source != domain.ClaimSourceAdmin || saved[0].CalledBy != "testadmin" {
		t.Errorf("the claim reads source %q called_by %q; the author is the session's person, screen or no screen",
			saved[0].Source, saved[0].CalledBy)
	}
}

// TestChangeoverStart_FlowFingerprintIsCheckedFirst: a stale flow_fingerprint
// is 409 stale and StartProcessChangeover (and so ClearPostCutoverFlag, its
// first step) never runs; a matching one starts; an absent one is today's
// behaviour — the existing start tests are untouched and still pass.
func TestChangeoverStart_FlowFingerprintIsCheckedFirst(t *testing.T) {
	t.Parallel()
	h, router := newOperatorStationsRouter(t)
	stub := h.orchestration.(*stubEngine)
	stub.flowFingerprint = "current"

	resp := doRequest(t, router, "POST", "/api/processes/1/changeover/start", map[string]any{"to_style_id": 7, "flow_fingerprint": "stale"}, nil)
	assertStatus(t, resp, http.StatusConflict)
	if b := flowBody(t, resp); b["stale"] != true {
		t.Errorf("stale start body = %v, want stale:true", b)
	}
	if stub.startCalls != 0 || stub.clearFlagCalls != 0 {
		t.Errorf("a stale start reached the engine: start %d, clear flag %d", stub.startCalls, stub.clearFlagCalls)
	}

	resp = doRequest(t, router, "POST", "/api/processes/1/changeover/start", map[string]any{"to_style_id": 7, "flow_fingerprint": "current"}, nil)
	assertStatus(t, resp, http.StatusOK)
	if stub.startCalls != 1 {
		t.Errorf("a matching fingerprint did not start: %d calls", stub.startCalls)
	}

	resp = doRequest(t, router, "POST", "/api/processes/1/changeover/start", map[string]any{"to_style_id": 7}, nil)
	assertStatus(t, resp, http.StatusOK)
	if stub.startCalls != 2 {
		t.Errorf("a start without a fingerprint did not start: %d calls", stub.startCalls)
	}

	stub.flowFingerprintErr = errors.New("sql: no rows in result set")
	resp = doRequest(t, router, "POST", "/api/processes/1/changeover/start", map[string]any{"to_style_id": 7, "flow_fingerprint": "current"}, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// publishRecorder hooks an existing Handlers' spec-change coalescer so a
// test can count per-process publishes — the publish-scale pin shape.
type publishRecorder struct {
	mu  sync.Mutex
	ids []int64
}

func recordSpecChangePublishes(t *testing.T, h *Handlers) *publishRecorder {
	t.Helper()
	rec := &publishRecorder{}
	h.specChangeCh = make(chan struct{}, 1)
	h.specChangeStop = make(chan struct{})
	h.SetPlantSpecChangeHook(
		func(processID int64) {
			rec.mu.Lock()
			rec.ids = append(rec.ids, processID)
			rec.mu.Unlock()
		},
		func() {
			rec.mu.Lock()
			rec.ids = append(rec.ids, specChangeAllMarker)
			rec.mu.Unlock()
		},
	)
	go h.specChangeLoop()
	t.Cleanup(func() { h.specChangeOnce.Do(func() { close(h.specChangeStop) }) })
	return rec
}

func (r *publishRecorder) calls() []int64 {
	time.Sleep(30 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.ids...)
}

func (r *publishRecorder) waitFor(t *testing.T, want []int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		n := len(r.ids)
		r.mu.Unlock()
		if n >= len(want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("publishes = %v, want exactly %v (-1 = whole plant)", got, want)
	}
}
