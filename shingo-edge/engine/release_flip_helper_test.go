package engine

import (
	"shingoedge/release"
	"shingoedge/store/processes"
)

// releaseFlipPartner performs the trunk's sequential flip for a node on its
// own — what a release on the feeding side does before its paperwork: the
// loader's flip facts, the plan's G5 verdict, the commit's flip.
func (e *Engine) releaseFlipPartner(node *processes.Node) error {
	f, partner := e.loadFlip(node)
	p := release.PlanFlip(f)
	e.emitLogs(p.Logs)
	if p.Verdict == release.Refuse {
		return p.Refusal
	}
	if !p.Flip {
		return nil
	}
	ll := &legLoad{node: node, partner: partner}
	ll.snap.Flip = f
	return e.commitFlip(ll)
}
