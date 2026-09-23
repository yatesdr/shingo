package www

// handlers_containment.go — the Edge side of quality containment (v100).
//
// The page is a shop-floor screen (public, like the operator stations): it
// renders Core's containment state (via the public /api/containment read —
// the Edge holds no Core credentials) grouped by the claims that declare a
// containment route, with the three verbs:
//
//	Verify Good  — a bin at a containment node, verified, walks to its
//	               claim's ordinary outbound (the FG drop). Per bin.
//	Hold         — parked here for the produce node's OWN station screen;
//	               this page shows the resulting state, it does not hold.
//	Recall       — a contained payload's bins still sitting at their FG
//	               outbound nodes walk into containment. Covers what landed
//	               at FG before/while the flag flipped (in-transit bins land
//	               at FG by design; recall is the mop).
//
// The state refreshes on reload; a kiosk left open sees stale bins and the
// release verb refuses a bin that is no longer there (the engine checks the
// bin is still standing at the node before creating anything).

import (
	"encoding/json"
	"net/http"
	"strings"

	"shingoedge/engine"
)

// containmentBin is one bin at one containment node as the page renders it —
// the Core bin read (FetchNodeBins) flattened onto the node it sits at.
type containmentBin struct {
	NodeName    string `json:"node_name"`
	BinID       int64  `json:"bin_id"`
	Label       string `json:"label"`
	PayloadCode string `json:"payload_code"`
	UOP         int    `json:"uop"`
}

// containmentSection is one containment node's render block: the node, the
// outbound a verified bin walks to (from the section's claims — refused
// ambiguous at the release verb, so the page shows the disagreement), and the
// bins standing there. For a GROUP destination the children render beside the
// group name (the parenthetical) and the bins read across the children —
// Core's group node has no bins of its own.
type containmentSection struct {
	NodeName  string           `json:"node_name"`
	Children  []string         `json:"children,omitempty"`
	Outbounds []string         `json:"outbounds"`
	Bins      []containmentBin `json:"bins"`
}

func (h *Handlers) handleContainmentPage(w http.ResponseWriter, r *http.Request) {
	state, sections, err := h.containmentPicture()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.renderTemplate(w, r, "containment.html", map[string]any{
		"Page":        "containment",
		"Containment": state.Containment,
		"Sections":    sections,
		"HeldBins":    state.HeldBins,
	})
}

// apiGetContainmentState is the page's JSON twin — same picture for a poller
// or the station screens.
func (h *Handlers) apiGetContainmentState(w http.ResponseWriter, r *http.Request) {
	state, sections, err := h.containmentPicture()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"containment": state.Containment, "sections": sections, "held_bins": state.HeldBins})
}

// containmentPicture assembles everything the page renders: Core's flag
// state + the claims that declare a containment route, grouped per
// destination, each with the bins standing there (read from Core, whose bins
// are the authoritative location record).
func (h *Handlers) containmentPicture() (*engine.ContainmentState, []containmentSection, error) {
	state, err := h.engine.CoreAPI().GetContainment()
	if err != nil {
		return nil, nil, err
	}
	claims, err := h.engine.StyleService().ListContainmentClaims()
	if err != nil {
		return nil, nil, err
	}
	byNode := map[string]*containmentSection{}
	var order []string
	for _, c := range claims {
		sec, ok := byNode[c.ContainmentDestination]
		if !ok {
			sec = &containmentSection{NodeName: c.ContainmentDestination}
			byNode[c.ContainmentDestination] = sec
			order = append(order, c.ContainmentDestination)
		}
		if c.OutboundDestination != "" && !contains(sec.Outbounds, c.OutboundDestination) {
			sec.Outbounds = append(sec.Outbounds, c.OutboundDestination)
		}
	}
	sections := make([]containmentSection, 0, len(order))
	coreNodes := h.engine.CoreNodes()
	for _, name := range order {
		sec := byNode[name]
		// A GROUP destination spreads across its children: read their names
		// for the header's parenthetical and read BINS across them — the
		// group node itself holds nothing. A concrete node reads direct.
		// Children render by SUFFIX only (the model names group children
		// "Group.Child"; the prefix is the parent's, printed beside them
		// already).
		binNodes := []string{name}
		if info, ok := coreNodes[name]; ok && info.NodeType == "NGRP" {
			if children, cerr := h.engine.CoreAPI().FetchNodeChildren(name, false); cerr == nil && len(children) > 0 {
				for _, ch := range children {
					if ch.NodeType == "NGRP" || ch.Name == name {
						continue
					}
					sec.Children = append(sec.Children, childDisplaySuffix(name, ch.Name))
				}
				if len(sec.Children) > 0 {
					binNodes = nil
					for _, ch := range children {
						if ch.NodeType == "NGRP" || ch.Name == name {
							continue
						}
						binNodes = append(binNodes, ch.Name)
					}
				}
			}
		}
		bins, _, berr := h.engine.CoreAPI().FetchNodeBins(binNodes)
		if berr == nil {
			for _, b := range bins {
				if b.Occupied {
					sec.Bins = append(sec.Bins, containmentBin{
						NodeName:    childDisplaySuffix(name, b.NodeName),
						BinID:       b.BinID,
						Label:       b.BinLabel,
						PayloadCode: b.PayloadCode,
						UOP:         b.UOPRemaining,
					})
				}
			}
		}
		sections = append(sections, *sec)
	}
	return state, sections, nil
}

// childDisplaySuffix renders a group child's name for a screen that already
// prints the parent: "Quality Hold.PLK_X1" beside "Quality Hold" reads as
// "PLK_X1". A child not prefixed with the parent renders verbatim.
func childDisplaySuffix(parent, child string) string {
	if prefix := parent + "."; strings.HasPrefix(child, prefix) {
		return child[len(prefix):]
	}
	return child
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// apiQualityHoldNode is the station screen's "Send to Quality Hold": the bin
// standing at this produce node goes to its claim's containment destination,
// now, as a move order.
func (h *Handlers) apiQualityHoldNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var body struct {
		Actor string `json:"actor"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	order, err := h.orchestration.SendBinToQualityHold(id, body.Actor)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]any{"status": "ok", "order_id": order.ID}, "refreshMaterial")
}

// apiContainmentRelease is Verify Good: one verified bin leaves containment
// for its claim's outbound.
func (h *Handlers) apiContainmentRelease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeName string `json:"node_name"`
		BinID    int64  `json:"bin_id"`
		Actor    string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.NodeName = strings.TrimSpace(req.NodeName)
	if req.NodeName == "" || req.BinID == 0 {
		writeError(w, http.StatusBadRequest, "node_name and bin_id are required")
		return
	}
	order, err := h.orchestration.ReleaseFromContainment(req.NodeName, req.BinID, req.Actor)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]any{"status": "ok", "order_id": order.ID}, "refreshMaterial")
}

// apiContainmentUnhold clears a bin's hold marker — the stray's way out. A
// bin held while in transit, or whose containment move died unobserved,
// otherwise sits on the held list forever: no release path reaches it, and
// the hold's purpose (keeping it out of the ordinary flow) is served by the
// marker, not by anything that would clear it. Machine path through Core's
// telemetry endpoint, station-level attribution.
func (h *Handlers) apiContainmentUnhold(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BinID int64 `json:"bin_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.BinID == 0 {
		writeError(w, http.StatusBadRequest, "bin_id is required")
		return
	}
	if err := h.engine.CoreAPI().SetBinQualityHold(req.BinID, false, "containment-screen"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

// apiContainmentRecall walks a contained payload's bins still at their FG
// outbound nodes into containment. Partial success is the honest shape: the
// response carries the count created AND the per-bin refusals.
func (h *Handlers) apiContainmentRecall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadCode string `json:"payload_code"`
		Actor       string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.PayloadCode = strings.TrimSpace(req.PayloadCode)
	if req.PayloadCode == "" {
		writeError(w, http.StatusBadRequest, "payload_code is required")
		return
	}
	created, refusals, err := h.orchestration.RecallContainedPayload(req.PayloadCode, req.Actor)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]any{"status": "ok", "created": created, "refusals": refusals}, "refreshMaterial")
}
