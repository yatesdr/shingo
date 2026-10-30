//go:build docker

package www

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
)

// Characterization tests for handlers_inventory.go —
//   - handleInventory: renders /inventory (HTML)
//   - handleInventoryCountHistory: renders /inventory/count-history (HTML)
//   - apiInventoryExport: GET /api/inventory/export streams an XLSX workbook

// --- handleInventory --------------------------------------------------------

// TestHandleInventory_RendersHTML pins the page-render path: the handler
// returns 200 with an HTML body containing the "Inventory" heading from
// inventory.html.
func TestHandleInventory_RendersHTML(t *testing.T) {
	t.Parallel()
	h, _ := testHandlersForPages(t)

	req := httptest.NewRequest(http.MethodGet, "/inventory", nil)
	rec := httptest.NewRecorder()
	h.handleInventory(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Errorf("expected HTML output, got prefix %q", body[:min(120, len(body))])
	}
	if !strings.Contains(body, "Inventory") {
		t.Errorf("expected 'Inventory' heading in body, not found; len=%d", len(body))
	}
}

// --- apiInventoryExport -----------------------------------------------------

// TestApiInventoryExport_ContentType pins the download headers: the handler
// should set Content-Type to the XLSX MIME and Content-Disposition to an
// attachment with filename "inventory.xlsx".
func TestApiInventoryExport_ContentType(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiInventoryExport, "/api/inventory/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body size=%d", rec.Code, rec.Body.Len())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "spreadsheetml.sheet") {
		t.Errorf("Content-Type: got %q, want .../spreadsheetml.sheet", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "inventory.xlsx") {
		t.Errorf("Content-Disposition: got %q", cd)
	}
}

// TestApiInventoryExport_BodyIsValidXLSX pins the body shape: it is a ZIP-like
// XLSX that excelize can re-open, with the expected Inventory sheet name and
// the expected header row.
func TestApiInventoryExport_BodyIsValidXLSX(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-EXPORT-1")

	rec := getPlain(t, h.apiInventoryExport, "/api/inventory/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	// Re-open the workbook with excelize to pin the sheet shape.
	f, err := excelize.OpenReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer f.Close()

	rows, err := f.GetRows("Inventory")
	if err != nil {
		t.Fatalf("get rows: %v", err)
	}
	if len(rows) < 1 {
		t.Fatalf("expected at least a header row, got %d", len(rows))
	}
	// Header row pinning: first column is "Group", bin-label is column E.
	want := []string{"Group", "Lane", "Node", "Zone", "Bin Label", "Bin Type", "Status"}
	for i, w := range want {
		if rows[0][i] != w {
			t.Errorf("header[%d]: got %q, want %q", i, rows[0][i], w)
		}
	}
	// The bin we seeded should appear in some data row under "Bin Label" (col E).
	foundBin := false
	for _, r := range rows[1:] {
		if len(r) >= 5 && r[4] == bin.Label {
			foundBin = true
		}
	}
	if !foundBin {
		t.Errorf("expected bin %q in exported rows, got %d rows", bin.Label, len(rows)-1)
	}
}

// --- apiInventoryInvariant (Item 13) ----------------------------------------

// TestApiInventoryInvariant_EmptyDB pins the zero case: with no bins
// or buckets, the endpoint returns Total=BinSum=BucketSum=0 plus a
// recent ComputedAt.
func TestApiInventoryInvariant_EmptyDB(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiInventoryInvariant, "/api/inventory/invariant")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got InventoryInvariant
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&got), "decode")
	if got.Total != 0 || got.BinSum != 0 || got.BucketSum != 0 {
		t.Errorf("empty DB: %+v, want all zero", got)
	}
	if got.ComputedAt.IsZero() {
		t.Errorf("ComputedAt is zero, want a real timestamp")
	}
}

// TestApiInventoryInvariant_ReflectsBinAndBucketSums pins the sum
// math: seed bins + lineside_buckets, hit the endpoint, assert each
// component reflects the seeded values and Total = BinSum + BucketSum.
func TestApiInventoryInvariant_ReflectsBinAndBucketSums(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	// Two bins: 30 + 40 = 70.
	if _, err := db.Exec(`UPDATE bins SET uop_remaining = 30 WHERE id = $1`,
		testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-INV-A").ID); err != nil {
		t.Fatalf("seed bin A: %v", err)
	}
	if _, err := db.Exec(`UPDATE bins SET uop_remaining = 40 WHERE id = $1`,
		testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-INV-B").ID); err != nil {
		t.Fatalf("seed bin B: %v", err)
	}
	// One active pile of 11, and a stranded row of 5 that does not count: the
	// invariant's bucket term is the active piles, as on-hand is.
	if _, err := db.Exec(`INSERT INTO lineside_buckets (station, core_node_name, payload_code, state, qty)
		VALUES ('STATION-INV', $1, 'PAY-INV', 'active', 11), ('STATION-INV', $1, 'PAY-INV', 'stranded', 5)`,
		sd.StorageNode.Name); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}

	rec := getPlain(t, h.apiInventoryInvariant, "/api/inventory/invariant")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var got InventoryInvariant
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&got), "decode")
	if got.BinSum != 70 {
		t.Errorf("BinSum = %d, want 70 (30 + 40)", got.BinSum)
	}
	if got.BucketSum != 11 {
		t.Errorf("BucketSum = %d, want 11 (the stranded 5 is not stock)", got.BucketSum)
	}
	if got.Total != 81 {
		t.Errorf("Total = %d, want 81 (70 + 11)", got.Total)
	}
}

// TestApiInventoryInvariant_ReportsNegativeBinSum pins the SME-locked
// signed-bin contract: a bin that overpacks past zero (uop_remaining
// = -3) shows up in BinSum as negative. Without this property,
// dashboards built against the endpoint would silently misreport
// inventory whenever overpack occurred.
func TestApiInventoryInvariant_ReportsNegativeBinSum(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	// Bin overpacked to -3 (one of the SME-lock examples).
	if _, err := db.Exec(`UPDATE bins SET uop_remaining = -3 WHERE id = $1`,
		testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-INV-NEG").ID); err != nil {
		t.Fatalf("seed bin: %v", err)
	}

	rec := getPlain(t, h.apiInventoryInvariant, "/api/inventory/invariant")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var got InventoryInvariant
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&got), "decode")
	if got.BinSum != -3 {
		t.Errorf("BinSum = %d, want -3 (signed; overpack must surface as negative)", got.BinSum)
	}
	if got.Total != -3 {
		t.Errorf("Total = %d, want -3 (signed total reflects negative bin sum)", got.Total)
	}
}

// --- cell helper ------------------------------------------------------------

// TestCellHelper pins the "A1"-style label generator used by the export path.
// This is a pure function but exported for the export code, so we lock it in.
func TestCellHelper(t *testing.T) {
	t.Parallel()
	cases := []struct {
		col  string
		row  int
		want string
	}{
		{"A", 1, "A1"},
		{"B", 2, "B2"},
		{"Z", 100, "Z100"},
	}
	for _, tc := range cases {
		if got := cell(tc.col, tc.row); got != tc.want {
			t.Errorf("cell(%q,%d): got %q, want %q", tc.col, tc.row, got, tc.want)
		}
	}
}

// The export's SECOND sheet had no assertion anywhere, which meant the whole
// lineside-bucket block could be moved, reordered or dropped without a test
// noticing. It ships in the same workbook operators open, so it gets the same
// header pinning the first sheet has.
func TestApiInventoryExport_LinesideBucketSheet(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	seedBucket(t, db, "STATION-EXP", sd.StorageNode.ID, "stranded", "PAY-EXP", 6)

	rec := getPlain(t, h.apiInventoryExport, "/api/inventory/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	f, err := excelize.OpenReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer f.Close()

	rows, err := f.GetRows("Lineside Buckets")
	if err != nil {
		t.Fatalf("the workbook has no \"Lineside Buckets\" sheet: %v", err)
	}
	if len(rows) < 1 {
		t.Fatal("lineside sheet has no header row")
	}
	want := []string{"Cell", "Process", "Station", "Node", "Zone", "Payload Code", "State", "Qty"}
	if len(rows[0]) != len(want) {
		t.Fatalf("lineside header has %d columns, want %d: %v", len(rows[0]), len(want), rows[0])
	}
	for i, w := range want {
		if rows[0][i] != w {
			t.Errorf("lineside header[%d]: got %q, want %q", i, rows[0][i], w)
		}
	}
	// A stranded row is labelled as the count anomaly it is.
	if len(rows) != 2 || len(rows[1]) < 7 || rows[1][6] != "count anomaly at cutover" {
		t.Errorf("lineside rows = %v, want one row whose State reads \"count anomaly at cutover\"", rows[1:])
	}
}

// --- apiInventoryLedgerExceptions: report divergences -----------------------

// TestApiInventoryLedgerExceptions_ListsOpenReportDivergences pins where the
// lineside checksum's findings are read (the owner ruled the placement: beside
// the delta-integrity panel on /inventory, in the same fetch). An open
// report_divergence episode is listed by bin with its class, epoch, the two
// counts and when it opened; a recovered one is not listed. Verify-red at the
// base: the response has no report_divergences key.
func TestApiInventoryLedgerExceptions_ListsOpenReportDivergences(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "BIN-DIVERGE")
	for _, recovered := range []string{"NULL", "now()"} {
		_, err := db.Exec(`INSERT INTO bin_uop_exception
			(kind, bin_id, payload_code, actor, occurred_at, op, recovered_at, detail)
			VALUES ('report_divergence', $1, $2, 'stn-page', now(), 'count', `+recovered+`,
			        jsonb_build_object('node', $3::text, 'edge_epoch', 4, 'core_epoch', 4,
			                           'edge_count', 10, 'core_count', 150))`,
			bin.ID, sd.Payload.Code, sd.LineNode.Name)
		testutil.MustNoErr(t, err, "seed divergence episode")
	}

	rec := getPlain(t, h.apiInventoryLedgerExceptions, "/api/inventory/ledger-exceptions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ReportDivergences []map[string]any `json:"report_divergences"`
	}
	testutil.MustNoErr(t, json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&body), "decode")
	if !strings.Contains(rec.Body.String(), `"report_divergences"`) {
		t.Fatalf("no report_divergences in the ledger-exceptions response: %s", rec.Body.String())
	}
	if len(body.ReportDivergences) != 1 {
		t.Fatalf("%d divergences listed, want 1 (the open one): %+v", len(body.ReportDivergences), body.ReportDivergences)
	}
	d := body.ReportDivergences[0]
	for key, want := range map[string]any{
		"class": "count", "bin_id": float64(bin.ID), "edge_epoch": float64(4),
		"edge_count": float64(10), "core_count": float64(150), "station": "stn-page",
		"node": sd.LineNode.Name, "payload_code": sd.Payload.Code,
	} {
		if d[key] != want {
			t.Errorf("%s = %v, want %v", key, d[key], want)
		}
	}
	if d["opened_at"] == nil || d["opened_at"] == "" {
		t.Error("opened_at is missing")
	}
}

// --- apiInventoryLedgerExceptions: hold drops and declared-full stamps ------

// TestApiInventoryLedgerExceptions_ListsHoldDropsAndStamps pins the two
// TestApiInventoryLedgerExceptions_HoldDropsAtALineRunningFull: a strict line
// at its pack with a full pack held drops a part a stroke (hold_cap) until a
// bin comes. Seeded as SPR's ALN_011 had it on 2026-10-29: two runs, 180 and
// 70 drops a minute apart, the runs an hour apart, all in the window.
func TestApiInventoryLedgerExceptions_HoldDropsAtALineRunningFull(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	seed := func(n int, startMinutesAgo int) {
		for i := 0; i < n; i++ {
			_, err := db.Exec(`INSERT INTO bin_uop_exception (kind, bin_id, payload_code, actor, occurred_at, op, detail)
				VALUES ('hold_dropped', NULL, $1, 'edge:tick', now() - make_interval(mins => $2), 'hold_cap',
				        jsonb_build_object('node', 'ALN_011', 'qty', 1, 'reason', 'hold_cap', 'station', 'stn-page',
				                           'msg_id', 'm' || $2::text))`,
				sd.Payload.Code, startMinutesAgo-i)
			testutil.MustNoErr(t, err, "seed hold drop")
		}
	}
	seed(180, 400) // 400..221 minutes ago
	seed(70, 150)  // 150..81 minutes ago

	rec := getPlain(t, h.apiInventoryLedgerExceptions, "/api/inventory/ledger-exceptions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		HoldDrops []map[string]any `json:"hold_drops"`
		Runs      []struct {
			Node   string `json:"node"`
			Reason string `json:"reason"`
			Drops  int64  `json:"drops"`
			Qty    int64  `json:"qty"`
		} `json:"hold_drop_runs"`
	}
	testutil.MustNoErr(t, json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&body), "decode")
	seen := fmt.Sprintf("rows=%d", len(body.HoldDrops))
	for _, r := range body.Runs {
		seen += fmt.Sprintf(" run=%s/%s:%d/%d", r.Node, r.Reason, r.Drops, r.Qty)
	}
	// It was one row per drop, newest first, cut at the limit: the page's head
	// read "200 hold drops" for 250, and 200 identical lines. Now two runs,
	// latest first, every drop counted.
	today := "rows=200"
	after := "rows=0 run=ALN_011/hold_cap:70/70 run=ALN_011/hold_cap:180/180"
	t.Logf("PIN inventory/hold-drops %s", seen)
	if seen != after {
		t.Errorf("hold drops at a line running full: got %s, want %s (today %s)", seen, after, today)
	}
}

// strict-pack lists the ledger fetch carries (lane P0): hold_drop_runs, the
// hold_dropped exceptions in the window as runs with their seat, part, reason,
// station, count and span; and declared_full_stamps, the release_declared_full ledger
// rows in the window with their seat, bin, counted and declared counts, who
// declared it and when. A row older than the window is not listed. Verify-red
// at the base: the response has neither key.
func TestApiInventoryLedgerExceptions_ListsHoldDropsAndStamps(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "BIN-STAMPED")
	for _, age := range []string{"1 hour", "30 days"} {
		_, err := db.Exec(`INSERT INTO bin_uop_exception (kind, bin_id, payload_code, actor, occurred_at, op, detail)
			VALUES ('hold_dropped', NULL, $1, 'edge:sweep', now() - $2::interval, 'sweep',
			        jsonb_build_object('node', 'ALN_011', 'qty', 24, 'reason', 'sweep', 'station', 'stn-page'))`,
			sd.Payload.Code, age)
		testutil.MustNoErr(t, err, "seed hold drop")
		_, err = db.Exec(`INSERT INTO bin_uop_ledger (bin_id, before_uop, after_uop, op, source, payload_code, actor, applied_at, detail)
			VALUES ($1, 131, 132, 'release_declared_full', 'test', $2, 'operator-7', now() - $3::interval,
			        jsonb_build_object('node', 'ALN_011', 'counted', 131, 'declared', 132, 'reason', 'no_pack'))`,
			bin.ID, sd.Payload.Code, age)
		testutil.MustNoErr(t, err, "seed stamp")
	}

	rec := getPlain(t, h.apiInventoryLedgerExceptions, "/api/inventory/ledger-exceptions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Runs   []map[string]any `json:"hold_drop_runs"`
		Stamps []map[string]any `json:"declared_full_stamps"`
	}
	testutil.MustNoErr(t, json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&body), "decode")
	for _, key := range []string{`"hold_drop_runs"`, `"declared_full_stamps"`} {
		if !strings.Contains(rec.Body.String(), key) {
			t.Fatalf("no %s in the ledger-exceptions response: %s", key, rec.Body.String())
		}
	}
	if len(body.Runs) != 1 {
		t.Fatalf("%d hold drop runs listed, want 1 (the one drop in the window): %+v", len(body.Runs), body.Runs)
	}
	for key, want := range map[string]any{
		"node": "ALN_011", "payload_code": sd.Payload.Code, "drops": float64(1), "qty": float64(24), "reason": "sweep",
		"station": "stn-page",
	} {
		if body.Runs[0][key] != want {
			t.Errorf("hold drop run %s = %v, want %v", key, body.Runs[0][key], want)
		}
	}
	if body.Runs[0]["first"] == nil || body.Runs[0]["first"] != body.Runs[0]["last"] {
		t.Errorf("a one-drop run spans one moment: first %v last %v", body.Runs[0]["first"], body.Runs[0]["last"])
	}
	if len(body.Stamps) != 1 {
		t.Fatalf("%d stamps listed, want 1 (the one in the window): %+v", len(body.Stamps), body.Stamps)
	}
	for key, want := range map[string]any{
		"node": "ALN_011", "bin_id": float64(bin.ID), "bin_label": "BIN-STAMPED", "payload_code": sd.Payload.Code,
		"counted": float64(131), "declared": float64(132), "actor": "operator-7", "reason": "no_pack",
	} {
		if body.Stamps[0][key] != want {
			t.Errorf("stamp %s = %v, want %v", key, body.Stamps[0][key], want)
		}
	}
	if body.Stamps[0]["applied_at"] == nil || body.Stamps[0]["applied_at"] == "" {
		t.Error("stamp applied_at is missing")
	}
}

// TestHandleInventory_PageShape pins the server-rendered parts of the two
// pages, which are two tabs of one (owner, 2026-10-30): Inventory and Count
// history, the active tab marked. /inventory is two tables (supermarket
// positions, lines) with the doubt line above them, and none of what left it,
// the Bins link included; /inventory/count-history carries the count ledger
// row and the flagged-carriers modal shell.
func TestHandleInventory_PageShape(t *testing.T) {
	t.Parallel()
	h, _ := testHandlersForPages(t)
	rec := httptest.NewRecorder()
	h.handleInventory(rec, httptest.NewRequest(http.MethodGet, "/inventory", nil))
	body := rec.Body.String()
	for _, want := range []string{`id="loop-doubt"`, `id="pos-body"`, `id="line-body"`, `id="loop-calc"`,
		`<a class="tab active" href="/inventory" aria-current="page">Inventory</a>`,
		`<a class="tab" href="/inventory/count-history">Count history</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("/inventory has no %s", want)
		}
	}
	// Gone from /inventory on 2026-10-29: the KPI strip, the ledger panels,
	// the keeper's levels, the piles, the tick feed — and earlier the Cell
	// column and the consumption drill.
	for _, gone := range []string{
		`id="inv-kpi"`, `class="ledger-row"`, `id="mg"`, "Lineside Buckets", `id="tick-feed"`,
		"<th>Cell</th>", `data-action="drillRange:14"`, "Consumption &amp; cover",
		"In-loop UoP against the UoP threshold", `id="loop-body"`, `href="/bins"`, `class="inv-title"`, "tab-btn",
	} {
		if strings.Contains(body, gone) {
			t.Errorf("%q is still on the inventory page", gone)
		}
	}

	rec = httptest.NewRecorder()
	h.handleInventoryCountHistory(rec, httptest.NewRequest(http.MethodGet, "/inventory/count-history", nil))
	body = rec.Body.String()
	if !strings.Contains(body, `<a class="tab" href="/inventory">Inventory</a>`) ||
		!strings.Contains(body, `<a class="tab active" href="/inventory/count-history" aria-current="page">Count history</a>`) {
		t.Error("count history does not carry the two tabs, Count history active")
	}
	if !strings.Contains(body, `class="ledger-row"`) || !strings.Contains(body, `id="rh-hold-drops"`) {
		t.Error("the count ledger row is not on /inventory/count-history")
	}
	if !strings.Contains(body, `id="inv-drill"`) || !strings.Contains(body, "Carriers flagged for a recount") {
		t.Error("the flagged-carriers modal shell is not on /inventory/count-history")
	}
	// Moved off count history (owner, 2026-10-29): the keeper's levels to the
	// Nodes page's maintained group, the lineside piles to the Bins page.
	for _, gone := range []string{`id="mg"`, "Maintained Groups", "Lineside Buckets", "/api/buckets", "maintained-groups"} {
		if strings.Contains(body, gone) {
			t.Errorf("%q is still on /inventory/count-history", gone)
		}
	}
	rec = httptest.NewRecorder()
	h.handleNodes(rec, httptest.NewRequest(http.MethodGet, "/nodes", nil))
	if !strings.Contains(rec.Body.String(), `id="keeper-levels"`) {
		t.Error("the keeper's levels block is not on the Nodes page")
	}
}

// THE BANNER AND THE PANEL COUNT DIFFERENT THINGS. The banner's figure is
// carriers flagged anomaly_at (live, no window); the panel's is dropped-delta
// ledger rows since seven days ago. A carrier with three drops and an unflagged
// carrier with two make 1 against 5 — neither is wrong.
func TestInventory_RejectedAndDroppedCountDifferentPopulations(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	bt := &bins.BinType{Code: "DROPS-UI", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	flagged := binsAt(t, db, bt, "FLAGGED-DROPS")
	quiet := binsAt(t, db, bt, "UNFLAGGED-DROPS")
	_, err := db.Exec(`UPDATE bins SET anomaly_at = NOW() WHERE id = $1`, flagged.ID)
	testutil.MustNoErr(t, err, "flag")
	drop := func(binID int64, n int) {
		for i := 0; i < n; i++ {
			_, err := db.Exec(`INSERT INTO bin_uop_ledger
				(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, applied_at)
				VALUES ($1, 0, 0, 'stale_epoch_dropped', 'test', 'PART-DROP', 'test', '{"delta":-1}'::jsonb, NOW() - INTERVAL '1 hour')`,
				binID)
			testutil.MustNoErr(t, err, "seed drop")
		}
	}
	drop(flagged.ID, 3)
	drop(quiet.ID, 2)

	var sum struct {
		RejectedDeltaBins int `json:"rejected_delta_bins"`
	}
	rec := getPlain(t, h.apiInventoryAnomalySummary, "/api/inventory/anomaly-summary")
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &sum), "decode summary")

	var lx struct {
		DeltaIntegrity []struct {
			DropRows int `json:"drop_rows"`
		} `json:"delta_integrity"`
	}
	rec = getPlain(t, h.apiInventoryLedgerExceptions, "/api/inventory/ledger-exceptions")
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &lx), "decode ledger exceptions")
	drops := 0
	for _, r := range lx.DeltaIntegrity {
		drops += r.DropRows
	}

	if sum.RejectedDeltaBins != 1 || drops != 5 {
		t.Errorf("banner figure %d (want 1 flagged carrier), panel figure %d (want 5 dropped deltas)",
			sum.RejectedDeltaBins, drops)
	}
}
