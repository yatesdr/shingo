package messaging

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// plant_claims_reconcile.go — the publisher as the engine's owned map drives
// it (engine owned.go, ClaimsSource). The engine caches one digest per
// process, compares it with what Core quotes on the heartbeat ack, and keeps
// the resend guard; this side does the reading and the sending.

// ClaimsMembers reads this Edge's spec as the claims family: every process
// with at least one style, name to the digest of its report. A process whose
// own read fails is in unread (the engine neither re-publishes nor empties
// it: an unread spec is not evidence that a process is gone). err is the
// process list itself failing, and then nothing is known.
//
// Reads: the process list and two per process (reportFor), the same reads a
// full snapshot makes, on the engine's boot build and its floor only.
func (p *PlantClaimsPublisher) ClaimsMembers() (members map[string]string, unread map[string]bool, err error) {
	procs, err := processes.List(p.db.DB)
	if err != nil {
		return nil, nil, fmt.Errorf("list processes: %w", err)
	}
	members, unread = map[string]string{}, map[string]bool{}
	for _, proc := range procs {
		report, err := p.reportFor(proc)
		if err != nil {
			log.Printf("plant_claims: read %s: %v — left as it is", proc.Name, err)
			unread[proc.Name] = true
			continue
		}
		if len(report.Styles) > 0 {
			members[proc.Name] = report.Digest
		}
	}
	return members, unread, nil
}

// SendClaims reads one process's report and publishes it when due(digest)
// says so. A name this Edge has no process for, or one with no styles, is
// sent as an empty report, built directly (a deleted or renamed process has no
// row to read), so Core drops its mirror. digest and styled describe the
// report read, sent or not.
//
// Reads: the process list and, for a process found, its two (reportFor).
func (p *PlantClaimsPublisher) SendClaims(name string, due func(digest string) bool) (digest string, styled bool, err error) {
	procs, err := processes.List(p.db.DB)
	if err != nil {
		return "", false, fmt.Errorf("list processes: %w", err)
	}
	var report protocol.PlantClaimsReport
	for _, proc := range procs {
		if proc.Name != name {
			continue
		}
		if report, err = p.reportFor(proc); err != nil {
			return "", false, fmt.Errorf("build %s: %w", name, err)
		}
		break
	}
	if len(report.Styles) == 0 {
		if report, err = withDigest(protocol.PlantClaimsReport{ProcessID: name}); err != nil {
			return "", false, fmt.Errorf("empty report for %s: %w", name, err)
		}
	}
	styled = len(report.Styles) > 0
	if due(report.Digest) {
		if err := p.publishOne(report); err != nil {
			return report.Digest, styled, err
		}
	}
	return report.Digest, styled, nil
}
