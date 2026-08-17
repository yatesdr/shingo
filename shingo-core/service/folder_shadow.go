package service

import (
	"log"
	"sort"
	"sync"
)

// ── THE SHADOW WINDOW FOR "DOES THIS ORDER MOVE A BIN OF ITS OWN?" ────────
//
// Seven sites ask that question by testing `order.BinID == nil`. That predicate
// is TRUE OF A COORDINATOR AND TRUE OF A DEFECT, and cannot tell them apart:
//
//   - a compound PARENT is a folder. It owns legs, never touches a bin, and its
//     bin_id is NULL permanently and correctly.
//   - a single-bin order whose planMove never persisted a bin_id reaches
//     FINISHED with its bin still at source. That is a real fault.
//
// The right spelling is the child rows — helpers.OwnsNoCargoSQL, the tree's own
// ruling at store/reconciliation, which is the ONE site that already had it.
// What it cost everywhere else, measured at the pin on the lane-stress rig
// 2026-08-13: twelve bin-state anomalies, ten service digs and two buried
// retrieves, every one a compound parent whose legs had delivered correctly.
// Zero true positives, and the health strip read "Core degraded" all run.
//
// ── WHY A WINDOW AND NOT A CUTOVER ────────────────────────────────────────
//
// Three of the seven have UNVERIFIED REACHABILITY for a coordinator —
// fulfillment/scanner.go's held-bin dispatch (guarded upstream by the scanner's
// own coordinator skip and by reshuffling ∉ IsAcquiring),
// service/tag_verify_service.go, and engine/recovery_service.go. Reading alone
// could not establish that a coordinator never arrives at them, and cutting a
// predicate over on the strength of "I could not find a path" is how a guard
// silently changes population.
//
// So: record what the right spelling WOULD have said at each site, run one
// window, then delete the losing spelling with the count quoted. That is this
// codebase's own pattern — service/burial_shadow.go and the dispatch shadow
// cutover — and the deletion is a separate commit with evidence attached.
//
// ── WHAT THIS COUNTED FIRST TIME ROUND, AND WHY IT WAS THE WRONG NUMBER ───
//
// It counted where the two spellings DISAGREED, through
// `NoteFolderShadow(site, id, binIDIsNil, ownsNoCargo, err)` and the test
// `binIDIsNil != ownsNoCargo`. That reads like a symmetric comparison of two
// answers. It is not: every one of the seven call sites is INSIDE its own
// `BinID == nil` branch, so binIDIsNil was the constant `true` at all of them
// and the test reduced to `!ownsNoCargo`.
//
// OrderOwnsNoCargo returns TRUE for a coordinator. So the instrument counted —
// and shouted about — the firings that landed on an ORDINARY order, which is
// the site being RIGHT, and was silent on every coordinator, which is the
// false-positive population it was built to measure. Its own log line then
// diagnosed one of those as "this site IS reachable by a coordinator", the
// exact inverse of what it meant.
//
// The window would have ended with a number that justified the opposite of the
// cutover. The constant parameter is what hid it, so it is gone: the callers
// now pass only the answer that varies.
//
// ── IT CHANGES NO BEHAVIOUR, DELIBERATELY ─────────────────────────────────
//
// Every site keeps its existing `BinID == nil` branch and keeps taking it. This
// only records what the other spelling WOULD have said. An instrument that also
// changes the outcome cannot be used to measure whether changing it was
// warranted — the same rule the ungated-dig tripwire is built on.

// FolderShadowMarker is the per-event line's search string, named once so the
// emitter and any guard test share one definition.
const FolderShadowMarker = "folder-recognition disagreement at"

// folderShadowTally counts, per site, how each firing of `BinID == nil` would
// have been answered by the child-rows spelling.
//
// In-process and reset-on-restart, like the arrival-refusal and bin-state-drift
// tallies: a window reading, not a fact anything recovers from.
var folderShadowTally = struct {
	mu          sync.Mutex
	coordinator map[string]int
	ordinary    map[string]int
	seen        map[string]int
}{coordinator: map[string]int{}, ordinary: map[string]int{}, seen: map[string]int{}}

// FolderShadowTally returns, by site, the firings that landed on a COORDINATOR
// — the false positives, and the population the cutover removes.
//
// NOT expected to be zero, and a non-zero count is the finding: that site is
// reachable by a coordinator and has been treating a permanently-and-correctly
// NULL bin_id as a fault. Twelve of these read "Core degraded" for a whole rig
// run.
func FolderShadowTally() map[string]int {
	folderShadowTally.mu.Lock()
	defer folderShadowTally.mu.Unlock()
	out := make(map[string]int, len(folderShadowTally.coordinator))
	for k, v := range folderShadowTally.coordinator {
		out[k] = v
	}
	return out
}

// FolderShadowOrdinary returns, by site, the firings that landed on an ORDINARY
// order — where the site was RIGHT and a NULL bin_id is a real fault.
//
// Reported beside the other count because the cutover has to KEEP them. A
// predicate swap that removed the false positives and these together would be
// trading a noisy guard for no guard, and the two numbers side by side are the
// only way to see the difference before it ships.
func FolderShadowOrdinary() map[string]int {
	folderShadowTally.mu.Lock()
	defer folderShadowTally.mu.Unlock()
	out := make(map[string]int, len(folderShadowTally.ordinary))
	for k, v := range folderShadowTally.ordinary {
		out[k] = v
	}
	return out
}

// FolderShadowSampled returns how many times each site was ASKED, agreement or
// not.
//
// Reported beside the disagreements because a zero disagreement count means
// nothing without it: a site that was never reached produces the same zero as a
// site that always agreed, and those are opposite conclusions. This is the
// check-knows-whether-it-had-input rule applied to the window itself.
func FolderShadowSampled() map[string]int {
	folderShadowTally.mu.Lock()
	defer folderShadowTally.mu.Unlock()
	out := make(map[string]int, len(folderShadowTally.seen))
	for k, v := range folderShadowTally.seen {
		out[k] = v
	}
	return out
}

// ResetFolderShadow exists for tests, which must not inherit a count.
func ResetFolderShadow() {
	folderShadowTally.mu.Lock()
	defer folderShadowTally.mu.Unlock()
	folderShadowTally.coordinator = map[string]int{}
	folderShadowTally.ordinary = map[string]int{}
	folderShadowTally.seen = map[string]int{}
}

// FolderShadowSites are the seven, named once so the emitter, the tally and any
// reader share one list.
const (
	FolderSiteDeliverySettle   = "wiring_completion delivery settle"
	FolderSiteCompletionNet    = "wiring_completion completion safety net"
	FolderSiteHeldBinDispatch  = "fulfillment held-bin dispatch"
	FolderSiteTagVerify        = "tag verify"
	FolderSiteRecoveryReapply  = "recovery reapply-completion"
	FolderSiteBuriedForHeldBin = "lane gate buried-for-held-bin"
	FolderSiteBlockCompleted   = "block-completed single-bin fallback"
)

// NoteFolderShadow records one firing of a site's `BinID == nil` branch under
// the RIGHT spelling.
//
// isCoordinator is OrderOwnsNoCargo's answer: TRUE means the order owns legs,
// so its NULL bin_id is permanent and correct and this firing is a FALSE
// POSITIVE. FALSE means an ordinary order, and the firing is a true one.
//
// THERE IS NO binIDIsNil PARAMETER, and its absence is the fix. Every caller is
// inside its own nil branch and passed the constant `true`, which made the
// comparison it fed look symmetric and inverted what the tally meant. A
// parameter that can only take one value is a parameter that hides which way
// round the answer is.
//
// Called for its side effect and returns nothing, because nothing may branch on
// it.
//
// A read failure is NOT counted either way — it is counted as a sample that
// could not be taken, and logged. Folding "I could not ask" into "the site was
// right" is exactly the failure this batch's other instruments were built to
// stop.
func NoteFolderShadow(site string, orderID int64, isCoordinator bool, readErr error) {
	folderShadowTally.mu.Lock()
	folderShadowTally.seen[site]++
	folderShadowTally.mu.Unlock()

	if readErr != nil {
		log.Printf("WARN: folder-recognition shadow could not read child rows for order %d at %s: %v "+
			"— NOT counted either way; the window is one sample short at this site",
			orderID, site, readErr)
		return
	}
	if !isCoordinator {
		folderShadowTally.mu.Lock()
		folderShadowTally.ordinary[site]++
		folderShadowTally.mu.Unlock()
		return // the site fired on an ordinary order and was right; nothing to report
	}

	folderShadowTally.mu.Lock()
	folderShadowTally.coordinator[site]++
	n := folderShadowTally.coordinator[site]
	folderShadowTally.mu.Unlock()

	log.Printf("WARN: "+FolderShadowMarker+" %s — order %d owns legs, so its NULL bin_id is permanent "+
		"and correct, and this site treated it as a fault (%d at this site). bin_id IS NULL is true of "+
		"a coordinator and true of a broken single-bin order and cannot tell them apart; the child rows "+
		"can. This firing is a FALSE POSITIVE and is what the cutover removes.",
		site, orderID, n)
}

// FolderShadowReport renders the window for a human, sites sorted, sampled
// counts beside disagreements.
//
// It exists so the cutover commit can quote a number rather than a claim: the
// losing spelling is deleted WITH this output, per the round's own condition.
func FolderShadowReport() string {
	coord := FolderShadowTally()
	ord := FolderShadowOrdinary()
	seen := FolderShadowSampled()
	sites := sampledSites(seen)

	out := "folder-recognition shadow window:\n"
	if len(sites) == 0 {
		return out + "  NO SITE WAS SAMPLED. This window says nothing — it is not evidence that the " +
			"sites are clean, it is evidence that the run did not exercise these paths.\n"
	}
	for _, s := range sites {
		out += "  " + s + ": " + itoa(coord[s]) + " coordinator (false positive), " +
			itoa(ord[s]) + " ordinary (true positive), " + itoa(seen[s]) + " sampled\n"
	}
	return out
}

// FolderShadowLine is FolderShadowReport on ONE line, for the reconciliation
// sweep. The multi-line form is what the cutover commit quotes; this is what
// the journal carries while the window runs. Per site: coordinator/ordinary/sampled.
func FolderShadowLine() string {
	coord := FolderShadowTally()
	ord := FolderShadowOrdinary()
	seen := FolderShadowSampled()
	sites := sampledSites(seen)
	if len(sites) == 0 {
		return "NO SITE SAMPLED YET"
	}
	out := ""
	for i, s := range sites {
		if i > 0 {
			out += "; "
		}
		out += s + " " + itoa(coord[s]) + "/" + itoa(ord[s]) + "/" + itoa(seen[s])
	}
	return out
}

// sampledSites returns the sites that have been asked, sorted. Keyed on the
// SAMPLED map rather than either count, so a site that was reached and never
// fired still appears — "never reached" and "always fine" are opposite
// conclusions and must not render the same.
func sampledSites(seen map[string]int) []string {
	sites := make([]string, 0, len(seen))
	for s := range seen {
		sites = append(sites, s)
	}
	sort.Strings(sites)
	return sites
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
