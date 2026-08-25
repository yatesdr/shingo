// NOTE: despite the "_test_" in the filename, this is not a Go test file.
// It implements the operator-facing /test-orders admin page — a synthetic
// order testbench operators use to exercise the system via the web UI
// (UUID prefix "test-", station "core-test"). Rename to
// handlers_synthetic_orders.go is deferred to a dedicated PR: changing
// the URL path is a breaking contract for operator bookmarks and
// external automation, so that lives on its own with redirects.
//
// This file holds the page render and read-only list endpoints. Action
// endpoints live in handlers_test_orders_{kafka,direct,commands}.go.

package www

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/fleet"
)

// --- Test Orders Page ---

// nodePickerOption is one line in this page's node dropdowns: the value the
// form posts, and what the human reads.
type nodePickerOption struct {
	ID    int64
	Name  string
	Label string
}

// nodeTypeLabel is the container badge a node carries in the picker: "group",
// "lane", or none. A synthetic node is a group by construction.
func nodeTypeLabel(n *domain.Node) string {
	switch strings.ToUpper(n.NodeTypeCode) {
	case protocol.NodeClassNGRP:
		return "group"
	case protocol.NodeClassLANE:
		return "lane"
	}
	if n.IsSynthetic {
		return "group"
	}
	return ""
}

// buildNodePickerOptions turns the node table into something safe to pick from.
//
// This page's fifteen node dropdowns were the raw node list, in table order,
// showing nothing but the name — which meant a disabled node looked exactly
// like a live one, and a synthetic container (a node group, a lane) looked
// exactly like a slot you could send a robot to. Picking a container is the
// dead end operators hit on the orders page before it started badging them;
// this page never got the same treatment.
//
// So: disabled nodes are gone, containers say what they are, and children sit
// under their parent, zone by zone. Everything enabled stays selectable — a
// group IS a valid source for a group-retrieve, so this marks the difference
// rather than hiding it.
//
// The orders page does the same job in JS (buildNodeOptionsHTML in
// static/pages/orders.js) because it fetches its nodes over /api/nodes; this
// page renders server-side. Two implementations of one idea, which is worth
// knowing about — the shapes are deliberately the same so they read alike.
// This one nests to any depth (group → lane → slot); the JS one stops at one
// level.
func buildNodePickerOptions(all []*domain.Node) []nodePickerOption {
	live := make([]*domain.Node, 0, len(all))
	byID := make(map[int64]*domain.Node, len(all))
	for _, n := range all {
		if !n.Enabled {
			continue
		}
		live = append(live, n)
		byID[n.ID] = n
	}

	children := make(map[int64][]*domain.Node, len(live))
	var roots []*domain.Node
	for _, n := range live {
		// A node whose parent is disabled (or gone) is displayed as a root
		// rather than dropped — it is still a real, selectable place.
		if n.ParentID != nil {
			if _, ok := byID[*n.ParentID]; ok {
				children[*n.ParentID] = append(children[*n.ParentID], n)
				continue
			}
		}
		roots = append(roots, n)
	}
	byName := func(ns []*domain.Node) {
		sort.Slice(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
	}
	byName(roots)
	for _, kids := range children {
		byName(kids)
	}

	// A container carries no zone of its own, so it sits with its contents'.
	// Searching DESCENDANTS, not just direct children: a group's children are
	// lanes, which have no zone either, so a one-level look always misses and
	// every group lands under "Other" — away from the slots it contains.
	var zoneOf func(n *domain.Node) string
	zoneOf = func(n *domain.Node) string {
		if n.Zone != "" {
			return n.Zone
		}
		for _, c := range children[n.ID] {
			if z := zoneOf(c); z != "Other" {
				return z
			}
		}
		return "Other"
	}

	byZone := make(map[string][]*domain.Node)
	for _, n := range roots {
		z := zoneOf(n)
		byZone[z] = append(byZone[z], n)
	}
	zoneNames := make([]string, 0, len(byZone))
	for z := range byZone {
		zoneNames = append(zoneNames, z)
	}
	sort.Strings(zoneNames)

	var out []nodePickerOption
	var walk func(n *domain.Node, depth int)
	walk = func(n *domain.Node, depth int) {
		label := strings.Repeat("  ", depth)
		if depth > 0 {
			label += "↳ "
		}
		label += n.Name
		if t := nodeTypeLabel(n); t != "" {
			label += "  · " + t
		}
		out = append(out, nodePickerOption{ID: n.ID, Name: n.Name, Label: label})
		for _, c := range children[n.ID] {
			walk(c, depth+1)
		}
	}
	for _, z := range zoneNames {
		for _, n := range byZone[z] {
			walk(n, 0)
		}
	}
	return out
}

func (h *Handlers) handleTestOrders(w http.ResponseWriter, r *http.Request) {
	all, _ := h.engine.NodeService().ListNodes()
	payloads, _ := h.engine.PayloadService().List()
	data := map[string]any{
		"Page":     "test-orders",
		"Nodes":    buildNodePickerOptions(all),
		"Payloads": payloads,
	}
	h.render(w, r, "test-orders.html", data)
}

func (h *Handlers) apiTestOrdersList(w http.ResponseWriter, r *http.Request) {
	orders, err := h.engine.OrderService().ListOrdersByStation("core-test", 50)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, orders)
}

func (h *Handlers) apiTestOrderDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	svc := h.engine.OrderService()
	order, err := svc.GetOrder(id)
	if err != nil {
		h.jsonError(w, "order not found", http.StatusNotFound)
		return
	}
	history, _ := svc.ListOrderHistory(id)
	h.jsonOK(w, map[string]any{"order": order, "history": history})
}

func (h *Handlers) apiTestRobots(w http.ResponseWriter, r *http.Request) {
	rl, ok := h.engine.Fleet().(fleet.RobotLister)
	if !ok {
		h.jsonError(w, "fleet backend does not support robot listing", http.StatusNotImplemented)
		return
	}
	robots, err := rl.GetRobotsStatus()
	if err != nil {
		h.jsonError(w, "fleet error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, robots)
}

func (h *Handlers) apiTestScenePoints(w http.ResponseWriter, r *http.Request) {
	points, err := h.engine.NodeService().ListScenePoints()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, points)
}
