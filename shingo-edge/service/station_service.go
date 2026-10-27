package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol"
	"shingo/protocol/lockcheck"
	"shingoedge/domain"
	"shingoedge/internal/lockorder"
	"shingoedge/store"
	"shingoedge/store/lineside"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// claimsByCoreNode indexes a style's node claims by core node name — the
// batched form of GetStyleNodeClaimByNode for a whole board. Exact rather than
// approximate: style_node_claims declares UNIQUE(style_id, core_node_name), so
// there is at most one claim per (style, node) and the map cannot lose one.
func claimsByCoreNode(db *sql.DB, styleID int64) (map[string]processes.NodeClaim, error) {
	claims, err := processes.ListClaims(db, styleID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]processes.NodeClaim, len(claims))
	for _, c := range claims {
		out[c.CoreNodeName] = c
	}
	return out, nil
}

// pressPositionClaimsForBoard derives the per-position claim for every press
// position that a fanned-out changeover gave its OWN task, keyed by process node
// ID. Returns nil when the board has no changeover tasks, which is the steady
// state — this costs nothing on an ordinary poll.
//
// The positions it covers have no style_node_claims row by design: the planner
// synthesizes their claims in memory and UpsertClaim refuses to persist the
// press_position marker. Before Hopkinsville 2026-08-05 the view simply had no
// route to them, so they rendered as unclaimed nodes with no actions — see the
// call site in BuildView for the incident.
//
// Derivation is deliberately the planner's own: the node task records the
// claim it was planned from (FromClaimID/ToClaimID → the PARENT press-index
// row, since a synthesized claim carries its parent's ID), and
// domain.SynthesizePositionClaim is the same function the planner used.
// Anything that fails a check is left claimless rather than given an invented
// claim — a wrong claim is worse than none.
func pressPositionClaimsForBoard(
	db *sql.DB,
	nodes []processes.Node,
	nodeTasks map[int64]processes.NodeTask,
	activeClaims, targetClaims map[string]processes.NodeClaim,
) map[int64]*processes.NodeClaim {
	if len(nodeTasks) == 0 {
		return nil
	}
	out := map[int64]*processes.NodeClaim{}
	// Parent claims are shared by the positions of one press, so read each at most
	// once. A failed read caches nil and is not retried per position.
	parents := map[int64]*processes.NodeClaim{}
	for _, node := range nodes {
		if node.CoreNodeName == "" {
			continue
		}
		if _, ok := activeClaims[node.CoreNodeName]; ok {
			continue
		}
		if _, ok := targetClaims[node.CoreNodeName]; ok {
			continue
		}
		task, ok := nodeTasks[node.ID]
		if !ok {
			continue
		}
		// Drop and Swap carry the position's current material on FromClaimID; Add
		// only ever has ToClaimID. Prefer From — it names the payload physically
		// sitting on the position, which is what an evac release acts on.
		claimID := task.FromClaimID
		if claimID == nil {
			claimID = task.ToClaimID
		}
		if claimID == nil {
			continue
		}
		parent, loaded := parents[*claimID]
		if !loaded {
			p, err := processes.GetClaim(db, *claimID)
			if err != nil {
				p = nil
			}
			parents[*claimID] = p
			parent = p
		}
		if parent == nil {
			continue
		}
		// Only a press-index parent fans out into per-position positions, and the
		// position must be one this parent actually names. Both checks live in
		// domain.PositionClaimFromParent, which is the same derivation the per-node
		// changeover actions and the cutover gate use — the view and the actions
		// have to agree about which positions are resolvable, or a position renders with
		// buttons that refuse. Anything that is not a position of a press-index
		// parent comes back nil and is left claimless.
		position := domain.PositionClaimFromParent(parent, node.CoreNodeName)
		if position == nil {
			continue
		}
		out[node.ID] = position
	}
	return out
}

// LoaderResolver resolves the Core-owned loader aggregate a node belongs to, for
// the operator view. It is consumer-defined HERE (service sits below engine, so it
// cannot import the engine's LoaderStore); the engine injects its
// LoaderStore via SetLoaderResolver. Routing the view through the SAME resolver the
// runtime uses is deliberate — it keeps "what loader is this node part of" a single
// source of truth (the B1 goal) instead of re-deriving it from the cache in the view.
//
// Contract: a clean miss returns (nil, nil) — the node is not a known aggregate
// loader, and BuildView takes the node's stored claim. A non-nil error is a
// real failure; the view takes the stored claim rather than inventing a loader.
type LoaderResolver interface {
	// LoaderForNode resolves the loader (any role) a node belongs to, so the view
	// can treat a Core-owned loader's window or position as a loader tile even
	// when it has no per-style edge claim. (nil, nil) on a clean miss.
	LoaderForNode(coreNode domain.NodeID) (*domain.Loader, error)
}

// StationService owns operator-station CRUD and the two cross-aggregate
// orchestrations that span stations + processes + orders + lineside:
// SetNodes (sync process_nodes for a station) and BuildView (the
// operator HMI projection). Phase 6.1 introduced the cross-aggregate
// methods; Phase 6.2′ folded in the per-station CRUD that previously
// sat as named methods on *engine.Engine.
type StationService struct {
	db *store.DB
	// loaders resolves a node's parent loader aggregate for the operator view.
	// Optional: nil makes no tile a loader tile, so the lighter test
	// constructors that don't wire it compile and pass unchanged. The engine injects the live resolver via SetLoaderResolver.
	loaders LoaderResolver
	// stranded resolves a node's active parked-ticks alarm sentence (P2-C8) by
	// core node name. Optional: nil leaves StrandedAlarm empty. The engine injects
	// the live resolver (its strandedAlarms map) via SetStrandedResolver.
	stranded func(coreNodeName string) string
	// keepStaged resolves a line's keep-staged keeper sentence; nil leaves
	// KeepStagedNote empty. The engine injects KeepStagedNote.
	keepStaged func(coreNodeName string) string
	// unloaderAsk resolves an unloader window's pending U1 note (the engine's
	// UnloaderAskNote); nil leaves AskPending empty.
	unloaderAsk func(coreNodeName string) string
	// binTypes resolves a payload code to its bin type, for a served row's
	// carrier. Optional: unset leaves every carrier blank, which the board reads
	// as unknown rather than guessing one.
	binTypes func(payloadCode string) string
	// carrierCatalog is Core's payload-to-bin-type catalog, for an unloader
	// tile's carriers. Optional: unset leaves carriers empty.
	carrierCatalog func() []protocol.PayloadBinTypeInfo
	// coreNodes reports the Core node names this edge currently knows, as a set.
	// Optional, and the nil-safety is load-bearing rather than incidental — see
	// unknownCoreNodes for why an unwired or empty resolver must accept
	// everything.
	coreNodes func() map[string]bool
	// sceneGeometry hands the view the scene cache for the cell picture.
	// Optional: nil, or a resolver answering nil, draws the schematic.
	sceneGeometry func() *domain.SceneGeometry
	// coreNodeGroups hands the view the NGRP membership for the dock strip.
	// Optional: nil lists no members under a group.
	coreNodeGroups func() map[string][]string
	// nodeRetirer retires a process_node through the engine, so a station's
	// node-list sync closes the node's demand episodes and takes its lineside
	// piles with it rather than only tombstoning the row. Optional: nil keeps
	// the plain store retire, for the lighter test constructors.
	nodeRetirer func(int64) error
	// plantGeneration is the engine's counter for the two caches a node-list
	// response replaces — the scene geometry and the NGRP membership — and is
	// how a station poll can say "the plant moved" WITHOUT calling either
	// resolver. That matters: CoreNodeGroups deep-copies the whole plant's
	// group map under the core-node lock, which is precisely the cost the
	// picture leaving the poll exists to remove, so the version must not pay
	// it. Optional; unset reads as 0, which is stable and therefore safe.
	plantGeneration func() uint64
	// loaderGeneration is the engine's counter for its copy of Core's
	// loaders, which the board's bin lists read. Optional; unset reads as 0.
	loaderGeneration func() uint64

	// bins is every station's bin list, held until the plant moves. See
	// station_bins.go.
	binsMu lockcheck.Mutex[lockorder.StationBins]
	bins   binNodes
	// binNodesChanged runs when a rebuild moves the watched names.
	binNodesChanged func()

	// sceneMemo is composerScene's derived adjacency, computed once per
	// geometry cache rather than per request; sceneMemoFor is the cache it
	// was derived from. Guarded because one service serves every board's
	// poll. See composerScene.
	sceneMu      lockcheck.Mutex[lockorder.StationScene]
	sceneMemoFor *domain.SceneGeometry
	sceneMemo    *domain.ComposerScene

	// curtained is every live node with the curtain interlock on, keyed by
	// (process, core node), as of process_nodes generation curtainedGen. See
	// curtainedNodes for why it is held rather than read per poll.
	curtainMu     lockcheck.Mutex[lockorder.StationCurtain]
	curtained     map[curtainKey]processes.Node
	curtainedGen  uint64
	curtainLoaded bool

	// touched throttles the liveness write — see Touch.
	touchMu lockcheck.Mutex[lockorder.StationTouch]
	touched map[int64]touchState
}

type touchState struct {
	status string
	at     time.Time
}

// touchThrottle is how stale last_seen_at is allowed to get while a board's
// stream keeps touching. Well under any human's sense of "is that screen alive", and the
// column has no behavioural consumer — nothing reaps or alarms on it, it is
// surfaced for display only.
const touchThrottle = 15 * time.Second

// NewStationService constructs a StationService wrapping the shared
// *store.DB. The loader resolver is wired separately via SetLoaderResolver so
// existing call sites (and tests) that need no loader tiles stay
// unchanged.
func NewStationService(db *store.DB) *StationService {
	return &StationService{db: db}
}

// SetLoaderResolver injects the loader-aggregate resolver the view reads a
// node's Core loader through (its claim and its loader fields). The engine calls
// this once at startup with its LoaderStore.
func (s *StationService) SetLoaderResolver(r LoaderResolver) { s.loaders = r }

// SetNodeRetirer injects the engine's node retire, so a station sync that
// drops a node runs the full retire (episode closes, pile removal, pile
// zero-sends) instead of the bare store delete. The engine calls this once at
// startup.
func (s *StationService) SetNodeRetirer(r func(int64) error) { s.nodeRetirer = r }

// SetBinTypeResolver injects the payload-to-bin-type lookup a served row's
// carrier reads. Optional; unset leaves every carrier blank.
func (s *StationService) SetBinTypeResolver(r func(payloadCode string) string) { s.binTypes = r }

// SetCarrierCatalog injects Core's payload-to-bin-type catalog, which an
// unloader tile's carriers are read from. Optional; unset leaves them empty.
func (s *StationService) SetCarrierCatalog(r func() []protocol.PayloadBinTypeInfo) {
	s.carrierCatalog = r
}

// SetCoreNodeResolver injects the set of Core node names this edge knows, used
// to refuse a station node list that names something Core does not have.
func (s *StationService) SetCoreNodeResolver(r func() map[string]bool) { s.coreNodes = r }

// SetSceneGeometryResolver injects the scene cache the cell picture places
// its positions from. Optional; unset or nil-answering draws the schematic.
func (s *StationService) SetSceneGeometryResolver(r func() *domain.SceneGeometry) {
	s.sceneGeometry = r
}

// SetCoreNodeGroupResolver injects the NGRP membership the cell picture's
// dock strip lists. Optional; unset lists no members.
func (s *StationService) SetCoreNodeGroupResolver(r func() map[string][]string) { s.coreNodeGroups = r }

// SetPlantGenerationResolver injects the counter that says the geometry cache
// or the NGRP map has been replaced — the cell picture's two plant-side
// inputs, both replaced only by a node-list response. Read on every poll to
// build the picture's version; see domain.CellPictureVersion.
func (s *StationService) SetPlantGenerationResolver(r func() uint64) { s.plantGeneration = r }

// plantGen is the counter, or 0 when nothing wired one.
//
// ZERO IS SAFE BECAUSE IT IS CONSTANT: an unwired resolver means an edge with
// no engine behind it (every service test), where the geometry and the group
// map do not change either. A version that never moves on an input that never
// moves is right.
func (s *StationService) plantGen() uint64 {
	if s.plantGeneration == nil {
		return 0
	}
	return s.plantGeneration()
}

// expandGroups replaces each node group in names with the nodes standing in it,
// in the group's order, and passes every other name through.
//
// A group is never a position: it has no slot, so a group on a station renders
// nothing (unknownCoreNodes names the Springfield rows that did). Picking one
// means its nodes, which is the Core-made group of a loader's windows put on a
// screen in one pick. Membership is Core's, from the last node list: the plain
// nodes directly in the group, never a lane or its slots, since Core does not
// send those as members. A group with no members, or no resolver wired, passes
// through unchanged.
//
// ONCE, AT SAVE. The screen holds the nodes, not the group, so a node added to
// the group later reaches the screen when the group is picked again.
func (s *StationService) expandGroups(names []string) []string {
	if s.coreNodeGroups == nil {
		return names
	}
	groups := s.coreNodeGroups()
	out := make([]string, 0, len(names))
	for _, name := range names {
		if members := groups[strings.TrimSpace(name)]; len(members) > 0 {
			out = append(out, members...)
			continue
		}
		out = append(out, name)
	}
	return out
}

// GroupMembers is the group membership expandGroups applies: NGRP name to the
// plain nodes directly in it. Empty with no resolver wired.
func (s *StationService) GroupMembers() map[string][]string {
	if s.coreNodeGroups == nil {
		return map[string][]string{}
	}
	return s.coreNodeGroups()
}

// ErrPositionOfOtherProcess rejects a station node list naming a slot that is
// already a live position of a different process (and no loader's window set
// names it). Like ErrUnknownCoreNodes it is bad input, not a server fault.
var ErrPositionOfOtherProcess = errors.New("already a position of another process")

// otherProcessPositionError carries the slot and the process holding it, and
// matches ErrPositionOfOtherProcess under errors.Is.
type otherProcessPositionError struct {
	name, process string
}

func (e *otherProcessPositionError) Error() string {
	return fmt.Sprintf("%q is already a position of process %s — a slot can be modelled by only one process unless a loader's window set names it",
		e.name, e.process)
}

func (e *otherProcessPositionError) Is(target error) bool {
	return target == ErrPositionOfOtherProcess
}

// ErrUnknownCoreNodes rejects a station node list naming something that is not a
// Core node.
var ErrUnknownCoreNodes = errors.New("not a Core node")

// unknownCoreNodes returns the names Core does not know, or nil when the check
// cannot be made.
//
// The edge `claimed-nodes` API did no validation at all, and process_nodes are
// matched by core_node_name everywhere, so a typo or — the real case — a node
// GROUP label pasted where a node name belongs produced a row that resolves to
// nothing on Core and renders on no board. Springfield carried three of them
// (`Unloader Pull From`, `Unloader Send To`, `SNF2 Loader`), and because SetNodes
// ADOPTS by name they stayed adoptable onto live stations long after the process
// that made them was deleted.
//
// FAIL OPEN, in two distinct cases, and both matter:
//
//   - NO RESOLVER WIRED. Test constructors and any caller that does not need the
//     check must behave exactly as before.
//   - AN EMPTY SET. An edge that has booted but not yet received its first node
//     list knows no names. Refusing on that would lock config out during exactly
//     the window somebody is most likely to be fixing something.
//
// Names are compared bare: engine.SetCoreNodes normalizes Core's display-only
// "Group.Child" form at ingestion, and the runtime keys on the bare child name.
func (s *StationService) unknownCoreNodes(names []string) []string {
	if s.coreNodes == nil {
		return nil
	}
	known := s.coreNodes()
	if len(known) == 0 {
		return nil
	}
	var bad []string
	for _, n := range names {
		if !known[n] {
			bad = append(bad, n)
		}
	}
	return bad
}

// livePositionsOfOtherProcesses maps each core_node_name held by a LIVE
// process_node row of a process other than excludeProcessID to that process's
// name (the first such row, in list order). A name absent from the map has no
// other holder — including the tombstone-only case, where re-adding under the
// same name is exactly the recovery. One read of the whole table, so a save
// checks every new name against it rather than reading once per name.
func (s *StationService) livePositionsOfOtherProcesses(excludeProcessID int64) (map[string]string, error) {
	nodes, err := s.db.ListProcessNodes()
	if err != nil {
		return nil, err
	}
	held := make(map[string]string, len(nodes))
	for _, n := range nodes {
		if n.ProcessID == excludeProcessID {
			continue
		}
		if _, seen := held[n.CoreNodeName]; !seen {
			held[n.CoreNodeName] = n.ProcessName
		}
	}
	return held, nil
}

// LoaderHolds reports whether a Core-owned loader's window/position set names
// coreNodeName — the shared-window exception to the one-process-per-slot rule.
// An unwired resolver or a clean miss is false: no loader, no exception.
// Exported for the node list the positions checklist reads, so the page greys
// out exactly the names SetNodes would refuse and no others.
func (s *StationService) LoaderHolds(coreNodeName string) bool {
	if s.loaders == nil || coreNodeName == "" {
		return false
	}
	l, err := s.loaders.LoaderForNode(domain.NodeID(coreNodeName))
	if err != nil || l == nil {
		return false
	}
	return l.Contains(domain.NodeID(coreNodeName))
}

// binTypeForPayload is the nil-safe read: "" is unknown, never a guess, and a
// served row with an unknown carrier says so (ServedRow.Carrier).
func (s *StationService) binTypeForPayload(payloadCode string) string {
	if s.binTypes == nil {
		return ""
	}
	return s.binTypes(payloadCode)
}

// SetStrandedResolver injects the parked-ticks alarm resolver (P2-C8) — the
// engine's StrandedAlarmDetail — so BuildView can render the tile chip. Optional;
// unset leaves StrandedAlarm empty for the lighter test constructors.
func (s *StationService) SetStrandedResolver(r func(coreNodeName string) string) { s.stranded = r }

// SetKeepStagedResolver injects the keep-staged keeper's sentence per line (the
// engine's KeepStagedNote) so BuildView can render its chip. Optional; unset
// leaves KeepStagedNote empty.
func (s *StationService) SetKeepStagedResolver(r func(coreNodeName string) string) { s.keepStaged = r }

// SetUnloaderAskResolver injects the pending U1 note per unloader window (the
// engine's UnloaderAskNote) so BuildView can put it on the window's card.
// Optional; unset leaves AskPending empty.
func (s *StationService) SetUnloaderAskResolver(r func(coreNodeName string) string) {
	s.unloaderAsk = r
}

// ── Cross-aggregate orchestrations ──────────────────────────────────

// SetNodes syncs process_nodes for a station to match the given core
// node names. Cross-aggregate: stations + processes + orders. Nodes
// with active orders are disabled rather than deleted to preserve
// referential integrity for downstream telemetry.
//
// kept names those disabled positions, in the station's order: the caller
// asked for them to go and they did not, so a save that reports only "ok"
// would tell the engineer something untrue. Empty when every removal retired.
//
// Phase 6.1 introduced this method as a thin delegate; Phase 6.4a
// moved the body in from the (now-deleted) outer
// store/station_nodes.go::SetStationNodes.
func (s *StationService) SetNodes(stationID int64, nodeNames []string) (kept []string, err error) {
	// EVERY EXIT PUBLISHES, INCLUDING THE FAILED ONES. This function writes
	// process_nodes row by row rather than in one transaction, so a refusal
	// half way through leaves rows already moved — and a board holding a cell
	// picture from before them would go on drawing the old shape with nothing
	// saying so. The over-invalidating direction is one extra picture fetch;
	// the other direction is a wrong picture. See processes.BumpNodeGeneration.
	defer processes.BumpNodeGeneration()

	station, err := s.db.GetOperatorStation(stationID)
	if err != nil {
		return nil, err
	}

	// Two different sets, deliberately.
	//
	// stationNodes — what THIS station currently owns. Only these may be detached
	// by the removal pass below; widening that pass to the whole process would rip
	// nodes out from under sibling stations.
	stationNodes, err := s.db.ListProcessNodesByStation(stationID)
	if err != nil {
		return nil, err
	}

	// byCoreName — every node under the PROCESS. The reuse-or-create decision has
	// to be process-global: a Core node already modelled in this process (orphaned,
	// or currently owned by a sibling station) must be ADOPTED, never re-created.
	// Making this decision from the station-local set is what let one Core node
	// spawn three process_nodes rows at HK (PLN_01 → ids 1, 13, 17): each rebind
	// that didn't already own the node minted a fresh row, and GenerateUniqueCode
	// suffixed the code (pln-01, pln-01-2, pln-01-3) to satisfy the only constraint
	// there was, UNIQUE(process_id, code). Every copy then carried its own runtime
	// row and drew its own copy of every PLC tick, because requestedClaimAtNode resolves
	// a claim by core_node_name rather than by node id — so all three matched.
	processNodes, err := s.db.ListProcessNodesByProcess(station.ProcessID)
	if err != nil {
		return nil, err
	}
	byCoreName := map[string]processes.Node{}
	for _, n := range processNodes {
		byCoreName[n.CoreNodeName] = n
	}

	clean, desired := s.wantedNames(nodeNames)

	// REFUSE THE WHOLE SAVE, rather than dropping the bad names and writing the
	// rest. This endpoint is a wholesale REPLACE of the station's node list, so a
	// partial write would report success for a list the caller never asked for —
	// the operator adds three nodes, two land, and nothing says which. That is the
	// same silent-partial-success shape this validation exists to remove.
	if bad := s.unknownCoreNodes(clean); len(bad) > 0 {
		return nil, fmt.Errorf("%w: %s — if it was just created on Core, sync nodes and try again",
			ErrUnknownCoreNodes, strings.Join(bad, ", "))
	}

	// ONE SLOT, ONE PROCESS — unless a loader aggregate holds the name. A name
	// that is already a live position of a DIFFERENT process must not be written
	// here as a second row: two rows resolving one physical slot means two tiles
	// counting one window and a UOP adjustment resolving by name with no right
	// answer. The exceptions, both real shapes:
	//
	//   - The name sits inside a Core-owned loader's window/position set (via the
	//     same LoaderResolver the view uses). A shared window IS legitimately
	//     named by several processes — that is what a shared loader is — and the
	//     loader aggregate is what makes the sharing coherent. A clean miss
	//     ((nil, nil)) or an unwired resolver is NOT the exception: it falls
	//     through to the refusal.
	//   - The existing row already belongs to this process (byCoreName is
	//     process-global), which is this station's own re-save and moves through
	//     the adopt-in-place path below.
	//
	// Checked BEFORE the first row is written, so the refusal is total like the
	// unknown-name one above.
	//
	// The other processes' positions are read once per save, and only when the
	// save adds a name this process does not already hold: a re-save of the
	// station's own list reads nothing more than it did before the check.
	var otherHolders map[string]string
	for _, name := range clean {
		if _, ours := byCoreName[name]; ours {
			continue
		}
		if otherHolders == nil {
			if otherHolders, err = s.livePositionsOfOtherProcesses(station.ProcessID); err != nil {
				return nil, err
			}
		}
		other, held := otherHolders[name]
		if !held {
			continue
		}
		if s.LoaderHolds(name) {
			continue
		}
		return nil, &otherProcessPositionError{name: name, process: other}
	}

	for i, name := range clean {
		if n, exists := byCoreName[name]; exists {
			// Adopt in place. UNIQUE(process_id, core_node_name) means a Core node
			// has exactly ONE row per process, so finding it on another station is a
			// MOVE, not a conflict — and it has to be a move, or we are back to
			// minting duplicates. Re-point it, re-sequence it, re-enable it.
			if n.OperatorStationID == nil {
				log.Printf("station: adopting orphaned process_node %d (%s) onto station %d", n.ID, name, stationID)
			} else if *n.OperatorStationID != stationID {
				log.Printf("station: moving process_node %d (%s) from station %d to station %d", n.ID, name, *n.OperatorStationID, stationID)
			}
			if _, err := s.db.Exec(`UPDATE process_nodes SET operator_station_id=?, sequence=?, enabled=1, updated_at=datetime('now')
				WHERE id=?`, stationID, i+1, n.ID); err != nil {
				return nil, err
			}
			if _, err := s.db.EnsureProcessNodeRuntime(n.ID); err != nil {
				return nil, err
			}
			continue
		}
		id, err := s.db.CreateProcessNode(processes.NodeInput{
			ProcessID:         station.ProcessID,
			OperatorStationID: &stationID,
			CoreNodeName:      name,
			Name:              name,
			Sequence:          i + 1,
			Enabled:           true,
		})
		if err != nil {
			return nil, err
		}
		if _, err := s.db.EnsureProcessNodeRuntime(id); err != nil {
			return nil, err
		}
	}

	// A POSITION IS NOT ALSO A ROUTING ROW. Positions stay in process_nodes and
	// the routing picture is the union of the two, so a name in both halves is
	// offered twice (processes.ErrRoutingNodeIsPosition refuses the routing
	// write; this is the other direction). The collision is born here, when a
	// name the set already holds becomes a position, so it is resolved here —
	// the boot-time backfill used to sweep it up after the fact (pick-a-part
	// unit A, 2026-10-18). Only names NEW to the process can collide: one that
	// was already a position could not have been written as a routing row
	// (processes.ErrRoutingNodeIsPosition), so a re-save issues nothing here.
	args := []any{station.ProcessID}
	for _, name := range clean {
		if _, ours := byCoreName[name]; !ours {
			args = append(args, name)
		}
	}
	if len(args) > 1 {
		if _, err := s.db.Exec(`DELETE FROM process_routing_nodes WHERE process_id = ? AND core_node_name IN (`+
			strings.TrimSuffix(strings.Repeat("?, ", len(args)-1), ", ")+`)`, args...); err != nil {
			return nil, err
		}
	}

	return s.releaseRemovedPositions(stationNodes, desired)
}

// wantedNames is how SetNodes reads a list: groups expanded, names trimmed,
// blanks and repeats dropped, order kept. RemovalImpact reads it the same way,
// so the warning is about the save the same list makes.
func (s *StationService) wantedNames(nodeNames []string) ([]string, map[string]bool) {
	clean := make([]string, 0, len(nodeNames))
	desired := map[string]bool{}
	for _, name := range s.expandGroups(nodeNames) {
		name = strings.TrimSpace(name)
		if name != "" && !desired[name] {
			desired[name] = true
			clean = append(clean, name)
		}
	}
	return clean, desired
}

// releaseRemovedPositions is SetNodes' removal pass: every position this
// station owned that the new list leaves out is retired, unless it has an
// active order — that one can only be disabled, because retiring it would
// orphan the order. Those kept positions are returned by name, in station
// order, so the save's reply can say which positions are still on the screen.
func (s *StationService) releaseRemovedPositions(stationNodes []processes.Node, desired map[string]bool) ([]string, error) {
	var kept []string
	for _, n := range stationNodes {
		if desired[n.CoreNodeName] {
			continue
		}
		active, err := s.db.ListActiveOrdersByProcessNode(n.ID)
		if err != nil {
			return nil, err
		}
		if len(active) > 0 {
			if _, err := s.db.Exec(`UPDATE process_nodes SET enabled=0, updated_at=datetime('now') WHERE id=?`, n.ID); err != nil {
				return nil, err
			}
			kept = append(kept, n.CoreNodeName)
			continue
		}
		if s.nodeRetirer != nil {
			if err := s.nodeRetirer(n.ID); err != nil {
				return nil, err
			}
		} else if err := s.db.DeleteProcessNode(n.ID); err != nil {
			return nil, err
		}
	}

	return kept, nil
}

// BuildView returns the operator station view used by the operator
// HMI. Cross-aggregate: stations + processes + claims + lineside.
//
// Phase 6.1 introduced this method as a thin delegate; Phase 6.4a
// moved the body in from the (now-deleted) outer
// store/station_views.go::BuildOperatorStationView. Its helpers
// (ComputeSwapReady, ReleasesAsPair) stay in store/ so the
// existing test file station_views_test.go can continue exercising
// them directly without import-cycle gymnastics.
// BuildView assembles the operator-station view.
//
// ctx is honoured at the tile-loop boundary so a build whose requester has gone
// away stops instead of running to completion. That matters more here than the
// usual "be a good citizen": Edge serialises every DB operation on one
// connection (store.Open sets SetMaxOpenConns(1)), and a build's reads all queue on it,
// so an abandoned 22-tile build holds the connection against work someone is
// still waiting on. Before this, a browser abort freed only the browser — the
// handler took no context, so each timed-out poll ADDED an orphan build and
// removed none, which is how Springfield's bin-loader board wound from 3.1s to
// 116s over a day of uptime.
//
// Cancellation is checked per TILE, not per query: it needs no ctx plumbing
// through the 300+ non-context store calls and still bounds the wasted work to
// a single tile.
func (s *StationService) BuildView(ctx context.Context, stationID int64) (*store.OperatorStationView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	view, err := s.newStationView(stationID)
	if err != nil {
		return nil, err
	}
	process := &view.Process

	nodes, nodeTaskMap, childOf, err := s.adoptChangeoverParticipants(stationID, view.ActiveChangeover)
	if err != nil {
		return nil, err
	}
	b := s.prefetchBoardData(process, nodes, nodeTaskMap, view.ActiveChangeover)
	// The board's own node rows, for the curtain points: a paired position
	// that is also on this board is read from here rather than from the
	// plant-wide set.
	stationNodes := make(map[curtainKey]processes.Node, len(nodes))
	for _, n := range nodes {
		stationNodes[curtainKey{n.ProcessID, n.CoreNodeName}] = n
	}
	for _, node := range nodes {
		// See the BuildView doc comment: one abandoned tile-loop iteration is the
		// granularity at which we give the single DB connection back.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tile := s.buildNodeTile(node, process, b, childOf, nodeTaskMap)
		tile.CurtainPoints = s.curtainPoints(node, b, stationNodes)
		view.Nodes = append(view.Nodes, tile)
	}

	view.Serves = s.servedRows(view.Nodes)

	return view, nil
}

// curtainKey names a process_node by what a claim's pairing names it by: the
// core node, within the claim's process.
type curtainKey struct {
	processID int64
	coreNode  string
}

// curtainPoints resolves the curtained nodes a tile's release sends a robot
// into: the tile's own node, and the paired and second-paired positions of
// its configured claim - the process's active style first, the target style
// when the active one has no claim here (store.ActiveStyleFirst, the
// resolution the release verbs use). Nil when none of them is curtained.
//
// THE BIN DICTATES, render half: the tile's release lifts the bin its claim
// filled, so the claim's OWN flag decides whether the tile is gated at all -
// a style that does not require the curtain renders UNGATED (no points, so
// CurtainOK stays nil and the button stays normal) whatever the node's
// curtain says, and a tile with no claim at all is likewise ungated, exactly
// as the gate reads it (a resident bin with no claim gates nothing). The
// points are only resolved for a claim that requires the curtain.
//
// NO QUERY OF ITS OWN ON AN ORDINARY POLL. The tile's node and any paired
// position on the same board come from rows the build already read. A
// paired position off the board - a press-index back position has no
// station - comes from curtainedNodes, which re-reads only when
// process_nodes has been written since it last did.
func (s *StationService) curtainPoints(node processes.Node, b *boardData, board map[curtainKey]processes.Node) []domain.CurtainPoint {
	var out []domain.CurtainPoint
	claim, ok := b.activeClaims[node.CoreNodeName]
	if !ok {
		claim, ok = b.targetClaims[node.CoreNodeName]
	}
	if !ok || !claim.CurtainRequired {
		return nil
	}
	add := func(n processes.Node) {
		if n.CurtainEnabled {
			out = append(out, domain.CurtainPoint{
				Node: n.Name, PLCName: n.CurtainPLCName, TagName: n.CurtainTagName, SafeValue: n.CurtainSafeValue,
			})
		}
	}
	add(node)
	if node.CoreNodeName == "" {
		return out
	}
	seen := map[string]bool{node.CoreNodeName: true}
	for _, name := range claim.ExtensionPositions() {
		if seen[name] {
			continue
		}
		seen[name] = true
		key := curtainKey{node.ProcessID, name}
		if n, onBoard := board[key]; onBoard {
			add(n)
			continue
		}
		if n, curtained := s.curtainedNodes()[key]; curtained {
			add(n)
		}
	}
	return out
}

// curtainedNodes is every live node with the interlock on, held between
// polls and re-read when processes.NodeGeneration moves - every writer of
// process_nodes bumps it, the curtain setter included, and
// node_generation_drift_test.go holds that list. The station poll runs every
// 500 ms per board on one SQLite connection, so a read here per poll would be
// a read multiplied by every board for a setting an engineer changes a few
// times a year. The generation is taken BEFORE the read, so a write racing it
// leaves the held set marked stale and the next poll reads again.
//
// A failed read is not held, and answers with no off-board curtained nodes
// for this poll: the render is advisory, and the gate reads the node itself
// at the click.
func (s *StationService) curtainedNodes() map[curtainKey]processes.Node {
	s.curtainMu.Lock()
	defer s.curtainMu.Unlock()
	gen := processes.NodeGeneration()
	if s.curtainLoaded && s.curtainedGen == gen {
		return s.curtained
	}
	rows, err := s.db.ListCurtainedProcessNodes()
	if err != nil {
		log.Printf("station view: read curtained nodes: %v", err)
		return nil
	}
	m := make(map[curtainKey]processes.Node, len(rows))
	for _, n := range rows {
		m[curtainKey{n.ProcessID, n.CoreNodeName}] = n
	}
	s.curtained, s.curtainedGen, s.curtainLoaded = m, gen, true
	return m
}

// CellPictureForStation builds one station's cell picture. THE FETCH, made by
// the page when the version on its last poll stopped matching the version on
// the picture it holds — not the poll, which carries the version alone.
//
// WHAT THE POLL USED TO PAY. This is two queries and a deep copy of the
// plant's NGRP map, and it rode every station view: at 500 ms a board while
// events flow, on a Pi with one SQLite connection, for a drawing that changes
// when an engineer edits a cell. It is now read when it changes.
//
// ONE CLAIMS READ FOR BOTH HALVES. The running style's claims (what the
// picture draws) and the process's back positions (what an unused card is
// captioned) are two questions over the same rows, and this reads them once —
// where the old shape read style_node_claims twice, once through
// ListBackPositionNames's own JOIN.
//
// Fail-open, like every other enrichment: a failed read draws less of the
// picture, never no picture.
func (s *StationService) CellPictureForStation(stationID int64) (*domain.CellPicture, error) {
	station, err := s.db.GetOperatorStation(stationID)
	if err != nil {
		return nil, err
	}
	if station == nil {
		return nil, nil
	}
	process, err := s.db.GetProcess(station.ProcessID)
	if err != nil {
		return nil, err
	}
	if process == nil {
		return nil, nil
	}
	live := s.liveClaims(process.ID)
	active := map[string]domain.NodeClaim{}
	if process.ActiveStyleID != nil {
		for _, c := range live {
			if c.StyleID == *process.ActiveStyleID {
				active[c.CoreNodeName] = c
			}
		}
	}
	// THE BOARD'S PICTURE DRAWS WHAT THE RUNNING FLOW USES, and the composer's
	// draws what the cell may use (§9.1 / R4). Neither carries LM coordinates:
	// the route strip is drawn from the names on the claim, so there is nothing
	// geographic for either read to fetch.
	return s.cellPicture(stationID, process, active, live, nil, cellPictureOptions{}), nil
}

// cellPictureOptions is what differs between the two pictures of one cell.
// Everything else — the positions, the claims, the join rule — is the same
// builder, deliberately: the day they came out of two builders is the day an
// operator and an engineer saw two different cells.
type cellPictureOptions struct {
	// stagingOffers is the process's enabled staging-role routing nodes, on the
	// composer's picture and not the board's.
	stagingOffers []string
}

// cellPicture is the builder both scopes go through: the station's fetch above
// and the desktop composer's process-wide read (processCellPicture). `nodes`
// is the process's node list when the caller already has it — the desktop read
// does, and reading it twice is what P2 removes — and nil to read it here.
func (s *StationService) cellPicture(stationID int64, process *processes.Process,
	active map[string]domain.NodeClaim, live []processes.NodeClaim, nodes []processes.Node,
	opts cellPictureOptions) *domain.CellPicture {

	if nodes == nil {
		var err error
		nodes, err = s.db.ListProcessNodesByProcess(process.ID)
		if err != nil {
			log.Printf("cell picture: list process nodes for %d: %v", process.ID, err)
		}
	}
	in := domain.CellPictureInput{
		StationID: stationID, Nodes: nodes, Claims: active,
		// DERIVED, NOT QUERIED. The removed store.ListBackPositionNames read the same rows
		// this caller already has, with its own JOIN. See
		// domain.BackPositionNames.
		BackPositions: domain.BackPositionNames(live),
		StagingOffers: opts.stagingOffers,
	}
	var activeStyleID int64
	if process.ActiveStyleID != nil {
		activeStyleID = *process.ActiveStyleID
	}
	if s.sceneGeometry != nil {
		in.Geometry = s.sceneGeometry()
	}
	if s.coreNodeGroups != nil {
		in.Groups = s.coreNodeGroups()
	}
	// THE PICTURE CARRIES ITS OWN VERSION, built from the same facts the poll
	// builds it from — so a picture assembled from rows NEWER than the version
	// that sent the page here comes back stamped with the newer one, and the
	// next poll matches instead of asking again.
	in.Version = domain.CellPictureVersion(domain.CellPictureVersionInput{
		StationID: stationID, ProcessID: process.ID, ActiveStyleID: activeStyleID,
		NodeGeneration: processes.NodeGeneration(), PlantGeneration: s.plantGen(),
		Claims: live,
	})
	return domain.BuildCellPicture(in)
}

// liveClaims is every live claim of a process as one flat list — the shape the
// picture's two derivations want. liveClaimsByStyle's map is the picker's.
func (s *StationService) liveClaims(processID int64) []processes.NodeClaim {
	rows, err := processes.ListLiveClaimsByProcess(s.db.DB, processID)
	if err != nil {
		log.Printf("cell picture: list live claims for process %d: %v", processID, err)
		return nil
	}
	return rows
}

// ── Per-station CRUD ────────────────────────────────────────────────

// List returns every operator_stations row.
func (s *StationService) List() ([]stations.Station, error) {
	return s.db.ListOperatorStations()
}

// ListByProcess returns operator_stations rows for one process.
func (s *StationService) ListByProcess(processID int64) ([]stations.Station, error) {
	return s.db.ListOperatorStationsByProcess(processID)
}

// Get returns one operator_station by id.
func (s *StationService) Get(id int64) (*stations.Station, error) {
	return s.db.GetOperatorStation(id)
}

// Create inserts a station, generating code and sequence when not
// supplied.
func (s *StationService) Create(in stations.Input) (int64, error) {
	return s.db.CreateOperatorStation(in)
}

// Update modifies an operator_station.
func (s *StationService) Update(id int64, in stations.Input) error {
	return s.db.UpdateOperatorStation(id, in)
}

// Delete removes an operator_station.
func (s *StationService) Delete(id int64) error {
	return s.db.DeleteOperatorStation(id)
}

// Touch updates last_seen_at and health_status.
//
// Called on every connect and heartbeat of a station's event stream
// (www EventHub.HandleSSE, ?station=); it used to be called on every view
// request, when the boards polled. Throttled because it is a WRITE on the
// single connection store.Open allows, so it queues ahead of the reads the
// boards are waiting on, for a column nothing behaves on.
//
// A status CHANGE always writes through immediately; only the repeated
// same-status refresh is collapsed. With a 30 s heartbeat every touch is past
// the throttle, so a connected screen writes once per heartbeat.
func (s *StationService) Touch(id int64, healthStatus string) error {
	s.touchMu.Lock()
	if s.touched == nil {
		s.touched = make(map[int64]touchState)
	}
	prev, seen := s.touched[id]
	if seen && prev.status == healthStatus && time.Since(prev.at) < touchThrottle {
		s.touchMu.Unlock()
		return nil
	}
	// Record before the write so a slow or failing write cannot turn into a
	// tight retry loop on the hot path.
	s.touched[id] = touchState{status: healthStatus, at: time.Now()}
	s.touchMu.Unlock()
	return s.db.TouchOperatorStation(id, healthStatus)
}

// Move swaps the sequence of a station with its neighbour in the
// given direction ("up" or "down").
func (s *StationService) Move(id int64, direction string) error {
	return s.db.MoveOperatorStation(id, direction)
}

// GetNodeNames returns the core_node_name list for a station.
func (s *StationService) GetNodeNames(stationID int64) ([]string, error) {
	return s.db.GetStationNodeNames(stationID)
}

// newStationView loads the station and its process and fills the board header:
// the current and target styles, the style list, and the active changeover with
// this station's task on it. The style and changeover reads are best-effort --
// a board that renders without a style chip beats no board.
func (s *StationService) newStationView(stationID int64) (*store.OperatorStationView, error) {
	station, err := s.db.GetOperatorStation(stationID)
	if err != nil {
		return nil, err
	}
	process, err := s.db.GetProcess(station.ProcessID)
	if err != nil {
		return nil, err
	}

	view := &store.OperatorStationView{
		Station: *station,
		Process: *process,
	}
	if process.ActiveStyleID != nil {
		if st, err := s.db.GetStyle(*process.ActiveStyleID); err == nil {
			view.CurrentStyle = st
		}
	}
	if process.TargetStyleID != nil {
		if st, err := s.db.GetStyle(*process.TargetStyleID); err == nil {
			view.TargetStyle = st
		}
	}
	view.AvailableStyles, _ = s.db.ListStylesByProcess(process.ID)
	// THE PICKER'S SHAPE, NOT THE COMPOSER'S (S8). The board polls this at
	// 500 ms while events flow and reads the composer half of it once, on a
	// tap; everything past the picker rows is fetched by
	// GET /api/operator-stations/{id}/composer when the composer opens.
	//
	// AvailableStyles is passed in rather than re-read: the line above just
	// listed the same styles.
	claimsByStyle := s.liveClaimsByStyle(process.ID)
	// nil for the node list: the picker scope returns before anything could
	// call it, and handing the poll a closure that CAN read process_nodes is
	// how a node-list query finds its way back onto it.
	view.Composer = s.buildComposerData(process.ID, view.AvailableStyles, claimsByStyle, composerPicker, nil)
	// THE CELL PICTURE'S VERSION, AND NOT THE PICTURE (owner ruling 4,
	// 2026-09-17). The picture cost this poll two queries and a deep copy of
	// the plant's NGRP map, every 500 ms, per board, for a drawing that changes
	// when an engineer edits a cell. The page fetches it from
	// GET /api/operator-stations/{id}/cell when this string stops matching the
	// one on the picture it holds.
	//
	// ZERO NEW QUERIES: every input is already in hand. The claims are the ones
	// the picker block was just built from — flattened here rather than re-read
	// — and the two generations are in-memory counters published by the writers
	// that change what they stand for.
	var activeStyleID int64
	if process.ActiveStyleID != nil {
		activeStyleID = *process.ActiveStyleID
	}
	flat := make([]processes.NodeClaim, 0, 32)
	for _, rows := range claimsByStyle {
		flat = append(flat, rows...)
	}
	view.CellVersion = domain.CellPictureVersion(domain.CellPictureVersionInput{
		StationID: stationID, ProcessID: process.ID, ActiveStyleID: activeStyleID,
		NodeGeneration: processes.NodeGeneration(), PlantGeneration: s.plantGen(),
		Claims: flat,
	})
	if co, err := s.db.GetActiveProcessChangeover(process.ID); err == nil {
		view.ActiveChangeover = co
		if stationTask, err := s.db.GetChangeoverStationTaskByStation(co.ID, stationID); err == nil {
			view.StationTask = stationTask
		}
	}
	return view, nil
}

// boardData is everything the tile loop needs that is read ONCE for the whole
// board rather than per tile.
//
// A struct rather than eleven parameters because that is what it already was:
// every field here started life as a query inside the tile loop, and the
// comments in prefetchBoardData are the record of hoisting them out. Naming the
// group states the invariant -- one read per board, never per tile -- instead of
// leaving it a convention eleven separate locals have to keep.
type boardData struct {
	activeClaims        map[string]processes.NodeClaim
	targetClaims        map[string]processes.NodeClaim
	pressPositionClaims map[int64]*processes.NodeClaim
	runtimes            map[int64]*processes.RuntimeState
	boardOrders         map[int64][]orders.Order
	refusals            map[string]map[string]domain.SupplyRefusal
	byPayload           map[string]domain.CellSupplyRefusal
	activeBuckets       map[int64][]lineside.Bucket
	strandedBuckets     map[int64][]lineside.Bucket
	// outsides is, per tile that is a fed produce press, its outside
	// position's process node (SEND BACK's orders are created there). The
	// board-wide order read covers them.
	outsides map[int64]processes.Node
	// changeover is the process's active changeover, the view's own
	// (newStationView's read), nil when none: SEND BACK's "a changeover is
	// armed", as its route asks it.
	changeover *processes.Changeover
}

// prefetchBoardData performs every board-wide read the tile loop depends on.
// Each is fail-open on its own terms: a failed enrichment renders a board
// missing that detail, never no board. changeover is the view's active
// changeover, already read, carried for the tiles.
func (s *StationService) prefetchBoardData(
	process *processes.Process, nodes []processes.Node, nodeTaskMap map[int64]processes.NodeTask,
	changeover *processes.Changeover,
) *boardData {
	// Per-tile lookups hoisted to one query each for the whole board: every
	// read serialises on one connection, so a query inside the tile loop is a
	// query multiplied by the tile count (22 on the Springfield bin loader).
	//
	// Claims are safe to index by core node name because style_node_claims
	// declares UNIQUE(style_id, core_node_name) — at most one claim per
	// (style, node), so a map lookup is exactly what GetStyleNodeClaimByNode
	// returns. Both maps fail open to nil: a nil map lookup yields the zero
	// value, and the tile then reads as "no claim", which is the same thing
	// GetStyleNodeClaimByNode's sql.ErrNoRows produced.
	var activeClaims, targetClaims map[string]processes.NodeClaim
	if process.ActiveStyleID != nil {
		activeClaims, _ = claimsByCoreNode(s.db.DB, *process.ActiveStyleID)
	}
	if process.TargetStyleID != nil {
		targetClaims, _ = claimsByCoreNode(s.db.DB, *process.TargetStyleID)
	}
	// Press-index fan-out claims, computed ONCE for the whole board for the same
	// reason as runtimes/boardOrders above — never a query inside
	// the tile loop. Empty (and free) on any board without an active changeover.
	pressPositionClaims := pressPositionClaimsForBoard(s.db.DB, nodes, nodeTaskMap, activeClaims, targetClaims)
	nodeIDs := make([]int64, 0, len(nodes))
	nodeKeys := make([]orders.NodeKey, 0, len(nodes))
	for _, node := range nodes {
		nodeIDs = append(nodeIDs, node.ID)
		nodeKeys = append(nodeKeys, orders.NodeKey{ProcessNodeID: node.ID, CoreNodeName: node.CoreNodeName})
	}
	// SEND BACK's orders live on each fed produce press's outside position's
	// own process node. The ONE board-wide order read below widens to those
	// nodes (sendBackOutsides, from the held node rows), so the line costs
	// the board no statement.
	outsides := s.sendBackOutsides(nodes, activeClaims)
	for _, on := range outsides {
		nodeKeys = append(nodeKeys, orders.NodeKey{ProcessNodeID: on.ID, CoreNodeName: on.CoreNodeName})
	}
	// Runtime rows: one SELECT for the board. This is the READ half of
	// EnsureProcessNodeRuntime — the tile loop still calls Ensure for any node
	// missing from this map, so the INSERT happens for exactly the same set of
	// nodes as before. On a live plant every node already has its row, so the
	// per-tile query disappears and the write path is untouched.
	runtimes, err := processes.RuntimesForNodes(s.db.DB, nodeIDs)
	if err != nil {
		runtimes = nil // fall back to per-node Ensure below
	}
	boardOrders, err := orders.ListActiveByNodeKeys(s.db.DB, nodeKeys)
	if err != nil {
		boardOrders = nil
	}
	// Standing supply refusals, ONE READ FOR THE WHOLE BOARD, indexed
	// loader_node → payload → refusal. The table holds only what is open — one
	// row per card actually refused right now — so the whole-table read is
	// cheaper than a query per card, and this is the board's poll path, which is
	// the one place that difference is felt. Best-effort, like every other
	// enrichment here: a failed read renders a board without refusal state
	// rather than no board.
	refusals := map[string]map[string]domain.SupplyRefusal{}
	// byPayload is the CUSTOMER's index of the same rows. A cell does not know
	// which window supplies it — that resolution is the thing the broadcast
	// design deliberately avoids needing — so it looks up by part alone and the
	// sentence names the window that said it.
	byPayload := map[string]domain.CellSupplyRefusal{}
	if open, rerr := s.db.ListOpenSupplyRefusals(); rerr == nil {
		for _, r := range open {
			if refusals[r.LoaderNode] == nil {
				refusals[r.LoaderNode] = map[string]domain.SupplyRefusal{}
			}
			refusals[r.LoaderNode][r.PayloadCode] = domain.SupplyRefusal{
				RefusedAt: r.RefusedAt,
				RefusedBy: r.RefusedBy,
				Answered:  r.Answered(),
				AckChoice: r.AckChoice,
			}
			// An UNANSWERED refusal wins over an answered one for the same part:
			// it is the one that still needs a person, and it is what the modal
			// fires on.
			if prev, seen := byPayload[r.PayloadCode]; seen && !prev.Answered {
				continue
			}
			byPayload[r.PayloadCode] = domain.CellSupplyRefusal{
				LoaderNode: r.LoaderNode, PayloadCode: r.PayloadCode,
				RefusedAt: r.RefusedAt, RefusedBy: r.RefusedBy,
				Answered: r.Answered(), AckChoice: r.AckChoice,
			}
		}
	}
	activeBuckets, err := lineside.ListActiveForNodes(s.db.DB, nodeIDs)
	if err != nil {
		activeBuckets = nil // best-effort, as the per-node call was
	}
	strandedBuckets, err := lineside.ListStrandedForNodes(s.db.DB, nodeIDs)
	if err != nil {
		strandedBuckets = nil
	}
	return &boardData{
		activeClaims:        activeClaims,
		targetClaims:        targetClaims,
		pressPositionClaims: pressPositionClaims,
		runtimes:            runtimes,
		boardOrders:         boardOrders,
		refusals:            refusals,
		byPayload:           byPayload,
		activeBuckets:       activeBuckets,
		strandedBuckets:     strandedBuckets,
		outsides:            outsides,
		changeover:          changeover,
	}
}

// adoptChangeoverParticipants returns the tiles this station's board shows,
// which is NOT simply the nodes assigned to it: a press-index extension position is
// auto-created with no operator_station_id, so it belongs on the board of the
// press it extends. It also returns the per-node task map and, for an adopted
// position, the name of the node it hangs under.
func (s *StationService) adoptChangeoverParticipants(
	stationID int64, changeover *processes.Changeover,
) ([]processes.Node, map[int64]processes.NodeTask, map[int64]string, error) {
	nodes, err := s.db.ListProcessNodesByStation(stationID)
	if err != nil {
		return nil, nil, nil, err
	}
	nodeTaskMap := map[int64]processes.NodeTask{}
	childOf := map[int64]string{}

	// THREE narrowings used to hide a changeover node from its own station, and
	// all three are relaxed here. A press-index extension position is auto-created
	// with no operator_station_id (changeover_service.go inserts only
	// process_id/core_node_name/code/name), so it fell through every one:
	//
	//  1. `if view.StationTask != nil` — a station with no
	//     changeover_station_tasks row got an EMPTY task map, so none of its
	//     nodes showed a task even when tasks existed. The real precondition is
	//     the CHANGEOVER existing, not this station having a row; gate on that.
	//  2. ListChangeoverNodeTasksByStation filters `n.operator_station_id=?`,
	//     which drops every task whose node has no station. Read the tasks
	//     unfiltered and key them by process_node_id — safe, because the map is
	//     only ever consulted for nodes in THIS station's list, so another
	//     station's task can never match.
	//  3. ListProcessNodesByStation likewise never returns a stationless node.
	//     Resolve each participant's station (own → owning task's node) and
	//     append the ones that belong here as CHILD tiles of the node they
	//     extend.
	if changeover != nil {
		allTasks, _ := s.db.ListChangeoverNodeTasks(changeover.ID)
		taskByID := make(map[int64]processes.NodeTask, len(allTasks))
		for _, nodeTask := range allTasks {
			nodeTaskMap[nodeTask.ProcessNodeID] = nodeTask
			taskByID[nodeTask.ID] = nodeTask
		}

		known := make(map[int64]bool, len(nodes))
		for i := range nodes {
			known[nodes[i].ID] = true
		}
		// Does THIS board own any of the changeover's work? Computed from the
		// station's own nodes, before any adoption, so it answers "is this
		// changeover being run from here" rather than "did we adopt something".
		// It is the anchor for the orphan fallback below.
		stationRunsChangeover := false
		for i := range nodes {
			if _, ok := nodeTaskMap[nodes[i].ID]; ok {
				stationRunsChangeover = true
				break
			}
		}
		parts, perr := s.db.ListParticipantsWithStation(changeover.ID)
		if perr != nil {
			log.Printf("station view: resolve participant stations for changeover %d: %v", changeover.ID, perr)
		}
		for _, p := range parts {
			if p.ProcessNodeID == nil || known[*p.ProcessNodeID] {
				continue
			}
			// Two ways a participant belongs on this board:
			//
			//  - OWNER: it has no station of its own but the task that owns it
			//    does, and that station is us. The press-index case — PLN_02 is
			//    an `indexed_over` position of PLN_01's task, so it rides along.
			//
			//  - ORPHAN: it resolves to NO station at all. That happens when a
			//    changeover FANS OUT and gives the position its OWN task: station
			//    resolution walks own -> owning-task's-node, and for a
			//    self-owning task both are the same stationless row, so it
			//    lands nil and the position renders NOWHERE. Hopkinsville
			//    2026-07-28: a tote->bin changeover dropped all four press
			//    positions independently, PLN_02/PLN_05 vanished from the
			//    board, and the two robots parked at them could not be
			//    released — there was no tile to press. Adopt onto the board
			//    already running this changeover.
			//
			// Adoption stays inside the ActiveChangeover guard, so these positions
			// appear only while they have work and disappear afterwards. That
			// matters: a paired on-deck position must NOT be a permanent tile —
			// LoadBin refuses to stamp a part there precisely because doing so
			// hung a press-index swap once already.
			byOwner := p.StationID != nil && *p.StationID == stationID && p.StationSource == "owner"
			orphan := p.StationID == nil && stationRunsChangeover
			if !byOwner && !orphan {
				continue
			}
			child, gerr := s.db.GetProcessNode(*p.ProcessNodeID)
			if gerr != nil || child == nil {
				continue
			}
			// Render as a child of the node whose task owns it — but never of
			// itself. A fanned-out position owns its own task, so naming it its own
			// parent would be meaningless; it stands as its own tile instead.
			if p.OwningTaskID != nil {
				if owner, ok := taskByID[*p.OwningTaskID]; ok && owner.ProcessNodeID != child.ID {
					childOf[child.ID] = owner.NodeName
				}
			}
			nodes = append(nodes, *child)
			known[child.ID] = true
		}
	}
	return nodes, nodeTaskMap, childOf, nil
}

// buildNodeTile assembles one tile: its claims, runtime, orders, swap-readiness,
// lineside buckets, release error, stranded chip, refusals, and -- for a
// loader window or position -- the loader as the board reads it.
//
// Every board-wide read it needs is already on b. It issues no query of its own,
// which is the property the whole prefetch exists to preserve: a query here is a
// query multiplied by the tile count, on one serialised connection.
func (s *StationService) buildNodeTile(
	node processes.Node, process *processes.Process, b *boardData,
	childOf map[int64]string, nodeTaskMap map[int64]processes.NodeTask,
) store.StationNodeView {
	nodeView := store.StationNodeView{Node: node}
	runtime := b.runtimes[node.ID]
	if runtime == nil {
		// No row yet (a freshly created node), or the batch read failed.
		// Ensure materialises it, exactly as the per-tile call always did.
		runtime, _ = s.db.EnsureProcessNodeRuntime(node.ID)
	}
	nodeView.Runtime = runtime
	// Core's loader first: a window or position of a Core loader reads as that
	// loader's synthesized manual_swap claim, whatever stored claim also sits at
	// the node, through the SAME resolver the runtime uses (engine claimAtNode),
	// so the view and the engine never disagree, and the tile carries the
	// loader's facts as the board reads them (Loader). A node no Core loader
	// names resolves to nil (clean miss) and takes its stored active claim. Pinned by
	// TestLoaderPrecedence_CoreLoaderWinsOverAStoredClaim (engine).
	if s.loaders != nil && node.CoreNodeName != "" {
		if l, lerr := s.loaders.LoaderForNode(domain.NodeID(node.CoreNodeName)); lerr == nil && l != nil {
			nodeView.ActiveClaim = l.SynthClaim(domain.NodeID(node.CoreNodeName))
			nodeView.Loader = s.tileLoader(l, node.CoreNodeName)
		}
	}
	if nodeView.ActiveClaim == nil && process.ActiveStyleID != nil && node.CoreNodeName != "" {
		if c, ok := b.activeClaims[node.CoreNodeName]; ok {
			claim := c
			nodeView.ActiveClaim = &claim
		}
	}
	if process.TargetStyleID != nil && node.CoreNodeName != "" {
		if c, ok := b.targetClaims[node.CoreNodeName]; ok {
			claim := c
			nodeView.TargetClaim = &claim
		}
	}
	// Press-index fan-out fallback — the same move as the loader fallback
	// above, for the other node kind that owns work but no claim row. When a
	// changeover FANS OUT (different bin types across the index, e.g. tote →
	// bin) each press position gets its OWN task and order instead of riding
	// along on the front position's. Those positions have no style_node_claims
	// row — press_position claims are in-memory only — so the view saw a
	// claimless node and every claim-keyed gate failed closed.
	//
	// Hopkinsville 2026-08-05, P400 changeover 51: PLN_02/PLN_05 each held a
	// robot at a staged wait needing a RELEASE. isReleaseReady keys off the
	// TASK and lit both tiles release-ready; the modal keys off the CLAIM and
	// rendered no buttons at all. 19 minutes later the operator cancelled both
	// orders to free the robots and hand-drove the bins out. Deriving the
	// claim the planner already built closes that split for every claim-keyed
	// gate at once, not just the one that surfaced.
	if nodeView.ActiveClaim == nil {
		nodeView.ActiveClaim = b.pressPositionClaims[node.ID]
	}
	if nodeTask, ok := nodeTaskMap[node.ID]; ok {
		taskCopy := nodeTask
		nodeView.ChangeoverTask = &taskCopy
	}
	// Child tile: rendered here only because the node it extends lives on
	// this station. Marked so the board can suppress the release button —
	// it owns no task and no order, so there is nothing to release.
	nodeView.ChildOfNode = childOf[node.ID]
	// Include orders sourcing FROM this node's CoreNode in addition to
	// orders tracked at this process_node. A manual_swap supermarket
	// loader (SMN_001 etc.) doesn't directly own orders — the line
	// operator's REQUEST creates orders tracked at the line node. But
	// the loader operator still needs to see "demand for my bin" so
	// they keep it loaded. Plant test 2026-04-27: line-initiated swap
	// orders went silent on the loader UI after the kanban-spam guard
	// stopped firing process-node-tracked orders here.
	nodeView.Orders = b.boardOrders[node.ID]
	nodeView.CanSkip = canSkipChangeoverStep(nodeView.ChangeoverTask, nodeView.Orders)
	nodeView.SwapReady = store.ComputeSwapReady(s.db, nodeView.ActiveClaim, runtime, nodeView.ChangeoverTask)
	nodeView.ReleasesAsPair = store.ReleasesAsPair(nodeView.ActiveClaim)
	// Lineside piles power the active-bar and count-anomaly chip UI on
	// the operator station modal. Best-effort — absence of buckets
	// just means the node has nothing pulled to lineside yet.
	nodeView.LinesideActive = b.activeBuckets[node.ID]
	nodeView.LinesideStranded = b.strandedBuckets[node.ID]
	// The chip and the buttons: from the node's live orders, which the
	// board's prefetch already holds (release_render.go).
	nodeView.LastReleaseError = releaseHeld(nodeView.Orders)
	nodeView.ReleasePurposes = releasePurposes(nodeView.Orders)
	// Surface any active parked-ticks alarm (P2-C7/C8): consume ticks piling
	// up on this node while no bin is bound. Rendered as an amber chip.
	if s.stranded != nil {
		nodeView.StrandedAlarm = s.stranded(node.CoreNodeName)
	}
	if s.keepStaged != nil {
		nodeView.KeepStagedNote = s.keepStaged(node.CoreNodeName)
	}
	if s.unloaderAsk != nil {
		nodeView.AskPending = s.unloaderAsk(node.CoreNodeName)
	}
	nodeView.CountWaits = countWaits(node.CoreNodeName, nodeView.ActiveClaim, runtime)
	// THE CUSTOMER'S HALF. A call and the part: this node has an outstanding
	// order for something a loader operator has said they cannot supply.
	//
	// THE SUPPLIER IS NOT A CUSTOMER OF ITSELF. This used to attach the
	// refusal wherever the payload matched, and the comment here called that
	// "the same test either way". It is not. A loader window's own orders ARE
	// the calls it is being asked to fill, so the window that just refused a
	// part always matched its own refusal — and the operator who had said
	// "no parts" was immediately asked to choose whether to wait for them or
	// change over. Reported from Springfield the first time it was pressed.
	//
	// Keyed on the core node name because that is what the refusal stores and
	// what a shared loader is identified by across its process nodes.
	if len(b.byPayload) > 0 {
		for i := range nodeView.Orders {
			o := nodeView.Orders[i]
			if o.PayloadCode == "" || protocol.IsTerminal(o.Status) {
				continue
			}
			r, ok := b.byPayload[o.PayloadCode]
			if !ok || r.LoaderNode == node.CoreNodeName {
				continue
			}
			rc := r
			nodeView.SupplyRefusedForMe = &rc
			break
		}
	}
	applyLoaderRefusals(&nodeView, node, b)
	nodeView.SendBackFacts = sendBackFacts(node, nodeView.ActiveClaim, runtime, b)
	return nodeView
}

// applyLoaderRefusals adds a loader window's standing refusals, from the one
// snapshot taken for the board. Role-agnostic on purpose: an unloader states
// the same kind of thing about empties that a loader states about parts, and
// only the WORDING differs, which lives on the render side.
func applyLoaderRefusals(nodeView *store.StationNodeView, node processes.Node, b *boardData) {
	if !nodeView.ActiveClaim.IsLoaderNode() {
		return
	}
	if forThisWindow := b.refusals[node.CoreNodeName]; len(forThisWindow) > 0 {
		nodeView.SupplyRefusals = forThisWindow
	}
}

// tileLoader is the board's reading of the Core loader at a node: what the
// station's JSON carries in place of the synthesised claim. Resolved by node,
// not by a claim's role, through the SAME resolver the runtime uses, so the
// board and the engine never disagree about which loader a window belongs to.
//
// Parts are scoped to THIS node, the same per-node set the load and request
// gates use: a dedicated home shows only its own pinned part, a buffer none.
func (s *StationService) tileLoader(l *domain.Loader, coreNode string) *domain.TileLoader {
	codes := l.LoadablePayloadCodesAt(domain.NodeID(coreNode))
	parts := make([]domain.TileLoaderPart, 0, len(codes))
	for _, c := range codes {
		parts = append(parts, domain.TileLoaderPart{Code: c})
	}
	tl := &domain.TileLoader{
		Role:               string(l.Role()),
		Layout:             string(l.Layout()),
		Replenishment:      string(l.Replenishment()),
		OutboundDest:       l.OutboundDest(),
		Parts:              parts,
		LeavesBare:         l.LeavesBare(),
		PulledDirectly:     l.PulledDirectly(),
		BoardShowsAllParts: l.BoardShowsAllParts(),
	}
	if l.Role() == domain.RoleConsume && s.carrierCatalog != nil {
		tl.Carriers = carriersFor(s.carrierCatalog(), codes)
	}
	return tl
}

// carriersFor is the bin types an unloader's CLEAR panels offer for a node's
// parts: the catalog's types for those parts, in catalog order, each once;
// every type in the catalog when the parts match none (or there are no parts),
// so an operator is never left in front of an empty menu on a node nobody has
// finished configuring. The rule the board's panels applied to the plant-wide
// catalog before they read this list. Pinned by TestCarriersFor.
func carriersFor(catalog []protocol.PayloadBinTypeInfo, parts []string) []string {
	pick := func(match func(protocol.PayloadBinTypeInfo) bool) []string {
		var out []string
		seen := map[string]bool{}
		for _, e := range catalog {
			if !seen[e.BinTypeCode] && match(e) {
				seen[e.BinTypeCode] = true
				out = append(out, e.BinTypeCode)
			}
		}
		return out
	}
	out := pick(func(e protocol.PayloadBinTypeInfo) bool {
		return len(parts) == 0 || slices.Contains(parts, e.PayloadCode)
	})
	if len(out) == 0 {
		out = pick(func(protocol.PayloadBinTypeInfo) bool { return true })
	}
	return out
}

// countWaits is the board's sentence for a sequential position the line has
// moved onto before its bin arrived. A RELEASE on the other side always moves
// the line here (owner, 2026-09-30); while no bin is bound, the tick path holds
// the count in pending_uop_delta and replays it when the bin binds. Built from
// the claim and runtime the view already holds: no read.
func countWaits(coreNode string, claim *processes.NodeClaim, rt *processes.RuntimeState) string {
	if claim == nil || claim.SwapMode != protocol.SwapModeSequential || claim.PairedCoreNode == "" {
		return ""
	}
	if rt == nil || !rt.ActivePull || rt.ActiveBinID != nil {
		return ""
	}
	return fmt.Sprintf("The line is on %s; its parts wait for %s's bin.", coreNode, coreNode)
}

// canSkipChangeoverStep is domain.AbandonRefusal over the orders the board
// already holds for the node. An order the board does not list is not live,
// which is how the abandon reads a terminal one: nothing left to cancel.
func canSkipChangeoverStep(task *domain.NodeTask, nodeOrders []domain.Order) bool {
	if task == nil {
		return false
	}
	find := func(id *int64) *domain.Order {
		if id == nil {
			return nil
		}
		for i := range nodeOrders {
			if nodeOrders[i].ID == *id {
				return &nodeOrders[i]
			}
		}
		return nil
	}
	return domain.AbandonRefusal(*task, find(task.NextMaterialOrderID), find(task.OldMaterialReleaseOrderID), false) == nil
}
