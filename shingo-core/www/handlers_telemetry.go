package www

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"shingo/protocol"

	"shingocore/asks"
	"shingocore/domain"
	"shingocore/fleet"
	"shingocore/service"
)

// heldPartials is the held-partials read's body: 400 with no payload, 500 on
// a failed read, else the list.
func (d *Doors) heldPartials(payload string) asks.Result {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return badRequest("payload is required")
	}
	list, err := d.engine.BinService().ListHeldPartials(payload)
	if err != nil {
		return refused(http.StatusInternalServerError, fmt.Sprintf("held partials of %s: %v", payload, err))
	}
	return accepted(list)
}

// binLoad is the LOAD's body.
func (d *Doors) binLoad(req protocol.BinLoadRequest) asks.Result {
	if req.NodeName == "" {
		return badRequest("node_name is required")
	}

	nodes := d.engine.NodeService()
	node, err := nodes.GetByDotName(req.NodeName)
	if err != nil {
		return d.refuseAt(req.NodeName, http.StatusNotFound, fmt.Sprintf("node %q not found", req.NodeName))
	}
	binList, err := nodes.ListBinsByNode(node.ID)
	if err != nil || len(binList) == 0 {
		return d.refuseAt(req.NodeName, http.StatusBadRequest, fmt.Sprintf("no bin at node %s", req.NodeName))
	}
	// Same resolution rule as bin-count and bin-clear: one carrier is
	// unambiguous, more than one and the caller has to name which. This took
	// binList[0] — whichever carrier the query happened to return first.
	bin := binList[0]
	switch {
	case req.BinID != 0:
		bin = nil
		for _, b := range binList {
			if b.ID == req.BinID {
				bin = b
				break
			}
		}
		if bin == nil {
			return d.refuseAt(req.NodeName, http.StatusConflict, fmt.Sprintf("bin %d is not at node %s", req.BinID, req.NodeName))
		}
	case len(binList) > 1:
		return d.refuseAt(req.NodeName, http.StatusConflict, fmt.Sprintf("node %s holds %d bins; specify bin_id to disambiguate", req.NodeName, len(binList)))
	}
	// THE CARRIER IS STILL WHAT THE SENDER READ, or nothing is loaded: its
	// generation (epoch) and what it is in the door's terms (expect), each
	// asked only when the sender asserted it. Read with the bins above, outside
	// the load's transaction, as the bin_id check is.
	if status, refusal := loadAssertionRefusal(req, bin); refusal != "" {
		return d.refuseAt(req.NodeName, status, refusal)
	}

	// The manifest stores WHICH parts. The per-line counts Edge sends are not
	// stored: a stored count goes stale as the bin is drawn down and nothing
	// rewrites it. The count is uop_remaining x parts_per_cycle at read time.
	manifest := domain.Manifest{Items: make([]domain.ManifestEntry, len(req.Manifest))}
	for i, item := range req.Manifest {
		manifest.Items[i] = domain.ManifestEntry{PartNumber: item.PartNumber}
	}
	manifestJSON, _ := json.Marshal(manifest)

	// ABSENCE IS THE QUESTION, NOT ZERO: a missing uop_count is the standard
	// pack, a declared one (zero included) is written as given. An unknown
	// payload is refused rather than loaded at 0 — a 0 there is a ledger entry
	// that reads as a measured empty bin. Both rules, and the one transaction
	// the set and the confirm now share, are BinManifestService.LoadAtLine's.
	newEpoch, written, err := d.engine.BinManifest().LoadAtLine(bin.ID, string(manifestJSON), req.PayloadCode, req.UOPCount)
	if err != nil {
		var carrier *service.CarrierRefusedError
		var held *service.HeldCarrierError
		switch {
		case errors.As(err, &held):
			// A live order holds the carrier (owner, 2026-10-18: "Refuse a
			// held bin"). The Edge shows this sentence as the LOAD's refusal.
			return d.refuseAt(req.NodeName, http.StatusConflict, held.Error())
		case errors.As(err, &carrier):
			// A conflict between the part and the carrier standing at the
			// window, answered with what the operator does about it. The Edge
			// shows this sentence as the LOAD's refusal.
			return d.refuseAt(req.NodeName, http.StatusConflict, loaderCarrierRefusal(carrier))
		case errors.Is(err, service.ErrCountRequired):
			return d.refuseAt(req.NodeName, http.StatusConflict, err.Error()+" — enter the count on the LOAD")
		case errors.Is(err, service.ErrUnknownPayload):
			return d.refuseAt(req.NodeName, http.StatusBadRequest, err.Error())
		default:
			return d.refuseAt(req.NodeName, http.StatusInternalServerError, err.Error())
		}
	}
	uop := int64(written)

	log.Printf("telemetry: bin-load bin=%d at node=%s payload=%s uop=%d epoch=%d", bin.ID, req.NodeName, req.PayloadCode, uop, newEpoch)
	d.hub.Broadcast("bin-update", sseJSON(map[string]any{
		"node_id": node.ID, "action": "loaded", "bin_id": bin.ID,
	}))
	return accepted(protocol.BinLoadResponse{
		Status:       "ok",
		BinID:        bin.ID,
		BinLabel:     bin.Label,
		PayloadCode:  req.PayloadCode,
		UOPRemaining: int(uop),
		DeltaEpoch:   newEpoch,
	})
}

// loadAssertionRefusal is the LOAD's refusal of a carrier that is no longer
// what the sender read: 409 when its generation moved (req.Epoch) or it is not
// the carrier asserted (req.Expect: bare, no part on it; partial, more than
// none and fewer than capacity of the part being loaded), 400 for an expect the
// door has no term for. "" when every asserted fact holds; nothing asserted
// passes. Pinned by TestBinLoadDoor_Arms.
func loadAssertionRefusal(req protocol.BinLoadRequest, b *domain.Bin) (int, string) {
	changed := req.Epoch != 0 && b.DeltaEpoch != req.Epoch
	switch req.Expect {
	case "":
	case protocol.LoadExpectBare:
		changed = changed || b.PayloadCode != ""
	case protocol.LoadExpectPartial:
		changed = changed || b.PayloadCode != req.PayloadCode || b.UOPRemaining <= 0 || b.UOPRemaining >= b.UOPCapacity
	default:
		// OWNER-WORDS
		return http.StatusBadRequest, fmt.Sprintf("expect %q is not a carrier the door knows: say %s or %s",
			req.Expect, protocol.LoadExpectBare, protocol.LoadExpectPartial)
	}
	if !changed {
		return 0, ""
	}
	// OWNER-WORDS
	return http.StatusConflict, "the carrier changed since you looked — it now holds " + carrierSummary(b)
}

// carrierSummary is a carrier as its bins row shows it: the label, and the
// part it carries or that it is bare. No count: the row carries none.
func carrierSummary(b *domain.Bin) string {
	if b.PayloadCode == "" {
		return b.Label + " (bare)"
	}
	return b.Label + " (" + b.PayloadCode + ")"
}

// loaderCarrierRefusal is the sentence an operator reads when the carrier rule
// refuses their LOAD at a loader window: the carrier, the carriers the part may
// ride, and that nothing was written (owner, 2026-10-12: it names the carriers
// the part may ride, and nothing else).
func loaderCarrierRefusal(r *service.CarrierRefusedError) string {
	allowed := "its allowed carriers could not be read"
	if len(r.Allowed) > 0 {
		allowed = "allowed: " + strings.Join(r.Allowed, ", ")
	}
	return fmt.Sprintf("%s may not ride a %s carrier (%s). Nothing was loaded.", r.Payload, r.BinType, allowed)
}

// binCount is the count's body.
func (d *Doors) binCount(req protocol.BinCountRequest) asks.Result {
	if req.NodeName == "" {
		return badRequest("node_name is required")
	}
	nodes := d.engine.NodeService()
	node, err := nodes.GetByDotName(req.NodeName)
	if err != nil {
		return d.refuseAt(req.NodeName, http.StatusNotFound, fmt.Sprintf("node %q not found", req.NodeName))
	}
	binList, err := nodes.ListBinsByNode(node.ID)
	if err != nil || len(binList) == 0 {
		return d.refuseAt(req.NodeName, http.StatusBadRequest, fmt.Sprintf("no bin at node %s", req.NodeName))
	}
	// Same resolution rule as bin-clear: one carrier is unambiguous, more than
	// one and the caller has to name which, rather than an arbitrary first.
	bin := binList[0]
	switch {
	case req.BinID != 0:
		bin = nil
		for _, b := range binList {
			if b.ID == req.BinID {
				bin = b
				break
			}
		}
		if bin == nil {
			return d.refuseAt(req.NodeName, http.StatusConflict, fmt.Sprintf("bin %d is not at node %s", req.BinID, req.NodeName))
		}
	case len(binList) > 1:
		return d.refuseAt(req.NodeName, http.StatusConflict, fmt.Sprintf("node %s holds %d bins; specify bin_id to disambiguate", req.NodeName, len(binList)))
	}
	// A COUNT BELONGS TO ONE GENERATION of the carrier. One taken under any
	// other (req.Epoch, when the sender asserts it) is not recorded: the answer
	// is the carrier's current stamp, the body a bin epoch refresh carries, for
	// the sender to adopt and count again under. The whole refusal is the
	// body, the refresh beside the sentence and the rows.
	if req.Epoch != 0 && req.Epoch != bin.DeltaEpoch {
		res := asks.Result{Status: http.StatusConflict,
			// OWNER-WORDS
			Sentence: fmt.Sprintf("bin %s changed since that count was taken — nothing was recorded", bin.Label),
			Rows:     d.engine.NodeService().BinsRows(req.NodeName)}
		res.Body = doorRefusal{
			Rows:         res.Rows,
			EpochRefresh: &protocol.BinEpochRefresh{BinID: bin.ID, CoreNodeName: req.NodeName, Epoch: bin.DeltaEpoch},
			Error:        res.Sentence,
		}
		return res
	}

	actor := req.Actor
	if actor == "" {
		actor = "edge-operator"
	}
	res, err := d.engine.BinService().RecordCount(bin, req.ActualUOP, actor)
	if err != nil {
		return d.refuseAt(req.NodeName, http.StatusBadRequest, err.Error())
	}
	d.engine.AuditService().Append("bin", bin.ID, "counted",
		strconv.Itoa(res.Expected), strconv.Itoa(res.Actual), actor)
	log.Printf("telemetry: bin-count bin=%d at node=%s expected=%d actual=%d actor=%s",
		bin.ID, req.NodeName, res.Expected, res.Actual, actor)
	d.hub.Broadcast("bin-update", sseJSON(map[string]any{
		"node_id": node.ID, "action": "counted", "bin_id": bin.ID,
	}))
	return accepted(protocol.BinCountResponse{
		Status:       "ok",
		BinID:        bin.ID,
		BinLabel:     bin.Label,
		Expected:     res.Expected,
		UOPRemaining: res.Actual,
		Discrepancy:  res.Discrepancy,
		Warning:      res.Warning,
		DeltaEpoch:   res.Epoch,
		AsOfNet:      res.AsOfNet,
		AsOfSeq:      res.AsOfSeq,
		AsOfStation:  res.AsOfStation,
	})
}

// doorRefusal is a write door's refusal body: the sentence, Core's current
// bins rows for the node the request named (Rows, for the Edge to write into
// its copy before it shows the sentence), and on a count from an ended
// generation the carrier's current one. Keys alphabetical, as the replies'
// are; with nothing beyond the sentence it is the bytes jsonError writes.
type doorRefusal struct {
	EpochRefresh *protocol.BinEpochRefresh `json:"epoch_refresh,omitempty"`
	Error        string                    `json:"error"`
	Rows         []protocol.NodeBinsRow    `json:"rows,omitempty"`
}

// binClear is the CLEAR's body.
func (d *Doors) binClear(req protocol.BinClearRequest) asks.Result {
	if req.NodeName == "" {
		return badRequest("node_name is required")
	}
	if req.Layers != nil && *req.Layers < 0 {
		return badRequest(fmt.Sprintf("layers %d is not a height", *req.Layers))
	}
	nodes := d.engine.NodeService()
	node, err := nodes.GetByDotName(req.NodeName)
	if err != nil {
		return d.refuseAt(req.NodeName, http.StatusNotFound, fmt.Sprintf("node %q not found", req.NodeName))
	}
	bins, err := nodes.ListBinsByNode(node.ID)
	if err != nil || len(bins) == 0 {
		return d.refuseAt(req.NodeName, http.StatusBadRequest, fmt.Sprintf("no bin at node %s", req.NodeName))
	}
	bin, refusal := d.binToClear(req, bins)
	if refusal != nil {
		return *refusal
	}

	// A HELD PARTIAL IS NOT CLEARED ON ONE TAP. An unfinished carrier standing in
	// a partial home of its own part waits there to be called back to its line
	// and finished; the clear would zero the count that makes it worth calling.
	// The person's second tap (unload) clears it, booked as any clear. A carrier
	// that is full, empty, or not in a home of its part clears as it always did.
	if !req.Unload {
		held, herr := d.engine.BinService().IsHeldPartial(bin.ID)
		if herr != nil {
			return d.refuseAt(req.NodeName, http.StatusInternalServerError, fmt.Sprintf("could not read whether bin %s is a held partial: %v", bin.Label, herr))
		}
		if held {
			return d.refuseAt(req.NodeName, http.StatusConflict, fmt.Sprintf("bin %s holds %d of %s, unfinished and held here to be called back to its line; "+
				"UNLOAD to clear it anyway", bin.Label, bin.UOPRemaining, bin.PayloadCode))
		}
	}

	stamp, refusal := d.binClearStamp(node.ID, req.NodeName, bin.BinTypeBare)
	if refusal != nil {
		return *refusal
	}

	// Resolve optional dunnage re-type. Existence check keeps the blast radius
	// of a mis-pick to "starves out, repair via re-type" (R3 severity).
	var binTypeID *int64
	if req.BinTypeCode != "" && stamp == domain.StampNone {
		bt, err := d.engine.BinService().GetBinTypeByCode(req.BinTypeCode)
		if err != nil {
			return d.refuseAt(req.NodeName, http.StatusBadRequest, fmt.Sprintf("unknown bin type code %q", req.BinTypeCode))
		}
		id := bt.ID
		binTypeID = &id
	}

	// THE ENGINE'S CLEAR, NOT THE SERVICE'S, and that is what makes this door
	// the one CMS hears. When the node resolves to a CMS boundary the bin's
	// contents are booked as a departure in the SAME transaction as the clear;
	// when it does not, this is byte-for-byte the old call. The UI's admin clear
	// deliberately still goes to the service and stays silent — see
	// engine.ClearForReuseAndBookDeparture.
	//
	// A REFUSAL HERE IS RECOVERABLE AND IS MEANT TO BE. If the departure rows
	// cannot be written the clear is refused with them, and the unloader presses
	// the button again; the alternative is a clear that destroys the count with
	// nothing recording that material left.
	newEpoch, err := d.orchestration.ClearForReuseStampAndBookDeparture(bin.ID, node.ID, binTypeID, stamp, req.Layers)
	if err != nil {
		return d.refuseAt(req.NodeName, http.StatusInternalServerError, err.Error())
	}
	// Read after the commit: the stamp is decided inside the transaction. A
	// failed read costs the board its line, never the clear.
	clearedType, err := d.engine.BinService().RealTypeCode(bin.ID)
	if err != nil {
		log.Printf("telemetry: bin-clear bin=%d: read its cart type: %v", bin.ID, err)
	}
	log.Printf("telemetry: bin-clear bin=%d at node=%s bin_type=%s epoch=%d", bin.ID, req.NodeName, req.BinTypeCode, newEpoch)
	d.hub.Broadcast("bin-update", sseJSON(map[string]any{
		"node_id": node.ID, "action": "cleared", "bin_id": bin.ID,
	}))
	// cleared_payload_code is what the carrier held before this clear — `bin` was
	// read above, before ClearForReuseAndBookDeparture. The Edge logs it with the
	// CLEAR; it no longer reads node-bins first to learn it, and a successful
	// answer here is what tells it a carrier was there (the no-bin refusal above).
	return accepted(protocol.BinClearResponse{
		Status:             "ok",
		BinID:              bin.ID,
		BinLabel:           bin.Label,
		DeltaEpoch:         newEpoch,
		ClearedPayloadCode: bin.PayloadCode,
		ClearedUOP:         bin.UOPRemaining,
		// The cart's type as an operator knows it — the carrier, never a
		// marker — for the board's CLEAR line.
		ClearedBinTypeCode: clearedType,
	})
}

// binToClear resolves which bin a clear names. One bin → unambiguous. More
// than one → the caller must name a bin_id, otherwise we'd silently clear an
// arbitrary one. No count/occupancy refusal: the unloader clears full finished
// bins by design, so a non-empty bin is not itself a reason to refuse. A
// refusal is returned in place of the bin.
func (d *Doors) binToClear(req protocol.BinClearRequest, atNode []*domain.Bin) (*domain.Bin, *asks.Result) {
	var refusal asks.Result
	switch {
	case req.BinID != 0:
		for _, b := range atNode {
			if b.ID == req.BinID {
				return b, nil
			}
		}
		refusal = d.refuseAt(req.NodeName, http.StatusConflict, fmt.Sprintf("bin %d is not at node %s", req.BinID, req.NodeName))
	case len(atNode) > 1:
		refusal = d.refuseAt(req.NodeName, http.StatusConflict, fmt.Sprintf("node %s holds %d bins; specify bin_id to disambiguate", req.NodeName, len(atNode)))
	default:
		return atNode[0], nil
	}
	return nil, &refusal
}

// binClearStamp decides the two-stage stamp for a clear at nodeID, before
// anything is written: a stage-1 window stamps the cart's marker (and refuses
// with 409 when the stage 1 has nowhere to send the cart); a bare cart
// elsewhere gets its carrier back; anything else stamps what it was told. A
// refusal is returned in place of the stamp.
func (d *Doors) binClearStamp(nodeID int64, nodeName string, bare bool) (domain.BareStamp, *asks.Result) {
	stageOne, sendsTo, err := d.engine.LoaderService().StageOneAt(nodeID)
	var refusal asks.Result
	switch {
	case err != nil:
		refusal = d.refuseAt(nodeName, http.StatusInternalServerError, fmt.Sprintf("read the loader at %s: %v", nodeName, err))
	case stageOne && sendsTo == "":
		refusal = d.refuseAt(nodeName, http.StatusConflict, fmt.Sprintf("this station has nowhere to send the cart: set where %s's carts go before clearing", nodeName))
	case stageOne:
		return domain.StampMarker, nil
	case bare:
		return domain.StampCarrier, nil
	default:
		return domain.StampNone, nil
	}
	return domain.StampNone, &refusal
}

// ── E-Maint Robot Telemetry ──────────────────────────────────
//
// Generates an on-demand telemetry snapshot from the in-memory robot cache.
// No persistence, no background goroutine — just reads what robotRefreshLoop
// already keeps warm (2-second freshness).

// apiEMaintRobotTelemetry returns a fleet-wide telemetry report for e-maintenance.
// GET /api/telemetry/e-maint
func (h *Handlers) apiEMaintRobotTelemetry(w http.ResponseWriter, r *http.Request) {
	report := h.buildEMaintReport()
	h.jsonOK(w, report)
}

// apiEMaintRobotTelemetryDownload returns the same report as a downloadable JSON file.
// GET /api/telemetry/e-maint/download
func (h *Handlers) apiEMaintRobotTelemetryDownload(w http.ResponseWriter, r *http.Request) {
	report := h.buildEMaintReport()
	filename := fmt.Sprintf("robot-telemetry-%s.json", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	json.NewEncoder(w).Encode(report)
}

func (h *Handlers) buildEMaintReport() map[string]any {
	return eMaintReport(h.engine.GetAllCachedRobots(), time.Now().UTC(), uuid.New().String())
}

// eMaintReport is the E-Maint report's shape, apart from the clock and the id.
// THE SHAPE IS A CONTRACT: the downloaded file is read outside Core, so every
// key here is pinned by TestEMaintReport_ShapeIsPinned. The report moved from
// Diagnostics to the Robots page's Fleet tab without changing a byte of it.
func eMaintReport(robots []fleet.RobotStatus, now time.Time, reportID string) map[string]any {
	entries := make([]map[string]any, 0, len(robots))
	for _, r := range robots {
		entry := map[string]any{
			"vehicle_id":  r.VehicleID,
			"connected":   r.Connected,
			"snapshot_at": now.Format(time.RFC3339),
			"position": map[string]any{
				"x":               r.X,
				"y":               r.Y,
				"angle":           r.Angle,
				"current_station": r.CurrentStation,
				"current_map":     r.CurrentMap,
			},
			"odometer": map[string]any{
				"total_m": r.OdoTotal,
				"today_m": r.OdoToday,
			},
			"runtime": map[string]any{
				"session_ms": r.SessionMs,
				"total_ms":   r.TotalMs,
			},
			"lifts": map[string]any{
				"total_count":       r.LiftCount,
				"current_height_mm": r.LiftHeight,
				"error_code":        r.LiftError,
			},
			"battery": map[string]any{
				"level_pct": r.BatteryLevel,
				"charging":  r.Charging,
				"voltage_v": r.BatteryV,
				"current_a": r.BatteryA,
			},
			"controller": map[string]any{
				"temp_c":       r.CtrlTemp,
				"humidity_pct": r.CtrlHumi,
				"voltage_v":    r.CtrlVoltage,
			},
			"safety": map[string]any{
				"blocked":   r.Blocked,
				"emergency": r.Emergency,
			},
			"task": map[string]any{
				"status":  r.State(),
				"model":   r.Model,
				"version": r.Version,
			},
		}
		entries = append(entries, entry)
	}

	return map[string]any{
		"report_id":    reportID,
		"generated_at": now.Format(time.RFC3339),
		"source":       "shingo-core",
		"robot_count":  len(robots),
		"robots":       entries,
	}
}

// clearHome is a home clear's body, for station. The engine's answer is the
// door's: its status and its ClearHomeResponse whatever the status, so a
// refusal past the zero keeps the carrier's id and epoch beside the sentence;
// a refusal's rows are the ones the engine put on it.
func (d *Doors) clearHome(station string, req protocol.ClearHomeRequest) asks.Result {
	req.Station = station
	if req.HomeNode == "" || req.Station == "" {
		return badRequest("home_node and station are required")
	}
	ans := d.orchestration.ClearLoaderHome(req)
	log.Printf("telemetry: clear-home %s for %s: %d %s%s", req.HomeNode, req.Station, ans.Status, ans.Body.Arm, ans.Body.Error)
	if ans.Status == http.StatusOK && ans.Body.BinID != 0 {
		d.hub.Broadcast("bin-update", sseJSON(map[string]any{"action": "cleared", "bin_id": ans.Body.BinID}))
	}
	res := asks.Result{Status: ans.Status, Body: ans.Body, Rows: ans.Body.Rows}
	if ans.Status != http.StatusOK {
		res.Sentence = ans.Body.Error
	}
	return res
}
