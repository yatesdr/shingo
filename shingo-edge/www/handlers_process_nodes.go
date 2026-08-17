// handlers_process_nodes.go — process-node CRUD endpoints. Process
// nodes are the physical-floor positions a station claims; CRUD lives
// separately from the station handlers so the file names match what
// they manage.

package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"shingoedge/domain"
)

// writeNodeWriteError maps a failed process-node write to a status.
//
// A Core node may be modelled by exactly ONE process_nodes row per process —
// UNIQUE(process_id, core_node_name), installed by the collapse migration after
// Hopkinsville accumulated three PLN_01 rows and counted every press stroke three
// times. Mapping a node that is already mapped is now refused by the database,
// which is the point; but it is the caller's mistake, not a server fault, and it
// deserves a sentence an admin can act on rather than a 500 carrying raw SQLite.
func writeNodeWriteError(w http.ResponseWriter, in domain.NodeInput, err error) {
	if strings.Contains(err.Error(), "idx_process_nodes_process_core_name") ||
		(strings.Contains(err.Error(), "UNIQUE constraint failed") && strings.Contains(err.Error(), "core_node_name")) {
		writeError(w, http.StatusConflict,
			"Core node "+in.CoreNodeName+" is already mapped to a process node in this process. "+
				"A Core node belongs to one process node — move the existing one to this station instead of adding a second.")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func (h *Handlers) apiListConfiguredProcessNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.engine.ProcessService().ListNodes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, nodes)
}

func (h *Handlers) apiListConfiguredProcessNodesByStation(w http.ResponseWriter, r *http.Request) {
	stationID, err := parseID(r, "stationID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	nodes, err := h.engine.ProcessService().ListNodesByStation(stationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, nodes)
}

func (h *Handlers) apiCreateProcessNode(w http.ResponseWriter, r *http.Request) {
	var in domain.NodeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if msg, unknown := h.coreNodeNameIsUnknown(in.CoreNodeName); unknown {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	id, err := h.engine.ProcessService().CreateNode(in)
	if err != nil {
		writeNodeWriteError(w, in, err)
		return
	}
	_, _ = h.engine.ProcessService().EnsureNodeRuntime(id)
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateProcessNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in domain.NodeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if msg, unknown := h.coreNodeNameIsUnknown(in.CoreNodeName); unknown {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if err := h.engine.ProcessService().UpdateNode(id, in); err != nil {
		writeNodeWriteError(w, in, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteProcessNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.engine.ProcessService().DeleteNode(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// ── A MINTED core_node_name IS A CLAIM ABOUT CORE'S PLANT ─────────────────
//
// process_nodes.core_node_name is Edge's pointer at a node Core owns. Nothing
// enforces that the pointer resolves: it is free text on this side, typed by an
// operator into a config form, and a name that matches nothing on Core is
// accepted, stored, and only discovered later — as an order that will not
// dispatch, a UOP adjustment that lands nowhere, or a claim configured against
// a slot that does not exist.
//
// The link between the two node families is GUARDED, NOT CONSTRAINED, and it
// cannot be constrained: they live in different databases in different services.
// The cured members of this family were cured with constraints; this one can
// only be cured with a check at the moment the claim is made, which is here.
//
// THE LIST IS ALREADY IN THE ROOM. Core's node set arrives on the wire roughly
// every two minutes and Edge keeps it in engine.CoreNodes() — where it has been
// display-only, feeding pickers and the manual-order form. Reading it here is
// the entire available remedy and it is small.
//
// ── AND IT MUST KNOW WHETHER IT HAD THE INPUT TO CHECK ────────────────────
//
// An EMPTY node set is not evidence that a name is wrong. It means Core has not
// been heard from — a fresh Edge, a restart, a Kafka gap — and refusing every
// configuration write on that basis would brick setup exactly when somebody is
// most likely to be doing it. So an empty set SKIPS the check and says so;
// absence of data must never render as a finding.
//
// A NON-EMPTY set that does not contain the name IS evidence, and it refuses.
//
// The bare-name fallback matches the runtime's own key handling: Core sends
// group children as "Group.CHILD" for display, and the rest of Edge keys on the
// bare child name (see bareNodeName).
func (h *Handlers) coreNodeNameIsUnknown(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false // the empty-name case belongs to the store's own validation
	}
	known := h.engine.CoreNodes()
	if len(known) == 0 {
		log.Printf("process-node write: core node list is EMPTY, so %q could not be checked against "+
			"Core's plant — allowing. This is not a pass: Core has not been heard from.", name)
		return "", false
	}
	if _, ok := known[name]; ok {
		return "", false
	}
	// Group children arrive as "Group.CHILD"; the runtime keys on the bare name.
	for full := range known {
		if i := strings.LastIndex(full, "."); i >= 0 && full[i+1:] == name {
			return "", false
		}
	}
	return fmt.Sprintf(
		"core node %q does not exist on Core (%d nodes known). A process_node's core_node_name is a "+
			"pointer at a node Core owns; a name that resolves to nothing is stored happily and "+
			"discovered later as an order that will not dispatch or a count written nowhere. Check "+
			"the spelling against the node picker, or sync nodes if Core has just been reconfigured.",
		name, len(known)), true
}
