package www

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shingo/protocol/clock"
)

func (h *Handlers) handleDashboard(w http.ResponseWriter, r *http.Request) {
	activeOrders, _ := h.engine.OrderService().ListActiveOrders()
	nodes, _ := h.engine.NodeService().ListNodes()
	// The wait clock, from the same reader the orders board uses — one
	// instrument, so the wall and the board cannot disagree about how long an
	// order has been waiting.
	waitSince := h.waitSinceFor(activeOrders)

	// Count orders by status
	statusCounts := map[string]int{}
	for _, o := range activeOrders {
		statusCounts[string(o.Status)]++
	}

	// Node stats
	enabledNodes := 0
	for _, n := range nodes {
		if n.Enabled {
			enabledNodes++
		}
	}

	// Fleet health check
	fleetOK := false
	if err := h.engine.Fleet().Ping(); err == nil {
		fleetOK = true
	}

	msgOK := h.engine.MsgClient().IsConnected()
	dbOK := h.engine.HealthService().PingDB() == nil
	recon, _ := h.engine.Reconciliation().Summary()

	trackerCount := 0
	if t := h.engine.Tracker(); t != nil {
		trackerCount = t.ActiveCount()
	}

	depsOK, depReasons := h.dependencyState()

	// Inventory count anomalies: the open report_divergence episodes, where the
	// Edge's lineside report and Core's count disagree about a carrier or a
	// seat (owner ruling, 2026-09-24). The same list the Inventory page shows.
	// A failed read leaves the tile out rather than showing a zero it did not
	// measure.
	anomalies, err := h.engine.InventoryService().OpenReportDivergences()
	if err != nil {
		log.Printf("dashboard: count anomalies: %v", err)
		anomalies = nil
	}

	data := map[string]any{
		"Page": "dashboard",
		// Server-rendered so the strip is correct on first paint rather than
		// flashing empty until the first poll lands.
		"Health":       h.coreHealth(depsOK, depReasons),
		"ActiveOrders": activeOrders,
		"WaitSince":    waitSince,
		"StatusCounts": statusCounts,
		"TotalOrders":  len(activeOrders),
		"TotalNodes":   len(nodes),
		"EnabledNodes": enabledNodes,
		"FleetOK":      fleetOK,
		"FleetName":    h.engine.Fleet().Name(),
		"MessagingOK":  msgOK,
		"DatabaseOK":   dbOK,
		"PollerActive": trackerCount,
		"SSEClients":   h.eventHub.ClientCount(),
		"Recon":        recon,
	}
	if err == nil {
		// Built here, from the service's rows, rather than in a helper that
		// names their type: a www handler does not import the store.
		now := clock.Now()
		views := make([]countAnomalyView, 0, len(anomalies))
		for _, d := range anomalies {
			v := countAnomalyView{
				Bin: "bucket", Seat: d.Node, Payload: d.Payload, Class: countAnomalyClass[d.Class],
				Edge: countOrDash(d.EdgeCount), Core: countOrDash(d.CoreCount),
				Open: openFor(now.Sub(d.OpenedAt)), Station: d.Station,
			}
			if v.Class == "" {
				v.Class = d.Class
			}
			if d.BinID != nil {
				v.Bin = d.BinLabel
				if v.Bin == "" {
					v.Bin = "bin " + strconv.FormatInt(*d.BinID, 10)
				}
			}
			views = append(views, v)
		}
		data["CountAnomalies"] = views
	}
	h.render(w, r, "dashboard.html", data)
}

// countAnomalyView is one inventory count anomaly as the homepage prints it:
// the carrier, the seat, both counts, and how long it has been open.
type countAnomalyView struct {
	Bin     string
	Seat    string
	Payload string
	Class   string
	Edge    string
	Core    string
	Open    string
	Station string
}

// countAnomalyClass is the same wording the Inventory page uses.
var countAnomalyClass = map[string]string{
	"count":           "count differs",
	"epoch":           "Edge counting an old load",
	"not_at_seat":     "Edge carrier not here in Core",
	"unbound_carrier": "Core carrier not bound at the Edge",
	"empty_seat":      "Core carrier where the Edge has nothing",
	"bucket":          "lineside bucket differs",
}

func countOrDash(n *int) string {
	if n == nil {
		return "-"
	}
	return strconv.Itoa(*n)
}

// openFor prints an episode's age in the two largest units: "12m", "1h 30m",
// "3d 4h".
func openFor(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	m := int(d / time.Minute)
	switch {
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m < 24*60:
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	default:
		return fmt.Sprintf("%dd %dh", m/(24*60), (m%(24*60))/60)
	}
}

// apiEdgeReregister asks one edge (?station=) or every edge (omitted) to
// re-send its registration, refreshing the cell catalog on demand (Q-034) — the
// Dashboard "Re-sync edges" button. The round-trip is the existing
// edge.register_request over Kafka; the edge answers in a second or two with a
// fresh catalog that HandleEdgeRegister upserts. Auth-gated (it drives the fleet).
func (h *Handlers) apiEdgeReregister(w http.ResponseWriter, r *http.Request) {
	station := strings.TrimSpace(r.URL.Query().Get("station"))
	if err := h.engine.RequestEdgeReregister(station); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}
