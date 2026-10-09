package www

// handlers_containment.go — the Edge side of quality containment (v100).
//
// The page is a shop-floor screen (public, like the operator stations): it
// renders the Edge's held copy of Core's containment feed (LocalContainment —
// Core pushes it on every known containment write and the heartbeat heals a
// missed push), grouped by this Edge's own claims that declare a containment
// route, with the three verbs:
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
// Rendering makes no call to Core. Nothing is blanked for being old: a copy
// Core has not confirmed for FeedAsOfAfter renders with "as of HH:MM" beside
// it. The page reloads on the SSE `containment` event, sent when the held copy
// changes; the release verb still refuses a bin that is no longer there (the
// engine checks the bin is still standing at the node before creating
// anything).

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine"
)

// The page's lines for a missing copy. With nothing held, the page says why:
// no snapshot has arrived yet, or the last ack came from a Core that does not
// send the feed at all, which no amount of waiting will fix.
const (
	containmentNoData   = "No data from Core yet"
	containmentOldCore  = "Core does not send containment (older Core)"
	containmentAsOfZero = "an unknown time"
)

// containmentFeedEngine is what the page reads about the feed beyond the copy:
// whether the last heartbeat ack came from a Core that sends feeds. Asserted on
// the orchestration surface, as /status asserts statusEngine, so neither
// interface widens for it.
type containmentFeedEngine interface {
	CoreSpeaksFeeds() bool
	LastCoreAck() (local, server time.Time)
}

// containmentFlagRow and containmentHeldRow are the feed's rows as the page and
// its JSON twin render them: the times as the RFC 3339 text Core's JSON
// carries, and "" for none — the shape the page served when it read Core's
// body into string fields, so the JSON and the rendered "Since" column are
// unchanged.
type containmentFlagRow struct {
	PayloadCode   string `json:"payload_code"`
	Active        bool   `json:"active"`
	Reason        string `json:"reason"`
	ActivatedBy   string `json:"activated_by"`
	ActivatedAt   string `json:"activated_at"`
	DeactivatedBy string `json:"deactivated_by"`
	DeactivatedAt string `json:"deactivated_at"`
}

type containmentHeldRow struct {
	BinID       int64  `json:"bin_id"`
	Label       string `json:"label"`
	PayloadCode string `json:"payload_code"`
	NodeName    string `json:"node_name"`
	HoldBy      string `json:"hold_by"`
	HoldAt      string `json:"hold_at"`
}

// containmentBin is one bin at one containment node as the page renders it —
// the feed's destination bin, named by its suffix under a group destination.
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

// containmentView is everything the page and its JSON twin render.
type containmentView struct {
	Flags    []containmentFlagRow
	HeldBins []containmentHeldRow
	Sections []containmentSection
	// AsOf is the plant-time HH:MM the held copy was last current at, set
	// only when that is longer ago than engine.FeedAsOfAfter.
	AsOf string
	// Notice replaces the flags and held bins when nothing is held.
	Notice string
}

func (h *Handlers) handleContainmentPage(w http.ResponseWriter, r *http.Request) {
	v, err := h.containmentPicture()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.renderTemplate(w, r, "containment.html", map[string]any{
		"Page":        "containment",
		"Containment": v.Flags,
		"Sections":    v.Sections,
		"HeldBins":    v.HeldBins,
		"AsOf":        v.AsOf,
		"Notice":      v.Notice,
	})
}

// apiGetContainmentState is the page's JSON twin — same picture for a poller
// or the station screens. "as_of" and "notice" appear only when set, so a
// current copy serves the body it always did.
func (h *Handlers) apiGetContainmentState(w http.ResponseWriter, r *http.Request) {
	v, err := h.containmentPicture()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	body := map[string]any{"containment": v.Flags, "sections": v.Sections, "held_bins": v.HeldBins}
	if v.AsOf != "" {
		body["as_of"] = v.AsOf
	}
	if v.Notice != "" {
		body["notice"] = v.Notice
	}
	writeJSON(w, body)
}

// containmentPicture assembles everything the page renders from the held copy
// of Core's containment feed and this Edge's own claims that declare a
// containment route, grouped per destination. No call to Core.
func (h *Handlers) containmentPicture() (containmentView, error) {
	var v containmentView
	claimRows, err := h.engine.StyleService().ListContainmentClaims()
	if err != nil {
		return v, err
	}
	dests := map[string]protocol.ContainmentDestination{}
	if state, held := h.engine.LocalContainment(); held {
		v.Flags = containmentFlagRows(state.Containment)
		v.HeldBins = containmentHeldRows(state.HeldBins)
		for _, d := range state.Destinations {
			dests[d.Node] = d
		}
		v.AsOf = containmentAsOf(state, time.Now())
	} else {
		v.Notice = h.containmentNotice()
	}
	v.Sections = containmentSections(claimRows, dests)
	return v, nil
}

// containmentSections groups the claims per destination, in claim order, with
// each destination's outbounds from the claims and its children and bins from
// the feed. Children render by SUFFIX only (the model names group children
// "Group.Child"; the prefix is the parent's, printed beside them already).
func containmentSections(claimRows []domain.NodeClaim, dests map[string]protocol.ContainmentDestination) []containmentSection {
	byNode := map[string]*containmentSection{}
	var order []string
	for _, c := range claimRows {
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
	for _, name := range order {
		sec := byNode[name]
		d := dests[name]
		for _, ch := range d.Children {
			sec.Children = append(sec.Children, childDisplaySuffix(name, ch))
		}
		for _, b := range d.Bins {
			sec.Bins = append(sec.Bins, containmentBin{
				NodeName: childDisplaySuffix(name, b.Node), BinID: b.BinID,
				Label: b.Label, PayloadCode: b.PayloadCode, UOP: b.UOP,
			})
		}
		sections = append(sections, *sec)
	}
	return sections
}

// containmentNotice says why nothing is held: an older Core (the last ack
// carried no feeds map) or no snapshot yet.
func (h *Handlers) containmentNotice() string {
	if fe, ok := h.orchestration.(containmentFeedEngine); ok {
		if local, _ := fe.LastCoreAck(); !local.IsZero() && !fe.CoreSpeaksFeeds() {
			return containmentOldCore
		}
	}
	return containmentNoData
}

// containmentAsOf is the "as of" time for a held copy, or "" while it is
// current. The copy is current as of the later of its arrival and Core's last
// confirmation: a snapshot that has just arrived is as fresh as a confirmed one.
func containmentAsOf(st *engine.ContainmentState, now time.Time) string {
	current := st.ConfirmedAt
	if st.ReceivedAt.After(current) {
		current = st.ReceivedAt
	}
	if now.Sub(current) <= engine.FeedAsOfAfter {
		return ""
	}
	if current.IsZero() {
		return containmentAsOfZero
	}
	return current.In(plantLocation).Format("15:04")
}

func containmentFlagRows(rows []protocol.PayloadContainmentRow) []containmentFlagRow {
	out := make([]containmentFlagRow, len(rows))
	for i, r := range rows {
		out[i] = containmentFlagRow{
			PayloadCode: r.PayloadCode, Active: r.Active, Reason: r.Reason,
			ActivatedBy: r.ActivatedBy, ActivatedAt: wireTimeText(r.ActivatedAt),
			DeactivatedBy: r.DeactivatedBy, DeactivatedAt: wireTimeText(r.DeactivatedAt),
		}
	}
	return out
}

func containmentHeldRows(rows []protocol.HeldBinRow) []containmentHeldRow {
	out := make([]containmentHeldRow, len(rows))
	for i, r := range rows {
		out[i] = containmentHeldRow{
			BinID: r.BinID, Label: r.Label, PayloadCode: r.PayloadCode,
			NodeName: r.NodeName, HoldBy: r.HoldBy, HoldAt: wireTimeText(r.HoldAt),
		}
	}
	return out
}

// wireTimeText is a time as encoding/json writes it (RFC 3339 with
// nanoseconds, in the time's own zone), "" for none.
func wireTimeText(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339Nano)
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
