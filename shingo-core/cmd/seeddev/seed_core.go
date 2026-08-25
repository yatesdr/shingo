package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/domain"
	"shingocore/plantspec"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
	"shingocore/store/registry"
)

// seedCore writes the plant's core (Postgres) topology through store accessors,
// in dependency order: node types → payloads + bin types → storage hierarchy
// (zone → lane → slot) + stations → bins (+ manifests) → node↔payload links →
// demand registry. Every create is existence-checked first, so it's idempotent.
//
// binIDByNode (caller-allocated) is filled with node-name → core bin id for
// every seeded bin, so seedEdge can bind a node's runtime active_bin_id to the
// bin physically at it (the loops only tick when a bin is bound).
func seedCore(db *store.DB, p *plantspec.Plant, binIDByNode map[string]int64) error {
	// --- node types (NGRP/LANE ship from migrations; ensure STOR + STATION) ---
	ngrpType, err := ensureNodeType(db, "NGRP", "Node Group", true)
	if err != nil {
		return err
	}
	laneType, err := ensureNodeType(db, "LANE", "Lane", true)
	if err != nil {
		return err
	}
	storType, err := ensureNodeType(db, "STOR", "Storage Slot", false)
	if err != nil {
		return err
	}
	stationType, err := ensureNodeType(db, "STATION", "Station", false)
	if err != nil {
		return err
	}

	// --- bin types + payloads (+ payload→bin-type links) ---
	binTypeIDs := make(map[string]int64)
	for _, bt := range p.BinTypes {
		id, err := ensureBinType(db, bt)
		if err != nil {
			return err
		}
		binTypeIDs[bt] = id
	}
	payloadIDs := make(map[string]int64)
	for _, pl := range p.Payloads {
		id, err := ensurePayload(db, pl)
		if err != nil {
			return err
		}
		payloadIDs[pl.Code] = id
		if btID, ok := binTypeIDs[pl.BinType]; ok {
			if err := db.SetPayloadBinTypes(id, []int64{btID}); err != nil {
				return fmt.Errorf("link payload %s → bin type %s: %w", pl.Code, pl.BinType, err)
			}
		}
	}

	// --- storage hierarchy: zone (NGRP) → lane (LANE) → slot (STOR, with depth) ---
	nodeIDs := make(map[string]int64) // node name → id (for parent links + bin placement)
	for _, z := range p.Zones {
		zID, err := ensureNode(db, z.Name, ptr(ngrpType), nil, z.Name, nil, true)
		if err != nil {
			return err
		}
		nodeIDs[z.Name] = zID
		for _, ln := range z.Lanes {
			lnID, err := ensureNode(db, ln.Name, ptr(laneType), ptr(zID), z.Name, nil, true)
			if err != nil {
				return err
			}
			nodeIDs[ln.Name] = lnID
			// The mark, if the spec placed one. Written unconditionally rather
			// than only-when-absent: re-seeding an edited spec has to be able to
			// MOVE a mark, and a lane's gate point is spec-owned state, not
			// operator-owned state that a seed should preserve.
			if ln.GatePoint != "" {
				if err := db.SetNodeProperty(lnID, dispatch.PropLaneGatePoint, ln.GatePoint); err != nil {
					return fmt.Errorf("lane %s gate point: %w", ln.Name, err)
				}
			}
			for _, s := range ln.Slots {
				depth := s.Depth
				sID, err := ensureNode(db, s.Name, ptr(storType), ptr(lnID), z.Name, &depth, false)
				if err != nil {
					return err
				}
				nodeIDs[s.Name] = sID
			}
		}
		// Flat positions: STOR children of the ZONE, with no lane between. This
		// is the shape a maintained group has to be in — the save-time rules
		// refuse a group with lanes — so a spec that could not express it could
		// not stage one.
		for _, s := range z.Positions {
			depth := s.Depth
			sID, err := ensureNode(db, s.Name, ptr(storType), ptr(zID), z.Name, &depth, false)
			if err != nil {
				return err
			}
			nodeIDs[s.Name] = sID
		}
	}
	// --- stations (line/press/weld/loader/unloader/staging/dest) ---
	for _, st := range p.Stations {
		id, err := ensureNode(db, st.Name, ptr(stationType), nil, st.Zone, nil, false)
		if err != nil {
			return err
		}
		nodeIDs[st.Name] = id
	}

	// --- bins (+ manifest for loaded ones) ---
	for _, b := range p.Bins {
		nodeID, ok := nodeIDs[b.Slot]
		if !ok {
			return fmt.Errorf("bin %s: slot %q not found among seeded nodes", b.Name, b.Slot)
		}
		btCode := b.BinType
		if btCode == "" && len(p.BinTypes) > 0 {
			btCode = p.BinTypes[0]
		}
		btID, ok := binTypeIDs[btCode]
		if !ok {
			return fmt.Errorf("bin %s: bin type %q not seeded", b.Name, btCode)
		}
		binID, created, err := ensureBin(db, b.Name, btID, nodeID)
		if err != nil {
			return err
		}
		binIDByNode[b.Slot] = binID
		// Bins must be 'available' for the retrieve resolver to
		// find them. Re-assert on every seed run (idempotent re-seed
		// may find existing bins with stale status from a prior run).
		if err := db.UpdateBinStatus(binID, domain.BinStatusAvailable); err != nil {
			return fmt.Errorf("bin %s set available: %w", b.Name, err)
		}
		if created && b.Payload != "" {
			manifest := buildManifest(b.Payload, b.UOP)
			if err := db.SetBinManifest(binID, manifest, b.Payload, int(b.UOP)); err != nil {
				return fmt.Errorf("bin %s set manifest: %w", b.Name, err)
			}
			// ConfirmBinManifest's 2nd arg is producedAt — it becomes the bin's
			// loaded_at, which FIFO retrieve orders by. Pass RFC3339 now so it
			// doesn't fall back to server time with a warning; AgeS backdates it so
			// a buried slot can be made the globally-oldest bin (reshuffle test).
			producedAt := time.Now().UTC()
			if b.AgeS > 0 {
				producedAt = producedAt.Add(-time.Duration(b.AgeS) * time.Second)
			}
			if err := db.ConfirmBinManifest(binID, producedAt.Format(time.RFC3339)); err != nil {
				return fmt.Errorf("bin %s confirm manifest: %w", b.Name, err)
			}
		}
	}

	// --- node↔payload links: every claim's node accepts its payload ---
	for _, c := range p.Claims {
		nodeID, ok := nodeIDs[c.CoreNode]
		if !ok {
			continue
		}
		if pid, ok := payloadIDs[c.Payload]; ok {
			if err := db.AssignPayloadToNode(nodeID, pid); err != nil {
				return fmt.Errorf("assign payload %s → node %s: %w", c.Payload, c.CoreNode, err)
			}
		}
		for _, ap := range c.AllowedPayloads {
			if pid, ok := payloadIDs[ap]; ok {
				if err := db.AssignPayloadToNode(nodeID, pid); err != nil {
					return fmt.Errorf("assign allowed payload %s → node %s: %w", ap, c.CoreNode, err)
				}
			}
		}
	}

	// --- bin-loader aggregate FIRST, then DERIVE demand_registry from it ---
	// BuildDemandRegistryFromAggregate is the Core-authored derivation production uses; it
	// stamps demand_registry.loader_id (the step-4 identity cutover) so the threshold
	// monitor mints LoaderKey and the Edge resolves a SYNTHETIC loader (e.g. multi-window
	// PLK_LOADER, whose anchor is not a node) by its token. The prior path wrote the
	// demands block straight to the registry, leaving loader_id NULL → the synthetic
	// loader's threshold signal dropped at the Edge (caught on the houseserver sim,
	// 2026-06-14). Thresholds still flow from the demands block: seedBinLoaders' threshold
	// map stamps them onto each loader's payload/home UOPThreshold, which the aggregate
	// derivation reads back. Must run AFTER the loaders exist.
	stationID := p.Namespace + "." + p.LineID

	// ENROLL THE STATION. This is the sim's stand-in for the production
	// enrollment step, and without it the dev stack is permanently broken after
	// the enrollment deploy: a register from an unenrolled uid is REFUSED and
	// writes nothing, so the Edge would heartbeat, publish, and never appear.
	//
	// The uid is the composed devplant.line1 rather than a minted stn-… for the
	// same reason both live plants keep theirs: it is what shingoedge.dev.yaml
	// carries, it never changes, and readable-but-immutable is not the defect —
	// self-asserted-and-mutable was.
	//
	// Already-enrolled is the normal case on a re-seed and is not an error.
	if _, err := db.EnrollEdge(stationID, p.Namespace+" "+p.LineID, stationID); err != nil &&
		!errors.Is(err, registry.ErrAlreadyEnrolled) {
		return fmt.Errorf("enroll station %s: %w", stationID, err)
	}

	if err := seedBinLoaders(db, p); err != nil {
		return fmt.Errorf("seed bin loaders: %w", err)
	}
	entries, err := db.BuildDemandRegistryFromAggregate(stationID)
	if err != nil {
		return fmt.Errorf("build demand registry from aggregate: %w", err)
	}
	if len(entries) > 0 {
		if _, err := db.SyncDemandRegistry(stationID, entries); err != nil {
			return fmt.Errorf("sync demand registry: %w", err)
		}
	}

	if err := seedMaintainedGroups(db, p, nodeIDs, binTypeIDs); err != nil {
		return fmt.Errorf("seed maintained groups: %w", err)
	}

	log.Printf("core: %d node types, %d payloads, %d nodes, %d bins, %d demands, %d maintained groups",
		4, len(payloadIDs), len(nodeIDs), len(p.Bins), len(entries), len(p.MaintainedGroups))
	return nil
}

// seedMaintainedGroups writes the config a maintained group is: four node
// properties and two node-keyed tables.
//
// THE SAME TWO HOMES THE UI WRITES, not a seed-only shortcut. The scalars go
// through SetNodeProperty and the sets through the maintain tables, so a seeded
// group and a hand-configured one are the same rows — which is the only thing
// that makes the sim's staged shape evidence about the real one.
//
// bin_type_mode is written explicitly, for the reason the save-time rules write
// it: inherit-by-default would let an ancestor's allowed-bins list silently
// govern a group that has just been told to hold two specific carrier types.
//
// SUPPORTS RESOLVE HERE. The spec names processes because that is what a person
// writes down; what lands in the table is the core nodes those processes' claims
// name, which is the same resolution the settings modal does at save time and for
// the same reason — Core cannot read an Edge claim when it has to decide
// anything.
//
// Written unconditionally rather than only-when-absent: re-seeding an edited
// spec has to be able to CHANGE a level, and a maintained group is spec-owned
// state, not operator-owned state a seed should preserve. Same rule the lane gate
// point follows.
func seedMaintainedGroups(db *store.DB, p *plantspec.Plant, nodeIDs map[string]int64, binTypeIDs map[string]int64) error {
	// process name → the core nodes its claims name, in spec order.
	processNodes := map[string][]string{}
	styleProcess := map[string]string{}
	for _, s := range p.Styles {
		styleProcess[s.Name] = s.Process
	}
	for _, c := range p.Claims {
		proc, ok := styleProcess[c.Style]
		if !ok || c.CoreNode == "" {
			continue
		}
		already := false
		for _, n := range processNodes[proc] {
			if n == c.CoreNode {
				already = true
				break
			}
		}
		if !already {
			processNodes[proc] = append(processNodes[proc], c.CoreNode)
		}
	}

	for _, mg := range p.MaintainedGroups {
		groupID, ok := nodeIDs[mg.Group]
		if !ok {
			return fmt.Errorf("maintained group %q: zone not found among seeded nodes", mg.Group)
		}
		props := []struct{ key, value string }{
			{nodes.PropMaintainEnabled, "on"},
			{nodes.PropStrictSourcing, boolProp(mg.Strict)},
			{nodes.PropMaintenanceStation, mg.Station},
			{nodes.PropOverflowDestination, mg.Overflow},
			{nodes.PropBinTypeMode, nodes.BinTypeModeAll},
		}
		for _, pr := range props {
			if err := db.SetNodeProperty(groupID, pr.key, pr.value); err != nil {
				return fmt.Errorf("maintained group %q property %s: %w", mg.Group, pr.key, err)
			}
		}
		for _, l := range mg.Levels {
			btID, ok := binTypeIDs[l.BinType]
			if !ok {
				return fmt.Errorf("maintained group %q: bin type %q not seeded", mg.Group, l.BinType)
			}
			if err := db.SetMaintainLevel(store.MaintainLevel{
				GroupNodeID: groupID, BinTypeID: btID, Want: l.Want,
			}); err != nil {
				return fmt.Errorf("maintained group %q level %s: %w", mg.Group, l.BinType, err)
			}
		}
		var supportIDs []int64
		for _, proc := range mg.Supports {
			for _, nodeName := range processNodes[proc] {
				id, ok := nodeIDs[nodeName]
				if !ok {
					continue
				}
				supportIDs = append(supportIDs, id)
			}
		}
		if err := db.SetMaintainSupports(groupID, supportIDs); err != nil {
			return fmt.Errorf("maintained group %q supports: %w", mg.Group, err)
		}
	}
	return nil
}

func boolProp(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// seedBinLoaders derives the Core-owned bin_loaders aggregate from the plant's
// manual_swap claims — the dev-sim equivalent of the production migration, and
// what makes the Core-owned loader read path exercisable in the sim. Grouped by
// (core_node, role); the plantspec has no
// home-location concept, so loaders are shared_window and their allowed payloads
// become bin_loader_payloads (min_stock = reorder_point, uop_threshold from the
// demand spec). Replenishment: consume → auto when auto_push else operator;
// produce → auto. Idempotent — skips a (core_node, role) that already exists.
func seedBinLoaders(db *store.DB, p *plantspec.Plant) error {
	// The station's own setting, by node name: whether a changeover commandeers
	// its card. It lives on the station because it describes the station.
	directive := map[string]bool{}
	for _, s := range p.Stations {
		if s.ChangeoverLoadDirective {
			directive[s.Name] = true
		}
	}
	threshold := map[string]int{}
	for _, d := range p.Demands {
		if d.ReplenishUOPThreshold != nil {
			threshold[d.Node+"|"+d.Payload] = *d.ReplenishUOPThreshold
		}
	}
	type lkey struct{ node, role string }
	type pcfg struct {
		code             string
		minStock, thresh int
	}
	first := map[lkey]plantspec.Claim{}
	pls := map[lkey][]pcfg{}
	var order []lkey
	// windowsByLoader groups window nodes under their parent shared loader. A
	// claim with window_of set is NOT its own loader — its node becomes a window
	// home of the parent, so a shared loader presents one demand across N windows
	// (the multi-window model the grid editor authors in production). The window
	// node still got a Core node + Edge process_node from the normal seed path.
	//
	// window_of names EITHER an anchor node that has its own manual_swap claim (the
	// legacy anchor+windows shape — the anchor becomes the loader, windows hang off
	// it) OR a SYNTHETIC identity with no claim of its own (the clean shape — the
	// loader has no physical anchor node; its windows are its only nodes, and the
	// loader config is derived from the windows' shared claim below). windowLead /
	// windowPls capture that derived config so a synthetic loader can be built.
	windowsByLoader := map[string][]string{}
	windowLead := map[string]plantspec.Claim{} // window_of → first window claim (config source)
	windowPls := map[string][]pcfg{}           // window_of → deduped payload configs
	windowPlSeen := map[string]bool{}          // window_of|code, dedup across N windows
	// home_of groups dedicated POSITIONS (one payload each) under a dedicated_positions
	// loader — the dual of window_of's shared windows. Each home claim is its own
	// position; same payload on two positions is legal (the O2 lead-time-buffer case).
	homesByLoader := map[string][]plantspec.Claim{} // home_of → dedicated position claims (ordered)
	homeLead := map[string]plantspec.Claim{}        // home_of → first home claim (config source)
	for _, c := range p.Claims {
		if c.SwapMode != string(protocol.SwapModeManualSwap) {
			continue
		}
		if c.WindowOf != "" {
			windowsByLoader[c.WindowOf] = append(windowsByLoader[c.WindowOf], c.CoreNode)
			if _, ok := windowLead[c.WindowOf]; !ok {
				windowLead[c.WindowOf] = c
			}
			codes := c.AllowedPayloads
			if len(codes) == 0 && c.Payload != "" {
				codes = []string{c.Payload}
			}
			for _, code := range codes {
				if windowPlSeen[c.WindowOf+"|"+code] {
					continue // windows share the loader's payload set — count it once
				}
				windowPlSeen[c.WindowOf+"|"+code] = true
				windowPls[c.WindowOf] = append(windowPls[c.WindowOf], pcfg{code, int(c.ReorderPoint), threshold[c.WindowOf+"|"+code]})
			}
			continue
		}
		if c.HomeOf != "" {
			homesByLoader[c.HomeOf] = append(homesByLoader[c.HomeOf], c)
			if _, ok := homeLead[c.HomeOf]; !ok {
				homeLead[c.HomeOf] = c
			}
			continue
		}
		k := lkey{c.CoreNode, c.Role}
		if _, ok := first[k]; !ok {
			first[k] = c
			order = append(order, k)
		}
		codes := c.AllowedPayloads
		if len(codes) == 0 && c.Payload != "" {
			codes = []string{c.Payload}
		}
		for _, code := range codes {
			pls[k] = append(pls[k], pcfg{code, int(c.ReorderPoint), threshold[c.CoreNode+"|"+code]})
		}
	}

	created := 0
	for _, k := range order {
		existing, err := db.GetLoaderByName(k.node, k.role)
		if err != nil {
			return fmt.Errorf("check loader %s/%s: %w", k.node, k.role, err)
		}
		if existing != nil {
			continue
		}
		c := first[k]
		// Role-aware: produce → threshold (UOP kanban); consume → operator (drain).
		repl := "threshold"
		if k.role == "consume" {
			repl = "operator"
		}
		id, err := db.CreateLoader(store.Loader{
			Name:          k.node,
			Role:          k.role,
			Layout:        "shared_window",
			Replenishment: repl,
			OutboundDest:  c.OutboundDestination,
			InboundSource: c.InboundSource,

			ChangeoverLoadDirective: directive[k.node],
		})
		if err != nil {
			return fmt.Errorf("create loader %s/%s: %w", k.node, k.role, err)
		}
		for _, pc := range pls[k] {
			if err := db.UpsertLoaderPayload(store.LoaderPayload{
				LoaderID: id, PayloadCode: pc.code, UOPThreshold: pc.thresh,
			}); err != nil {
				return fmt.Errorf("seed loader payload %s/%s: %w", k.node, pc.code, err)
			}
		}
		// Window homes. A loader authored with explicit windows (window_of children)
		// gets one window per child. A plain single-node loader (no window_of) gets its
		// ANCHOR materialised as its sole window — step 1: every loader resolves to at
		// least one real member row, so the projectCoreLoader empty-windows fallback is
		// dead code (removed in 6b). Behaviour-preserving: the fallback already treated
		// such a loader's anchor as its window; this just makes it explicit data. Windows
		// carry no per-position payload (the shared set rides bin_loader_payloads).
		wins := windowsByLoader[k.node]
		if len(wins) == 0 {
			wins = []string{k.node} // materialise the anchor as the sole window
		}
		for i, win := range wins {
			node, err := db.GetNodeByName(win)
			if err != nil || node == nil {
				return fmt.Errorf("seed loader %s window %s: node lookup: %w", k.node, win, err)
			}
			if err := db.UpsertLoaderHome(store.LoaderHome{
				LoaderID: id, PositionNodeID: node.ID, PayloadCode: "", SortOrder: i,
			}); err != nil {
				return fmt.Errorf("seed loader %s window home %s: %w", k.node, win, err)
			}
		}
		created++
	}

	// Synthetic-identity loaders. A window_of that names no anchor claim is a loader
	// with NO physical anchor node: its identity is its surrogate id (minted onto the
	// wire as the loader_key token) and its name is the window_of label; its windows are
	// its only nodes (each a delivery target + its own operator HMI), and its config
	// comes from the windows' shared claim. This is the clean multi-window shape — no
	// phantom anchor node that never receives a bin. Nothing downstream needs the
	// identity to be a node: the loader carries no node column at all, the threshold
	// monitor accounts UOP system-wide per payload, and empties deliver to the windows.
	// Demand keys on the loader's first window node. Sorted for a deterministic seed.
	anchorNode := make(map[string]bool, len(order))
	for _, k := range order {
		anchorNode[k.node] = true
	}
	var synthIDs []string
	for id := range windowsByLoader {
		if !anchorNode[id] {
			synthIDs = append(synthIDs, id)
		}
	}
	sort.Strings(synthIDs)
	for _, id := range synthIDs {
		lead := windowLead[id]
		existing, err := db.GetLoaderByName(id, lead.Role)
		if err != nil {
			return fmt.Errorf("check synthetic loader %s/%s: %w", id, lead.Role, err)
		}
		if existing != nil {
			continue
		}
		repl := "threshold"
		if lead.Role == "consume" {
			repl = "operator"
		}
		lid, err := db.CreateLoader(store.Loader{
			Name:          id,
			Role:          lead.Role,
			Layout:        "shared_window",
			Replenishment: repl,
			OutboundDest:  lead.OutboundDestination,
			InboundSource: lead.InboundSource,

			ChangeoverLoadDirective: directive[id],
		})
		if err != nil {
			return fmt.Errorf("create synthetic loader %s/%s: %w", id, lead.Role, err)
		}
		for _, pc := range windowPls[id] {
			if err := db.UpsertLoaderPayload(store.LoaderPayload{
				LoaderID: lid, PayloadCode: pc.code, UOPThreshold: pc.thresh,
			}); err != nil {
				return fmt.Errorf("seed synthetic loader payload %s/%s: %w", id, pc.code, err)
			}
		}
		for i, win := range windowsByLoader[id] {
			node, err := db.GetNodeByName(win)
			if err != nil || node == nil {
				return fmt.Errorf("seed synthetic loader %s window %s: node lookup: %w", id, win, err)
			}
			if err := db.UpsertLoaderHome(store.LoaderHome{
				LoaderID: lid, PositionNodeID: node.ID, PayloadCode: "", SortOrder: i,
			}); err != nil {
				return fmt.Errorf("seed synthetic loader %s window home %s: %w", id, win, err)
			}
		}
		created++
	}

	// Dedicated-positions loaders (home_of). Each home is a single-payload position;
	// the loader's identity is the home_of label (synthetic — never a node). Same
	// payload on two positions is legal and is the same-payload-two-position fixture
	// (each its own demand_registry row; the pooled threshold trips, ReservationTarget
	// routes to the named position). Sorted for a deterministic seed.
	var deckIDs []string
	for id := range homesByLoader {
		deckIDs = append(deckIDs, id)
	}
	sort.Strings(deckIDs)
	for _, id := range deckIDs {
		lead := homeLead[id]
		existing, err := db.GetLoaderByName(id, lead.Role)
		if err != nil {
			return fmt.Errorf("check dedicated loader %s/%s: %w", id, lead.Role, err)
		}
		if existing != nil {
			continue
		}
		repl := "threshold"
		if lead.Role == "consume" {
			repl = "operator"
		}
		lid, err := db.CreateLoader(store.Loader{
			Name:          id,
			Role:          lead.Role,
			Layout:        "dedicated_positions",
			Replenishment: repl,
			OutboundDest:  lead.OutboundDestination,
			InboundSource: lead.InboundSource,

			ChangeoverLoadDirective: directive[id],
		})
		if err != nil {
			return fmt.Errorf("create dedicated loader %s/%s: %w", id, lead.Role, err)
		}
		for i, hc := range homesByLoader[id] {
			node, err := db.GetNodeByName(hc.CoreNode)
			if err != nil || node == nil {
				return fmt.Errorf("seed dedicated loader %s position %s: node lookup: %w", id, hc.CoreNode, err)
			}
			code := hc.Payload
			if code == "" && len(hc.AllowedPayloads) > 0 {
				code = hc.AllowedPayloads[0]
			}
			thr := threshold[hc.CoreNode+"|"+code]
			if thr == 0 {
				thr = threshold[id+"|"+code] // loader-id-keyed demand fallback
			}
			if err := db.UpsertLoaderHome(store.LoaderHome{
				LoaderID: lid, PositionNodeID: node.ID, PayloadCode: code,
				UOPThreshold: thr, SortOrder: i,
			}); err != nil {
				return fmt.Errorf("seed dedicated loader %s position home %s: %w", id, hc.CoreNode, err)
			}
		}
		created++
	}

	if created > 0 {
		log.Printf("core: seeded %d bin loader(s) into the aggregate", created)
	}
	return nil
}

func ensureNodeType(db *store.DB, code, name string, synthetic bool) (int64, error) {
	if t, err := db.GetNodeTypeByCode(code); err == nil && t != nil {
		return t.ID, nil
	}
	nt := &nodes.NodeType{Code: code, Name: name, IsSynthetic: synthetic}
	if err := db.CreateNodeType(nt); err != nil {
		return 0, fmt.Errorf("create node type %s: %w", code, err)
	}
	return nt.ID, nil
}

func ensureNode(db *store.DB, name string, typeID, parentID *int64, zone string, depth *int, synthetic bool) (int64, error) {
	if n, err := db.GetNodeByName(name); err == nil && n != nil {
		return n.ID, nil
	}
	n := &nodes.Node{
		Name:        name,
		NodeTypeID:  typeID,
		ParentID:    parentID,
		Zone:        zone,
		Depth:       depth,
		IsSynthetic: synthetic,
		Enabled:     true,
	}
	if err := db.CreateNode(n); err != nil {
		return 0, fmt.Errorf("create node %s: %w", name, err)
	}
	return n.ID, nil
}

func ensureBinType(db *store.DB, code string) (int64, error) {
	if bt, err := db.GetBinTypeByCode(code); err == nil && bt != nil {
		return bt.ID, nil
	}
	bt := &bins.BinType{Code: code, Description: code + " (dev)"}
	if err := db.CreateBinType(bt); err != nil {
		return 0, fmt.Errorf("create bin type %s: %w", code, err)
	}
	return bt.ID, nil
}

func ensurePayload(db *store.DB, pl plantspec.Payload) (int64, error) {
	if p, err := db.GetPayloadByCode(pl.Code); err == nil && p != nil {
		// Re-seed onto an existing plant: bring the robot group up to what the
		// spec now says. Without this, a plant seeded before the spec carried
		// robot groups keeps dispatching every part to the vendor default, and
		// the reason would be invisible — the yaml says one thing and the
		// database another.
		if p.RobotGroup != pl.RobotGroup {
			p.RobotGroup = pl.RobotGroup
			if err := db.UpdatePayload(p); err != nil {
				return 0, fmt.Errorf("update robot group on payload %s: %w", pl.Code, err)
			}
		}
		return p.ID, nil
	}
	p := &payloads.Payload{
		Code:        pl.Code,
		UOPCapacity: int(pl.UOPCapacity),
		Description: pl.Code + " (dev)",
		RobotGroup:  pl.RobotGroup,
	}
	if err := db.CreatePayload(p); err != nil {
		return 0, fmt.Errorf("create payload %s: %w", pl.Code, err)
	}
	return p.ID, nil
}

// ensureBin returns (id, createdNow, err). createdNow=false means the bin
// already existed (re-run) and its manifest is left untouched.
func ensureBin(db *store.DB, label string, binTypeID, nodeID int64) (int64, bool, error) {
	if b, err := db.GetBinByLabel(label); err == nil && b != nil {
		return b.ID, false, nil
	}
	nid := nodeID
	b := &bins.Bin{BinTypeID: binTypeID, Label: label, NodeID: &nid, Status: "available"}
	if err := db.CreateBin(b); err != nil {
		return 0, false, fmt.Errorf("create bin %s: %w", label, err)
	}
	return b.ID, true, nil
}

// buildManifest renders a one-line manifest JSON for a loaded bin.
func buildManifest(payloadCode string, uop int64) string {
	type item struct {
		PartNumber string `json:"part_number"`
		Quantity   int64  `json:"quantity"`
	}
	m := struct {
		Items []item `json:"items"`
	}{Items: []item{{PartNumber: payloadCode, Quantity: uop}}}
	b, err := json.Marshal(m)
	if err != nil {
		return `{"items":[]}`
	}
	return string(b)
}

func ptr(v int64) *int64 { return &v }
