// Edge↔Core HTTP contract round-trip — the real Edge CoreClient against the
// real Core handlers (www.NewRouter), over httptest, against a real Postgres.
//
// Lane C1 moved the contract types into shingo/protocol; this is the proof
// both sides still speak the same language. Every endpoint the Edge calls on
// Core's public /api group is exercised once (three for the manifest, whose
// arms answer in different shapes), through the same CoreClient the plant
// runs, against the same handlers the plant runs.
//
// BYTE CAPTURE. The recorder tees every request/response pair at the HTTP
// layer (gunzipping what the Compress middleware produced), so a run before
// and a run after a contract change can be diffed key-for-key. Set
// C1_CAPTURE to a directory to write the capture file; the sha256 in each
// entry covers the exact response bytes including the trailing newline
// json.Encoder appends. Response-body byte identity per endpoint is the
// bar the move was held to; request bodies are captured for the record
// (request key ORDER may differ map→struct, which is decode-equivalent).
//
//go:build docker

package scenarios

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"shingo/protocol/debuglog"
	coreconfig "shingocore/config"
	"shingocore/domain"
	coreengine "shingocore/engine"
	"shingocore/fleet/simulator"
	"shingocore/store"
	coreharness "shingocore/testharness"
	"shingocore/www"
	edgeengine "shingoedge/engine"
)

// capturedCall is one request/response pair seen by the recorder.
type capturedCall struct {
	method   string
	path     string
	status   int
	reqBody  []byte
	respBody []byte // logical body: gunzipped when the Compress middleware fired
}

// callRecorder tees the traffic between CoreClient and the Core router.
type callRecorder struct {
	mu    sync.Mutex
	calls []capturedCall
}

func (cr *callRecorder) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody []byte
		if r.Body != nil {
			reqBody, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
		}
		tw := &teeWriter{ResponseWriter: w}
		next.ServeHTTP(tw, r)
		body := tw.buf.Bytes()
		if strings.Contains(tw.Header().Get("Content-Encoding"), "gzip") {
			if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
				if logical, err := io.ReadAll(zr); err == nil {
					body = logical
				}
				_ = zr.Close()
			}
		}
		cr.mu.Lock()
		cr.calls = append(cr.calls, capturedCall{
			method: r.Method, path: r.URL.Path, status: tw.status,
			reqBody: reqBody, respBody: body,
		})
		cr.mu.Unlock()
	})
}

// teeWriter copies everything the Core stack writes, so the capture sees the
// bytes as they left the handler stack.
type teeWriter struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
}

func (t *teeWriter) WriteHeader(code int) {
	t.status = code
	t.ResponseWriter.WriteHeader(code)
}

func (t *teeWriter) Write(b []byte) (int, error) {
	if t.status == 0 {
		t.status = http.StatusOK
	}
	t.buf.Write(b)
	return t.ResponseWriter.Write(b)
}

func (t *teeWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// startCoreStack brings up the real Core (Postgres + engine + www.NewRouter)
// behind an httptest server, and points a real Edge CoreClient at it.
func startCoreStack(t *testing.T) (*store.DB, *edgeengine.CoreClient, *callRecorder) {
	t.Helper()
	db := coreharness.OpenDB(t)

	cfg := coreconfig.Defaults()
	cfg.Messaging.StationID = "c1-roundtrip"
	eng := coreengine.New(coreengine.Config{
		AppConfig: cfg,
		DB:        db,
		Fleet:     simulator.New(),
		MsgClient: nil,
		LogFunc:   t.Logf,
	})
	eng.Start()
	t.Cleanup(eng.Stop)

	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	router, stopRouter, err := www.NewRouter(eng, dbg)
	if err != nil {
		t.Fatalf("core router: %v", err)
	}
	t.Cleanup(stopRouter)

	rec := &callRecorder{}
	srv := httptest.NewServer(rec.middleware(router))
	t.Cleanup(srv.Close)
	return db, edgeengine.NewCoreClient(srv.URL), rec
}

func TestHTTPContractRoundTrip(t *testing.T) {
	db, client, rec := startCoreStack(t)

	// ── Fixtures (identical sequence on every run — the capture is only
	// comparable across trees because the fixture sequence is fixed) ──
	sd := coreharness.SetupStandardData(t, db)
	bin := coreharness.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RT-1")

	// Manifest arm 1 (normal): PART-A with one template line and a declared
	// carrier (payload_bin_types → bin_type_code in the answer).
	item := &domain.PayloadManifestItem{
		PayloadID: sd.Payload.ID, PartNumber: "RT-PART", PartsPerCycle: 2, Description: "rt",
	}
	if err := db.CreatePayloadManifestItem(item, ""); err != nil {
		t.Fatalf("manifest item: %v", err)
	}
	if err := db.SetPayloadBinTypes(sd.Payload.ID, []int64{sd.BinType.ID}); err != nil {
		t.Fatalf("payload bin types: %v", err)
	}
	// Manifest arm 2 (fallback): a payload with no template rows answers the
	// single self-referential line, with bin_type_code "".
	if err := db.CreatePayload(&coreharness.Payload{Code: "PART-BARE", Description: "bare rt", UOPCapacity: 50}); err != nil {
		t.Fatalf("PART-BARE: %v", err)
	}
	// Manifest arm 3 (unknown): a code Core has no payload for.
	if err := db.CreatePayload(&coreharness.Payload{Code: "PART-NOBIN", Description: "no bins", UOPCapacity: 10}); err != nil {
		t.Fatalf("PART-NOBIN: %v", err)
	}
	// node-children: an NGRP with one physical child.
	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("NGRP type: %v", err)
	}
	grp := &coreharness.Node{Name: "RT-GRP", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("RT-GRP: %v", err)
	}
	child := &coreharness.Node{Name: "RT-CHILD", Enabled: true, ParentID: &grp.ID}
	if err := db.CreateNode(child); err != nil {
		t.Fatalf("RT-CHILD: %v", err)
	}

	// ── 1. node-bins: one occupied storage window, one empty line node ──
	rows, ok, err := client.FetchNodeBins([]string{sd.StorageNode.Name, sd.LineNode.Name})
	if err != nil || !ok {
		t.Fatalf("FetchNodeBins: ok=%v err=%v", ok, err)
	}
	if len(rows) != 2 {
		t.Fatalf("node-bins rows: got %d, want 2 (%+v)", len(rows), rows)
	}
	if rows[0].NodeName != sd.StorageNode.Name || !rows[0].Occupied ||
		rows[0].BinID != bin.ID || rows[0].PayloadCode != sd.Payload.Code || rows[0].UOPRemaining != 100 {
		t.Errorf("storage row = %+v, want occupied BIN-RT-1 holding PART-A at 100 UOP", rows[0])
	}
	if rows[1].NodeName != sd.LineNode.Name || rows[1].Occupied {
		t.Errorf("line row = %+v, want empty LINE1-IN", rows[1])
	}

	// ── 2-4. manifest: normal, fallback (no template), unknown code ──
	normal, err := client.FetchPayloadManifest(sd.Payload.Code)
	if err != nil || normal == nil {
		t.Fatalf("manifest normal: %v %+v", err, normal)
	}
	if normal.UOPCapacity != 1000 || normal.BinTypeCode != sd.BinType.Code || len(normal.Items) != 1 ||
		normal.Items[0].PartNumber != "RT-PART" || normal.Items[0].PartsPerCycle != 2 || normal.Items[0].Description != "rt" {
		t.Errorf("manifest normal = %+v, want capacity 1000, type DEFAULT, one RT-PART line ratio 2", normal)
	}
	fallback, err := client.FetchPayloadManifest("PART-BARE")
	if err != nil || fallback == nil {
		t.Fatalf("manifest fallback: %v %+v", err, fallback)
	}
	if fallback.UOPCapacity != 50 || fallback.BinTypeCode != "" || len(fallback.Items) != 1 ||
		fallback.Items[0].PartNumber != "PART-BARE" || fallback.Items[0].PartsPerCycle != 1 {
		t.Errorf("manifest fallback = %+v, want the self-referential single line, no bin type", fallback)
	}
	unknown, err := client.FetchPayloadManifest("PART-NOSUCH")
	if err != nil || unknown == nil {
		t.Fatalf("manifest unknown: %v %+v", err, unknown)
	}
	if unknown.UOPCapacity != 0 || len(unknown.Items) != 0 {
		t.Errorf("manifest unknown = %+v, want zero capacity and EMPTY items ([] not null)", unknown)
	}

	// ── 5. node-children: the NGRP's physical child, dot-named ──
	children, err := client.FetchNodeChildren(grp.Name, false)
	if err != nil {
		t.Fatalf("FetchNodeChildren: %v", err)
	}
	if len(children) != 1 || children[0].Name != "RT-GRP.RT-CHILD" {
		t.Errorf("children = %+v, want exactly RT-GRP.RT-CHILD", children)
	}

	// ── 6. bin-load: declare 42, with a manifest line ──
	fortyTwo := int64(42)
	loadResp, err := client.LoadBin(&edgeengine.BinLoadRequest{
		NodeName:    sd.StorageNode.Name,
		PayloadCode: sd.Payload.Code,
		UOPCount:    &fortyTwo,
		Manifest:    []edgeengine.BinLoadItem{{PartNumber: "RT-PART", Quantity: 84, Description: "rt"}},
	})
	if err != nil {
		t.Fatalf("LoadBin: %v", err)
	}
	if loadResp.Status != "ok" || loadResp.BinID != bin.ID || loadResp.BinLabel != "BIN-RT-1" ||
		loadResp.PayloadCode != sd.Payload.Code || loadResp.UOPRemaining != 42 || loadResp.DeltaEpoch == 0 {
		t.Errorf("bin-load reply = %+v, want ok on BIN-RT-1 holding PART-A at 42 with a fresh epoch", loadResp)
	}
	epochAfterLoad := loadResp.DeltaEpoch

	// ── 7. bin-count: 42 declared against 42 expected — no discrepancy ──
	countResp, err := client.RecordBinCount(sd.StorageNode.Name, 42, "rt-actor")
	if err != nil {
		t.Fatalf("RecordBinCount: %v", err)
	}
	if countResp.Status != "ok" || countResp.Expected != 42 || countResp.UOPRemaining != 42 ||
		countResp.Discrepancy || countResp.Warning != "" {
		t.Errorf("bin-count reply = %+v, want 42-vs-42 with no discrepancy and no warning", countResp)
	}
	if countResp.AsOfNet != nil || countResp.AsOfSeq != nil || countResp.AsOfStation != "" {
		t.Errorf("bin-count fence = %v/%v/%q, want nil/nil/\"\" — no station cursor exists in this fixture", countResp.AsOfNet, countResp.AsOfSeq, countResp.AsOfStation)
	}

	// ── 8. bin-clear without a code (payload held → cleared_* filled) ──
	clearPlain, err := client.ClearBin(sd.StorageNode.Name, "")
	if err != nil {
		t.Fatalf("ClearBin plain: %v", err)
	}
	if clearPlain.Status != "ok" || clearPlain.BinID != bin.ID ||
		clearPlain.ClearedPayloadCode != sd.Payload.Code || clearPlain.ClearedBinTypeCode != sd.BinType.Code ||
		clearPlain.DeltaEpoch <= epochAfterLoad {
		t.Errorf("bin-clear plain = %+v, want BIN-RT-1 cleared from PART-A/DEFAULT with a newer epoch", clearPlain)
	}

	// ── 9-10. load again, then the typed clear (bin_type_code path) ──
	if _, err := client.LoadBin(&edgeengine.BinLoadRequest{
		NodeName:    sd.StorageNode.Name,
		PayloadCode: sd.Payload.Code,
		UOPCount:    &fortyTwo,
	}); err != nil {
		t.Fatalf("LoadBin (re-load for typed clear): %v", err)
	}
	clearTyped, err := client.ClearBin(sd.StorageNode.Name, sd.BinType.Code)
	if err != nil {
		t.Fatalf("ClearBin typed: %v", err)
	}
	if clearTyped.Status != "ok" || clearTyped.BinID != bin.ID ||
		clearTyped.ClearedPayloadCode != sd.Payload.Code || clearTyped.ClearedBinTypeCode != sd.BinType.Code {
		t.Errorf("bin-clear typed = %+v, want the same carrier re-typed to DEFAULT", clearTyped)
	}

	// ── 11. load once more so preflight sees sourceable stock ──
	loadFinal, err := client.LoadBin(&edgeengine.BinLoadRequest{
		NodeName:    sd.StorageNode.Name,
		PayloadCode: sd.Payload.Code,
		UOPCount:    &fortyTwo,
	})
	if err != nil {
		t.Fatalf("LoadBin (final): %v", err)
	}

	// ── 12. preflight: one stocked payload, one with no bins anywhere ──
	preflight, err := client.PreflightInventory("RT-STATION", []string{sd.Payload.Code, "PART-NOBIN"})
	if err != nil {
		t.Fatalf("PreflightInventory: %v", err)
	}
	if len(preflight.Missing) != 1 || preflight.Missing[0] != "PART-NOBIN" ||
		len(preflight.Absent) != 1 || preflight.Absent[0] != "PART-NOBIN" || !preflight.AbsentKnown {
		t.Errorf("preflight missing/absent = %v/%v (known=%v), want PART-NOBIN in both", preflight.Missing, preflight.Absent, preflight.AbsentKnown)
	}
	if len(preflight.Available) != 2 ||
		preflight.Available[0].PayloadCode != sd.Payload.Code || preflight.Available[0].BinCount != 1 ||
		preflight.Available[1].PayloadCode != "PART-NOBIN" || preflight.Available[1].BinCount != 0 {
		t.Errorf("preflight available = %+v, want PART-A:1 and PART-NOBIN:0 in request order", preflight.Available)
	}

	// ── 13. system-count: bins in the kanban loop, request order ──
	counts, ok := client.SystemBinCount([]string{sd.Payload.Code, "PART-NOBIN"})
	if !ok {
		t.Fatalf("SystemBinCount: not ok")
	}
	if len(counts) != 2 ||
		counts[0].PayloadCode != sd.Payload.Code || counts[0].BinCount != 1 ||
		counts[1].PayloadCode != "PART-NOBIN" || counts[1].BinCount != 0 {
		t.Errorf("system-count = %+v, want PART-A:1 and PART-NOBIN:0", counts)
	}
	_ = loadFinal

	// ── The traffic seen is exactly the calls above, in order ──
	want := []struct{ label, method, path string }{
		{"node-bins", http.MethodGet, "/api/telemetry/node-bins"},
		{"manifest-normal", http.MethodGet, "/api/telemetry/payload/" + sd.Payload.Code + "/manifest"},
		{"manifest-fallback", http.MethodGet, "/api/telemetry/payload/PART-BARE/manifest"},
		{"manifest-unknown", http.MethodGet, "/api/telemetry/payload/PART-NOSUCH/manifest"},
		{"node-children", http.MethodGet, "/api/telemetry/node/" + grp.Name + "/children"},
		{"bin-load-declared", http.MethodPost, "/api/telemetry/bin-load"},
		{"bin-count", http.MethodPost, "/api/telemetry/bin-count"},
		{"bin-clear-plain", http.MethodPost, "/api/telemetry/bin-clear"},
		{"bin-load-retyped", http.MethodPost, "/api/telemetry/bin-load"},
		{"bin-clear-typed", http.MethodPost, "/api/telemetry/bin-clear"},
		{"bin-load-final", http.MethodPost, "/api/telemetry/bin-load"},
		{"preflight", http.MethodPost, "/api/inventory/preflight"},
		{"system-count", http.MethodPost, "/api/inventory/system-count"},
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != len(want) {
		t.Fatalf("recorded %d calls, want %d: %+v", len(rec.calls), len(want), rec.calls)
	}
	for i, w := range want {
		c := rec.calls[i]
		if c.method != w.method || c.path != w.path {
			t.Errorf("call %d = %s %s, want %s %s (%s)", i, c.method, c.path, w.method, w.path, w.label)
		}
		if c.status != http.StatusOK {
			t.Errorf("call %d (%s): status %d, want 200", i, w.label, c.status)
		}
	}
	if dir := os.Getenv("C1_CAPTURE"); dir != "" {
		writeCapture(t, dir, rec.calls, want)
	}
}

// writeCapture dumps every call's exact bytes (request and logical response)
// one labelled block per endpoint, for diffing a before/after pair of runs.
func writeCapture(t *testing.T, dir string, calls []capturedCall, labels []struct{ label, method, path string }) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("capture dir: %v", err)
	}
	var b strings.Builder
	for i, c := range calls {
		fmt.Fprintf(&b, "=== %02d %s %s %s status=%d\n", i, labels[i].label, c.method, c.path, c.status)
		fmt.Fprintf(&b, "REQ sha256=%s %s\n", shaHex(c.reqBody), strings.TrimRight(string(c.reqBody), "\n"))
		fmt.Fprintf(&b, "RES sha256=%s %s\n", shaHex(c.respBody), strings.TrimRight(string(c.respBody), "\n"))
	}
	path := filepath.Join(dir, "capture.txt")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write capture: %v", err)
	}
	t.Logf("capture written to %s", path)
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// keep the json import meaningful for future field-level assertions without
// inviting an unused-import cycle on edit.
var _ = json.Marshal
