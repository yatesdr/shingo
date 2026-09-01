package www

import (
	"net/http"
	"strconv"

	"shingoedge/domain"
)

func (h *Handlers) handleOrders(w http.ResponseWriter, r *http.Request) {
	processes, _ := h.engine.ProcessService().List()

	// Determine active process from query param (0 = all processes)
	var activeProcessID int64
	if lineParam := r.URL.Query().Get("process"); lineParam != "" {
		if id, err := strconv.ParseInt(lineParam, 10, 64); err == nil {
			for _, l := range processes {
				if l.ID == id {
					activeProcessID = id
					break
				}
			}
		}
	}

	// Status filter — mirrors Core's orders handler exactly:
	//   status == ""      → Active tab: strict non-terminal set
	//   status == "all"   → All tab: every order
	//   anything else     → orders of that specific status (from all orders)
	filterStatus := r.URL.Query().Get("status")

	var orders []domain.Order
	switch {
	case filterStatus == "":
		// Active tab — non-terminal only (Core's ListActiveOrders predicate)
		if activeProcessID > 0 {
			orders, _ = h.engine.OrderService().ListActiveStrictByProcess(activeProcessID)
		} else {
			orders, _ = h.engine.OrderService().ListActiveStrict()
		}
	case filterStatus == "all":
		// All tab — every order
		if activeProcessID > 0 {
			orders, _ = h.engine.OrderService().ListAllByProcess(activeProcessID)
		} else {
			orders, _ = h.engine.OrderService().ListAll()
		}
	default:
		// Specific status pill — from ALL orders, post-filtered
		if activeProcessID > 0 {
			orders, _ = h.engine.OrderService().ListAllByProcess(activeProcessID)
		} else {
			orders, _ = h.engine.OrderService().ListAll()
		}
		var filtered []domain.Order
		for _, o := range orders {
			if string(o.Status) == filterStatus {
				filtered = append(filtered, o)
			}
		}
		orders = filtered
	}

	// Core-synced nodes for redirect dropdown
	coreNodes := h.engine.CoreNodes()
	knownNodes := make([]string, 0, len(coreNodes))
	for name := range coreNodes {
		knownNodes = append(knownNodes, name)
	}

	anomalies, rpMap := loadAnomalyData(h)

	data := map[string]any{
		"Page":              "orders",
		"Processes":         processes,
		"ActiveProcessID":   activeProcessID,
		"FilterStatus":      filterStatus,
		"ActiveOrders":      orders,
		"KnownNodes":        knownNodes,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
		// How long each still-acquiring order has been waiting. The board has
		// always shown WHY a parked order waits; without a duration beside it the
		// sentence reads the same at forty seconds and at four hours.
		"WaitSince": h.engine.OrderService().WaitSince(orders),
	}

	h.renderTemplate(w, r, "orders.html", data)
}

func (h *Handlers) handleOrdersPartial(w http.ResponseWriter, r *http.Request) {
	var activeProcessID int64
	if p := r.URL.Query().Get("process"); p != "" {
		if id, err := strconv.ParseInt(p, 10, 64); err == nil {
			activeProcessID = id
		}
	}

	filterStatus := r.URL.Query().Get("status")

	var orders []domain.Order
	switch {
	case filterStatus == "":
		if activeProcessID > 0 {
			orders, _ = h.engine.OrderService().ListActiveStrictByProcess(activeProcessID)
		} else {
			orders, _ = h.engine.OrderService().ListActiveStrict()
		}
	case filterStatus == "all":
		if activeProcessID > 0 {
			orders, _ = h.engine.OrderService().ListAllByProcess(activeProcessID)
		} else {
			orders, _ = h.engine.OrderService().ListAll()
		}
	default:
		if activeProcessID > 0 {
			orders, _ = h.engine.OrderService().ListAllByProcess(activeProcessID)
		} else {
			orders, _ = h.engine.OrderService().ListAll()
		}
		var filtered []domain.Order
		for _, o := range orders {
			if string(o.Status) == filterStatus {
				filtered = append(filtered, o)
			}
		}
		orders = filtered
	}

	data := map[string]any{
		"ActiveOrders": orders,
		// Same map the page builds — the partial IS the page's rows, and a
		// refresh that dropped the clock would blank it every three seconds.
		"WaitSince": h.engine.OrderService().WaitSince(orders),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "orders-body", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
