package service

import (
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/release"
	"shingoedge/store/orders"
)

// release_render.go — the board's half of the release layer (SHAPE §3.8):
// built from columns on the order rows the view build already holds, with no
// read, steps decode, Core call or tag read of its own. The act at the click
// decides; this only shows which decisions the node owes.

// releasePurposes is one entry per purpose present at the node, in the order a
// cell owes them (ready, tooling done, swap): a live leg standing at, or
// heading to, a station wait of that purpose. Ready when some leg is parked at
// it; a leg at a lane wait, still driving, or not yet moving makes the purpose
// present but not ready (SHAPE 3.8: disabled). A row with no stored release
// facts (created before Edge v8) says nothing here, and the tile keeps its
// other arms.
func releasePurposes(legs []orders.Order) []domain.ReleasePurpose {
	type state struct{ present, ready bool }
	byPurpose := map[release.Purpose]*state{}
	for i := range legs {
		o := &legs[i]
		if protocol.IsTerminal(o.Status) || o.ReleaseFacts == "" {
			continue
		}
		facts, err := release.DecodeFacts(o.ReleaseFacts)
		if err != nil || len(facts.Purposes) == 0 {
			continue
		}
		intent, _ := release.DecodeIntent(o.ReleaseIntent)
		pt := release.PointOf(o.Status, o.StationWait, o.WaitKind, intent, facts.Purposes)
		if !pt.HasWait {
			continue
		}
		st := byPurpose[pt.Purpose]
		if st == nil {
			st = &state{}
			byPurpose[pt.Purpose] = st
		}
		st.present = true
		if pt.AtWait && !pt.AtLane {
			st.ready = true
		}
	}
	var out []domain.ReleasePurpose
	for _, p := range release.PurposeOrder() {
		if st := byPurpose[p]; st != nil {
			out = append(out, domain.ReleasePurpose{Purpose: string(p), Ready: st.ready})
		}
	}
	return out
}

// releaseHeld is the chip: the hold or rejection sentence of every live leg at
// the node (L9), not only the runtime's active order's.
func releaseHeld(legs []orders.Order) string {
	var out []string
	seen := map[string]bool{}
	for i := range legs {
		s := legs[i].ReleaseHeld
		if s == "" || protocol.IsTerminal(legs[i].Status) || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}
