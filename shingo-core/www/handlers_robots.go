package www

import (
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"shingocore/dispatch"
	"shingocore/fleet"
)

func (h *Handlers) handleRobots(w http.ResponseWriter, r *http.Request) {
	robots := h.engine.GetAllCachedRobots()
	data := map[string]any{
		"Page":   "robots",
		"Robots": robots,
	}
	h.render(w, r, "robots.html", data)
}

func (h *Handlers) apiRobotsStatus(w http.ResponseWriter, r *http.Request) {
	h.jsonOK(w, h.engine.GetAllCachedRobots())
}

func (h *Handlers) apiRobotSetAvailability(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VehicleID string `json:"vehicle_id"`
		Available bool   `json:"available"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	rl, ok := h.engine.Fleet().(fleet.RobotLister)
	if !ok {
		h.jsonError(w, "fleet backend does not support robot management", http.StatusNotImplemented)
		return
	}
	if err := rl.SetAvailability(req.VehicleID, req.Available); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiRobotRetryFailed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VehicleID string `json:"vehicle_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	rl, ok := h.engine.Fleet().(fleet.RobotLister)
	if !ok {
		h.jsonError(w, "fleet backend does not support robot management", http.StatusNotImplemented)
		return
	}
	if err := rl.RetryFailed(req.VehicleID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiRobotForceComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VehicleID string `json:"vehicle_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	rl, ok := h.engine.Fleet().(fleet.RobotLister)
	if !ok {
		h.jsonError(w, "fleet backend does not support robot management", http.StatusNotImplemented)
		return
	}
	if err := rl.ForceComplete(req.VehicleID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiRobotMoveTo sends a robot to a location. That is the whole feature: no bin
// is picked up, nothing is delivered, and no order is created.
//
// This used to be an order type. It wrote a row in the orders table, stamped it
// dispatched, and handed the fleet a one-block request with Complete=false
// under a comment saying the robot "dwells (staged) at the destination until
// released". None of that was true.
//
// The documented way to hold a robot is Complete=false WITH a Wait block (see
// fleet.Backend). There was no Wait block — just a location — so nothing ever
// told the robot to hold. It finished its one block, went idle, and the fleet
// parked it, which is what the floor always saw. All Complete=false achieved
// was keeping the VENDOR order open forever, matching the shingo row that also
// could never close: releasing one needs an owning station, and no edge owns
// Core's own. What actually ended these orders was the stuck-order sweep,
// which auto-cancelled each one and filed a recovery action for it.
//
// So the row recorded nothing anyone read, could not be closed by the feature
// that claimed to close it, and put a fake entry in the recovery audit every
// time. Telling a robot where to go is a fleet command, and it now lives with
// the other fleet commands.
func (h *Handlers) apiRobotMoveTo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeliveryNode string `json:"delivery_node"`
		Priority     int    `json:"priority"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.DeliveryNode == "" {
		h.jsonError(w, "destination node is required", http.StatusBadRequest)
		return
	}
	destNode := h.resolveDropoff(w, req.DeliveryNode)
	if destNode == nil {
		return
	}

	// LANES ARE OFF-LIMITS TO THIS DOOR, and the refusal is STATIC — never, not
	// not-right-now.
	//
	// Every other way a robot enters a lane goes through admission, which can
	// answer "not yet" because there is an order to park and something to wake it
	// when the lane clears. This command has neither: no order row, no queue, no
	// releaser. An admission ask here could only refuse-and-forget, which asks an
	// operator to guess when to retry, and an admission PASS would put an
	// unrecorded robot in a corridor that the next order's occupancy read cannot
	// see — the collision the unification exists to prevent, arriving through the
	// one door that keeps no record.
	//
	// So the rule is geometry, not state: if the destination resolves into a lane,
	// this door says no. It costs nothing to maintain — no hold, no cause, no
	// event to wire — and it does not remove a capability, because the two things
	// an operator actually wants a robot in a lane FOR are both still there. Move
	// a bin: use a bin move, which is an order and goes through the gate. Drive a
	// robot for maintenance: use the vendor's own console, which is outside Core,
	// which is honestly where an ungoverned move belongs.
	lane, err := h.engine.Dispatcher().LaneForNode(destNode.ID)
	if err != nil {
		h.jsonError(w, "could not tell whether "+destNode.Name+" is in a lane: "+err.Error(),
			http.StatusInternalServerError)
		return
	}
	if lane != nil {
		h.jsonError(w, "manual robot moves cannot target lane slots ("+destNode.Name+" is in lane "+
			lane.Name+"); use a bin move, or the fleet console for maintenance",
			http.StatusBadRequest)
		return
	}

	// NO OCCUPANCY GATE, AND THAT IS THE POINT OF THIS ENDPOINT.
	//
	// This used to call rejectIfOccupied and refuse the move with 409 and
	// "Waiting for a slot at ALN_003 — 1 bin there now". That gate asks whether
	// a BIN CAN BE DROPPED at the destination, and this command drops nothing:
	// no bin is picked up, no bin is placed, no order row is written. The robot
	// drives there and parks. A bin already sitting at the node is not an
	// obstacle to that, it is scenery.
	//
	// The old comment argued the gate "was written for the order door and its
	// reason survives the door", because a robot sent to an occupied spot
	// arrives and stops. It does arrive and stop — that is what being sent
	// somewhere means. Nothing is left half-done, no slot is consumed, and
	// nothing downstream is waiting on it.
	//
	// Worse, it refused the case the endpoint is most useful for: sending a
	// robot to a node precisely BECAUSE something is there and someone wants
	// eyes on it. The gate turned a positioning command into one that only
	// works where nothing is happening.
	//
	// PreviewDropoffCapacity still guards the real order doors, and
	// /api/dispatch/preview-capacity still serves the manual-order form for the
	// two tabs that create orders.

	// No order row means no order id to build a vendor id from, and nothing for
	// an ExternalID to correlate back to — so it is left empty rather than
	// pointed at itself. The vendor id is the only handle this command has,
	// which is why it carries a whole uuid instead of eight characters of one.
	vendorOrderID := dispatch.VendorIDPrefix + "move-" + uuid.New().String()
	if _, err := h.engine.Fleet().CreateOrder(fleet.CreateOrderRequest{
		OrderID: vendorOrderID,
		Blocks: []fleet.OrderBlock{
			{BlockID: vendorOrderID + "-b1", Location: destNode.Name},
		},
		Priority: req.Priority,
		// The fleet closes this order when the robot arrives. The old false
		// left one open at the vendor for every send-to ever issued.
		Complete: true,
	}); err != nil {
		h.jsonError(w, "fleet did not accept the move: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.jsonOK(w, map[string]any{
		"vendor_order_id": vendorOrderID,
		"destination":     destNode.Name,
	})
}

// ── The localization board ─────────────────────────────────────────────────

// boardWindows are the windows the page may ask for.
//
// AN ALLOW-LIST, NOT A FREE PARAMETER, and the reason is the one the whole
// design keeps returning to: a window is only offerable if the record can
// answer it. These four can, because the daily histograms sum — before that
// column existed, 30 d could not be served at all against fourteen days of raw
// and a control offering it would have returned a shorter answer than its own
// label.
var boardWindows = map[string]int{
	"24h": 1,
	"7d":  7,
	"30d": 30,
}

// apiLocalizationBoard serves the robots page's map overlay.
//
// STATE ONLY — the page draws geometry from /api/map/edges, which already
// exists. Two copies of the network under two URLs is the objection that keeps
// the .smap's curves unparsed, and it does not stop applying because the second
// copy would be convenient.
//
// ?robot= is the per-AMR filter. Absent or empty is the fleet view. A value that
// is not a known vehicle is a 400 rather than a silent fall-through to fleet: a
// page that asked for AMR-99 and got the whole fleet would show the user the
// answer to a question they did not ask, exactly the window-label failure.
func (h *Handlers) apiLocalizationBoard(w http.ResponseWriter, r *http.Request) {
	label := r.URL.Query().Get("window")
	if label == "" {
		label = "7d"
	}
	days, ok := boardWindows[label]
	if !ok {
		// A window this cannot serve is a 400, never a silent fallback to one
		// it can. Quietly answering a different question than the one asked is
		// how a reader compares last week's numbers against this week's label.
		h.jsonError(w, fmt.Sprintf("window: %q is not one of 24h, 7d, 30d", label),
			http.StatusBadRequest)
		return
	}
	robot := r.URL.Query().Get("robot")
	if robot != "" {
		known := false
		for _, rb := range h.engine.GetAllCachedRobots() {
			if rb.VehicleID == robot {
				known = true
				break
			}
		}
		if !known {
			h.jsonError(w, fmt.Sprintf("robot: %q is not a known vehicle", robot),
				http.StatusBadRequest)
			return
		}
	}
	board, err := h.engine.NodeService().LocalizationBoardAt(label, days, time.Now(), robot)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, board)
}

// apiLaneChange serves the change annotation for one lane's most recent edit.
//
// Its own endpoint rather than a field on the board payload: most lanes have
// never been edited, so folding it in would compute a before/after for 212
// lanes to answer a question about the one the reader clicked.
func (h *Handlers) apiLaneChange(w http.ResponseWriter, r *http.Request) {
	area := r.URL.Query().Get("area")
	lane := r.URL.Query().Get("lane")
	if area == "" || lane == "" {
		h.jsonError(w, "area and lane are required", http.StatusBadRequest)
		return
	}
	ch, ok, err := h.engine.NodeService().LaneChangeAt(area, lane, time.Now())
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		// Never edited is not an error and not an empty annotation — it is a
		// different answer, and the page renders it as one.
		h.jsonOK(w, map[string]any{"changed": false})
		return
	}
	h.jsonOK(w, map[string]any{"changed": true, "annotation": ch})
}
