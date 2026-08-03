package www

import (
	"net/http"

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

	// The occupancy gate, kept. It was written for the order door and its
	// reason survives the door: a robot sent to an occupied spot arrives and
	// stops, and the operator finds out by watching. Nothing downstream would
	// catch it either — the block carries no binTask, so the position gate
	// waves it through. This is the only place left to refuse.
	if h.rejectIfOccupied(w, destNode) {
		return
	}

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
