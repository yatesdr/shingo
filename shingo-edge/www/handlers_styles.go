// handlers_styles.go — style CRUD and clone. A style's claims are written by
// the flow (flow/save, flow/mint) and nothing here: the single-claim editor
// endpoints, copy-to and generate went in pick-a-part U6.

package www

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"shingoedge/domain"
)

// --- Styles Admin ---

func (h *Handlers) apiListStyles(w http.ResponseWriter, r *http.Request) {
	styles, err := h.engine.StyleService().List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, styles)
}

func (h *Handlers) apiCreateStyle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		Description   string `json:"description"`
		ProcessID     int64  `json:"process_id"`
		ExpectedCATID string `json:"expected_catid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ProcessID == 0 {
		writeError(w, http.StatusBadRequest, "process_id is required")
		return
	}
	id, err := h.engine.StyleService().Create(req.Name, req.Description, req.ProcessID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// expected_catid is a separate, optional field (own setter) so the many
	// Create call sites stay untouched. Trim + persist alongside the create.
	if err := h.engine.StyleService().SetExpectedCATID(id, strings.TrimSpace(req.ExpectedCATID)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-created")
	// A new style changes what the process can run, and PlantClaimsReport
	// carries every style with its Active flag — so Core's mirror is wrong
	// until this lands. Missing here until 2026-08-22; the 5-minute snapshot
	// timer hid it by catching up within a tick, which stops being true now
	// the safety snapshot is hourly.
	h.requestSpecChangePublish(req.ProcessID)
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name          string `json:"name"`
		Description   string `json:"description"`
		ProcessID     int64  `json:"process_id"`
		ExpectedCATID string `json:"expected_catid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ProcessID == 0 {
		writeError(w, http.StatusBadRequest, "process_id is required")
		return
	}
	// The process the style belonged to BEFORE the write. An update can move
	// a style between processes, and then both reports are stale: the old
	// one still lists it, the new one does not. Read it first; the row is
	// about to change.
	prev, _ := h.engine.StyleService().Get(id)
	if err := h.engine.StyleService().Update(id, req.Name, req.Description, req.ProcessID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// expected_catid is written by its own setter (see apiCreateStyle) so the
	// Update signature and its call sites stay unchanged. Trim + persist.
	if err := h.engine.StyleService().SetExpectedCATID(id, strings.TrimSpace(req.ExpectedCATID)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-updated")
	// Same as apiCreateStyle: the style NAME is what rides the wire as
	// PlantClaimsStyle.StyleID, so a rename that never republishes leaves Core
	// mirroring a style that no longer exists under that name. A move
	// republishes the process it left as well.
	h.requestSpecChangePublish(req.ProcessID)
	if prev != nil && prev.ProcessID != 0 && prev.ProcessID != req.ProcessID {
		h.requestSpecChangePublish(prev.ProcessID)
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiStyleDeleteImpact answers "what is behind this delete button".
//
// The delete itself is a soft delete and destroys nothing, so this is not a
// warning — it is the confirmation being honest about scope. On the Springfield
// edge the same button covers a style carrying one row and a style carrying
// 91,581, and the operator currently cannot tell those apart. It is also the
// exact cost of a future hard purge, which is the number that had never been
// put in front of anybody.
func (h *Handlers) apiStyleDeleteImpact(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	imp, err := h.engine.StyleService().DeleteImpact(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, imp)
}

// apiDeleteStyle RETIRES a style. The row survives, so changeover history keeps
// resolving its name and nothing that points at it is left dangling.
//
// It refuses (409) a style a process is RUNNING, or one an open changeover is
// taking a process TO. Retiring drops the style's claims from Core's demand
// registry, so the first ended every demand of a live cell with nothing on the
// floor saying why, and the second left a changeover heading for a style with
// no claims. Episodes need no closing here: they key on node, payload, process
// or changeover (protocol/episode_key.go), never on a style, and a style that
// is neither running nor a target is driving none.
func (h *Handlers) apiDeleteStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if reason, err := h.styleInUse(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	} else if reason != "" {
		writeError(w, http.StatusConflict, reason)
		return
	}
	if err := h.engine.StyleService().Delete(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-deleted")
	// A retired style's claims must leave Core's demand_registry, exactly as a
	// hard delete's would have: ListStylesByProcess (which is what the publisher
	// walks) no longer returns it. GetStyle is unfiltered, so the retired row
	// still names its process.
	h.publishSpecChangeForStyle(id)
	writeJSON(w, map[string]string{"status": "ok"})
}

// styleInUse names why a style cannot be retired, or "" when it can. A read
// that fails is an error, never an allow: the refusal exists because the
// unguarded path is silent.
func (h *Handlers) styleInUse(styleID int64) (string, error) {
	procs, err := h.engine.ProcessService().List()
	if err != nil {
		return "", fmt.Errorf("check style in use: list processes: %w", err)
	}
	for _, p := range procs {
		if p.ActiveStyleID != nil && *p.ActiveStyleID == styleID {
			return fmt.Sprintf("style is the active style of process %q — change the process to another style first", p.Name), nil
		}
		co, err := h.engine.ChangeoverService().GetActive(p.ID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return "", fmt.Errorf("check style in use: changeover of process %q: %w", p.Name, err)
		}
		if co != nil && co.ToStyleID == styleID {
			return fmt.Sprintf("style is the target of process %q's open changeover — finish or cancel it first", p.Name), nil
		}
	}
	return "", nil
}

// apiRestoreStyle un-retires a style. Soft delete without an undo is just a
// slower delete.
func (h *Handlers) apiRestoreStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := h.engine.StyleService().Restore(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-restored")
	h.publishSpecChangeForStyle(id)
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiCloneStyle scaffolds one new style from an existing one, copying its
// node-claim choreography verbatim. The operator edits the per-payload fields
// on the result afterward (in the Node Claims compare grid). The clone starts
// inactive — it never triggers a changeover.
func (h *Handlers) apiCloneStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	// The clone's claims are attributed to the session user, source 'cloned'.
	calledBy, _ := h.sessions.getUser(r)
	newID, err := h.engine.StyleService().Clone(h.coreNodeKinds(), id, strings.TrimSpace(req.Name), strings.TrimSpace(req.Description), calledBy)
	if err != nil {
		writeError(w, styleWriteStatus(err), err.Error())
		return
	}
	h.requestBackup("style-cloned")
	// Cloned claims must reach Core's demand_registry just like edits do. The
	// clone lives in the source's process.
	h.publishSpecChangeForStyle(newID)
	writeJSON(w, map[string]int64{"id": newID})
}

// coreNodeKinds is Core's lane and maintained-group sets as the engine holds
// them, for a store write that refuses a lane on a leg. Empty before the first
// node list, which refuses nothing.
func (h *Handlers) coreNodeKinds() domain.CoreNodeKinds {
	return domain.CoreNodeKindsOf(h.engine.CoreNodes())
}

// styleWriteStatus is 400 for a clone refused because a copied
// claim names a lane, and 500 for anything else.
func styleWriteStatus(err error) int {
	if errors.Is(err, domain.ErrLaneLeg) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// publishSpecChangeForStyle requests a plant-claims publish for the process
// that owns styleID. The style handlers arrive holding a style id and not a
// process id, so the style → process step lives here once.
//
// The whole-plant fallback is the failure path, not the default: it fires
// only when the style cannot be read back after a write that already landed,
// and it logs, because a Core mirror that silently stays stale is the
// condition the hourly safety snapshot exists to bound and nothing should
// lean on it on purpose.
func (h *Handlers) publishSpecChangeForStyle(styleID int64) {
	style, err := h.engine.StyleService().Get(styleID)
	if err != nil || style == nil || style.ProcessID == 0 {
		log.Printf("spec change: style %d has no resolvable process (%v) — publishing every process", styleID, err)
		h.requestSpecChangePublishAll()
		return
	}
	h.requestSpecChangePublish(style.ProcessID)
}
