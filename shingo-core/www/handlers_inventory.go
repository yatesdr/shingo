package www

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/xuri/excelize/v2"
	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/domain"
)

// deltaDailyDays is how far back the daily drop trend reaches.
//
// Wider than the per-payload panel's 7 days on purpose. The drops are
// episodic: Springfield's 30 days to 2026-07-29 had drops on nine of them and
// one day carrying 76% of the net. At 7 days that spike has nothing to be a
// spike ABOVE, and a reader cannot tell an incident from the new normal —
// which is the exact judgement this trend exists to support.
//
// Not a sim-derived constant: it is a window, not a threshold. Nothing is
// classified by it and no verdict turns on it.
const deltaDailyDays = 30

// InventoryInvariant is the Item 13 plant-wide running totals shape.
// BinSum is signed because the SME lock allows bins to go negative
// (overpack/underpack); over time the signed sum drifts in either
// direction as production smooths out, useful as a trend indicator
// rather than a hard equation. BucketSum stays non-negative by
// schema CHECK constraint. Total = BinSum + BucketSum, so dashboards
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

func (h *Handlers) handleInventory(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Page": "inventory",
	}
	h.render(w, r, "inventory.html", data)
}

func (h *Handlers) apiInventory(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.InventoryService().List()
	if err != nil {
		h.jsonError(w, "Failed to load inventory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, rows)
}

// apiInventoryMonitorTotals returns the per-payload Replenishment Health rollup:
// DB on-hand (bins + lineside split), which payloads are monitored, and their
// configured thresholds. Powers the inventory page's Replenishment Health
// meters. (The monitor holds no private tally anymore, so there is no
// cache-vs-DB drift to surface — that chip was deleted with the tally.)
func (h *Handlers) apiInventoryMonitorTotals(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.ReplenishmentHealth(r.Context())
	if err != nil {
		h.jsonError(w, "replenishment health: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, rows)
}

// apiInventoryLedgerExceptions returns the ledger-integrity exception list:
// bins whose UOP is negative right now, and the payloads whose plant-wide
// total is negative — the ones the threshold monitor is signalling
// replenishment for from a count it cannot trust. (It still signals; the
// refusal was removed.)
//
// BLANK ON A GOOD DAY. That is the design: a page of charts computed from a
// handful of points is worse than nothing, and a list that is empty until
// something is wrong is the right instrument.
//
// Read-side only — every value comes from bins and bin_uop_audit, which are
// already on disk. No new table.
//
// ?since=<RFC3339> and ?limit= control the excursion history (default 7 days,
// 200 rows). Excursions are the forensics; the two lists above are the
// actionable part.
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
	// says "this payload is negative" and this says "and here is the mechanism
	// that probably did it", and the two are only worth anything read
	// together. One fetch means they cannot drift apart in the UI either.
	deltaIntegrity, err := h.engine.DeltaIntegrityByPayload(since)
	if err != nil {
		h.jsonError(w, "delta integrity: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// The time axis for the same drops. Deliberately a WIDER window than the
	// panel above: a trend needs enough days to have a shape, and 7 of them at
	// Springfield showed one spike and nothing to compare it against.
	dailySince := clock.Now().UTC().AddDate(0, 0, -deltaDailyDays)
	if dailySince.After(since) {
		dailySince = since
	}
	deltaDaily, err := h.engine.DeltaIntegrityDaily(dailySince, plantLocation.String())
	if err != nil {
		h.jsonError(w, "delta integrity daily: "+err.Error(), http.StatusInternalServerError)
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
	if deltaDaily == nil {
		deltaDaily = []domain.DeltaDay{}
	}
	h.jsonOK(w, map[string]any{
		"since":             since,
		"open_bins":         openBins,
		"negative_payloads": payloads,
		"excursions":        excursions,
		"delta_integrity":   deltaIntegrity,
		"delta_daily":       deltaDaily,
		"delta_daily_since": dailySince,
	})
}

// apiSourceabilityEvents serves the persisted sourceability verdict-change
// history: when a (process, style) stopped being sourceable, what it was
// missing, and when it recovered.
//
// The monitor has computed this diff every two minutes since it was written
// and never wrote it down, so the root physical condition of 2026-07-21 —
// zero system stock on 74577-6SA0A.06 — was known continuously and recorded
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
// rollup behind the inventory page's alerts banner (P2-C6): reason-split drop
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

// apiInventoryRejectedDeltas is the drill-down behind the inventory page's
// "N rejected deltas" banner: the list of carriers whose deltas are being
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

// apiBuckets returns every authoritative lineside_buckets row as JSON.
// Powers the "Lineside Buckets" section on the operator-facing
// inventory page. Round-3 Obs 10 added the Delete column on top of
// this read-side: apiBucketDelete (below) is the admin recovery hatch
// for clearing Core-only orphan rows.
//
// See lineside-buckets-investigation-2026-05-18.md.
func (h *Handlers) apiBuckets(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.InventoryService().ListLinesideBuckets()
	if err != nil {
		h.jsonError(w, "Failed to load lineside buckets: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, rows)
}

// apiBucketDelete removes one lineside_buckets row + its
// inventory_delta_dedup row by primary key. Round-3 Obs 10 — the
// operator-driven recovery hatch for the cross-namespace orphan
// shape that the Obs 8 protocol fix made impossible to create going
// forward. Auth-gated via requireAuth (binary in this codebase; no
// finer role distinction).
//
// The audit row records source="ui", actor=session username, so
// operations can trace which engineer cleared which bucket.
func (h *Handlers) apiBucketDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.ID <= 0 {
		h.jsonError(w, "id required", http.StatusBadRequest)
		return
	}

	n, err := h.engine.InventoryService().DeleteLinesideBucket(req.ID)
	if err != nil {
		h.jsonError(w, "delete lineside bucket: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n == 0 {
		h.jsonError(w, "no lineside bucket with that id", http.StatusNotFound)
		return
	}

	actor := h.getUsername(r)
	if actor == "" {
		actor = protocol.AuditActorUI
	}
	if as := h.engine.AuditService(); as != nil {
		as.Append("lineside_bucket", req.ID, "deleted", "active", "deleted", actor)
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiInventoryExport(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.InventoryService().List()
	if err != nil {
		http.Error(w, "Failed to load inventory", http.StatusInternalServerError)
		return
	}

	f := excelize.NewFile()
	sheet := "Inventory"
	f.SetSheetName("Sheet1", sheet)

	// Headers
	headers := []string{"Group", "Lane", "Node", "Zone", "Bin Label", "Bin Type", "Status", "In Transit", "Destination", "Payload Code", "Cat-ID", "Qty", "UOP Remaining", "Confirmed"}
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
		f.SetCellValue(sheet, cell("K", rn), row.CatID)
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
	if bucketRows, err := h.engine.InventoryService().ListLinesideBuckets(); err == nil {
		bucketSheet := "Lineside Buckets"
		if _, err := f.NewSheet(bucketSheet); err == nil {
			bucketHeaders := []string{"Cell", "Process", "Station", "Node", "Zone", "Style ID", "Part", "Payload Code", "State", "Qty"}
			for i, hdr := range bucketHeaders {
				c, _ := excelize.CoordinatesToCellName(i+1, 1)
				f.SetCellValue(bucketSheet, c, hdr)
			}
			f.SetRowStyle(bucketSheet, 1, 1, style)
			for i, br := range bucketRows {
				rn := i + 2
				f.SetCellValue(bucketSheet, cell("A", rn), br.GroupName)
				f.SetCellValue(bucketSheet, cell("B", rn), br.LaneName)
				f.SetCellValue(bucketSheet, cell("C", rn), br.Station)
				f.SetCellValue(bucketSheet, cell("D", rn), br.NodeName)
				f.SetCellValue(bucketSheet, cell("E", rn), br.Zone)
				f.SetCellValue(bucketSheet, cell("F", rn), br.StyleID)
				f.SetCellValue(bucketSheet, cell("G", rn), br.PartNumber)
				f.SetCellValue(bucketSheet, cell("H", rn), br.PayloadCode)
				f.SetCellValue(bucketSheet, cell("I", rn), br.State)
				f.SetCellValue(bucketSheet, cell("J", rn), br.Qty)
			}
		}
	}

	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="inventory.xlsx"`)
	f.Write(w)
}

// cell builds a cell reference like "A2" from a column letter and row number.
func cell(col string, row int) string {
	return fmt.Sprintf("%s%d", col, row)
}

// apiInventoryMaintainedGroups returns the keeper's last tick, one row per
// (group, bin type): the declared level and the three populations it subtracted.
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
