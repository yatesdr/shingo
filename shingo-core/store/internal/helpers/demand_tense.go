package helpers

// ── THE TENSE OF A DEMAND RECORD, SPELLED ONCE ──────────────────────────────
//
// One demand_origins row carries a demand from forecast to open to settled.
// The tense is not stored; it is derived from the two timestamps the row
// already has, so there is no third column that could disagree with them:
//
//	forecast   opened_at IS NULL     AND closed_at IS NULL
//	open       opened_at IS NOT NULL AND closed_at IS NULL
//	settled    closed_at IS NOT NULL  (with opened_at NULL: a forecast that
//	                                   settled without its demand opening)
//
// "Unsettled" is forecast or open: the slot the partial unique index
// idx_demand_origins_open_key (v59, WHERE closed_at IS NULL) allows one of per
// episode key. The index predicate and DemandUnsettledSQL are the same text,
// and the drift test reads both.
//
// Every query that decides a row's tense composes one of these; the Go twin is
// domain.DemandTenseOf. TestDemandTense_NoQueryDecidesTenseOutsideTheHelpers
// fails on a statement or a Go site that spells the predicate itself. Before
// these existed, eleven statements read "open" as "not closed", which stopped
// being true the moment a row could be a forecast.
//
// alias is the table alias the statement uses ("o", "d"), or "" for an
// unaliased demand_origins. Each fragment is parenthesised so it composes
// safely beside OR.

func demandCol(alias, col string) string {
	if alias == "" {
		return col
	}
	return alias + "." + col
}

// DemandForecastSQL — the record has not opened and has not settled.
func DemandForecastSQL(alias string) string {
	return "(" + demandCol(alias, "opened_at") + " IS NULL AND " + demandCol(alias, "closed_at") + " IS NULL)"
}

// DemandOpenSQL — the demand has opened and has not settled. This, not
// "closed_at IS NULL", is what every "which episode is open" reader asks.
func DemandOpenSQL(alias string) string {
	return "(" + demandCol(alias, "opened_at") + " IS NOT NULL AND " + demandCol(alias, "closed_at") + " IS NULL)"
}

// DemandUnsettledSQL — forecast or open: the one-record-per-key slot the
// partial unique index guards, and the rows a close may still settle.
func DemandUnsettledSQL(alias string) string {
	return "(" + demandCol(alias, "closed_at") + " IS NULL)"
}

// DemandSettledSQL — the record has settled, whether or not its demand opened.
func DemandSettledSQL(alias string) string {
	return "(" + demandCol(alias, "closed_at") + " IS NOT NULL)"
}
