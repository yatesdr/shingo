// handlers_test_orders_commands.go — fleet-vendor command submission
// and status polling for the /test-orders page. Used to exercise
// vendor-side commands (move/jack/unjack/terminate/switch_map/
// bind_goods/unbind_goods/unbind_container) outside the order flow.

package www

import (
	"log"
	"net/http"
	"strconv"

	"shingocore/domain"
	"shingocore/fleet"
)

// --- Fleet Test Commands ---

func (h *Handlers) apiTestCommandSubmit(w http.ResponseWriter, r *http.Request) {
	commander, ok := h.engine.Fleet().(fleet.VendorCommander)
	if !ok {
		h.jsonError(w, "fleet backend does not support vendor commands", http.StatusNotImplemented)
		return
	}

	var req struct {
		CommandType   string `json:"command_type"`
		RobotID       string `json:"robot_id"`
		Location      string `json:"location"`
		ConfigID      string `json:"config_id"`
		DispatchType  string `json:"dispatch_type"`
		MapName       string `json:"map_name"`
		OrderID       string `json:"order_id"`
		ContainerName string `json:"container_name"`
		GoodsID       string `json:"goods_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.CommandType == "" {
		h.jsonError(w, "command_type is required", http.StatusBadRequest)
		return
	}

	// Validate required fields per command type
	switch req.CommandType {
	case "terminate":
		if req.OrderID == "" {
			h.jsonError(w, "order_id is required", http.StatusBadRequest)
			return
		}
	case "switch_map":
		if req.MapName == "" {
			h.jsonError(w, "map_name is required", http.StatusBadRequest)
			return
		}
		if req.RobotID == "" {
			h.jsonError(w, "robot_id is required", http.StatusBadRequest)
			return
		}
	case "bind_goods":
		if req.ContainerName == "" || req.GoodsID == "" {
			h.jsonError(w, "container_name and goods_id are required", http.StatusBadRequest)
			return
		}
	case "unbind_goods":
		if req.GoodsID == "" {
			h.jsonError(w, "goods_id is required", http.StatusBadRequest)
			return
		}
	case "unbind_container":
		if req.ContainerName == "" {
			h.jsonError(w, "container_name is required", http.StatusBadRequest)
			return
		}
	case "move":
		if req.Location == "" {
			h.jsonError(w, "location is required for move command", http.StatusBadRequest)
			return
		}
		// robot_id is optional for move — fleet auto-assigns
	case "jack", "unjack":
		if req.ConfigID == "" {
			h.jsonError(w, "config_id is required for jack/unjack commands", http.StatusBadRequest)
			return
		}
		fallthrough
	default:
		if req.RobotID == "" && req.CommandType != "terminate" {
			h.jsonError(w, "robot_id is required", http.StatusBadRequest)
			return
		}
	}

	result, err := commander.ExecuteVendorCommand(fleet.VendorCommand{
		Type:          req.CommandType,
		RobotID:       req.RobotID,
		Location:      req.Location,
		ConfigID:      req.ConfigID,
		DispatchType:  req.DispatchType,
		MapName:       req.MapName,
		OrderID:       req.OrderID,
		ContainerName: req.ContainerName,
		GoodsID:       req.GoodsID,
	})

	tc := &domain.TestCommand{
		CommandType: req.CommandType,
		RobotID:     req.RobotID,
		Location:    req.Location,
		ConfigID:    req.ConfigID,
	}
	if result != nil {
		tc.VendorOrderID = result.VendorOrderID
		tc.VendorState = result.State
		tc.Detail = result.Detail
	}

	if dbErr := h.engine.TestCommandService().Create(tc); dbErr != nil {
		log.Printf("test-commands: db save error: %v", dbErr)
	}
	if result != nil && result.State == "COMPLETED" {
		h.engine.TestCommandService().Complete(tc.ID)
	}

	if err != nil {
		log.Printf("test-commands: %s failed: %v", req.CommandType, err)
		h.jsonError(w, "vendor command failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("test-commands: %s succeeded: robot=%s", req.CommandType, req.RobotID)
	h.jsonOK(w, map[string]any{
		"id":              tc.ID,
		"vendor_order_id": result.VendorOrderID,
		"status":          result.State,
	})
}

func (h *Handlers) apiTestCommandStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	tc, err := h.engine.TestCommandService().Get(id)
	if err != nil {
		h.jsonError(w, "command not found", http.StatusNotFound)
		return
	}

	var vendorDetail any
	if tc.CompletedAt == nil && tc.VendorOrderID != "" {
		if commander, ok := h.engine.Fleet().(fleet.VendorCommander); ok {
			detail, err := commander.GetVendorOrderDetail(tc.VendorOrderID)
			if err == nil {
				vendorDetail = detail.Raw
				if detail.State != tc.VendorState {
					h.engine.TestCommandService().UpdateStatus(id, detail.State, "")
					tc.VendorState = detail.State
				}
				if detail.IsTerminal {
					h.engine.TestCommandService().Complete(id)
				}
			}
		}
	}

	h.jsonOK(w, map[string]any{
		"command":       tc,
		"vendor_detail": vendorDetail,
	})
}

// apiTestCommandCancel terminates the vendor order behind a still-running
// test command, reusing the same vendor "terminate" path the manual
// terminate form uses. Keyed off the command's VendorOrderID — which is
// empty in the brief window between recording the command and dispatching
// it to the vendor, so we reject that case rather than fire a no-op
// terminate.
func (h *Handlers) apiTestCommandCancel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	tc, err := h.engine.TestCommandService().Get(id)
	if err != nil {
		h.jsonError(w, "command not found", http.StatusNotFound)
		return
	}
	if tc.CompletedAt != nil {
		h.jsonError(w, "command already completed", http.StatusConflict)
		return
	}
	if tc.VendorOrderID == "" {
		h.jsonError(w, "command has no vendor order to terminate yet", http.StatusConflict)
		return
	}

	commander, ok := h.engine.Fleet().(fleet.VendorCommander)
	if !ok {
		h.jsonError(w, "fleet backend does not support vendor commands", http.StatusNotImplemented)
		return
	}

	if _, err := commander.ExecuteVendorCommand(fleet.VendorCommand{
		Type:    "terminate",
		OrderID: tc.VendorOrderID,
	}); err != nil {
		log.Printf("test-commands: terminate id=%d vendor=%s failed: %v", id, tc.VendorOrderID, err)
		h.jsonError(w, "terminate failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.TestCommandService().UpdateStatus(id, "TERMINATED", "terminated from /test-orders")
	h.engine.TestCommandService().Complete(id)
	log.Printf("test-commands: terminated id=%d vendor=%s", id, tc.VendorOrderID)
	h.jsonOK(w, map[string]any{"id": id, "status": "terminated"})
}

func (h *Handlers) apiTestCommandsList(w http.ResponseWriter, r *http.Request) {
	cmds, err := h.engine.TestCommandService().List(50)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, cmds)
}
