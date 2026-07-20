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
	// FeedsNode is the consumption node this claim feeds — where the material is
	// GOING. It is the claim's own node (c.CoreNodeName). Rendered as secondary
	// "feeds X" text, distinct from where the material currently IS.
	FeedsNode string
	Payload   string
	Free      int // dispatch-sourceable now (same predicate the computation nets)
	Held      int // claimed / reserved / locked in the confirmed pool
	// FreeLocations is where the free bins physically are, most-first — the
	// answer to "Free 4, but 4 where?". Empty when Free is 0.
	FreeLocations []engineNodeCount
	HasTTE        bool
	TTESeconds    float64
	TTEDisplay    string // human-readable, e.g. "12m 30s"; empty when not at-risk
}

// engineNodeCount mirrors sourceability.NodeCount for the view layer (a free-bin
// count at one physical node).
type engineNodeCount struct {
	Node  string
	Count int
}

// SourcingStyleView is one style's chip + drill-in.
type SourcingStyleView struct {
	StyleID string
	Status  string
	Missing []string
	Reason  string
	Claims  []SourcingClaimView
}

// SourcingProcessView is one process's rail entry and detail pane.
type SourcingProcessView struct {
	ProcessID string
	Styles    []SourcingStyleView
	// Status is the process-level roll-up shown on the rail: the worst verdict
	// across its styles.
	Status string
	// RunningStyle is the style this process is currently running, mirrored
	// from Edge via the plant-claims feed. Empty when Edge has no active style
	// for the process, or when the mirror predates the active flag — the rail
	// then shows no style line rather than guessing.
	RunningStyle string
	// Ready and Blocked count the styles this process can and cannot change over
	// to. Unconfigured styles are in neither: they have no verdict.
	Ready   int
	Blocked int
}

// processChipStatus is the verdict the process chip shows. It is the RUNNING
// style's own status — whether the process can sustain what it is running now —
// which is the single most useful signal for a process and the one the owner
// asked the chip to carry.
//
// Falls back to the worst-across-styles roll-up when Core has no running style
// (an older Edge, or none set) or when the running style has no view because it
// was the dropped Default placeholder. In the Default case the fallback lands on
// not_configured, which is the honest state: the process is running a style with
// no claims.
func processChipStatus(pv *SourcingProcessView) string {
	if pv.RunningStyle != "" {
		for _, s := range pv.Styles {
			if s.StyleID == pv.RunningStyle {
				return s.Status
			}
		}
	}
	return rollUpStatus(pv.Styles)
}

// statusSeverity ranks a verdict for the rail's worst-first sort:
// blocked → at-risk → sourcing → no-data → not-set-up. "no-data" has no real
// status feeding it today (the verdict enum is green/yellow/red/not_configured),
// but it is ranked so the ordering is total if one is ever added.
func statusSeverity(status string) int {
	switch status {
	case string(sourceability.StatusRed):
		return 0
	case string(sourceability.StatusYellow):
		return 1
	case string(sourceability.StatusGreen):
		return 2
	case "no_data":
		return 3
	case string(sourceability.StatusNotConfigured):
		return 4
	default:
		return 5
	}
}

// rollUpStatus reduces a process's style verdicts to the one shown on the rail.
// Worst-first: a red anywhere means something is blocked, and that is what an
// operator scanning the rail needs to see. A process whose styles are all
// unconfigured reports not_configured rather than borrowing a health color it
// has not earned.
func rollUpStatus(styles []SourcingStyleView) string {
	var sawGreen, sawYellow bool
	for _, s := range styles {
		switch s.Status {
		case string(sourceability.StatusRed):
			return string(sourceability.StatusRed)
		case string(sourceability.StatusYellow):
			sawYellow = true
		case string(sourceability.StatusGreen):
			sawGreen = true
		}
	}
	switch {
	case sawYellow:
		return string(sourceability.StatusYellow)
	case sawGreen:
		return string(sourceability.StatusGreen)
	default:
		return string(sourceability.StatusNotConfigured)
	}
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
	// RunningStyleKnown reports whether Core has a running-style signal at all:
	// true once ANY process arrives with an active style on the plant-claims
	// feed. False means no Edge has published one yet (an Edge older than the
	// active flag, or a plant with no active style set anywhere), and the page
	// explains that rather than showing an empty column that reads as a bug.
	//
	// Per-process absence is a different thing and is carried by
	// SourcingProcessView.RunningStyle being empty.
	RunningStyleKnown bool
	// StateSummary is how many processes sit in each chip status, worst-first,
	// for the plant summary strip above the rail (e.g. "4 blocked · 2 sourcing ·
	// 7 not set up"). Ordered here rather than as a map so the template renders
	// a stable severity order instead of Go's alphabetical map-key sort. States
	// with a zero count are omitted.
	StateSummary []StateCount
	// NotConfiguredCount is how many processes collapse into the not-set-up tail
	// of the rail, so the collapsed row can be labelled with a count.
	NotConfiguredCount int
}

// StateCount is one chip status and how many processes are in it.
type StateCount struct {
	Status string
	Count  int
}

// SourceabilityPage assembles the sourcing page's read model. Pure reads: the
// gated snapshot plus claim + pool context. It never recomputes a verdict.
func (e *Engine) SourceabilityPage() (SourceabilityPageView, error) {
	view := SourceabilityPageView{
		YellowEnabled: e.cfg.Sourceability.EnableAtRisk,
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
	activeStyles, err := sourceability.ActiveStyles(e.db.DB)
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
				FeedsNode: c.CoreNodeName,
				Payload:   c.PayloadCode,
				Free:      pb.Free,
				Held:      pb.Held,
			}
			for _, nc := range pb.FreeByNode {
				cv.FreeLocations = append(cv.FreeLocations, engineNodeCount{Node: nc.Node, Count: nc.Count})
			}
			if s, ok := tteByNode[c.CoreNodeName]; ok {
				cv.HasTTE = true
				cv.TTESeconds = s
				cv.TTEDisplay = formatTTE(s)
			}
			sv.Claims = append(sv.Claims, cv)
		}

		// Drop the "Default" placeholder when it carries no claims. "Default" is
		// a real styles row on Edge (from the plant spec / RoboShop import — no
		// shingo code creates one; verified across shingo-core and shingo-edge),
		// but on this plant every "Default" style has zero claims and is never a
		// genuine changeover target. A NON-Default style with no claims still
		// surfaces as "not set up" so the operator knows to configure it;
		// Default-with-no-claims is plant-spec noise, so it is dropped.
		if st.StyleID == "Default" && len(sv.Claims) == 0 {
			continue
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
		pv.RunningStyle = activeStyles[pv.ProcessID]
		if pv.RunningStyle != "" {
			view.RunningStyleKnown = true
		}
		pv.Status = processChipStatus(pv)
		for _, s := range pv.Styles {
			switch s.Status {
			case string(sourceability.StatusRed):
				pv.Blocked++
			case string(sourceability.StatusGreen), string(sourceability.StatusYellow):
				pv.Ready++
			}
		}
		view.Processes = append(view.Processes, *pv)
	}

	// Rail order is by severity, worst first: an operator scanning the rail
	// should hit the blocked processes before the healthy ones. Ties break by
	// name for a stable order. The template collapses the not-set-up tail into
	// one expandable row, but they still sort last here.
	sort.Slice(view.Processes, func(i, j int) bool {
		si, sj := statusSeverity(view.Processes[i].Status), statusSeverity(view.Processes[j].Status)
		if si != sj {
			return si < sj
		}
		return view.Processes[i].ProcessID < view.Processes[j].ProcessID
	})

	// Plant summary: how many processes sit in each state, worst-first for the
	// strip above the rail. Also the collapsed-tail count.
	counts := map[string]int{}
	for _, pv := range view.Processes {
		counts[pv.Status]++
	}
	view.NotConfiguredCount = counts[string(sourceability.StatusNotConfigured)]
	for _, status := range []string{
		string(sourceability.StatusRed),
		string(sourceability.StatusYellow),
		string(sourceability.StatusGreen),
		"no_data",
		string(sourceability.StatusNotConfigured),
	} {
		if n := counts[status]; n > 0 {
			view.StateSummary = append(view.StateSummary, StateCount{Status: status, Count: n})
		}
	}

	// Replenishment queue: lowest time-to-empty fills first.
	sort.Slice(queue, func(i, j int) bool { return queue[i].TTESeconds < queue[j].TTESeconds })
	view.Queue = queue

	return view, nil
}
