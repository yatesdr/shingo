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

	// ── THE A/B TWO-CONTAINER RULE ───────────────────────────────────────
	//
	// "If a process wants to run an A/B swap it needs at least two containers
	// in the system. Two nodes lineside holding the same part — it can't
	// function with less than 2." Paired positions are counted from the
	// claims' own geometry (see carrier_pairs.go); a frozen baseline reports
	// instead of refusing, same polarity as the census above.
	if cp := p.CarrierPairsAtBirth(); !cp.Clean() {
		if p.BaselineFrozenAt != "" {
			for _, f := range cp.Findings() {
				fmt.Printf("plantspec: KNOWN SEED DEFECT (frozen as a baseline by %s, not corrected on "+
					"purpose): %s\n", p.BaselineFrozenAt, f)
			}
		} else {
			errs = append(errs, cp.Findings()...)
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
	familyCodes := map[string]bool{}
	familyOfType := map[string]string{}
	for _, f := range p.CarrierFamilies {
		if familyCodes[f.Code] {
			add("carrier family %q is declared twice", f.Code)
		}
		familyCodes[f.Code] = true
		for _, t := range f.Types {
			if !binTypes[t] {
				add("carrier family %q names unknown bin type %q", f.Code, t)
			}
			if other, ok := familyOfType[t]; ok && other != f.Code {
				add("bin type %q is in two families, %q and %q", t, other, f.Code)
			}
			familyOfType[t] = f.Code
		}
	}
	checkAdmits := func(s Slot) {
		for _, code := range s.Admits {
			switch {
			case familyCodes[code] && binTypes[code]:
				add("slot %q admits %q, which is both a bin type and a carrier family", s.Name, code)
			case !familyCodes[code] && !binTypes[code]:
				add("slot %q admits %q, which is no bin type and no carrier family", s.Name, code)
			}
		}
	}
	for _, z := range p.Zones {
		for _, s := range z.Positions {
			checkAdmits(s)
		}
		for _, l := range z.Lanes {
			for _, s := range l.Slots {
				checkAdmits(s)
			}
		}
	}
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
		switch {
		case pl.NoBinType && pl.BinType != "":
			add("payload %q says no_bin_type and names bin_type %q; it is one or the other", pl.Code, pl.BinType)
		case pl.NoBinType:
		case pl.BinType == "" || !binTypes[pl.BinType]:
			add("payload %q references unknown bin_type %q", pl.Code, pl.BinType)
		}
		if pl.UOPCapacity <= 0 {
			add("payload %q has non-positive uop_capacity %d", pl.Code, pl.UOPCapacity)
		}
	}
	for code, size := range p.CarrierSizes {
		if !binTypes[code] {
			add("carrier_sizes names %q, which is not a bin_type", code)
		}
		if size.WidthIn <= 0 || size.LengthIn <= 0 || size.HeightIn < 0 {
			add("carrier_sizes %q: width_in and length_in must be above 0 and height_in not below it (a type with no footprint has no entry)", code)
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
		// A lane is never without a mark: the store refuses a lane whose group
		// has no wait point (nodes.ErrLaneUnmarked), so a plant that has one
		// would validate and then fail its seed. A zone of positions only needs
		// none.
		if len(z.Lanes) > 0 && !hasWaitPoint(z.WaitPoints) {
			add("zone %q has %d lane(s) and no wait_points; a lane needs a waiting spot beside its group", z.Name, len(z.Lanes))
		}
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
		// as lanes, which is the distinction a kanban turns on.
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
		add("storage hierarchy missing: need at least one zone → lane → slot (threshold replenishment only sees nodes under a LANE/NGRP parent)")
	}
	ref := func(name string) bool { _, ok := nodes[name]; return ok }
	kanbans := make(map[string]bool, len(p.Kanbans))
	for _, mg := range p.Kanbans {
		kanbans[mg.Group] = true
	}

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

	// --- per-loader settings ---
	// A bare marker is not declared here: it derives from each cart's own type
	// at a stage-1 CLEAR (bins.EnsureBareMarkerTx), so a fixture cannot name one.
	for name := range p.LoaderSettings {
		if !ref(name) && !loaderIdentities[name] {
			add("loader_settings names %q, which is neither a node nor a window_of/home_of loader", name)
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
	for _, pr := range p.Processes {
		for _, code := range pr.Parts {
			if !payloads[code] {
				add("process %q parts names unknown payload %q", pr.Name, code)
			}
		}
		// A station uid is namespace.line_id; anything else seeds an Edge nothing
		// will ever run as.
		if ns, line := splitStation(pr.Edge); pr.Edge != "" && (ns == "" || line == "") {
			add("process %q edge %q is not a station uid (namespace.line_id)", pr.Name, pr.Edge)
		}
	}
	// A pin is the CATID values the PLC reports for the style's part: integers,
	// comma-separated. Anything else never matches a read.
	for _, s := range p.Styles {
		if s.ExpectedCATID == "" {
			continue
		}
		for _, v := range strings.Split(s.ExpectedCATID, ",") {
			if v = strings.TrimSpace(v); v == "" || strings.Trim(v, "0123456789") != "" {
				add("style %q expected_catid %q is not a comma-separated list of integers", s.Name, s.ExpectedCATID)
				break
			}
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
		if c.PartialDestination != "" && !ref(c.PartialDestination) {
			add("%s: unknown partial_destination %q", where, c.PartialDestination)
		}
		if c.PartialDestination != "" && c.Role != "produce" {
			add("%s: partial_destination is a produce claim's; a %s claim has no unfinished carrier to route", where, c.Role)
		}
		// A SOURCE IS A GROUP OR A LOADER POSITION, NEVER A LANE (owner,
		// 2026-10-03). A need that names a lane searches that lane only, so a bin
		// put back anywhere else in its group is one that need never finds. The
		// same refusal as the loader save check (service.ErrSourceIsALane).
		for _, leg := range []struct{ field, name string }{
			{"inbound_source", c.InboundSource}, {"outbound_destination", c.OutboundDestination},
		} {
			if leg.name != "" && nodes[leg.name] == "lane" {
				add("%s: %s %q is a lane; a lane is not a source — name its group", where, leg.field, leg.name)
			}
		}
		// A CONSUMER'S FULLS DO NOT COME FROM A KANBAN. A kanban
		// refuses a carrier holding a part (binresolver.ResolveStore's empties-only
		// refusal), so a full could be drawn from it and never put back. The same
		// refusal as service.ErrFullsFromAKanban.
		if c.Role == "consume" && c.InboundSource != "" && kanbans[c.InboundSource] {
			add("%s: inbound_source %q is a kanban; a consumer's fulls cannot come from it",
				where, c.InboundSource)
		}
		if c.PairedCoreNode != "" && !ref(c.PairedCoreNode) {
			add("%s: unknown paired_core_node %q", where, c.PairedCoreNode)
		}
		// Per-mode swap field requirements — mirror BuildSwapDispatch's runtime
		// checks (shingo-edge/engine/swap_dispatch.go) so a spec that validates
		// won't strand at dispatch. single_robot needs BOTH staging nodes (and
		// they must be distinct — same node collides the new + old bins);
		// two_robot needs inbound staging + outbound_destination (the evac
		// leg's only dropoff; blank, the supply leg ships and the evac leg is
		// refused, leaving the pair half-dispatched); two_robot_press_index
		// needs paired_core_node + outbound_destination (no staging);
		// sequential (A/B) needs neither.
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
			if c.OutboundDestination == "" {
				add("%s: two_robot requires outbound_destination", where)
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
		}
		// ── WHO BRINGS THE NEXT CARRIER ─────────────────────────────────────
		//
		// Robot 2 bringing it is press-index choreography; nothing else has
		// two robots to swap between, and a spec that sets it on another mode
		// describes a cell that cannot exist. Outside the mode switch, so it
		// holds for every other mode. The Edge's ValidateNodeClaim refuses the
		// same claim.
		if c.RefillBy != "" && !c.RefillBy.Valid() {
			add("%s: refill_by %q is not a robot that brings the next carrier (want one of %v)", where, c.RefillBy, protocol.RefillBys())
		} else if c.RefillBy.OrDefault() != protocol.RefillByClearingRobot &&
			protocol.SwapMode(c.SwapMode) != protocol.SwapModeTwoRobotPressIndex {
			add("%s: refill_by %q applies to two_robot_press_index only", where, c.RefillBy)
		}
		// ── A TOOL CHANGE'S MARKS ───────────────────────────────────────────
		//
		// Every line mode a robot serves may mark positions; a manual_swap
		// loader has none to clear. A tool change holds the incoming carrier at
		// inbound staging until the operator's Tooling done, so a claim with
		// marks names where. Both are the Edge's save rules.
		if len(c.ChangeoverEvacNodes) > 0 {
			if c.IsLoader() {
				add("%s: changeover_evac_nodes applies to a line a robot serves; a manual_swap loader has no positions to clear", where)
			} else if c.InboundStaging == "" {
				add("%s: changeover_evac_nodes needs inbound_staging: a tool change holds the new carrier there until Tooling done", where)
			}
		}
		// ── A MARKED NODE MUST BE ONE THIS CLAIM HOLDS ─────────────────────
		//
		// The marks name core nodes. A node the claim does not hold is not an
		// unlikely configuration, it is a reference to nothing — and the
		// clearance it asks for silently never happens, which is the failure
		// mode hardest to notice on a sim. Same rule the Edge's
		// ValidateNodeClaim applies; stated here too because a spec is written
		// long before an Edge sees it.
		held := map[string]bool{}
		for _, n := range c.Positions() {
			held[n] = true
		}
		seenMark := map[string]bool{}
		for _, node := range c.ChangeoverEvacNodes {
			if !held[node] {
				add("%s: changeover_evac_nodes marks %q, which is not one of this claim's nodes", where, node)
			}
			if seenMark[node] {
				add("%s: changeover_evac_nodes lists %q more than once", where, node)
			}
			seenMark[node] = true
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
		if len(c.KeyRoute) > 0 && c.IsLoader() {
			add("%s: key_route applies to robot-served claims; a manual_swap loader does not drive", where)
		}
		if c.KeyTask != "" && c.KeyTask != "load" && c.KeyTask != "unload" {
			add("%s: key_task must be \"load\", \"unload\", or empty; got %q", where, c.KeyTask)
		}
		// Any staging node that IS set must exist.
		if c.InboundStaging != "" && !ref(c.InboundStaging) {
			add("%s: unknown inbound_staging %q", where, c.InboundStaging)
		}
		if c.OutboundStaging != "" && !ref(c.OutboundStaging) {
			add("%s: unknown outbound_staging %q", where, c.OutboundStaging)
		}
		// manual_swap needs an outbound destination (claims.go enforces this) —
		// EXCEPT a dedicated home, which must not have one.
		//
		// A produce loader pushes every carrier it fills to its OutboundDestination:
		// createLoaderOutbound fires unconditionally at the tail of LoadBin. For a
		// dedicated_positions loader whose LINE swaps directly against the home,
		// that push is wrong — the carrier is supposed to sit on its home until the
		// line's supply leg comes for it (Springfield: `complex SMN_016 -> ALN_002`,
		// with no L2 move anywhere in the chain). Springfield's loader 7 expresses
		// that by leaving the destination blank.
		//
		// PRODUCE ONLY. The consume side is the opposite and still needs one: an
		// unloader receives direct from the press and sends its drained carriers OUT,
		// to a node group — Springfield's loader 9 outbounds to SMN_BUF_100, an NGRP.
		// Exempting on home_of alone would have let an unloader declare no
		// destination and silently have nowhere to put what it drains.
		//
		// Requiring one unconditionally made that shape inexpressible here. Given a
		// destination to satisfy this rule, the loader shuffles carriers straight
		// back off the homes — pointed into its own pool it ping-pongs
		// (`move PLK_H1 -> PLK_H2`, `move PLK_B2 -> PLK_B1`) and the homes stand
		// empty while the pool sources from buffers; pointed at a market it drains
		// the loop outright. Either way the plant cannot model a dedicated loader
		// feeding a line, which is the shape the park side exists to serve.
		if c.IsLoader() && c.OutboundDestination == "" && !(c.HomeOf != "" && c.Role == "produce") {
			add("%s: manual_swap requires outbound_destination", where)
		}
	}

	errs = append(errs, p.keepStagedFindings()...)

	// --- demands / reporting points / cell configs ---
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
	stationTags := map[string]bool{}
	for _, rp := range p.ReportingPoints {
		stationTags[rp.TagName] = true
	}
	for _, sc := range p.StationCounters {
		if !processes[sc.Process] {
			add("station counter %q references unknown process %q", sc.TagName, sc.Process)
		}
		if sc.TagName == "" {
			add("station counter on process %q needs tag_name", sc.Process)
		} else if stationTags[sc.TagName] {
			add("station counter tag %q is already read by a reporting point or another station counter", sc.TagName)
		}
		stationTags[sc.TagName] = true
		if len(sc.Nodes) == 0 {
			add("station counter %q counts no node", sc.TagName)
		}
		for _, n := range sc.Nodes {
			if !ref(n) {
				add("station counter %q references unknown node %q", sc.TagName, n)
			}
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
	// station is unreachable, silently. UNLOADER-B-OPS was in exactly that state,
	// so nothing bound to it rendered and no run noticed (N3, sim 2026-08-24).
	for _, s := range p.OperatorStations {
		if !builtStations[s] {
			add("operator station %q is declared but nothing builds it — add a cell_config for "+
				"its process, or pin a claim to it with operator_station; a station that is not "+
				"created binds no node and renders nothing", s)
		}
	}

	// --- payload parity (G6): every payload must have both a producer and a consumer ---
	// Producers are produce claims with an outbound_destination (sends material to a
	// market). Consumers are consume claims with an inbound_source (pulls material from
	// a market). manual_swap nodes are included — they're legitimate producers (loaders)
	// and consumers (unloaders) in the material flow.
	type payloadFlow struct{ produce, consume int }
	flowByPayload := make(map[string]*payloadFlow)
	for i, c := range p.Claims {
		// A LOADER THAT NAMES NO PAYLOAD. An unloader's shared window may name
		// none: the second stage of a two-stage unloader is fed an empty carrier
		// directly by stage 1, has nothing to pull and no part to name, so it is
		// in no payload's flow and parity has nothing to say about it. Counting
		// it as a consumer of payload "" refused the shape the Edge allows
		// (TestValidate_AZeroPayloadUnloaderIsAllowed). A LOADER still needs a
		// payload, because it is what the loader loads; that is said as its own
		// finding rather than as parity on "". A dedicated position (home_of) is
		// one payload by definition and stays under parity.
		if c.IsLoader() && c.Payload == "" && len(c.AllowedPayloads) == 0 && c.HomeOf == "" {
			if c.Role == "produce" {
				add("claim[%d] %s/%s: a loader needs at least one payload — only an unloader's window may declare none",
					i, c.CoreNode, c.Style)
			}
			continue
		}
		// EVERY PART THE CLAIM SERVES. A shared loader's parts are its allowed list
		// (the seeder's rule, seed_core.seedBinLoaders), so an unloader that takes
		// five parts is a consumer of all five.
		for _, code := range c.FlowPayloads() {
			fl := flowByPayload[code]
			if fl == nil {
				fl = &payloadFlow{}
				flowByPayload[code] = fl
			}
			// A dedicated home produces without an outbound: the line's swap lifts the
			// carrier off it. Counting only claims that push meant such a loader read as
			// "not a producer", and its payload as consumed-but-never-produced.
			if c.Role == "produce" && (c.OutboundDestination != "" || c.HomeOf != "") {
				fl.produce++
			}
			// The mirror of the produce case above. An unloader can receive DIRECT from
			// the cell it serves and name no inbound source at all — Springfield's
			// loader 9 is exactly that: blank inbound_source, outbound_dest to the node
			// group SMN_BUF_100. Counting only claims that name an inbound made such an
			// unloader invisible as a consumer, so its payload read as produced-but-
			// never-consumed and the plant was refused for a shape the plant actually runs.
			if c.Role == "consume" && (c.InboundSource != "" || c.WindowOf != "" || c.HomeOf != "") {
				fl.consume++
			}
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

	// ── Kanbans ────────────────────────────────────────────────────
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
	for _, mg := range p.Kanbans {
		if mg.Group == "" {
			add("kanban with empty group")
			continue
		}
		if seenGroup[mg.Group] {
			add("duplicate kanban %q", mg.Group)
		}
		seenGroup[mg.Group] = true

		if kind, ok := nodes[mg.Group]; !ok {
			add("kanban %q references unknown zone", mg.Group)
		} else if kind != "zone" {
			add("kanban %q names a %s; a kanban is a zone", mg.Group, kind)
		} else {
			// FLAT, because the save-time rule is flat. A lane means a carrier can
			// be buried, and a level counted over buried carriers is a number whose
			// meaning changes with what is parked in front of it.
			if zoneLanes[mg.Group] > 0 {
				add("kanban %q has %d lane(s); a kanban is flat — declare its slots as positions",
					mg.Group, zoneLanes[mg.Group])
			}
			if zonePositions[mg.Group] == 0 {
				add("kanban %q has no positions to hold a level in", mg.Group)
			}
		}
		// projectOrder no-ops on a blank StationID. A kanban with no supports
		// declares what it holds and needs none.
		if len(mg.Supports) > 0 && strings.TrimSpace(mg.Station) == "" {
			add("kanban %q has no station; its top-up orders would show on no board", mg.Group)
		}
		for family, pool := range mg.Pools {
			if pool < 0 {
				add("kanban %q has pool %d of %q; it cannot be negative", mg.Group, pool, family)
			}
			if !familyCodes[family] && !binTypes[family] {
				add("kanban %q has a pool of %q, which is no family and no bin type", mg.Group, family)
			}
		}
		seenSource := map[string]bool{}
		for _, src := range mg.Sources {
			if src == mg.Group {
				add("kanban %q names itself as a source", mg.Group)
			} else if kind, ok := nodes[src]; !ok || kind != "zone" {
				add("kanban %q sources from %q, which is not a declared zone", mg.Group, src)
			}
			if seenSource[src] {
				add("kanban %q lists source %q twice", mg.Group, src)
			}
			seenSource[src] = true
		}
		if len(mg.Levels) == 0 {
			add("kanban %q declares no levels", mg.Group)
		}
		seenType := map[string]bool{}
		for _, l := range mg.Levels {
			if l.BinType == "" || !binTypes[l.BinType] {
				add("kanban %q level references unknown bin_type %q", mg.Group, l.BinType)
				continue
			}
			if seenType[l.BinType] {
				add("kanban %q declares bin_type %q twice", mg.Group, l.BinType)
			}
			seenType[l.BinType] = true
			if l.Want < 0 {
				add("kanban %q wants %d of %q; a level cannot be negative", mg.Group, l.Want, l.BinType)
			}
		}
		for _, proc := range mg.Supports {
			if !processes[proc] {
				add("kanban %q supports unknown process %q", mg.Group, proc)
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

// hasWaitPoint reports whether points names at least one point, blanks
// dropped as the store's parse drops them (nodes.ParseWaitPoints).
func hasWaitPoint(points []string) bool {
	for _, pt := range points {
		if strings.TrimSpace(pt) != "" {
			return true
		}
	}
	return false
}
