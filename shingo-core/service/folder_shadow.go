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
// So: log where the two spellings DISAGREE, run one window, then delete the
// losing spelling with the count quoted. That is this codebase's own pattern —
// service/burial_shadow.go and the dispatch shadow cutover — and the deletion
// is a separate commit with evidence attached, not this one.
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

// folderShadowTally counts, per site, the times OwnsNoCargo disagreed with
// BinID == nil.
//
// In-process and reset-on-restart, like the arrival-refusal and bin-state-drift
// tallies: a window reading, not a fact anything recovers from.
var folderShadowTally = struct {
	mu     sync.Mutex
	bySite map[string]int
	seen   map[string]int
}{bySite: map[string]int{}, seen: map[string]int{}}

// FolderShadowTally returns disagreement counts so far, by site.
//
// NOT expected to be zero. A non-zero count at a site means that site is
// reachable by a coordinator and has been answering the wrong question — which
// is the finding, and the reason the cutover waits for this number.
func FolderShadowTally() map[string]int {
	folderShadowTally.mu.Lock()
	defer folderShadowTally.mu.Unlock()
	out := make(map[string]int, len(folderShadowTally.bySite))
	for k, v := range folderShadowTally.bySite {
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
	folderShadowTally.bySite = map[string]int{}
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

// NoteFolderShadow records one site's answer under both spellings.
//
// binIDIsNil is what the site tested. ownsNoCargo is what the child-rows
// spelling says. Called for its side effect and returns nothing, because
// nothing may branch on it.
//
// A read failure is NOT a disagreement and is not counted as one — it is
// counted as a sample that could not be taken, and logged. Folding "I could not
// ask" into "they agree" is exactly the failure this batch's other instruments
// were built to stop.
func NoteFolderShadow(site string, orderID int64, binIDIsNil, ownsNoCargo bool, readErr error) {
	folderShadowTally.mu.Lock()
	folderShadowTally.seen[site]++
	folderShadowTally.mu.Unlock()

	if readErr != nil {
		log.Printf("WARN: folder-recognition shadow could not read child rows for order %d at %s: %v "+
			"— NOT counted as agreement; the window is one sample short at this site",
			orderID, site, readErr)
		return
	}
	if binIDIsNil == ownsNoCargo {
		return
	}

	folderShadowTally.mu.Lock()
	folderShadowTally.bySite[site]++
	n := folderShadowTally.bySite[site]
	folderShadowTally.mu.Unlock()

	log.Printf("WARN: "+FolderShadowMarker+" %s — order %d: BinID==nil says %v, child rows say %v "+
		"(%d at this site). The two spellings of 'does this order move a bin of its own' disagree, "+
		"which means this site IS reachable by a coordinator and has been answering the wrong "+
		"question about it. bin_id is NULL for a folder permanently and correctly; it is NULL for a "+
		"broken single-bin order too, and the bin-id spelling cannot tell them apart.",
		site, orderID, binIDIsNil, ownsNoCargo, n)
}

// FolderShadowReport renders the window for a human, sites sorted, sampled
// counts beside disagreements.
//
// It exists so the cutover commit can quote a number rather than a claim: the
// losing spelling is deleted WITH this output, per the round's own condition.
func FolderShadowReport() string {
	dis := FolderShadowTally()
	seen := FolderShadowSampled()
	sites := make([]string, 0, len(seen))
	for s := range seen {
		sites = append(sites, s)
	}
	sort.Strings(sites)

	out := "folder-recognition shadow window:\n"
	if len(sites) == 0 {
		return out + "  NO SITE WAS SAMPLED. This window says nothing — it is not evidence of " +
			"agreement, it is evidence that the run did not exercise these paths.\n"
	}
	for _, s := range sites {
		out += "  " + s + ": " + itoa(dis[s]) + " disagreement(s) in " + itoa(seen[s]) + " sample(s)\n"
	}
	return out
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
