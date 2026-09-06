package www

import "net/http"

// apiCMSHealth answers whether the middleware feed is working.
//
// POSITIVE EVIDENCE, NOT INFERRED SILENCE. The interesting fields are the ones
// that say something WORKED — last_successful_post_at, posted_last_hour,
// configured_storerooms — because an empty queue is equally the signature of a
// subsystem nothing is handing work to. The `healthy` boolean and the `why`
// sentence beside it are computed in one place (service.CMSPostingService) so
// the page and any future reader agree on what the word means.
//
// GET /api/cms-health
func (h *Handlers) apiCMSHealth(w http.ResponseWriter, r *http.Request) {
	health, err := h.orchestration.CMSFeedHealth()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, health)
}
