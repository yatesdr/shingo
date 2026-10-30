package www

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/xuri/excelize/v2"
	"shingo/protocol/clock"
	"shingocore/domain"
)

// InventoryInvariant is the Item 13 plant-wide running totals shape.
// BinSum is signed because the SME lock allows bins to go negative
// (overpack/underpack); over time the signed sum drifts in either
// direction as production smooths out, useful as a trend indicator
// rather than a hard equation. BucketSum is the active piles only (the
// ones on-hand counts) and stays non-negative by schema CHECK constraint. Total = BinSum + BucketSum, so dashboards
// can present either the components or the rolled-up plant total.
type InventoryInvariant struct {
	Total      int64     `json:"total"`
	BinSum     int64     `json:"bin_sum"`    // signed; can be negative per SME lock
	BucketSum  int64     `json:"bucket_sum"` // always >= 0
	ComputedAt time.Time `json:"computed_at"`
}

// apiInventoryInvariant returns the plant-wide running totals as JSON.
// Item 13 invariant probe — dashboards verify the signed sum stays
// approximately stable (overpack/underpack wash out at the aggregate
// level). The handler returns sums regardless of sign — clients must
// not assume non-negative bin_sum.
func (h *Handlers) apiInventoryInvariant(w http.ResponseWriter, r *http.Request) {
	inv, err := h.engine.InventoryDeltaService().SumInvariant()
	if err != nil {
		h.jsonError(w, "sum invariant: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, InventoryInvariant{
		Total:      inv.Total,
		BinSum:     inv.BinSum,
		BucketSum:  inv.BucketSum,
		ComputedAt: time.Now().UTC(),
	})
}

// handleInventory renders /inventory: two tables (supermarket positions and
// lines), filled by apiInventoryLoop.
func (h *Handlers) handleInventory(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "inventory.html", map[string]any{"Page": "inventory"})
}

// handleInventoryCountHistory renders /inventory/count-history: the count
// ledger lists that left /inventory (2026-10-29), moved as they were, with the
// production tick feed panel (partials/tick-feed-panel.html). A failed feed
// read renders the panel's empty state rather than failing the page.
func (h *Handlers) handleInventoryCountHistory(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Page": "inventory"}
	if feed, err := h.engine.HeartbeatService().TickFeedStatus(clock.Now().UTC()); err != nil {
		log.Printf("count history: tick feed status: %v", err)
	} else {
		data["TickFeed"] = feed
	}
	h.render(w, r, "count-history.html", data)
}

// apiInventoryLoop is /inventory's one read (service.InventoryService.Loop):
// every (loader position, part) the threshold monitor watches with its in-loop
// UoP, threshold, provenance, cover, the calculator's inputs and coming; every
// claim of a running style at a line, consume and produce, with its bin, time
// left and coming; whether each row needs a look; and the bins that make a sum
// doubtful.
func (h *Handlers) apiInventoryLoop(w http.ResponseWriter, r *http.Request) {
	view, err := h.engine.InventoryService().Loop(r.Context(), clock.Now(), h.engine.AppConfig().Sourceability.RateWindow)
	if err != nil {
		h.jsonError(w, "inventory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, view)
}

// apiInventoryLedgerExceptions is /inventory/count-history's count ledger:
// bins whose UOP is negative right now, the payloads whose plant-wide total is
// negative, the dropped deltas per payload beside each payload's ledger total,
// the open report divergences, and the strict-pack lists (hold drop runs,
// declared-full stamps). The zero-crossing excursions ride along as API
// forensics; no page draws them.
//
// BLANK ON A GOOD DAY, the exception lists. That is the design: a page of
// charts computed from a handful of points is worse than nothing, and a list
// that is empty until something is wrong is the right instrument.
//
// Read-side only — every value comes from bins, bin_uop_ledger and
// bin_uop_exception (the open report_divergence episodes the lineside report's
// comparison records), which are already on disk. No new table.
//
// ?since=<RFC3339> and ?limit= bound the excursions, the dropped deltas and the
// strict-pack lists (default 7 days, 200 rows): the lists are routine records
// rather than faults, so the
// page shows them as lists with a count, and a quiet line when the window
// holds none. hold_drop_runs counts every drop in the window; the limit bounds
// the runs, never the count.
func (h *Handlers) apiInventoryLedgerExceptions(w http.ResponseWriter, r *http.Request) {
	openBins, err := h.engine.OpenNegativeBins()
	if err != nil {
		h.jsonError(w, "open negative bins: "+err.Error(), http.StatusInternalServerError)
		return
	}
	payloads, err := h.engine.NegativeLedgerPayloads()
	if err != nil {
		h.jsonError(w, "negative ledger payloads: "+err.Error(), http.StatusInternalServerError)
		return
	}

	since := clock.Now().UTC().AddDate(0, 0, -7)
	if v := r.URL.Query().Get("since"); v != "" {
		if t, perr := time.Parse(time.RFC3339, v); perr == nil {
			since = t.UTC()
		}
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, cerr := strconv.Atoi(v); cerr == nil && n > 0 {
			limit = n
		}
	}

	// A release and the delta it races are seconds apart, so the window only
	// needs to be wide enough to survive queue latency.
	excursions, err := h.engine.NegativeLedgerExcursions(since, 5*time.Minute, limit)
	if err != nil {
		h.jsonError(w, "negative ledger excursions: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Delta integrity rides the same request and the same window. It belongs
	// beside the exception list, not on its own endpoint: the exception list
	// says "this bin is negative" and this says "and here is the mechanism
	// that probably did it", and the two are only worth anything read
	// together. One fetch means they cannot drift apart in the UI either.
	deltaIntegrity, err := h.engine.DeltaIntegrityByPayload(since)
	if err != nil {
		h.jsonError(w, "delta integrity: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// The lineside checksum's open findings, beside the delta panel (the owner
	// ruled the placement). The delta panel says which counts never landed;
	// this says which carriers the Edge and Core still disagree about, which is
	// where a count that never landed shows up next. Open episodes only: a
	// recovered one is history, and the page is blank on a good day.
	divergences, err := h.engine.InventoryService().OpenReportDivergences()
	if err != nil {
		h.jsonError(w, "report divergences: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// The strict-pack lists, in the same window: the produce holds the Edges
	// dropped, and the bins a station declared full above their count. Read
	// together: a dropped hold makes the next bin short and that bin is then
	// stamped, so both rows can describe the same parts, which is why every
	// row carries its node and time. Routine lists, not alarms.
	holdDropRuns, err := h.engine.InventoryService().HoldDropRuns(since, limit)
	if err != nil {
		h.jsonError(w, "hold drops: "+err.Error(), http.StatusInternalServerError)
		return
	}
	stamps, err := h.engine.InventoryService().DeclaredFullStamps(since, limit)
	if err != nil {
		h.jsonError(w, "declared-full stamps: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if openBins == nil {
		openBins = []domain.OpenNegativeBin{}
	}
	if excursions == nil {
		excursions = []domain.NegativeExcursion{}
	}
	if deltaIntegrity == nil {
		deltaIntegrity = []domain.DeltaIntegrity{}
	}
	h.jsonOK(w, map[string]any{
		"since":                since,
		"open_bins":            openBins,
		"negative_payloads":    payloads,
		"excursions":           excursions,
		"delta_integrity":      deltaIntegrity,
		"report_divergences":   divergences,
		"hold_drop_runs":       holdDropRuns,
		"declared_full_stamps": stamps,
	})
}

// apiSourceabilityEvents serves the persisted sourceability verdict-change
// history: when a (process, style) stopped being sourceable, what it was
// missing, and when it recovered.
//
// The monitor has computed this diff every two minutes since it was written
// and never wrote it down, so the root physical condition of 2026-07-21 —
// zero system stock on SYN-PART07A.06 — was known continuously and recorded
// nowhere.
//
// ?since=<RFC3339> (default 7 days), ?process=, ?payload=, ?limit=.
func (h *Handlers) apiSourceabilityEvents(w http.ResponseWriter, r *http.Request) {
	since := clock.Now().UTC().AddDate(0, 0, -7)
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			since = t.UTC()
		}
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := h.engine.SourceabilityEvents(since,
		r.URL.Query().Get("process"), r.URL.Query().Get("payload"), limit)
	if err != nil {
		h.jsonError(w, "sourceability events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []domain.SourceabilityEvent{}
	}
	h.jsonOK(w, map[string]any{"since": since, "events": rows})
}

// apiInventoryAnomalySummary returns the read-only rejected-delta + stale-staged
// rollup behind count history's alerts banner (P2-C6): reason-split drop
// counters, the count of anomaly-flagged bins whose deltas are being refused, and
// the count of bins parked staged past their own TTL. Pure observability — no
// behavior change, safe to poll.
func (h *Handlers) apiInventoryAnomalySummary(w http.ResponseWriter, r *http.Request) {
	sum, err := h.engine.InventoryDeltaService().AnomalySummary()
	if err != nil {
		h.jsonError(w, "anomaly summary: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, sum)
}

// apiInventoryRejectedDeltas is the drill-down behind count history's
// "N carriers flagged for a recount" banner: the list of carriers whose deltas are being
// refused, each with its node, payload/part, when it was flagged, the latest
// drop reason + time, and the drop count — so the operator can see WHICH carrier
// to cycle-count instead of just a number. Pure read; safe to poll.
func (h *Handlers) apiInventoryRejectedDeltas(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.InventoryDeltaService().RejectedDeltaDetail()
	if err != nil {
		h.jsonError(w, "rejected-delta detail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, rows)
}

// apiBuckets returns every lineside_buckets row as JSON: Core's mirror of the
// Edge's piles, active and stranded. Powers the "Lineside Buckets" section on
// the Bins page (bins-lineside.js). Read-only; Core never writes a pile.
func (h *Handlers) apiBuckets(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.InventoryService().ListLinesideBuckets()
	if err != nil {
		h.jsonError(w, "Failed to load lineside buckets: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, rows)
}

func (h *Handlers) apiInventoryExport(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.InventoryService().List()
	if err != nil {
		http.Error(w, "Failed to load inventory", http.StatusInternalServerError)
		return
	}

	f := excelize.NewFile()
	// excelize.File holds a temp-file backing store; not closing it leaks one
	// per export for the life of the process. Deferred rather than closed after
	// the write, so an early return added later cannot skip it.
	defer func() {
		if cerr := f.Close(); cerr != nil {
			log.Printf("inventory export: close workbook: %v", cerr)
		}
	}()
	sheet := "Inventory"
	f.SetSheetName("Sheet1", sheet)

	// Headers
	headers := []string{"Group", "Lane", "Node", "Zone", "Bin Label", "Bin Type", "Status", "In Transit", "Destination", "Payload Code", "Part Number", "Qty", "UOP Remaining", "Confirmed"}
	for i, hdr := range headers {
		c, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(sheet, c, hdr)
	}

	// Style the header row bold
	style, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	f.SetRowStyle(sheet, 1, 1, style)

	// Data rows
	for i, row := range rows {
		rn := i + 2
		f.SetCellValue(sheet, cell("A", rn), row.GroupName)
		f.SetCellValue(sheet, cell("B", rn), row.LaneName)
		f.SetCellValue(sheet, cell("C", rn), row.NodeName)
		f.SetCellValue(sheet, cell("D", rn), row.Zone)
		f.SetCellValue(sheet, cell("E", rn), row.BinLabel)
		f.SetCellValue(sheet, cell("F", rn), row.BinType)
		f.SetCellValue(sheet, cell("G", rn), row.Status)
		transit := ""
		if row.InTransit {
			transit = "Yes"
		}
		f.SetCellValue(sheet, cell("H", rn), transit)
		f.SetCellValue(sheet, cell("I", rn), row.Destination)
		f.SetCellValue(sheet, cell("J", rn), row.PayloadCode)
		f.SetCellValue(sheet, cell("K", rn), row.PartNumber)
		f.SetCellValue(sheet, cell("L", rn), row.Qty)
		f.SetCellValue(sheet, cell("M", rn), row.UOPRemaining)
		confirmed := ""
		if row.Confirmed {
			confirmed = "Yes"
		}
		f.SetCellValue(sheet, cell("N", rn), confirmed)
	}

	// Set reasonable column widths
	colWidths := map[string]float64{
		"A": 14, "B": 14, "C": 18, "D": 10, "E": 14, "F": 10, "G": 12,
		"H": 10, "I": 18, "J": 14, "K": 14, "L": 8, "M": 14, "N": 10,
	}
	for col, wd := range colWidths {
		f.SetColWidth(sheet, col, col, wd)
	}

	// Second sheet: lineside buckets. Same workbook so operators can
	// review both inventory views in one download. Read failures
	// degrade gracefully — the bins sheet still ships.
	h.appendLinesideBucketSheet(f, style)

	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="inventory.xlsx"`)
	// THE HEADERS ARE ALREADY SENT, so there is no status code left to change:
	// http.Error here would append its message to a half-written .xlsx and the
	// browser would save a corrupt file with a 200. Log it instead — a
	// truncated download the operator can see and retry, and a line naming why.
	if err := f.Write(w); err != nil {
		log.Printf("inventory export: write workbook to response: %v — the download is truncated", err)
	}
}

// appendLinesideBucketSheet adds the second sheet of the inventory export, one
// row per lineside pile, active or stranded.
//
// Read failures degrade gracefully and deliberately: the bins sheet still ships,
// which is the sheet the export is actually for. Same for a NewSheet failure --
// a workbook with one good sheet beats a 500.
func (h *Handlers) appendLinesideBucketSheet(f *excelize.File, headerStyle int) {
	bucketRows, err := h.engine.InventoryService().ListLinesideBuckets()
	if err != nil {
		return
	}
	const bucketSheet = "Lineside Buckets"
	if _, err := f.NewSheet(bucketSheet); err != nil {
		return
	}
	bucketHeaders := []string{"Cell", "Process", "Station", "Node", "Zone", "Payload Code", "State", "Qty"}
	for i, hdr := range bucketHeaders {
		c, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(bucketSheet, c, hdr)
	}
	f.SetRowStyle(bucketSheet, 1, 1, headerStyle)
	for i, br := range bucketRows {
		rn := i + 2
		f.SetCellValue(bucketSheet, cell("A", rn), br.GroupName)
		f.SetCellValue(bucketSheet, cell("B", rn), br.LaneName)
		f.SetCellValue(bucketSheet, cell("C", rn), br.Station)
		f.SetCellValue(bucketSheet, cell("D", rn), br.NodeName)
		f.SetCellValue(bucketSheet, cell("E", rn), br.Zone)
		f.SetCellValue(bucketSheet, cell("F", rn), br.PayloadCode)
		f.SetCellValue(bucketSheet, cell("G", rn), bucketStateLabel(br.State))
		f.SetCellValue(bucketSheet, cell("H", rn), br.Qty)
	}
}

// bucketStateLabel is a pile's state as the export sheet shows it: a stranded
// row is a count anomaly left at a cutover, and is labelled as one.
func bucketStateLabel(state string) string {
	if state == "stranded" {
		return "count anomaly at cutover"
	}
	return state
}

// cell builds a cell reference like "A2" from a column letter and row number.
func cell(col string, row int) string {
	return fmt.Sprintf("%s%d", col, row)
}

// apiInventoryMaintainedGroups returns the keeper's last tick, one row per
// (group, bin type): the declared level and the three populations it subtracted.
// Read by the Nodes page's group modal (nodes-maintain.js renderKeeperLevels),
// beside the level it keeps; the path keeps its /inventory prefix from when
// the inventory page drew it.
//
// THE SUBTRACTION IS THE PAYLOAD, not a status word. An operator looking at a
// group that is short and quiet needs to see WHICH term closed the gap — asks
// already out, or carriers already coming — because those have opposite
// remedies, and a single "ok / low" pill hides exactly that. Every term is a
// separate question with a separate way of being wrong, so every term is a
// column.
//
// EMPTY ON A PLANT WITH NO MAINTAINED GROUP, and empty before the first tick.
// Both render as the section not appearing, which is correct: there is nothing
// to say, and a card reading "0 groups" is a card that has to be scrolled past
// forever.
func (h *Handlers) apiInventoryMaintainedGroups(w http.ResponseWriter, r *http.Request) {
	h.jsonOK(w, h.engine.MaintainedGroupStates())
}
