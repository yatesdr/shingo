package plantspec

import (
	"fmt"
	"strings"

	"shingo/protocol"
)

// Validate checks the spec for the mistakes that silently break the demo:
// dangling node references, swap claims missing staging, no LANE/NGRP storage
// hierarchy, payloads with an unknown bin type. It returns ALL problems found
// (joined) rather than the first, so a spec author fixes them in one pass.
func (p *Plant) Validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if strings.TrimSpace(p.Namespace) == "" {
		add("namespace is required")
	}
	if strings.TrimSpace(p.LineID) == "" {
		add("line_id is required")
	}

	// --- bin types + payloads ---
	binTypes := toSet(p.BinTypes)
	if len(binTypes) == 0 {
		add("at least one bin_type is required")
	}
	payloads := make(map[string]bool, len(p.Payloads))
	for _, pl := range p.Payloads {
		if pl.Code == "" {
			add("payload with empty code")
			continue
		}
		if payloads[pl.Code] {
			add("duplicate payload code %q", pl.Code)
		}
		payloads[pl.Code] = true
		if pl.BinType == "" || !binTypes[pl.BinType] {
			add("payload %q references unknown bin_type %q", pl.Code, pl.BinType)
		}
		if pl.UOPCapacity <= 0 {
			add("payload %q has non-positive uop_capacity %d", pl.Code, pl.UOPCapacity)
		}
	}

	// --- node-name universe: zones, lanes, slots, stations ---
	nodes := make(map[string]string) // name → kind (for duplicate detection + messages)
	addNode := func(name, kind string) {
		if name == "" {
			add("%s with empty name", kind)
			return
		}
		if prev, ok := nodes[name]; ok {
			add("duplicate node name %q (%s and %s)", name, prev, kind)
		}
		nodes[name] = kind
	}

	laneCount, slotCount := 0, 0
	for _, z := range p.Zones {
		addNode(z.Name, "zone")
		for _, ln := range z.Lanes {
			addNode(ln.Name, "lane")
			laneCount++
			for _, s := range ln.Slots {
				addNode(s.Name, "slot")
				slotCount++
				if s.Depth <= 0 {
					add("slot %q in lane %q has non-positive depth %d", s.Name, ln.Name, s.Depth)
				}
			}
		}
	}
	for _, st := range p.Stations {
		addNode(st.Name, "station")
	}
	if laneCount == 0 || slotCount == 0 {
		add("storage hierarchy missing: need at least one zone → lane → slot (kanban only sees nodes under a LANE/NGRP parent)")
	}
	ref := func(name string) bool { _, ok := nodes[name]; return ok }

	// loaderIdentities are SYNTHETIC loader-aggregate ids named by window_of claims
	// that are not themselves declared nodes — a multi-window loader with no physical
	// anchor node (the clean shape: its windows are its only nodes). Its demand-registry
	// entry keys on this label, so a demand may reference a loader identity even though
	// it is not a physical node. A window_of that DOES name a declared node is the
	// legacy anchor shape and is already a valid ref.
	loaderIdentities := map[string]bool{}
	for _, c := range p.Claims {
		if c.WindowOf != "" && !ref(c.WindowOf) {
			loaderIdentities[c.WindowOf] = true
		}
		// HomeOf names a dedicated_positions loader identity the same way WindowOf
		// names a shared_window one — a demand may key on it even when it is not a
		// declared physical node (the synthetic clean shape).
		if c.HomeOf != "" && !ref(c.HomeOf) {
			loaderIdentities[c.HomeOf] = true
		}
	}

	// --- edge processes / styles / operator stations ---
	processes := make(map[string]bool)
	for _, pr := range p.Processes {
		if pr.Name == "" {
			add("process with empty name")
			continue
		}
		processes[pr.Name] = true
	}
	styles := make(map[string]bool)
	styleProcess := make(map[string]string)
	for _, s := range p.Styles {
		if s.Name == "" {
			add("style with empty name")
			continue
		}
		styles[s.Name] = true
		styleProcess[s.Name] = s.Process
		if !processes[s.Process] {
			add("style %q references unknown process %q", s.Name, s.Process)
		}
		if !payloads[s.Payload] {
			add("style %q references unknown payload %q", s.Name, s.Payload)
		}
	}
	opStations := toSet(p.OperatorStations)

	// Each process's ActiveStyle must be one of its own styles — without it
	// findActiveClaim returns nil and the node's ticks are dropped (no
	// reorder/relief, no orders). Empty = the process never runs (warned).
	for _, pr := range p.Processes {
		if pr.ActiveStyle == "" {
			add("process %q has no active_style — its nodes will never tick", pr.Name)
			continue
		}
		if !styles[pr.ActiveStyle] {
			add("process %q active_style %q is not a defined style", pr.Name, pr.ActiveStyle)
		} else if styleProcess[pr.ActiveStyle] != pr.Name {
			add("process %q active_style %q belongs to process %q", pr.Name, pr.ActiveStyle, styleProcess[pr.ActiveStyle])
		}
	}

	// --- bins ---
	for _, b := range p.Bins {
		if b.Name == "" {
			add("bin with empty name")
		}
		if !ref(b.Slot) {
			add("bin %q sits at unknown node %q", b.Name, b.Slot)
		}
		if b.Payload != "" && !payloads[b.Payload] {
			add("bin %q has unknown payload %q", b.Name, b.Payload)
		}
		if b.BinType != "" && !binTypes[b.BinType] {
			add("bin %q has unknown bin_type %q", b.Name, b.BinType)
		}
	}

	// --- claims (the critical topology) ---
	for i, c := range p.Claims {
		where := fmt.Sprintf("claim[%d] %s/%s", i, c.CoreNode, c.Style)
		if !ref(c.CoreNode) {
			add("%s: unknown core_node %q", where, c.CoreNode)
		}
		if c.Style != "" && !styles[c.Style] {
			add("%s: unknown style %q", where, c.Style)
		}
		if c.Role != "produce" && c.Role != "consume" {
			add("%s: role must be produce|consume, got %q", where, c.Role)
		}
		if c.Payload != "" && !payloads[c.Payload] {
			add("%s: unknown payload %q", where, c.Payload)
		}
		for _, ap := range c.AllowedPayloads {
			if !payloads[ap] {
				add("%s: unknown allowed payload %q", where, ap)
			}
		}
		if c.InboundSource != "" && !ref(c.InboundSource) {
			add("%s: unknown inbound_source %q", where, c.InboundSource)
		}
		if c.OutboundDestination != "" && !ref(c.OutboundDestination) {
			add("%s: unknown outbound_destination %q", where, c.OutboundDestination)
		}
		if c.BufferDest != "" && !ref(c.BufferDest) {
			add("%s: unknown buffer_dest %q", where, c.BufferDest)
		}
		if c.PairedCoreNode != "" && !ref(c.PairedCoreNode) {
			add("%s: unknown paired_core_node %q", where, c.PairedCoreNode)
		}
		// Per-mode swap field requirements — mirror BuildSwapDispatch's runtime
		// checks (shingo-edge/engine/swap_dispatch.go) so a spec that validates
		// won't strand at dispatch. single_robot needs BOTH staging nodes (and
		// they must be distinct — same node collides the new + old bins);
		// two_robot needs inbound staging; two_robot_press_index needs
		// paired_core_node + outbound_destination (no staging — the live press
		// uses this); sequential (A/B) needs neither. (Previously this required
		// both staging nodes for ALL multi-step modes, which wrongly rejected
		// press_index / two_robot / sequential.)
		switch protocol.SwapMode(c.SwapMode) {
		case protocol.SwapModeSingleRobot:
			if c.InboundStaging == "" || c.OutboundStaging == "" {
				add("%s: single_robot requires inbound_staging and outbound_staging (distinct nodes)", where)
			} else if c.InboundStaging == c.OutboundStaging {
				add("%s: single_robot inbound_staging and outbound_staging must be DISTINCT nodes (got %q for both — new + old bins collide)", where, c.InboundStaging)
			}
		case protocol.SwapModeTwoRobot:
			if c.InboundStaging == "" {
				add("%s: two_robot requires inbound_staging", where)
			}
		case protocol.SwapModeTwoRobotPressIndex:
			if c.PairedCoreNode == "" {
				add("%s: two_robot_press_index requires paired_core_node", where)
			}
			if c.OutboundDestination == "" {
				add("%s: two_robot_press_index requires outbound_destination", where)
			}
		}
		// Any staging node that IS set must exist.
		if c.InboundStaging != "" && !ref(c.InboundStaging) {
			add("%s: unknown inbound_staging %q", where, c.InboundStaging)
		}
		if c.OutboundStaging != "" && !ref(c.OutboundStaging) {
			add("%s: unknown outbound_staging %q", where, c.OutboundStaging)
		}
		// manual_swap needs an outbound destination (claims.go enforces this).
		if protocol.SwapMode(c.SwapMode) == protocol.SwapModeManualSwap && c.OutboundDestination == "" {
			add("%s: manual_swap requires outbound_destination", where)
		}
	}

	// --- demands / reporting points / cell configs / lineside buckets ---
	for _, d := range p.Demands {
		if !payloads[d.Payload] {
			add("demand references unknown payload %q", d.Payload)
		}
		if !ref(d.Node) && !loaderIdentities[d.Node] {
			add("demand for %q references unknown node %q", d.Payload, d.Node)
		}
	}
	for _, rp := range p.ReportingPoints {
		if rp.PLCName == "" || rp.TagName == "" {
			add("reporting point at %q needs plc_name and tag_name", rp.Node)
		}
		if !ref(rp.Node) {
			add("reporting point %s/%s references unknown node %q", rp.PLCName, rp.TagName, rp.Node)
		}
		if rp.Style != "" && !styles[rp.Style] {
			add("reporting point %s/%s references unknown style %q", rp.PLCName, rp.TagName, rp.Style)
		}
	}
	for _, cc := range p.CellConfigs {
		if !processes[cc.Process] {
			add("cell_config references unknown process %q", cc.Process)
		}
		if len(opStations) > 0 && !opStations[cc.Station] {
			add("cell_config references unknown operator station %q", cc.Station)
		}
	}
	for _, lb := range p.LinesideBuckets {
		if !ref(lb.Node) {
			add("lineside bucket references unknown node %q", lb.Node)
		}
		if !payloads[lb.Payload] {
			add("lineside bucket at %q references unknown payload %q", lb.Node, lb.Payload)
		}
	}

	// --- payload parity (G6): every payload must have both a producer and a consumer ---
	// Producers are produce claims with an outbound_destination (sends material to a
	// market). Consumers are consume claims with an inbound_source (pulls material from
	// a market). manual_swap nodes are included — they're legitimate producers (loaders)
	// and consumers (unloaders) in the material flow.
	type payloadFlow struct{ produce, consume int }
	flowByPayload := make(map[string]*payloadFlow)
	for _, c := range p.Claims {
		fl := flowByPayload[c.Payload]
		if fl == nil {
			fl = &payloadFlow{}
			flowByPayload[c.Payload] = fl
		}
		if c.Role == "produce" && c.OutboundDestination != "" {
			fl.produce++
		}
		if c.Role == "consume" && c.InboundSource != "" {
			fl.consume++
		}
	}
	for code, fl := range flowByPayload {
		if fl.produce > 0 && fl.consume == 0 {
			add("payload %q has %d producer(s) but no consumer — market will fill and jam", code, fl.produce)
		}
		if fl.consume > 0 && fl.produce == 0 {
			add("payload %q has %d consumer(s) but no producer — consumers will starve", code, fl.consume)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("plantspec invalid (%d problem(s)):\n  - %s", len(errs), strings.Join(errs, "\n  - "))
	}
	return nil
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		if x != "" {
			m[x] = true
		}
	}
	return m
}
