// handlers_parts_registry.go — the parts TABLE: a part number and the cat id a
// PLC declares for it. Distinct from handlers_parts.go next door, which serves
// the Operations dashboard's per-part METRICS (produced, duration, consumption)
// and reads no part row at all.
package www

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

// apiGetPart answers "do we already know this part number, and what is its cat
// id?" — the one piece of recall the payloads form does.
//
// IT IS A LOOKUP, NOT A PICKER. The form has no dropdown over parts and is not
// getting one: a person typing a part number is stating a fact, and a list to
// choose from turns that into a search through hundreds of codes where the
// wrong neighbour is one keystroke away. What this buys is the opposite — the
// operator types the number they have, and if shingo has seen it the cat id
// fills itself in, so the common case involves no second decision.
//
// 404 IS A NORMAL ANSWER and means "new part". The payloads page is where a
// part is originated, so much of what is typed here has never been seen before;
// an unknown number is the start of a part, not a failed lookup.
func (h *Handlers) apiGetPart(w http.ResponseWriter, r *http.Request) {
	number := strings.TrimSpace(r.URL.Query().Get("part_number"))
	if number == "" {
		h.jsonError(w, "part_number is required", http.StatusBadRequest)
		return
	}
	part, err := h.engine.PayloadService().GetPart(number)
	if errors.Is(err, sql.ErrNoRows) {
		h.jsonError(w, "no part "+number, http.StatusNotFound)
		return
	}
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, part)
}

// apiListParts returns every part with both of its names.
func (h *Handlers) apiListParts(w http.ResponseWriter, r *http.Request) {
	parts, err := h.engine.PayloadService().ListParts()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, parts)
}

// apiSetPartCATID records a part's controls identity, overwriting what is
// there. This is the answer to the conflict prompt — "yes, same part, the new
// one is right" — and it is a separate, explicit call for that reason: an
// ordinary manifest save REFUSES the change rather than making it, so nothing
// rewrites a part's controls identity as a side effect of editing a payload.
func (h *Handlers) apiSetPartCATID(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PartNumber string `json:"part_number"`
		CATID      string `json:"catid"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.PartNumber) == "" {
		h.jsonError(w, "part_number is required", http.StatusBadRequest)
		return
	}
	if err := h.engine.PayloadService().SetPartCATID(req.PartNumber, strings.TrimSpace(req.CATID)); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.jsonSuccess(w)
}
