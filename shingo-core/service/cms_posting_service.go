package service

import (
	"strconv"
	"time"

	"shingocore/material"
	"shingocore/store"
	"shingocore/store/cms"
)

// CMSPostingService answers "is the CMS feed working?".
//
// SEPARATE FROM CMSTransactionService, and the split is the same one the two
// tables draw. A transaction is a local fact — a bin crossed a boundary,
// recorded whether or not anything downstream exists. A posting is a claim
// about a remote system. Reading them through one service invites a health
// answer built from transaction counts, and transaction counts are exactly
// what stays healthy-looking while nothing is being sent.
type CMSPostingService struct {
	db *store.DB
	// healthWindow bounds how far back a terminal posting still counts toward
	// the verdict. See cms.Health's RejectedRecent/FailedRecent.
	healthWindow time.Duration
}

func NewCMSPostingService(db *store.DB, healthWindow time.Duration) *CMSPostingService {
	return &CMSPostingService{db: db, healthWindow: healthWindow}
}

// ProcessState is the half of the feed's health that lives in this process
// rather than in the database.
//
// A STRUCT, NOT TWO POSITIONAL BOOLS. Health(enabled, muted, reason) invites a
// caller to hand them over the wrong way round, and the two mistakes are not
// symmetrical: a disabled feed reported as muted is noise, while a MUTED feed
// reported as disabled renders the grey "this site does not post to the
// middleware" pill on a site that does and is currently sending nothing. There
// is one caller today; the type costs four lines and removes the class.
type ProcessState struct {
	// Enabled is whether a cms: block is configured at all.
	Enabled bool
	// Muted is set when the poster halted on a credential fault.
	Muted       bool
	MutedReason string
}

// FeedHealth is what the diagnostics page renders.
//
// The Healthy verdict is computed here rather than in the page so that one
// definition serves every reader, and it is a POSITIVE test: recent evidence of
// a successful post, plus at least one tagged boundary. "No pending rows" is
// deliberately not part of it — an empty queue is equally the signature of a
// subsystem nothing is handing work to.
type FeedHealth struct {
	// Enabled is whether a cms: block is configured at all. Everything below
	// is meaningless when it is false, and a page that did not say so would
	// render a disabled integration as a broken one.
	Enabled bool `json:"enabled"`
	// ConfiguredStorerooms is how many nodes carry cms_storeroom. Zero with
	// Enabled true is the normal state between configuring the endpoint and
	// tagging the plant — and it is also what a plant looks like if someone
	// deleted the properties. Either way nothing will ever be posted.
	ConfiguredStorerooms int `json:"configured_storerooms"`
	// Muted is set when the poster halted on a credential fault. A muted feed
	// with a quiet queue looks exactly like a healthy idle one from the counts.
	Muted       bool   `json:"muted"`
	MutedReason string `json:"muted_reason,omitempty"`

	*cms.Health

	// BuildFailures counts movements whose CMS rows could not be built since
	// this process started. Supplied by the caller because it is process
	// state; it is the ONLY visible trace of that loss — a build failure
	// writes no transaction, so it writes no posting, and every count in this
	// struct would report a plant that simply did not move anything.
	BuildFailures int64 `json:"build_failures"`

	// Healthy and Why are the verdict and its sentence. Why is populated
	// whether or not Healthy is true, because "why do you think it is fine"
	// is as much a diagnostic question as the other one.
	Healthy bool   `json:"healthy"`
	Why     string `json:"why"`
}

// Health assembles the feed's state. ps comes from the caller because those
// fields are process state, not database state.
func (s *CMSPostingService) Health(ps ProcessState) (*FeedHealth, error) {
	out := &FeedHealth{Enabled: ps.Enabled, Muted: ps.Muted, MutedReason: ps.MutedReason}
	if !ps.Enabled {
		out.Why = "no cms: block is configured — this site does not post to the middleware"
		out.Health = &cms.Health{}
		return out, nil
	}

	h, err := s.db.CMSPostingHealth(s.healthWindow)
	if err != nil {
		return nil, err
	}
	out.Health = h

	n, err := material.CountCMSBoundaries(s.db.DB)
	if err != nil {
		return nil, err
	}
	out.ConfiguredStorerooms = n

	return out, nil
}

// Verdict computes Healthy/Why. Called by the Engine AFTER it supplies
// BuildFailures, because a dropped movement is a finding and the verdict
// cannot see it otherwise.
func (h *FeedHealth) Verdict() {
	if !h.Enabled {
		return
	}
	h.Healthy, h.Why = verdict(h)
}

// verdict names the first thing that is wrong, or says what the evidence for
// working is.
//
// ORDERED WORST FIRST, because a page that reports the mildest of several
// problems sends someone to fix the wrong one. A muted poster with a growing
// backlog should say "muted", not "backlog". The order below is the whole
// ranking and it is deliberate at every step; TestVerdict_Ranking pins it, so
// changing one of these cases means changing that test on purpose.
//
//	BuildFailures        a move that reached no table at all. Invisible
//	                     everywhere else, so nothing may rank above it.
//	Muted                the whole feed is sending nothing.
//	No storerooms        the whole feed CAN never send anything.
//	UnresolvableInflight a POST whose fate is unknown. Inventory in an
//	                     ambiguous state; only a person can settle it.
//	Unposted             recorded and never queued — an ongoing leak, still
//	                     happening, unlike the parked rows below it.
//	Failed               out of attempts. Definitely not booked; a person
//	                     requeues.
//	Rejected             refused. Definitely not booked; the body needs a fix.
//	Age findings         a queue that is not draining, or a reconciler that is
//	                     not settling.
//	Never posted         configured but unproven. Not a failure.
//
// Failed and Rejected are read through their WINDOWED counts. Everything above
// them is a live condition that clears itself when the cause is fixed; those
// two are permanent marks on a row, and ranked on the lifetime totals they
// would hold the verdict red forever. See cms.Health.RejectedRecent.
func verdict(h *FeedHealth) (bool, string) {
	switch {
	case h.BuildFailures > 0:
		// FIRST, because it is the only failure that leaves no row anywhere.
		// Every other case below is visible in a table; this one is visible
		// only here, so a page that ranked it below a backlog would bury the
		// one loss nothing else can show.
		return false, countOf(int(h.BuildFailures), "movement") +
			" could not be turned into CMS transactions since this core started — " +
			"those moves are lost to the ledger and appear nowhere else"
	case h.Muted:
		return false, "the poster is muted and is sending nothing: " + h.MutedReason
	case h.ConfiguredStorerooms == 0:
		return false, "no node carries a cms_storeroom property, so no movement can ever " +
			"produce a posting — the boundary nodes have not been tagged"
	case h.UnresolvableInflight > 0:
		return false, countOf(h.UnresolvableInflight, "posting") +
			" went out and was never acknowledged with a transaction id. Nothing can resolve " +
			"that automatically; someone has to check the middleware for it"
	case h.Unposted > 0:
		return false, countOf(h.Unposted, "transaction") +
			" has been recorded but never queued for posting — the subscriber is failing to enqueue"
	case h.FailedRecent > 0:
		return false, countOf(h.FailedRecent, "posting") + " ran out of attempts and is waiting for a person"
	case h.RejectedRecent > 0:
		return false, countOf(h.RejectedRecent, "posting") + " was refused by the middleware and will not be retried"
	case h.OldestPendingAgeSeconds > agedPendingSeconds:
		return false, "the oldest pending posting has been waiting " +
			forSeconds(h.OldestPendingAgeSeconds) + " — the queue is not draining"
	case h.OldestInflightAgeSeconds > agedInflightSeconds:
		return false, "the oldest inflight posting has been unresolved for " +
			forSeconds(h.OldestInflightAgeSeconds) + " — the reconciler is not settling it"
	case h.LastPostedAt == nil:
		// Not a failure. Nothing has been posted YET, which is what a
		// freshly-configured plant looks like, and saying "healthy" here would
		// be a claim about a pipe nothing has been through.
		return false, "nothing has been posted yet — the feed is configured but unproven"
	default:
		why := "last successful post " + h.LastPostedAt.Format("2006-01-02 15:04:05") +
			", " + countOf(h.PostedLastHour, "posting") + " in the last hour, " +
			countOf(h.ConfiguredStorerooms, "tagged boundary node")
		// Older terminal rows do not hold the verdict red, but they are not
		// allowed to become invisible either — a windowed finding that nothing
		// mentions again is a finding that was deleted rather than aged out.
		if parked := h.Failed + h.Rejected; parked > 0 {
			why += ". " + countOf(parked, "older posting") +
				" is parked failed or rejected from before the health window and still needs a person"
		}
		return true, why
	}
}

// The thresholds past which a queue is a finding rather than a queue. Generous
// on purpose: the poll interval is 30s and the settle window 5m by default, so
// these are "something is wrong" rather than "something is slow".
const (
	agedPendingSeconds  = 15 * 60
	agedInflightSeconds = 60 * 60
)

// countOf renders "1 posting" / "3 postings". The package's own plural() takes
// two ready-made words and does not carry the number, which is the half a
// diagnostic sentence needs most.
func countOf(n int, noun string) string {
	return strconv.Itoa(n) + " " + noun + plural(n, "", "s")
}

// forSeconds renders an age at the coarsest unit that still says something. A
// backlog measured in seconds and one measured in hours are different findings
// and should not read the same.
func forSeconds(seconds int64) string {
	switch {
	case seconds < 120:
		return strconv.FormatInt(seconds, 10) + "s"
	case seconds < 7200:
		return strconv.FormatInt(seconds/60, 10) + "m"
	default:
		return strconv.FormatInt(seconds/3600, 10) + "h"
	}
}
