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

	// ── THE CENSUS AT BIRTH (§R.78) ───────────────────────────────────────
	//
	// A seed that ships pre-fragmented, or with no room to dig in, is a defect
	// in the spec and not a condition of the plant — see census.go for what the
	// rig cost before anything asked.
	//
	// A FROZEN BASELINE REPORTS INSTEAD OF REFUSING, and the findings are still
	// printed in full. A spec that a published measurement was taken on cannot be
	// corrected without invalidating the measurement, so the honest handling is to
	// say exactly what is wrong with it and let it run — not to fall silent, which
	// would let the defects be forgotten, and not to refuse, which would delete
	// the comparison the number exists for.
	if census := p.CensusAtBirth(); !census.Clean() {
		if p.BaselineFrozenAt != "" {
			for _, f := range census.Findings() {
				fmt.Printf("plantspec: KNOWN SEED DEFECT (frozen as a baseline by %s, not corrected on "+
					"purpose): %s\n", p.BaselineFrozenAt, f)
			}
		} else {
			errs = append(errs, census.Findings()...)
		}
	}

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
	zonePositions := make(map[string]int, len(p.Zones))
	zoneLanes := make(map[string]int, len(p.Zones))
	for _, z := range p.Zones {
		addNode(z.Name, "zone")
		zoneLanes[z.Name] = len(z.Lanes)
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
		// Flat positions: slots parented by the zone itself. They count as slots
		// for the hierarchy check below — they ARE storage under an NGRP — but not
		// as lanes, which is the distinction a maintained group turns on.
		for _, s := range z.Positions {
			addNode(s.Name, "position")
			slotCount++
			zonePositions[z.Name]++
			if s.Depth != 1 {
				add("position %q in zone %q has depth %d; a position hangs directly off the group, so nothing can be buried behind it and its depth is 1",
					s.Name, z.Name, s.Depth)
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
		case protocol.SwapModeSimple:
			// "simple" is retired as a configurable mode (the Edge upsert
			// allowlist rejects it too); it survives only as a runtime downgrade
			// descriptor. Reject at spec time so Core validation and Edge upsert
			// agree on what is configurable.
			add("%s: swap_mode \"simple\" is retired; use single_robot / two_robot / two_robot_press_index / sequential, or a Core-owned manual_swap loader", where)
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
			// The third position must exist and must be its own node. Both are
			// checked here rather than left to the runtime because a
			// second_paired_core_node that names nothing produces a step aimed
			// at a place the fleet cannot resolve, and one that repeats another
			// position produces a robot asked to move a bin to where it is.
			if c.SecondPairedCoreNode != "" {
				if !ref(c.SecondPairedCoreNode) {
					add("%s: unknown second_paired_core_node %q", where, c.SecondPairedCoreNode)
				}
				if c.SecondPairedCoreNode == c.CoreNode || c.SecondPairedCoreNode == c.PairedCoreNode {
					add("%s: second_paired_core_node %q must differ from the front and back positions", where, c.SecondPairedCoreNode)
				}
			}
		default:
			// The flip is press-index choreography; nothing else has two robots
			// to swap between, and a spec that sets it on another mode is
			// describing a cell that cannot exist. Marked evacuation positions are
			// the same argument: a position is a press position.
			if c.IndexRobotSupplies {
				add("%s: index_robot_supplies applies to two_robot_press_index only", where)
			}
			if len(c.ChangeoverEvacPositions) > 0 {
				add("%s: changeover_evac_positions applies to two_robot_press_index only", where)
			}
		}
		// ── MARKED POSITIONS MUST BE POSITIONS THIS PRESS HAS ───────────────────────
		//
		// A position the layout does not have is not an unlikely configuration, it
		// is a reference to nothing — and the evacuation it asks for silently
		// never happens, which is the failure mode hardest to notice on a sim.
		// Same rule the Edge's ValidateNodeClaim applies; stated here too
		// because a spec is written long before an Edge sees it.
		seenPosition := map[string]bool{}
		for _, position := range c.ChangeoverEvacPositions {
			switch position {
			case "front":
			case "paired":
				if c.PairedCoreNode == "" {
					add("%s: changeover_evac_positions marks the back position, but this press has no paired_core_node", where)
				}
			case "second":
				if c.SecondPairedCoreNode == "" {
					add("%s: changeover_evac_positions marks the third position, but this press has no second_paired_core_node", where)
				}
			default:
				add("%s: unknown changeover_evac_position %q (want front, paired or second)", where, position)
			}
			if seenPosition[position] {
				add("%s: changeover_evac_positions lists %q more than once", where, position)
			}
			seenPosition[position] = true
		}
		// An evacuation destination that names nothing sends the bins nowhere.
		if c.ChangeoverEvacDestination != "" && !ref(c.ChangeoverEvacDestination) {
			add("%s: unknown changeover_evac_destination %q", where, c.ChangeoverEvacDestination)
		}
		// ── KEY ROUTE ───────────────────────────────────────────────────────
		//
		// Shape only. The POINTS are deliberately not resolved against the
		// scenario's nodes: a key route names points in the vendor's MAP, which
		// is a superset of the nodes Shingo gave jobs to, and a corridor
		// waypoint is the feature's primary use. Refusing a spec because a
		// waypoint is not a node is the exact mistake the Edge validator was
		// just corrected for; the sim has no map to check against at all.
		seenPoint := map[string]bool{}
		for _, pt := range c.KeyRoute {
			switch {
			case strings.TrimSpace(pt) == "":
				add("%s: key_route contains a blank point", where)
			case pt == "SELF_POSITION":
				add("%s: SELF_POSITION is never valid in a key route", where)
			case seenPoint[pt]:
				add("%s: key_route lists %q more than once", where, pt)
			}
			seenPoint[pt] = true
		}
		if len(c.KeyRoute) > 0 && protocol.SwapMode(c.SwapMode) == protocol.SwapModeManualSwap {
			add("%s: key_route applies to robot-served claims; a manual_swap loader does not drive", where)
		}
		if c.KeyTask != "" && c.KeyTask != "load" && c.KeyTask != "unload" {
			add("%s: key_task must be \"load\", \"unload\", or empty; got %q", where, c.KeyTask)
		}
		// The directive is an instruction to a LOADER's card.
		if c.ChangeoverLoadDirective && protocol.SwapMode(c.SwapMode) != protocol.SwapModeManualSwap {
			add("%s: changeover_load_directive is a loader's card instruction (manual_swap)", where)
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
	builtStations := map[string]bool{}
	for _, cc := range p.CellConfigs {
		if !processes[cc.Process] {
			add("cell_config references unknown process %q", cc.Process)
		}
		if len(opStations) > 0 && !opStations[cc.Station] {
			add("cell_config references unknown operator station %q", cc.Station)
		}
		builtStations[cc.Station] = true
	}
	for _, c := range p.Claims {
		if c.OperatorStation != "" {
			builtStations[c.OperatorStation] = true
		}
	}
	// EVERY DECLARED STATION MUST BE BUILDABLE, and this is the reverse of the
	// check above rather than a restatement of it.
	//
	// seed_edge creates operator_stations from cell_configs and from claims that
	// pin their own window. A name that appears in `operator_stations` and in
	// neither of those is declared and never created: the node that would render
	// on it keeps operator_station_id NULL and every surface bound to that
	// station is unreachable, silently. UNLOADER-B-OPS was in exactly that state
	// while FGN_002 carried the plant's only changeover_load_directive, so the
	// LOAD directive card could not be rendered at all and the feature shipped
	// unobserved (N3, sim 2026-08-24).
	for _, s := range p.OperatorStations {
		if !builtStations[s] {
			add("operator station %q is declared but nothing builds it — add a cell_config for "+
				"its process, or pin a claim to it with operator_station; a station that is not "+
				"created binds no node and renders nothing", s)
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

	// ── Maintained groups ────────────────────────────────────────────────────
	//
	// MIRRORS THE SAVE-TIME REFUSALS, one for one. A spec that can declare a
	// configuration the settings modal refuses would seed a plant nobody could
	// then edit — the first save of an untouched screen would come back with a
	// reason, and the operator would be right to read it as a bug.
	//
	// Only the refusals. The two save-time WARNINGS (a level filling every
	// position, a supported position with no carrier types) stay warnings there
	// and are absent here: a seed is allowed to be in a state a plant is allowed
	// to be in.
	seenGroup := map[string]bool{}
	for _, mg := range p.MaintainedGroups {
		if mg.Group == "" {
			add("maintained_group with empty group")
			continue
		}
		if seenGroup[mg.Group] {
			add("duplicate maintained_group %q", mg.Group)
		}
		seenGroup[mg.Group] = true

		if kind, ok := nodes[mg.Group]; !ok {
			add("maintained_group %q references unknown zone", mg.Group)
		} else if kind != "zone" {
			add("maintained_group %q names a %s; a maintained group is a zone", mg.Group, kind)
		} else {
			// FLAT, because the save-time rule is flat. A lane means a carrier can
			// be buried, and a level counted over buried carriers is a number whose
			// meaning changes with what is parked in front of it.
			if zoneLanes[mg.Group] > 0 {
				add("maintained_group %q has %d lane(s); a maintained group is flat — declare its slots as positions",
					mg.Group, zoneLanes[mg.Group])
			}
			if zonePositions[mg.Group] == 0 {
				add("maintained_group %q has no positions to hold a level in", mg.Group)
			}
		}
		// projectOrder no-ops on a blank StationID.
		if strings.TrimSpace(mg.Station) == "" {
			add("maintained_group %q has no station; its top-up orders would show on no board", mg.Group)
		}
		if mg.Overflow != "" {
			if mg.Overflow == mg.Group {
				add("maintained_group %q overflows to itself", mg.Group)
			} else if kind, ok := nodes[mg.Overflow]; !ok || kind != "zone" {
				add("maintained_group %q overflows to %q, which is not a declared zone", mg.Group, mg.Overflow)
			}
		}
		if len(mg.Levels) == 0 {
			add("maintained_group %q declares no levels", mg.Group)
		}
		seenType := map[string]bool{}
		for _, l := range mg.Levels {
			if l.BinType == "" || !binTypes[l.BinType] {
				add("maintained_group %q level references unknown bin_type %q", mg.Group, l.BinType)
				continue
			}
			if seenType[l.BinType] {
				add("maintained_group %q declares bin_type %q twice", mg.Group, l.BinType)
			}
			seenType[l.BinType] = true
			if l.Want < 0 {
				add("maintained_group %q wants %d of %q; a level cannot be negative", mg.Group, l.Want, l.BinType)
			}
			// The episode key is `mnt|<group>|<type>`, so a code carrying the
			// separator parses back into different components than it was built
			// from. Refused where a code can still be changed.
			if strings.Contains(l.BinType, "|") {
				add("maintained_group %q level bin_type %q contains %q, which cannot be used in an episode key",
					mg.Group, l.BinType, "|")
			}
		}
		for _, proc := range mg.Supports {
			if !processes[proc] {
				add("maintained_group %q supports unknown process %q", mg.Group, proc)
			}
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
