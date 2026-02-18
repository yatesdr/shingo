package www

import (
	"net/http"
)

func (h *Handlers) handleNodeState(w http.ResponseWriter, r *http.Request) {
	states, _ := h.engine.NodeState().GetAllNodeStates()
	nodes, _ := h.engine.DB().ListNodes()

	data := map[string]any{
		"Page":          "nodestate",
		"Nodes":         nodes,
		"States":        states,
		"Authenticated": h.isAuthenticated(r),
	}
	h.render(w, "nodestate.html", data)
}
