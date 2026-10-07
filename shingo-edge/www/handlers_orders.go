package www

import (
	"net/http"
	"net/url"
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

	filterStatus := r.URL.Query().Get("status")
	orders, pager := h.ordersView(activeProcessID, filterStatus, requestedPage(r))

	// Core-synced nodes for redirect dropdown
	coreNodes := h.engine.CoreNodes()
	knownNodes := make([]string, 0, len(coreNodes))
	for name := range coreNodes {
		knownNodes = append(knownNodes, name)
	}

	data := map[string]any{
		"Page":            "orders",
		"Processes":       processes,
		"ActiveProcessID": activeProcessID,
		"FilterStatus":    filterStatus,
		"ActiveOrders":    orders,
		"Pager":           pager,
		"PageNumber":      pagerPage(pager),
		"KnownNodes":      knownNodes,
		// How long each still-acquiring order has been waiting. The board has
		// always shown WHY a parked order waits; without a duration beside it the
		// sentence reads the same at forty seconds and at four hours.
		"WaitSince": h.engine.OrderService().WaitSince(orders),
		// What became of the bin a cancelled order left on a robot — one line
		// under the status, only where Core sent a notice (bin_returns).
		"BinReturns": h.engine.OrderService().BinReturnLines(orders),
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
	orders, pager := h.ordersView(activeProcessID, filterStatus, requestedPage(r))

	data := map[string]any{
		"ActiveOrders": orders,
		"Pager":        pager,
		// Same map the page builds — the partial IS the page's rows, and a
		// refresh that dropped the clock would blank it every three seconds.
		"WaitSince":  h.engine.OrderService().WaitSince(orders),
		"BinReturns": h.engine.OrderService().BinReturnLines(orders),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "orders-body", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ordersView reads the rows for one view of the Orders page. Status, mirroring
// Core's orders handler:
//
//	""     → Active tab: the strict non-terminal set, not paged (it is small)
//	"all"  → All tab: every order, one page
//	other  → that status only (filtered in the SQL), one page
//
// A paged view shows the newest historyPageSize rows on page 1 (R25). The
// pager is nil on the Active tab.
func (h *Handlers) ordersView(processID int64, status string, page int) ([]domain.Order, *historyPage) {
	svc := h.engine.OrderService()
	if status == "" {
		var list []domain.Order
		if processID > 0 {
			list, _ = svc.ListActiveStrictByProcess(processID)
		} else {
			list, _ = svc.ListActiveStrict()
		}
		return list, nil
	}
	sqlStatus := status
	if status == "all" {
		sqlStatus = ""
	}
	read := func(p int) ([]domain.Order, int) {
		list, total, err := svc.ListPage(sqlStatus, processID, historyPageSize, (p-1)*historyPageSize)
		if err != nil {
			return nil, 0
		}
		return list, total
	}
	list, total := read(page)
	if clamped := clampPage(page, total); clamped != page {
		page = clamped
		list, total = read(page)
	}
	q := url.Values{}
	q.Set("status", status)
	q.Set("process", strconv.FormatInt(processID, 10))
	return list, newHistoryPage("/orders", q, page, len(list), total)
}

// pagerPage is the page number a view's live refresh must keep (1 when the
// view is not paged).
func pagerPage(p *historyPage) int {
	if p == nil {
		return 1
	}
	return p.Page
}
