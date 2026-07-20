// Package sourceability computes, for every configured (process, style), whether
// the plant can source it right now — GREEN (every claim satisfiable), RED (at
// least one payload has no available bin, with the missing list), or YELLOW (all
// satisfiable but a needed line projects empty within the horizon).
//
// The whole package is a pure READ. It counts what is available and what is
// held; it never acquires, reserves, or moves anything. "Available" is exactly
// what dispatch could source today (the FindSourceFIFO predicate): a bin that is
// unclaimed, unreserved (no pending reservation), manifest-confirmed, unlocked,
// healthy, on a real enabled node.
//
// Compute (this file) is pure — no database, no clock — so it is fixture-tested
// directly. The DB reads that build its Inputs live in read.go.
package sourceability

import (
	"sort"
	"time"

	"shingocore/store/plantclaims"
)

// Status is a style's sourceability verdict.
type Status string

const (
	// StatusGreen: every claim in the style can be sourced from the pool now.
	StatusGreen Status = "green"
	// StatusYellow: sourceable now, but a needed line projects empty within the
	// horizon. Only reported when the at-risk tier is enabled (see Config).
	StatusYellow Status = "yellow"
	// StatusRed: at least one payload has no available bin — the style cannot be
	// changed over to until the missing payloads are replenished.
	StatusRed Status = "red"
)

// Config gates the at-risk (yellow) tier. The computation ALWAYS computes
// time-to-empty and the at-risk lines; YellowEnabled only controls whether a
// style is allowed to report YELLOW. It ships false so the plant sees green/red
// only until the owner validates the consumption-rate window on real audit data,
// then flips it on — no schema or recompute change, just the surfaced status.
type Config struct {
	// YellowEnabled lets a satisfiable-but-at-risk style report YELLOW instead of
	// GREEN. Default false (green/red only).
	YellowEnabled bool
	// Horizon is the "projects empty within" threshold: a line whose
	// time-to-empty is under Horizon is at risk.
	Horizon time.Duration
}

// LineTTE is one claim's line-level time-to-empty projection. Computed for every
// satisfiable line; used both for the yellow decision and (later) the
// replenishment queue ordered by TimeToEmpty ascending.
type LineTTE struct {
	NodeName     string
	PayloadCode  string
	UOPRemaining int
	// RatePerSec is the payload's consumption velocity (UOP/sec, positive),
	// derived from the negative bin_uop_delta history over the rate window.
	RatePerSec float64
	// TimeToEmpty = UOPRemaining / RatePerSec. Meaningful only when Known is
	// true; a line with nothing staged or no consumption history has no
	// projection.
	TimeToEmpty time.Duration
	Known       bool
}

// StyleState is the sourceability verdict for one (process, style).
type StyleState struct {
	ProcessID string
	StyleID   string
	Status    Status
	// Missing is the distinct set of payloads that no available bin could
	// satisfy (populated for RED). Sorted for a stable feed/display.
	Missing []string
	// AtRisk is every line projecting empty within the horizon. It is part of
	// the GATED output: populated only when the yellow tier is enabled, empty
	// otherwise — so a dark plant emits GREEN with no at-risk anywhere.
	AtRisk     []LineTTE
	ComputedAt time.Time
}

// Inputs is the plant snapshot the computation reads. All DB access happens in
// read.go and fills this struct; Compute consumes it purely.
type Inputs struct {
	// Styles is every configured (process, style) — including styles with no
	// claims (trivially GREEN) — so an all-styles recompute reports every one.
	Styles []plantclaims.ProcessKey
	// Claims are the sourceability claims grouped by (process, style).
	Claims map[plantclaims.ProcessKey][]plantclaims.ClaimRow
	// Pool is the count of AVAILABLE bins per payload code (the FindSourceFIFO
	// predicate — unclaimed, unreserved, manifest-confirmed, healthy).
	Pool map[string]int
	// LineUOP is the uop_remaining of the bin currently at a claim's node,
	// keyed by node name. Absent = nothing staged at that line.
	LineUOP map[string]int
	// RatePerSec is the consumption velocity per payload (UOP/sec, positive).
	RatePerSec map[string]float64
}

// Compute nets the available pool against every style's claims and returns a
// verdict per (process, style). Pure: no DB, no clock — now is passed in.
//
// Netting is greedy first-fit by claim sequence: each claim draws one available
// bin of its payload, or failing that one of its allowed alternatives. A claim
// that can draw nothing is unsatisfiable and its payload joins the missing set
// (→ RED). All satisfiable → GREEN, upgraded to YELLOW (only when
// cfg.YellowEnabled) if any line projects empty within cfg.Horizon.
//
// The greedy pass is deterministic (claims sorted by Seq) and models contention:
// two claims needing the same payload draw two bins, so a pool of one leaves the
// second unsatisfiable. It is not a maximum matching across allowed-sets — a
// richer allocator can replace drawOne later without changing the verdict shape.
func Compute(in Inputs, cfg Config, now time.Time) []StyleState {
	out := make([]StyleState, 0, len(in.Styles))
	for _, key := range in.Styles {
		claims := append([]plantclaims.ClaimRow(nil), in.Claims[key]...)
		sort.SliceStable(claims, func(i, j int) bool { return claims[i].Seq < claims[j].Seq })

		st := StyleState{ProcessID: key.ProcessID, StyleID: key.StyleID, ComputedAt: now}

		// Working copy of the pool, seeded only with the payloads this style
		// touches so the map stays small at plant scale.
		avail := make(map[string]int)
		seed := func(p string) {
			if p == "" {
				return
			}
			if _, ok := avail[p]; !ok {
				avail[p] = in.Pool[p]
			}
		}
		for _, c := range claims {
			seed(c.PayloadCode)
			for _, a := range c.AllowedPayloadCodes {
				seed(a)
			}
		}

		missing := map[string]struct{}{}
		for _, c := range claims {
			if drawOne(avail, c) {
				continue
			}
			need := c.PayloadCode
			if need == "" && len(c.AllowedPayloadCodes) > 0 {
				need = c.AllowedPayloadCodes[0]
			}
			if need != "" {
				missing[need] = struct{}{}
			}
		}

		if len(missing) > 0 {
			st.Status = StatusRed
			st.Missing = sortedKeys(missing)
			out = append(out, st)
			continue
		}

		// The at-risk tier is a GATED OUTPUT, not a per-reader choice. When the
		// yellow tier is disabled a style reports GREEN with no at-risk lines at
		// all — the identical gated result every reader (wire, Edge, Core page)
		// consumes — so no two surfaces disagree and nobody sees a yellow the
		// owner hasn't validated. Flip YellowEnabled on and every surface gains
		// yellow together. (The TTE query still runs; only its surfacing is gated.)
		if cfg.YellowEnabled {
			for _, c := range claims {
				line := lineTTE(c, in)
				if line.Known && line.TimeToEmpty < cfg.Horizon {
					st.AtRisk = append(st.AtRisk, line)
				}
			}
		}
		if len(st.AtRisk) > 0 {
			st.Status = StatusYellow
		} else {
			st.Status = StatusGreen
		}
		out = append(out, st)
	}
	return out
}

// drawOne consumes one available bin for the claim: its primary payload first,
// then each allowed alternative in order. Returns false when nothing is
// available (the claim is unsatisfiable).
func drawOne(avail map[string]int, c plantclaims.ClaimRow) bool {
	if c.PayloadCode != "" && avail[c.PayloadCode] > 0 {
		avail[c.PayloadCode]--
		return true
	}
	for _, a := range c.AllowedPayloadCodes {
		if a != "" && avail[a] > 0 {
			avail[a]--
			return true
		}
	}
	return false
}

// lineTTE projects one line's time-to-empty from the bin staged there and the
// payload's consumption rate. Known is false when nothing is staged, the rate is
// non-positive (no recent consumption), or the line is empty — none of which is
// "at risk", they are "no projection".
func lineTTE(c plantclaims.ClaimRow, in Inputs) LineTTE {
	uop, staged := in.LineUOP[c.CoreNodeName]
	rate := in.RatePerSec[c.PayloadCode]
	lt := LineTTE{NodeName: c.CoreNodeName, PayloadCode: c.PayloadCode, UOPRemaining: uop, RatePerSec: rate}
	if !staged || uop <= 0 || rate <= 0 {
		return lt
	}
	lt.TimeToEmpty = time.Duration(float64(uop) / rate * float64(time.Second))
	lt.Known = true
	return lt
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
