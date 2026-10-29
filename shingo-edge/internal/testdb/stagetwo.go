package testdb

import "shingo/protocol"

// StageTwoPair is a two-stage unloader as Core sends it on the node list:
// stage 1, whose CLEAR leaves the cart bare and whose outbound is the pair
// group, and stage 2, whose windows are the pair group's members. It is the
// one construction every stage-2 window pin builds its stage 2 from (engine,
// and through the engine the station view), so the role those pins run under
// is set here and nowhere else.
//
// Stage 2 is shared_window, operator-replenished, with no payloads unless the
// test names some (SPR's has none). Its keys sort ahead of stage 1's, as a
// stage 2 Core created first does ("loader:14" before "loader:15"): the Edge
// walks its loaders in loader_key order.
type StageTwoPair struct {
	// Group is the pair group: stage 2's name and stage 1's outbound.
	Group string
	// Stage1Window is stage 1's one window; Stage1Payloads its parts.
	Stage1Window   string
	Stage1Payloads []string
	// Windows are stage 2's windows, the pair group's members.
	Windows []string
	// PulledDirectly, Outbound, Inbound and Payloads are stage 2's: mode (a)
	// is pulled directly with no outbound; (b) direct with an outbound; (c)
	// pull mode, an inbound wait group.
	PulledDirectly bool
	Outbound       string
	Inbound        string
	Payloads       []string
}

// Stage2Key and Stage1Key are the halves' loader keys.
func (p StageTwoPair) Stage2Key() string { return "loader:" + p.Group + "-a" }
func (p StageTwoPair) Stage1Key() string { return "loader:" + p.Group + "-b" }

// Infos is the pair as the node list carries it: stage 2, then stage 1.
func (p StageTwoPair) Infos() []protocol.LoaderInfo {
	return []protocol.LoaderInfo{p.Stage2(), p.Stage1()}
}

// Stage2 is the stage-2 loader as Core makes it since 2026-10-29: produce,
// and TakesBare. Until then it was consume with no TakesBare; the pins that
// build from here ran green on that shape before the Edge keyed on TakesBare.
func (p StageTwoPair) Stage2() protocol.LoaderInfo {
	info := protocol.LoaderInfo{
		Name: p.Group, LoaderKey: p.Stage2Key(), Role: string(protocol.ClaimRoleProduce), TakesBare: true,
		Layout: protocol.LoaderLayoutSharedWindow, Replenishment: protocol.LoaderReplenishmentOperator,
		InboundSource: p.Inbound, OutboundDest: p.Outbound, PulledDirectly: p.PulledDirectly,
	}
	for _, w := range p.Windows {
		info.Positions = append(info.Positions, protocol.LoaderPosition{CoreNodeName: w})
	}
	for _, c := range p.Payloads {
		info.Payloads = append(info.Payloads, protocol.LoaderPayloadInfo{PayloadCode: c})
	}
	return info
}

// Stage1 is the stage-1 loader: consume, leaves the cart bare, fed directly
// (no inbound), its outbound the pair group.
func (p StageTwoPair) Stage1() protocol.LoaderInfo {
	info := protocol.LoaderInfo{
		Name: p.Group + "-S1", LoaderKey: p.Stage1Key(), Role: string(protocol.ClaimRoleConsume),
		Layout: protocol.LoaderLayoutSharedWindow, Replenishment: protocol.LoaderReplenishmentOperator,
		OutboundDest: p.Group, LeavesBare: true,
		Positions: []protocol.LoaderPosition{{CoreNodeName: p.Stage1Window}},
	}
	for _, c := range p.Stage1Payloads {
		info.Payloads = append(info.Payloads, protocol.LoaderPayloadInfo{PayloadCode: c})
	}
	return info
}

// CoreNodes is the pair's places as Core's node list names them: each stage-2
// window as a member of the pair group ("Group.Window"), stage 1's window, and
// the members of any group named in extra (group name to member names).
func (p StageTwoPair) CoreNodes(extra map[string][]string) []protocol.NodeInfo {
	out := []protocol.NodeInfo{{Name: p.Group}, {Name: p.Stage1Window}}
	for _, w := range p.Windows {
		out = append(out, protocol.NodeInfo{Name: p.Group + "." + w})
	}
	for g, members := range extra {
		out = append(out, protocol.NodeInfo{Name: g})
		for _, m := range members {
			out = append(out, protocol.NodeInfo{Name: g + "." + m})
		}
	}
	return out
}
