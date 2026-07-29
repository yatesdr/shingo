package www

import (
	"net/http"
	"time"
)

// handlers_material_flags.go — Stage 5.11.
//
// RENAMED FROM "the material-downtime flag" TO "Material flags", and the rename
// is the row's whole correction rather than a tidy-up. ShinGo cannot see
// downtime: it records that a place asked for material and when the asking
// stopped, and nothing anywhere records whether a line was stopped, whether
// anybody was waiting, or what it cost. A page called "material downtime" makes
// a claim its own data cannot support — and the old name is what sent every deep
// negative ledger to the maintenance owner when a negative ledger is a CYCLE
// COUNT.
//
// TWO SECTIONS ON ONE PAGE, AND THAT IS THE DESIGN. The style guide's rule
// against two grains under one heading is honoured by giving each half its own
// heading, its own selector and its own stated owner; what would break 5.11 is
// SPLITTING them, because the entire point of the row is that these two readings
// look alike on a bad day and go to different people. A reader who sees only one
// of them is the reader the old wording misled.
//
// SEPARATE PAGE FROM /demand-episodes for the reason that page's own header
// gives: its row is an episode, and half of this page's rows are carriers. The
// browser answers "what did this demand cost in orders"; this answers "what
// should someone go and look at now".

// materialFlagLimit caps the episode read.
//
// NOT A DISPLAY CONSTANT in the provenance sense — it is a query cap, the same
// class as episodeLimit and stated on the page in words when it bites. The
// constants that carry provenance are the ones a reader could mistake for
// knowledge about the floor.
const materialFlagLimit = 200

func (h *Handlers) handleMaterialFlags(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	c := h.engine.AppConfig().DisplayConstants()

	data := map[string]any{
		"Page":              "material-flags",
		"WorryAfter":        FormatDuration(c.WorryAfter),
		"ConcernAfter":      FormatDuration(c.ConcernAfter),
		"StaleBindingAfter": FormatDuration(c.StaleBindingAfter),
		"OverpackBinloads":  FormatRatio(c.OverpackBinloads),
	}

	// THE DISPLAY CONSTANTS ARE VALIDATED HERE, AND BEFORE THIS THEY WERE
	// VALIDATED NOWHERE. config.DisplayConfig.Validate had no production caller at
	// all — only two tests — so a plant that inverted worry_after and
	// concern_after got no error from anything: the bands would be unreachable in
	// an order no rendering code checks for and the surface would simply be wrong
	// and quiet. Reported on the page rather than refused at startup on purpose: a
	// display constant must not be able to stop a plant's core from booting, and a
	// display problem belongs on the display. The page still renders with the
	// resolved values, and says they are inconsistent.
	if err := c.Validate(); err != nil {
		data["ConfigError"] = err.Error()
	}

	// ── The episode half ─────────────────────────────────────────────────────
	//
	// since = now, deliberately. ListDemandEpisodes' scope is "open, plus
	// anything that closed inside the window", so passing the present instant
	// returns every open episode and essentially no closed ones. A CLOSED EPISODE
	// IS NOT A FLAG — it is history, and history is what /demand-episodes is for.
	episodes, truncated, err := h.engine.DemandEpisodeService().List(now, materialFlagLimit)
	if err != nil {
		// SHOWN, NOT SWALLOWED. An empty flag list is the most reassuring thing
		// this page can say and, on the day the read breaks, the least true. Same
		// family as rendering absence as zero, and worse here: this page exists to
		// be believed when it is quiet.
		data["FlagError"] = err.Error()
	} else {
		rows := make([]EpisodeRow, 0, len(episodes))
		for _, e := range episodes {
			rows = append(rows, BuildEpisodeRow(e, now, c))
		}
		flags, summary := SelectMaterialFlags(rows)
		data["Flags"] = flags
		data["FlagSummary"] = summary
		data["FlagsTruncated"] = truncated
		data["FlagLimit"] = materialFlagLimit
	}

	// ── The ledger half ──────────────────────────────────────────────────────
	//
	// A SECOND READ, AND A FAILURE HERE IS NOT A FAILURE OF THE PAGE. The two
	// halves are independent by design; one being unreadable must not make the
	// other look absent. What is not acceptable is rendering an empty candidate
	// table as though the carriers had been read.
	carriers, err := h.engine.CarrierBindings()
	if err != nil {
		data["BindingError"] = err.Error()
	} else {
		rows, summary := BuildBindingRows(carriers, now, c)
		data["Bindings"] = rows
		data["BindingSummary"] = summary
	}

	h.render(w, r, "material-flags.html", data)
}
