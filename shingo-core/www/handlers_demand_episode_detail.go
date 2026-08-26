package www

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// handlers_demand_episode_detail.go — origin-indexed forensics (Stage 5.12, N2).
//
// /demand-episodes/{originID}. The list page ranks demands by what they cost;
// this one opens a single demand and shows every order it spawned, each linked
// to the mission detail that already exists for it. The design's words for the
// grain: "one demand, every order it spawned, every transition with code/actor/
// ref, every leg, and the logs for all of it" — the last three of those live on
// /missions/{orderID} and are reached from here rather than reproduced.
//
// NOT A CYCLE BROWSER OVER TAKT. "Cycle" is overloaded in this codebase: the
// production-cycle surfaces read bin_uop_ledger at the takt grain, while this
// reads the order/mission lifecycle at the demand grain. Different table,
// different grain, no dependency between them.

// childOrderLimit caps how many child orders one episode's page will list.
//
// NOT A DISPLAY CONSTANT in the provenance sense, and the distinction is the one
// config/provenance.go draws: the constants that rule covers are the ones that
// LOOK LIKE KNOWLEDGE about the floor — a worry line, a minimum denominator.
// This is a query scope. A reader cannot mistake "the first 500 orders" for a
// claim that 500 orders is normal or healthy, and the page says so in words when
// it bites. Same standing as episodeWindow and episodeLimit next door.
//
// The number is a guess and is declared as one. Children-per-episode is
// UNMEASURED at a plant; the simulator's mean of 3.36 with a max of 13 is a
// property of demo.yaml's tick rates, not a bound. 500 is set high enough that
// the cap should never bite on anything resembling the sim, so if it ever does
// bite that is itself the finding — which is why it is reported to the page
// rather than applied silently.
const childOrderLimit = 500

func (h *Handlers) handleDemandEpisodeDetail(w http.ResponseWriter, r *http.Request) {
	originID := chi.URLParam(r, "originID")

	// VALIDATED BEFORE IT REACHES POSTGRES, and this is not defensive
	// boilerplate. origin_id is a UUID column, so a malformed path segment makes
	// the driver raise `invalid input syntax for type uuid` — an ERROR, which
	// this page would then render as "the episode store could not be read". That
	// is the same conflation the whole surface exists to prevent, one level up:
	// a bad request rendered as an outage. A typo in a URL is a 400.
	if _, err := uuid.Parse(originID); err != nil {
		http.Error(w, "invalid episode id", http.StatusBadRequest)
		return
	}

	now := time.Now()
	c := h.engine.AppConfig().DisplayConstants()

	data := map[string]any{
		"Page":         "demand-episodes",
		"WorryAfter":   FormatDuration(c.WorryAfter),
		"ConcernAfter": FormatDuration(c.ConcernAfter),
		"MinExpected":  c.MinExpectedOrders,
		"OriginID":     originID,
	}

	origin, err := h.engine.DemandEpisodeService().Get(originID)
	if err != nil {
		// A read failure, shown as itself. Rendering "no such episode" here would
		// tell someone their link was wrong when the truth is the database is
		// unreachable.
		data["LoadError"] = err.Error()
		h.render(w, r, "demand-episode.html", data)
		return
	}
	if origin == nil {
		// The OTHER fact, and it gets the other status code. nil-nil from the
		// service is "the query ran and this episode does not exist" — a 404, not
		// an error banner.
		http.Error(w, "episode not found", http.StatusNotFound)
		return
	}

	// The child read is allowed to fail ON ITS OWN and the page still renders.
	// The error is carried INTO the builder rather than handled here, because the
	// decision it drives — "unknown orders" instead of "no orders" — belongs
	// beside the count it replaces, where a test can reach it.
	children, truncated, childErr := h.engine.DemandEpisodeService().Orders(originID, childOrderLimit)

	detail := BuildEpisodeDetail(*origin, children, truncated, childErr, now, c)
	detail.ChildrenLimit = childOrderLimit

	data["Detail"] = detail
	h.render(w, r, "demand-episode.html", data)
}
