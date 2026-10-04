package plantspec

import (
	"fmt"
	"slices"

	"shingo/protocol"
)

// keepStagedFindings is the twin of the Edge's dedicated-spot check
// (shingo-edge store/processes.CheckKeepStagedSpots), over the spec's claims.
//
// WHY A TWIN. seeddev writes claims with a raw INSERT, because it cannot
// import the Edge module, so a seeded plant never meets the Edge's check. The
// rule is restated here and run before anything is seeded:
//
//   - keep_staged_node is for the four swap modes, the ones that fetch a
//     fresh carrier for a swap;
//   - a claim's spot is its keep_staged_node, and no other claim in the same
//     style, or in a style of another process, may name that node in any
//     routing column; another style of the SAME process may name it as
//     staging (inbound or outbound) or as its own spot only;
//   - the claim's own routes may not name its spot, except its
//     inbound_staging (the same node as both is allowed).
//
// The Edge's other clause, refusing to move a spot while orders deliver to it,
// has no counterpart: a spec has no orders. Loader claims are left out because
// seeddev does not write them to the Edge's claim table, so the Edge's check
// never sees them either. The spec has no containment_destination; the Edge
// checks that column too.
func (p *Plant) keepStagedFindings() []string {
	type row struct {
		where, style, process string
		spot                  string
		cols                  [][2]string // (column, node)
	}
	processOf := make(map[string]string, len(p.Styles))
	for _, s := range p.Styles {
		processOf[s.Name] = s.Process
	}
	var out []string
	var rows []row
	for i, c := range p.Claims {
		if c.IsLoader() {
			continue
		}
		r := row{
			where: fmt.Sprintf("claim[%d] %s/%s", i, c.CoreNode, c.Style),
			style: c.Style, process: processOf[c.Style], spot: c.KeepStagedNode,
		}
		if c.KeepStagedNode != "" && !slices.Contains(protocol.ConfigurableSwapModes(), protocol.SwapMode(c.SwapMode)) {
			out = append(out, fmt.Sprintf("%s: keep_staged_node applies to the swap modes only", r.where))
		}
		for _, cn := range [][2]string{
			{"core_node", c.CoreNode}, {"inbound_staging", c.InboundStaging}, {"outbound_staging", c.OutboundStaging},
			{"paired_core_node", c.PairedCoreNode}, {"second_paired_core_node", c.SecondPairedCoreNode},
			{"inbound_source", c.InboundSource}, {"outbound_destination", c.OutboundDestination},
			{"changeover_evac_destination", c.ChangeoverEvacDestination}, {"keep_staged_node", c.KeepStagedNode},
		} {
			if cn[1] != "" {
				r.cols = append(r.cols, cn)
			}
		}
		for _, n := range c.ChangeoverEvacNodes {
			r.cols = append(r.cols, [2]string{"changeover_evac_nodes", n})
		}
		rows = append(rows, r)
	}
	for i, k := range rows {
		spot := k.spot
		if spot == "" {
			continue
		}
		for _, cn := range k.cols {
			if cn[0] != "inbound_staging" && cn[0] != "keep_staged_node" && cn[1] == spot {
				out = append(out, fmt.Sprintf("%s: keeps its spare at %s and also names it as %s", k.where, spot, cn[0]))
			}
		}
		for j, c := range rows {
			if i == j {
				continue
			}
			sameProcessOtherStyle := c.process == k.process && c.style != k.style
			for _, cn := range c.cols {
				if cn[1] != spot {
					continue
				}
				if sameProcessOtherStyle && (cn[0] == "inbound_staging" || cn[0] == "outbound_staging" || cn[0] == "keep_staged_node") {
					continue
				}
				if cn[0] == "keep_staged_node" && j < i {
					continue // two kept spots on one node: reported once
				}
				out = append(out, fmt.Sprintf("%s: node %s is %s's kept spot (keep_staged_node) and this claim names it as %s",
					c.where, spot, k.where, cn[0]))
			}
		}
	}
	return out
}
