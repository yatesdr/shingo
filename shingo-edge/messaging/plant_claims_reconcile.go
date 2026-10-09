package messaging

import (
	"log"
	"sort"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// ClaimsGuard is the Edge's resend guard for its plant claims, which lives with
// the rest of the feed bookkeeping in the engine (engine/feeds.go). Seen here
// through an interface so the publisher needs no engine import.
type ClaimsGuard interface {
	// ClaimSendDue reports whether one process's report, at this digest,
	// should be sent now, and records the send: the same digest inside two
	// minutes is in flight and held back, and a third send in a row flags it —
	// unless Core's quote (quoted, "" for none) is the digest sent last, which
	// ends the streak.
	ClaimSendDue(process, digest, quoted string) bool
	// ClaimsSettled marks every process NOT in pending as converged — Core
	// quoted it back, or it needs nothing — ending its streak and any flag.
	ClaimsSettled(pending map[string]bool)
}

// ReconcileClaims answers the Claims map on a heartbeat ack from a Core that
// keeps one digest per process for this station. A process with at least one
// style whose digest Core does not hold is re-published; a name Core holds that
// this Edge no longer has, or has with no styles, gets an empty report so Core
// drops its mirror. Everything else is converged.
//
// Reads: the process list and two per process (reportFor), the same reads a
// full snapshot makes. A failed list read publishes nothing — an unread spec
// is not evidence that a process is gone. A process whose own read fails is
// left alone, neither re-published nor emptied.
func (p *PlantClaimsPublisher) ReconcileClaims(coreHeld map[string]string, guard ClaimsGuard) {
	procs, err := processes.List(p.db.DB)
	if err != nil {
		log.Printf("plant_claims: reconcile with Core: list processes: %v — nothing published", err)
		return
	}
	pending := map[string]bool{}
	reported := map[string]bool{} // has styles here, or could not be read
	for _, proc := range procs {
		report, err := p.reportFor(proc)
		if err != nil {
			log.Printf("plant_claims: reconcile with Core: build %s: %v — left as it is", proc.Name, err)
			reported[proc.Name], pending[proc.Name] = true, true
			continue
		}
		if len(report.Styles) == 0 {
			continue
		}
		reported[proc.Name] = true
		if held, ok := coreHeld[proc.Name]; ok && held == report.Digest {
			continue
		}
		pending[proc.Name] = true
		p.sendIfDue(report, guard, coreHeld[proc.Name])
	}
	p.emptyUnreported(coreHeld, reported, pending, guard)
	guard.ClaimsSettled(pending)
}

// emptyUnreported sends an empty report for every name Core holds that this
// Edge does not report with styles. It is built directly, not through
// reportFor: a deleted or renamed process has no row to read.
func (p *PlantClaimsPublisher) emptyUnreported(coreHeld map[string]string, reported, pending map[string]bool, guard ClaimsGuard) {
	names := make([]string, 0, len(coreHeld))
	for name := range coreHeld {
		if !reported[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		report, err := withDigest(protocol.PlantClaimsReport{ProcessID: name})
		if err != nil {
			log.Printf("plant_claims: reconcile with Core: empty report for %s: %v", name, err)
			continue
		}
		pending[name] = true
		p.sendIfDue(report, guard, coreHeld[name])
	}
}

// sendIfDue publishes one report unless the guard holds it back. quoted is
// the digest Core holds for the process.
func (p *PlantClaimsPublisher) sendIfDue(report protocol.PlantClaimsReport, guard ClaimsGuard, quoted string) {
	if !guard.ClaimSendDue(report.ProcessID, report.Digest, quoted) {
		return
	}
	if err := p.publishOne(report); err != nil {
		log.Printf("plant_claims: reconcile with Core: %v", err)
	}
}
