package www

import (
	"log"
	"net/http"
	"strconv"

	"shingocore/fleet"
	"shingocore/store"
)

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

	tc := &store.TestCommand{
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

	if dbErr := h.engine.DB().CreateTestCommand(tc); dbErr != nil {
		log.Printf("test-commands: db save error: %v", dbErr)
	}
	if result != nil && result.State == "COMPLETED" {
		h.engine.DB().CompleteTestCommand(tc.ID)
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

	tc, err := h.engine.DB().GetTestCommand(id)
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
					h.engine.DB().UpdateTestCommandStatus(id, detail.State, "")
					tc.VendorState = detail.State
				}
				if detail.IsTerminal {
					h.engine.DB().CompleteTestCommand(id)
				}
			}
		}
	}

	h.jsonOK(w, map[string]any{
		"command":       tc,
		"vendor_detail": vendorDetail,
	})
}

func (h *Handlers) apiTestCommandsList(w http.ResponseWriter, r *http.Request) {
	cmds, err := h.engine.DB().ListTestCommands(50)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, cmds)
}
