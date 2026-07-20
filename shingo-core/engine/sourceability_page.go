package engine

import (
	"fmt"
	"sort"
	"time"

	"shingocore/store/plantclaims"
	"shingocore/store/sourceability"
)

// The Core sourcing page's read model. Verdicts come from the SAME gated monitor
// snapshot the wire publishes — never a re-derivation, never ungated data. The
// claim detail and the free-vs-held pool split are added context (pure reads);
// they never change a style's status. Time-to-empty and the replenishment queue
// are populated only from the snapshot's at-risk lines, which are empty unless
// the owner has enabled the yellow tier — so the page's gate is the same gate as
// the wire and the HMI.

// SourcingClaimView is one claim's row in a style's drill-in.
type SourcingClaimView struct {
	Node       string
	Payload    string
	Free       int // dispatch-sourceable now (same predicate the computation nets)
	Held       int // claimed / reserved / locked in the confirmed pool
	HasTTE     bool
	TTESeconds float64
	TTEDisplay string // human-readable, e.g. "12m 30s"; empty when not at-risk
}

// SourcingStyleView is one style's chip + drill-in.
type SourcingStyleView struct {
	StyleID string
	Status  string
	Missing []string
	Reason  string
	Claims  []SourcingClaimView
}

// SourcingProcessView is one process's row of style chips.
type SourcingProcessView struct {
	ProcessID string
	Styles    []SourcingStyleView
}

// SourcingQueueRow is one at-risk line in the replenishment queue.
type SourcingQueueRow struct {
	ProcessID  string
	StyleID    string
	Node       string
	Payload    string
	TTESeconds float64
	TTEDisplay string
}

// formatTTE renders a time-to-empty for the page. Empty for a non-positive
// value (no projection).
func formatTTE(sec float64) string {
	if sec <= 0 {
		return ""
	}
	d := time.Duration(sec) * time.Second
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(sec))
	}
	return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(sec)%60)
}

// SourceabilityPageView is everything the Core sourcing page renders.
type SourceabilityPageView struct {
	Processes []SourcingProcessView
	// Queue is the at-risk lines ordered by time-to-empty ascending (fill first).
	// Empty when the at-risk tier is disabled.
	Queue []SourcingQueueRow
	// YellowEnabled tells the page whether the at-risk tier is on, so it can
	// explain an empty queue rather than imply nothing is ever at risk.
	YellowEnabled bool
	// RunningStyleKnown is false: Core has no authoritative running-style signal
	// (the plant.claims feed carries no active flag). The page marks no style
	// running rather than guess.
	RunningStyleKnown bool
}

// SourceabilityPage assembles the sourcing page's read model. Pure reads: the
// gated snapshot plus claim + pool context. It never recomputes a verdict.
func (e *Engine) SourceabilityPage() (SourceabilityPageView, error) {
	view := SourceabilityPageView{
		YellowEnabled:     e.cfg.Sourceability.EnableAtRisk,
		RunningStyleKnown: false,
	}
	if e.sourceabilityMonitor == nil {
		return view, nil
	}

	snapshot := e.sourceabilityMonitor.Snapshot()
	claims, err := sourceability.LoadClaims(e.db.DB)
	if err != nil {
		return view, err
	}
	pool, err := sourceability.PoolBreakdownByPayload(e.db.DB)
	if err != nil {
		return view, err
	}

	byProcess := map[string]*SourcingProcessView{}
	var queue []SourcingQueueRow

	for _, st := range snapshot {
		tteByNode := make(map[string]float64, len(st.AtRisk))
		for _, r := range st.AtRisk {
			tteByNode[r.NodeName] = r.TimeToEmpty.Seconds()
			queue = append(queue, SourcingQueueRow{
				ProcessID:  st.ProcessID,
				StyleID:    st.StyleID,
				Node:       r.NodeName,
				Payload:    r.PayloadCode,
				TTESeconds: r.TimeToEmpty.Seconds(),
				TTEDisplay: formatTTE(r.TimeToEmpty.Seconds()),
			})
		}

		sv := SourcingStyleView{
			StyleID: st.StyleID,
			Status:  string(st.Status),
			Missing: st.Missing,
			Reason:  reasonFor(st), // the same sentence the wire carries
		}
		for _, c := range claims[plantclaims.ProcessKey{ProcessID: st.ProcessID, StyleID: st.StyleID}] {
			pb := pool[c.PayloadCode]
			cv := SourcingClaimView{
				Node:    c.CoreNodeName,
				Payload: c.PayloadCode,
				Free:    pb.Free,
				Held:    pb.Held,
			}
			if s, ok := tteByNode[c.CoreNodeName]; ok {
				cv.HasTTE = true
				cv.TTESeconds = s
				cv.TTEDisplay = formatTTE(s)
			}
			sv.Claims = append(sv.Claims, cv)
		}

		pv := byProcess[st.ProcessID]
		if pv == nil {
			pv = &SourcingProcessView{ProcessID: st.ProcessID}
			byProcess[st.ProcessID] = pv
		}
		pv.Styles = append(pv.Styles, sv)
	}

	for _, pv := range byProcess {
		sort.Slice(pv.Styles, func(i, j int) bool { return pv.Styles[i].StyleID < pv.Styles[j].StyleID })
		view.Processes = append(view.Processes, *pv)
	}
	sort.Slice(view.Processes, func(i, j int) bool { return view.Processes[i].ProcessID < view.Processes[j].ProcessID })

	// Replenishment queue: lowest time-to-empty fills first.
	sort.Slice(queue, func(i, j int) bool { return queue[i].TTESeconds < queue[j].TTESeconds })
	view.Queue = queue

	return view, nil
}
