package service

import (
	"strconv"

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
}

func NewCMSPostingService(db *store.DB) *CMSPostingService {
	return &CMSPostingService{db: db}
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

// Health assembles the feed's state. enabled/muted come from the caller
// because they are process state, not database state.
func (s *CMSPostingService) Health(enabled, muted bool, mutedReason string) (*FeedHealth, error) {
	out := &FeedHealth{Enabled: enabled, Muted: muted, MutedReason: mutedReason}
	if !enabled {
		out.Why = "no cms: block is configured — this site does not post to the middleware"
		out.Health = &cms.Health{}
		return out, nil
	}

	h, err := cms.PostingHealth(s.db.DB)
	if err != nil {
		return nil, err
	}
	out.Health = h

	if err := s.db.QueryRow(`SELECT count(*) FROM node_properties WHERE key = $1`,
		material.CMSStoreroomProperty).Scan(&out.ConfiguredStorerooms); err != nil {
		return nil, err
	}

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
// backlog should say "muted", not "backlog".
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
	case h.Failed > 0:
		return false, countOf(h.Failed, "posting") + " ran out of attempts and is waiting for a person"
	case h.Rejected > 0:
		return false, countOf(h.Rejected, "posting") + " was refused by the middleware and will not be retried"
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
		return true, "last successful post " + h.LastPostedAt.Format("2006-01-02 15:04:05") +
			", " + countOf(h.PostedLastHour, "posting") + " in the last hour, " +
			countOf(h.ConfiguredStorerooms, "tagged boundary node")
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
