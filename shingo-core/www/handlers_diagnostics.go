package www

import (
	"errors"
	"net/http"

	"shingo/protocol"
	"shingocore/engine"
)

func (h *Handlers) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	cfg := h.engine.AppConfig()
	subsystem := r.URL.Query().Get("subsystem")
	anomalies, err := h.engine.Reconciliation().ListAnomalies()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	summary, err := h.engine.Reconciliation().Summary()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	recoveryActions, err := h.engine.Reconciliation().ListRecoveryActions(50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Page":                "logs", // DO NOT change — drives nav active state
		"Entries":             h.debugLog.Entries(subsystem),
		"Subsystems":          h.debugLog.Subsystems(),
		"Subsystem":           subsystem,
		"Anomalies":           anomalies,
		"Recon":               summary,
		"RecoveryActions":     recoveryActions,
		"FireAlarmEnabled":    cfg.FireAlarm.Enabled,
		"FireAlarmAutoResume": cfg.FireAlarm.AutoResumeDefault,
	}
	h.render(w, r, "diagnostics.html", data)
}

func (h *Handlers) apiHealthCheck(w http.ResponseWriter, r *http.Request) {
	fleetOK := false
	if err := h.engine.Fleet().Ping(); err == nil {
		fleetOK = true
	}
	dbOK := h.engine.HealthService().PingDB() == nil
	recon, err := h.engine.Reconciliation().Summary()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]any{
		"status":         recon.Status,
		"fleet":          fleetOK,
		"messaging":      h.engine.MsgClient().IsConnected(),
		"database":       dbOK,
		"reconciliation": recon,
	})
}

func (h *Handlers) apiReconciliation(w http.ResponseWriter, r *http.Request) {
	summary, err := h.engine.Reconciliation().Summary()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	anomalies, err := h.engine.Reconciliation().ListAnomalies()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]any{
		"summary":   summary,
		"anomalies": anomalies,
	})
}

func (h *Handlers) apiListDeadLetterOutbox(w http.ResponseWriter, r *http.Request) {
	msgs, err := h.engine.Reconciliation().ListDeadLetterOutbox(200)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, msgs)
}

func (h *Handlers) apiListRecoveryActions(w http.ResponseWriter, r *http.Request) {
	items, err := h.engine.Reconciliation().ListRecoveryActions(100)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, items)
}

func (h *Handlers) apiReplayOutbox(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseIDParam(w, r, "id")
	if !ok {
		return
	}
	if err := h.engine.Reconciliation().RequeueOutbox(id); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiRepairAnomaly(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action  string `json:"action"`
		OrderID int64  `json:"order_id"`
		BinID   int64  `json:"bin_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	actor := h.getUsername(r)
	if actor == "" {
		actor = protocol.AuditActorUI
	}

	switch req.Action {
	case "reapply_completion":
		if req.OrderID == 0 {
			h.jsonError(w, "order_id is required", http.StatusBadRequest)
			return
		}
		if err := h.engine.Recovery().ReapplyOrderCompletion(req.OrderID, actor); err != nil {
			h.jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	case "force_confirm_delivered":
		if req.OrderID == 0 {
			h.jsonError(w, "order_id is required", http.StatusBadRequest)
			return
		}
		if err := h.engine.Recovery().ForceConfirmDelivered(req.OrderID, actor); err != nil {
			h.jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	case "release_terminal_claim":
		if req.BinID == 0 {
			h.jsonError(w, "bin_id is required", http.StatusBadRequest)
			return
		}
		if err := h.engine.Recovery().ReleaseTerminalBinClaim(req.BinID, actor); err != nil {
			h.jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	case "release_staged_bin":
		if req.BinID == 0 {
			h.jsonError(w, "bin_id is required", http.StatusBadRequest)
			return
		}
		if err := h.engine.Recovery().ReleaseStagedBin(req.BinID, actor); err != nil {
			h.jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	case "cancel_stuck_order":
		if req.OrderID == 0 {
			h.jsonError(w, "order_id is required", http.StatusBadRequest)
			return
		}
		if err := h.engine.Recovery().CancelStuckOrder(req.OrderID, actor); err != nil {
			h.jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	case "recover_carried_bin":
		// Ask the robot holding this bin to set it down. The only recovery
		// action here that DISPATCHES rather than repairing a record — every
		// other case above rewrites Core's bookkeeping, this one puts a robot
		// on the floor in motion, which is why the refusals are surfaced
		// verbatim rather than flattened to "could not repair".
		if req.BinID == 0 {
			h.jsonError(w, "bin_id is required", http.StatusBadRequest)
			return
		}
		order, detail, err := h.engine.Recovery().RecoverCarriedBin(req.BinID, actor)
		if err != nil {
			// THE REASON, VERBATIM. Every refusal from this door is a sentence
			// somebody wrote for a person — "AMR-09 is not dispatchable, the
			// plant has taken it out of the pool" — and the caller is a button
			// on the bins page that shows what it is given. Error() would wrap
			// it in "bin 5 cannot be recovered by order right now:", which the
			// row the operator is looking at already says.
			var refused *engine.CarriedBinNotRecoverable
			if errors.As(err, &refused) {
				h.jsonError(w, refused.Reason, http.StatusBadRequest)
				return
			}
			h.jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		// THE ONLY CASE THAT ANSWERS WITH MORE THAN "ok", because it is the only
		// one that put a robot in motion. The detail names the destination and
		// which tier chose it, and the person who pressed the button is owed
		// both — "why did it go there" is the question a misplaced bin raises.
		h.jsonOK(w, map[string]any{
			"status":   "ok",
			"order_id": order.ID,
			"detail":   detail,
		})
		return
	default:
		h.jsonError(w, "unknown recovery action", http.StatusBadRequest)
		return
	}

	h.jsonSuccess(w)
}
