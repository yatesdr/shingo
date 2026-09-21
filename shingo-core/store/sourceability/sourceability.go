// Package sourceability computes, for every configured (process, style), whether
// the plant can source it right now — GREEN (every claim satisfiable), RED (at
// least one payload has no available bin, with the missing list), or YELLOW (all
// satisfiable but a needed line projects empty within the horizon).
//
// The whole package is a pure READ. It counts what is available and what is
// held; it never acquires, reserves, or moves anything. "Available" is exactly
// what dispatch could source today, because it is counted through the one
// sourcing predicate — helpers.BinSourceableSQL, the same one FindSourceFIFO
// asks. This package holds no spelling of it.
//
// Compute (this file) is pure — no database, no clock — so it is fixture-tested
// directly. The DB reads that build its Inputs live in read.go.
package sourceability

import (
	"sort"
	"time"

	"shingo/protocol"
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
	// StatusNotConfigured: the style has no sourceability claims, so there is
	// nothing to net the pool against and no verdict to give.
	//
	// This used to report GREEN. A style with zero claims trivially satisfied
	// every claim it had — none — so it fell through the missing check and came
	// out "can change over", which is the strongest claim the system makes,
	// derived from the complete absence of configuration. An unconfigured
	// process is not capable; it is unknown. It never reports green and is never
	// selectable in the changeover picker.
	StatusNotConfigured Status = "not_configured"
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
	// RatePerSec is the velocity the projection used (UOP/sec, positive) —
	// the node's own rate when RateGrain is "node", the plant-wide payload
	// rate when it is "payload" (the fallback for a node with no rows in the
	// window).
	RatePerSec float64
	// RateGrain names which grain produced RatePerSec: "node" (per-node rate
	// from RatePerNode) or "payload" (plant-wide fallback). Empty on a line
	// with no projection (Known=false), where no rate was consulted. B6's
	// sample records it so a scored forecast can tell a fresh per-node number
	// from a stale plant-wide one.
	RateGrain string
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
	//
	// A PAYLOAD WITH FLAGGED STOCK IS STILL MISSING. It is unsourceable — every
	// sourcing reader refuses a carrier the payload does not declare — so it
	// belongs here; UndeclaredCarriers below says WHY, and removing it from this
	// list would make the missing set incomplete for every reader of the wire.
	Missing []string

	// UndeclaredCarriers names, for each MISSING payload that has any, how many
	// bins are standing in a carrier that payload is not declared to travel in.
	// Sorted by payload, and EMPTY on the ordinary plant.
	//
	// IT IS SCOPED TO THE MISSING SET on purpose. A payload with flagged stock
	// that is nonetheless satisfiable has a shortage nobody is waiting on, and
	// naming it in a changeover verdict would put a maintenance job in front of
	// an operator who is trying to change over. The flagged carrier is still
	// listed on /material-flags and counted on /inventory, which are the
	// surfaces that belong to the person who fixes it.
	UndeclaredCarriers []PayloadCount
	// AtRisk is every line projecting empty within the horizon. It is part of
	// the GATED output: populated only when the yellow tier is enabled, empty
	// otherwise — so a dark plant emits GREEN with no at-risk anywhere.
	AtRisk     []LineTTE
	ComputedAt time.Time
}

// PayloadCount is a payload and a number of bins. Used for the undeclared-
// carrier finding, where the payload alone is not actionable — "resolve the bin
// type" reads very differently against one carrier and against thirty.
type PayloadCount struct {
	PayloadCode string
	Bins        int
}

// Inputs is the plant snapshot the computation reads. All DB access happens in
// read.go and fills this struct; Compute consumes it purely.
type Inputs struct {
	// Styles is every configured (process, style) — including styles with no
	// claims, which report NOT_CONFIGURED — so an all-styles recompute reports
	// every one.
	Styles []plantclaims.ProcessKey
	// Claims are the sourceability claims grouped by (process, style).
	Claims map[plantclaims.ProcessKey][]plantclaims.ClaimRow
	// Pool is the count of AVAILABLE bins per payload code, counted through
	// bins.BinSourceableSQL — the one sourcing predicate, so this figure and
	// what FindSourceFIFO would actually pick cannot disagree.
	Pool map[string]int
	// UndeclaredCarrier is the count of bins per payload standing in a carrier
	// that payload is not declared to travel in — stock that EXISTS, counts on
	// every inventory surface, and that no sourcing reader will ever fetch.
	// Disjoint from Pool by construction: the bin-type arm refuses exactly these
	// bins. A payload with no findings is ABSENT from the map rather than
	// present with a zero.
	UndeclaredCarrier map[string]int
	// OnLine is the count of bins ALREADY INSIDE a process carrying what it needs
	// but which dispatch cannot fetch (status='staged'), keyed process → payload.
	// Disjoint from Pool by construction. A claim draws from here FIRST — a bin
	// standing at the line is better than one a robot has to go and get, and
	// spending it leaves the fetchable pool for whoever actually needs a delivery.
	// See onLinePoolByProcess for why the scope is the process and not the node.
	OnLine map[string]map[string]int
	// LineUOP is the uop_remaining of the bin currently at a claim's node,
	// keyed by node name. Absent = nothing staged at that line.
	LineUOP map[string]int
	// RatePerSec is the consumption velocity per payload (UOP/sec, positive).
	RatePerSec map[string]float64
	// RatePerNode is the consumption velocity per (node name, payload)
	// (UOP/sec, positive), from the same scan as RatePerSec. A cell's line
	// TTE prefers this grain — its staged bin is one node's stock, burning at
	// that node's rate — and falls back to RatePerSec when the node has no
	// rows in the window (first cycle after a changeover). Absent key = no
	// per-node history, not a zero rate.
	RatePerNode map[nodePayload]float64
	// ActiveStyles is the style each process is currently RUNNING, keyed by
	// process ID — the same map ActiveStyles() returns. It scopes the kept TTE
	// samples and nothing else: no verdict reads it, because every configured
	// style still gets a verdict whether or not it is the one running.
	//
	// A process absent from the map contributes no samples. That is deliberate
	// and matches ActiveStyles' own rule — Core must not guess at a running
	// style — and it fails to the safe side: a missing sample is a visible blind
	// spot in the score, where a guessed one is a wrong number nothing flags.
	ActiveStyles map[string]string
}

// TTESample is one claim's time-to-empty projection at one pass, kept so the
// forecast can later be scored against the demand it predicted.
//
// IT IS TAKEN BEFORE THE VERDICT, for every claim of the running style,
// INCLUDING THE CLAIMS OF A RED ONE. Compute returns early for RED, so a sample
// set built from what Compute surfaced would hold only the lines that were
// already fine — precisely the lines whose forecast nobody needs to check. The
// line that ran dry is the one the score exists to grade.
//
// StyleStatus is stamped afterwards with the verdict that same pass reached, so
// a sample carries the state of the world it was taken in.
type TTESample struct {
	ProcessID    string
	StyleID      string
	Line         LineTTE
	StyleStatus  Status
	ReorderPoint int
	// Kind names which kind of demand episode this sample is meant to be scored
	// against, in demand_origins' own vocabulary. A cell sample is keyed on its
	// process; a threshold sample on its node. ScoreTTE's two arms filter on it
	// so a cell row that happens to share a node and payload with a monitored
	// binding cannot be scored as that binding's forecast — they are projections
	// about different stock, one line's staged bin against a payload's whole
	// in-loop total, and averaging them would read as one number about neither.
	Kind string
}

// The sample kinds, aliased to the episode vocabulary rather than respelled.
// A sample exists to be joined to a demand_origins row of the same kind, so the
// two must not be free to drift; binding them here makes a rename a compile
// error instead of a silently empty join.
const (
	SampleKindCell      = protocol.EpisodeKindCell
	SampleKindThreshold = protocol.EpisodeKindThreshold
)

// LoaderSample is the projection for one monitored loader binding: the payload's
// whole in-loop total against the rate the plant burns it at.
//
// PURE, so the arithmetic is fixture-tested without a database — the same split
// this package already keeps between Compute and read.go. The caller supplies
// the two reads (the binding list and the system total); nothing here touches a
// connection.
//
// IT IS A DIFFERENT PROJECTION FROM A CELL'S, deliberately. lineTTE asks "how
// long until the bin standing at this line is empty" and divides one node's
// stock by that node's rate. A loader binding has no staged bin and no node
// rate: what it is monitoring is the payload's plant-wide in-loop total against
// the threshold the loader replenishes at, so the total is the numerator and
// the plant-wide payload rate is the denominator. RateGrain is "payload"
// always, for that reason, and never "node".
//
// KNOWN IS FALSE ON THE SAME TWO CONDITIONS lineTTE uses — no stock, or no rate
// — and for the same reason: neither is "about to run dry", both are "no
// projection", and a zero would read as "empty right now", which is the
// opposite. One scorer fix later therefore covers a loader row and a cell row
// together, which is the point of giving them the same shape.
func LoaderSample(nodeName, payloadCode string, totalUOP int, ratePerSec float64, reorderPoint int) TTESample {
	lt := LineTTE{
		NodeName: nodeName, PayloadCode: payloadCode,
		UOPRemaining: totalUOP, RatePerSec: ratePerSec, RateGrain: "payload",
	}
	if totalUOP <= 0 || ratePerSec <= 0 {
		lt.RateGrain = "" // no projection: no rate was consulted, name no grain
	} else {
		lt.TimeToEmpty = time.Duration(float64(totalUOP) / ratePerSec * float64(time.Second))
		lt.Known = true
	}
	// ProcessID, StyleID and StyleStatus stay empty: a binding belongs to a
	// loader, not to a process running a style, and there is no verdict about
	// it to stamp. Writing a process here would make the cell arm's join find
	// a row that is not a cell's.
	return TTESample{Line: lt, ReorderPoint: reorderPoint, Kind: SampleKindThreshold}
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
	states, _ := ComputeWithSamples(in, cfg, now)
	return states
}

// ComputeWithSamples is Compute, plus the per-line time-to-empty projections it
// computes on the way to the verdict.
//
// COMPUTE DELEGATES HERE rather than the two sharing a copied loop. There is one
// netting pass in this package and one place a verdict is decided; a sibling
// with its own copy would be a second spelling of the rule, free to drift from
// the one every reader consumes, and the drift would show up as a score that
// grades a forecast the plant never actually made.
//
// The samples are a SECOND RETURN and not a field on StyleState: they are a
// different grain (one per claim, not per style), they are scoped to the running
// style where the verdict covers every configured one, and StyleState is on the
// wire — where nothing has asked for them and a new field would have to be
// gated, versioned and explained to every reader.
func ComputeWithSamples(in Inputs, cfg Config, now time.Time) ([]StyleState, []TTESample) {
	out := make([]StyleState, 0, len(in.Styles))
	var samples []TTESample
	for _, key := range in.Styles {
		claims := append([]plantclaims.ClaimRow(nil), in.Claims[key]...)
		sort.SliceStable(claims, func(i, j int) bool { return claims[i].Seq < claims[j].Seq })

		st := StyleState{ProcessID: key.ProcessID, StyleID: key.StyleID, ComputedAt: now}

		// No claims = nothing to net against. Report that, rather than letting
		// the style fall through every check below and emerge GREEN on the
		// strength of having satisfied zero requirements.
		if len(claims) == 0 {
			st.Status = StatusNotConfigured
			out = append(out, st)
			continue
		}

		// THE PROJECTION IS TAKEN via pendingSamples, above every exit from this
		// loop, so the RED return below cannot skip it. Only the running style is
		// sampled (see pendingSamples for why).
		pending := pendingSamples(key, claims, in)

		// keep stamps the verdict this pass reached onto the style's samples and
		// moves them to the output. Called at each verdict exit so the status a
		// sample carries is the one the same pass published.
		keep := func(status Status) {
			for i := range pending {
				pending[i].StyleStatus = status
				samples = append(samples, pending[i])
			}
		}

		// Working copy of the pool, seeded only with the payloads this style
		// touches so the map stays small at plant scale.
		avail := make(map[string]int)
		// Second working copy: what this PROCESS already has standing at its own
		// nodes. Read of a nil inner map is a zero, so a process with nothing
		// staged needs no special case.
		onLine := make(map[string]int)
		procOnLine := in.OnLine[key.ProcessID]
		seed := func(p string) {
			if p == "" {
				return
			}
			if _, ok := avail[p]; !ok {
				avail[p] = in.Pool[p]
			}
			if _, ok := onLine[p]; !ok {
				onLine[p] = procOnLine[p]
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
			// IN-PROCESS STOCK FIRST. A bin already standing inside the process
			// satisfies the claim without a robot move, and spending it here leaves
			// the fetchable pool for a claim that genuinely needs a delivery — so
			// preferring it is both truer and a strictly better allocation. Order
			// matters only for that; a claim satisfied either way is still GREEN.
			if drawOne(onLine, c) || drawOne(avail, c) {
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
			// THE SECOND FACT, CARRIED SEPARATELY. For each missing payload that
			// has stock standing in an undeclared carrier, say so — the operator's
			// action is "resolve the bin type", not "go and make more parts", and
			// those two must not arrive in one sentence.
			for _, p := range st.Missing {
				if n := in.UndeclaredCarrier[p]; n > 0 {
					st.UndeclaredCarriers = append(st.UndeclaredCarriers,
						PayloadCount{PayloadCode: p, Bins: n})
				}
			}
			keep(StatusRed)
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
		keep(st.Status)
		out = append(out, st)
	}
	return out, samples
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

// pendingSamples takes the per-line projection for one style's claims. Only
// the running style is sampled: in.Styles carries every configured style, and
// sampling all of them would multiply the rows by the styles-per-process
// without adding a line anyone is running dry on.
func pendingSamples(key plantclaims.ProcessKey, claims []plantclaims.ClaimRow, in Inputs) []TTESample {
	if key.StyleID == "" || in.ActiveStyles[key.ProcessID] != key.StyleID {
		return nil
	}
	pending := make([]TTESample, 0, len(claims))
	for _, c := range claims {
		pending = append(pending, TTESample{
			ProcessID:    key.ProcessID,
			StyleID:      key.StyleID,
			Line:         lineTTE(c, in),
			ReorderPoint: c.ReorderPoint,
			Kind:         SampleKindCell,
		})
	}
	return pending
}

// lineTTE projects one line's time-to-empty from the bin staged there and the
// rate at the grain that line actually burns: the NODE's rate when it has
// rows in the window (its staged bin is one node's stock), else the plant-wide
// payload rate (first cycle after a changeover; a node whose consumption has
// only ever run through its lineside bucket until Fix 2 lands). Known is
// false when nothing is staged, both rates are non-positive, or the line is
// empty — none of which is "at risk", they are "no projection" — and
// RateGrain is empty alongside it: no rate, no grain.
func lineTTE(c plantclaims.ClaimRow, in Inputs) LineTTE {
	uop, staged := in.LineUOP[c.CoreNodeName]
	nodeRate := in.RatePerNode[nodePayload{Node: c.CoreNodeName, Payload: c.PayloadCode}]
	payloadRate := in.RatePerSec[c.PayloadCode]
	rate, grain := payloadRate, "payload"
	if nodeRate > 0 {
		rate, grain = nodeRate, "node"
	}
	lt := LineTTE{NodeName: c.CoreNodeName, PayloadCode: c.PayloadCode, UOPRemaining: uop,
		RatePerSec: rate, RateGrain: grain}
	if !staged || uop <= 0 || rate <= 0 {
		lt.RateGrain = "" // no projection: no rate was consulted, name no grain
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
