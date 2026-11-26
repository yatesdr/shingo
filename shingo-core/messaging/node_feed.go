package messaging

import (
	"cmp"
	"fmt"
	"log"
	"slices"
	"strings"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
	"shingocore/store/scene"
)

// node_feed.go — the node list, the scene and the catalog: answered on request
// (boot, register, NodeStructureChanged, a Kafka reconnect, an older Edge's
// two-minute re-ask) and sent as feeds when a heartbeat's digest differs. One
// builder per value serves both paths, so the Digest on a reply is the digest
// the feed compares.
//
// THE TWO PATHS DEGRADE DIFFERENTLY. A reply keeps today's posture: a failed
// read of a slice the Edge holds only in memory (kanban marks, bin types,
// the scene) still sends the rest. The feed path is strict: any failed read
// fails the build, the key is left out of the ack, and nothing is sent — a
// digest of a partial read would confirm a copy nobody read. A degraded reply
// carries the digest of what it does carry, so the next heartbeat sees the
// difference and heals it.

// HandleNodeListRequest answers an Edge's node-list sync. req carries the
// scene revision the Edge already holds (empty from an older binary); see
// sceneSlices for what that decides.
//
// THE REPLY IS LENIENT AND IS NEVER MEMOISED; AFTER IT, THE STATION'S
// NODE-LIST AND SCENE MEMOS ARE FORGOTTEN. The Edge holds this answer's
// digests from now on, and the heartbeat compares against the memo: one built
// before a write that drops no memo (a group deleted or reparented, which Core
// announces with NodeStructureChanged, the very notice that makes the Edge
// re-ask; a scene sync) would differ from it, and a converged Edge quoting a
// new digest reads as one that lost its copy, so the old list or the old scene
// went back at once and the Edge applied it until the memo aged out. With the
// memos gone the next heartbeat builds afresh. The reply's degraded build (a
// failed optional read still sends the rest) is not stored: only the strict
// feed build is (feedCurrent). Pinned by TestNodeFeed_AReAskedListIsNotSentBack
// and TestNodeFeed_ASceneChangeThenAReAskIsNotSentTheOldScene.
func (s *CoreDataService) HandleNodeListRequest(env *protocol.Envelope, req *protocol.NodeListRequest) {
	resp, err := s.buildNodeList(env.Src.Station, false)
	if err != nil {
		log.Printf("core_handler: node list for %s: %v — not sent", env.Src.Station, err)
		return
	}
	var edgeRevision string
	if req != nil {
		edgeRevision = req.SceneRevision
	}
	resp.ScenePoints, resp.SceneEdges, resp.SceneRevision = s.sceneSlices(edgeRevision, env.Src.Station)
	s.resp.replyData(env, protocol.SubjectNodeListResponse, resp)
	s.feeds.dropStation(env.Src.Station, protocol.FeedNodes)
	s.feeds.dropStation(env.Src.Station, protocol.FeedScene)
	logNodeList("sent node list", resp, edgeRevision, env.Src.Station)
}

func logNodeList(what string, resp *protocol.NodeListResponse, edgeRevision, station string) {
	log.Printf("core_handler: %s (%d nodes, %d loaders, %d scene points, %d scene edges, geometry=%v, digest=%s) to %s",
		what, len(resp.Nodes), len(resp.Loaders), len(resp.ScenePoints), len(resp.SceneEdges),
		resp.SceneRevision != "" && edgeRevision != resp.SceneRevision, resp.Digest, station)
}

// buildNodeList builds one station's node list, loaders and payload bin types
// — the FeedNodes value — and its digest. strict is the feed path: every read
// must succeed.
func (s *CoreDataService) buildNodeList(stationID string, strict bool) (*protocol.NodeListResponse, error) {
	// A station with no nodes of its own gets the plant-wide list. A station
	// whose list could not be READ gets nothing: falling back on an error
	// would hand it every node in the plant as though they were its own.
	nodeList, err := s.db.ListNodesForStation(stationID)
	if err != nil {
		return nil, fmt.Errorf("list nodes for station: %w", err)
	}
	stationScoped := len(nodeList) > 0
	if !stationScoped {
		if nodeList, err = s.db.ListNodes(); err != nil {
			return nil, fmt.Errorf("list nodes: %w", err)
		}
	}

	// Which groups keep a level of empties: one read for the whole list. A
	// failed read sends the list unmarked, which the Edge reads exactly as it
	// reads an older Core — it refuses nothing on this account — so the read
	// does not hold the topology back.
	kanbans, merr := s.db.KanbanIDs()
	if merr != nil {
		if strict {
			return nil, fmt.Errorf("list kanbans: %w", merr)
		}
		log.Printf("core_handler: list kanbans for %s: %v", stationID, merr)
	}
	// Windows of a loader that takes partials, for the Edge's partial-home
	// refusal. A failed read sends no marks on a reply, as the kanbans read does.
	partials, perr := s.db.AcceptsPartialsNodeIDs()
	if perr != nil {
		if strict {
			return nil, fmt.Errorf("list accept-partials windows: %w", perr)
		}
		log.Printf("core_handler: list accept-partials windows for %s: %v", stationID, perr)
	}
	// Nodes other stations claim, by uid, for the Edge's picker. A failed read
	// sends no marks on a reply, as the kanbans read does: they refuse nothing.
	claimed, cerr := s.db.NodesClaimedElsewhere(stationID)
	if cerr != nil {
		if strict {
			return nil, fmt.Errorf("list nodes claimed elsewhere: %w", cerr)
		}
		log.Printf("core_handler: list nodes claimed elsewhere for %s: %v", stationID, cerr)
	}

	// Loader refactor cutover: include the Core-owned loader config as a sibling
	// slice so Edge's persistent cache receives it atomically with the topology.
	//
	// Sending the node list WITHOUT loaders is not "degraded but safe" — the Edge
	// cannot distinguish an absent Loaders field from "no loaders configured", and
	// ReplaceCoreLoaders(nil) truncates every loader cache table. Send nothing; the
	// Edge keeps its last-known-good cache and the next heartbeat asks again.
	loaderInfos, lerr := s.db.BuildLoaderInfos()
	if lerr != nil {
		return nil, fmt.Errorf("build loader infos: %w", lerr)
	}
	// The display name of the Edge each loader names and of each station
	// claiming a node elsewhere, from ONE registry read, and none when nothing
	// names an Edge. Filled here, before the digest, so the digest covers what
	// is sent and a rename moves it. A name is display only (the Edge falls
	// back to the uid, and a claimant reads as its uid), so a reply degrades as
	// the kanbans read does: the uids go, without names.
	var edges []domain.RegistryEdge
	if loadersNameAnEdge(loaderInfos) || len(claimed) > 0 {
		var eerr error
		edges, eerr = s.db.ListEdges()
		if eerr != nil {
			if strict {
				return nil, fmt.Errorf("list edges: %w", eerr)
			}
			log.Printf("core_handler: list edges for %s: %v", stationID, eerr)
		}
		nameLoaderEdges(loaderInfos, edges)
	}
	infos := nodeInfos(nodeList, stationScoped, kanbans, partials, claimantNames(claimed, edges))
	// Payload→dunnage mapping: one query replaces the N+1 per-node
	// GetEffectiveBinTypes calls. Edge uses this to derive picker options
	// from the node's allowed payloads (claim.AllowedPayloadCodes).
	//
	// Unlike the loader read above, a reply does NOT fail on this one: the
	// slice is memory-only on the Edge (re-derived from the next node list),
	// while the loader slice backs a durable cache that a wrong read destroys.
	// Do not "unify" the two on the reply path.
	pbtPairs, pbtErr := s.db.ListPayloadBinTypeMappings()
	if pbtErr != nil {
		if strict {
			return nil, fmt.Errorf("list payload bin types: %w", pbtErr)
		}
		log.Printf("core_handler: list payload bin types for %s: %v", stationID, pbtErr)
	}
	payloadBinTypes := payloadBinTypeRows(pbtPairs)

	// Each bin type's family, for the Edge's CLEAR panels. Memory-only on the Edge
	// like the slice above, and degrades the same way: a reply with none leaves
	// its panels unfiltered until the next heartbeat's digest differs.
	binTypes, btErr := s.db.ListBinTypes()
	if btErr != nil {
		if strict {
			return nil, fmt.Errorf("list bin types: %w", btErr)
		}
		log.Printf("core_handler: list bin types for %s: %v", stationID, btErr)
	}
	families := binTypeFamilies(binTypes)

	digest, err := protocol.NodesDigest(infos, loaderInfos, payloadBinTypes, families, !stationScoped)
	if err != nil {
		return nil, fmt.Errorf("digest node list: %w", err)
	}
	return &protocol.NodeListResponse{
		Nodes:           infos,
		Loaders:         loaderInfos,
		PayloadBinTypes: payloadBinTypes,
		Family:          families,
		PlantWide:       !stationScoped,
		Digest:          digest,
	}, nil
}

// payloadBinTypeRows is NodeListResponse.PayloadBinTypes: one row per (payload
// code, bin type code) pair, in the order read. Nil with no pairs.
func payloadBinTypeRows(pairs [][2]string) []protocol.PayloadBinTypeInfo {
	var out []protocol.PayloadBinTypeInfo
	for _, p := range pairs {
		out = append(out, protocol.PayloadBinTypeInfo{PayloadCode: p[0], BinTypeCode: p[1]})
	}
	return out
}

// loadersNameAnEdge reports whether any loader names the Edge that works it.
func loadersNameAnEdge(infos []protocol.LoaderInfo) bool {
	for _, li := range infos {
		if li.EdgeStationUID != "" {
			return true
		}
	}
	return false
}

// nameLoaderEdges gives each loader that names an Edge that Edge's display
// name as Core holds it. A uid with no registry row, or a row with no display
// name, sends none.
func nameLoaderEdges(infos []protocol.LoaderInfo, edges []domain.RegistryEdge) {
	names := make(map[string]string, len(edges))
	for _, e := range edges {
		names[e.StationUID] = e.DisplayName
	}
	for i := range infos {
		if uid := infos[i].EdgeStationUID; uid != "" {
			infos[i].EdgeDisplayName = names[uid]
		}
	}
}

// claimantNames is each claimed node's claimants as a person reads them: the
// display name the registry holds, else the uid, sorted.
func claimantNames(claimed map[string][]string, edges []domain.RegistryEdge) map[string][]string {
	if len(claimed) == 0 {
		return nil
	}
	names := make(map[string]string, len(edges))
	for _, e := range edges {
		if e.DisplayName != "" {
			names[e.StationUID] = e.DisplayName
		}
	}
	out := make(map[string][]string, len(claimed))
	for node, uids := range claimed {
		labels := make([]string, 0, len(uids))
		for _, uid := range uids {
			labels = append(labels, cmp.Or(names[uid], uid))
		}
		slices.Sort(labels)
		out[node] = labels
	}
	return out
}

// binTypeFamilies is NodeListResponse.Family: each type's carrier family, as the walk compares
// them (kanban.SameFamily). Nil with no types.
func binTypeFamilies(types []*bins.BinType) map[string]string {
	var out map[string]string
	for _, t := range types {
		if t.Bare {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(types))
		}
		out[t.Code] = t.FamilyCode
	}
	return out
}

// nodeInfos projects the node rows for the wire: a station-scoped list names
// group children "Group.Child"; the plant-wide fallback sends the roots and the
// children of node groups. claimed is by the node's own name, as claims spell
// it, never the dotted one.
func nodeInfos(nodeList []*nodes.Node, stationScoped bool, kanbans, partials map[int64]bool,
	claimed map[string][]string) []protocol.NodeInfo {
	var infos []protocol.NodeInfo
	info := func(n *nodes.Node, name string) protocol.NodeInfo {
		return protocol.NodeInfo{Name: name, NodeType: n.NodeTypeCode, Kanban: kanbans[n.ID],
			AcceptsPartials: partials[n.ID], ClaimedBy: strings.Join(claimed[n.Name], ", ")}
	}
	if stationScoped {
		for _, n := range nodeList {
			name := n.Name
			if n.ParentID != nil && !n.IsSynthetic && n.ParentName != "" {
				name = n.ParentName + "." + n.Name
			}
			infos = append(infos, info(n, name))
		}
		return infos
	}
	nodeMap := make(map[int64]*nodes.Node, len(nodeList))
	for _, n := range nodeList {
		nodeMap[n.ID] = n
	}
	for _, n := range nodeList {
		if n.ParentID == nil {
			infos = append(infos, info(n, n.Name))
		} else if !n.IsSynthetic {
			if parent, ok := nodeMap[*n.ParentID]; ok && parent.NodeTypeCode == protocol.NodeClassNGRP {
				infos = append(infos, info(n, parent.Name+"."+n.Name))
			}
		}
	}
	return infos
}

// sceneRows is the FeedScene value: the scene as read, from which a send
// projects names, and geometry when the Edge's revision differs.
type sceneRows struct {
	points []*scene.Point
	edges  []*scene.Edge
}

// buildScene reads the scene for the feed path. Its digest is the scene's
// revision, a hash of what the rows say (scene.Revision); a failed read of
// either half fails the build.
func (s *CoreDataService) buildScene() (string, sceneRows, error) {
	points, err := s.db.ListScenePoints()
	if err != nil {
		return "", sceneRows{}, fmt.Errorf("list scene points: %w", err)
	}
	edges, err := s.db.ListSceneEdges()
	if err != nil {
		return "", sceneRows{}, fmt.Errorf("list scene edges: %w", err)
	}
	return scene.Revision(points, edges), sceneRows{points: points, edges: edges}, nil
}

// sendNodeFeed sends one station its node list when the nodes or the scene
// digest differs: the memoised node list, the scene's names always (the Edge
// replaces its name set on every reply), and the geometry only when the scene
// revision the Edge holds is not the current one — the request path's rule,
// with the held revision taken from the heartbeat.
//
// A node list that cannot be read is not sent at all, even when only the scene
// differs: an empty list would wipe the Edge's.
//
// The send of both the FeedNodes and the FeedScene rows (coreFeeds), so it
// reads both builds through current, whichever of the two is due.
func sendNodeFeed(s *CoreDataService, station string, _ coreFeed, _ feedMemoEntry, current feedReader, held map[string]string) {
	ne, ok := current(protocol.FeedNodes)
	if !ok {
		return
	}
	resp := *ne.value.(*protocol.NodeListResponse)
	heldRevision := held[protocol.FeedScene]
	if se, ok := current(protocol.FeedScene); ok {
		rows := se.value.(sceneRows)
		withGeometry := se.digest != heldRevision
		resp.ScenePoints = projectScenePoints(rows.points, withGeometry)
		resp.SceneEdges = projectSceneEdges(rows.edges, withGeometry)
		resp.SceneRevision = se.digest
	} else {
		resp.ScenePoints, resp.SceneEdges, resp.SceneRevision = s.sceneSlices(heldRevision, station)
	}
	s.resp.sendData(protocol.SubjectNodeListResponse, station, &resp)
	logNodeList("node list feed", &resp, heldRevision, station)
}

// HandleCatalogPayloadsRequest answers an Edge's catalog sync, leniently, as
// the node-list reply does, and then forgets the catalog memo, which is
// plant-wide: the next heartbeat from any Edge compares against a build as new
// as this answer, so one made before a write that called no hook is not sent
// back over it. Pinned by TestNodeFeed_ACatalogReplyAfterAnEdit.
func (s *CoreDataService) HandleCatalogPayloadsRequest(env *protocol.Envelope) {
	log.Printf("core_handler: catalog payloads request from %s", env.Src.Station)
	resp, err := s.buildCatalog(false)
	if err != nil {
		log.Printf("core_handler: catalog for %s: %v", env.Src.Station, err)
		return
	}
	s.resp.replyData(env, protocol.SubjectCatalogPayloadsResponse, resp)
	s.feeds.drop(protocol.FeedCatalog)
	log.Printf("core_handler: sent payload catalog (%d payloads, digest=%s) to %s", len(resp.Payloads), resp.Digest, env.Src.Station)
}

// buildCatalog builds the payload catalog — the FeedCatalog value — and its
// digest. strict is the feed path.
func (s *CoreDataService) buildCatalog(strict bool) (*protocol.CatalogPayloadsResponse, error) {
	payloads, err := s.db.ListPayloads()
	if err != nil {
		return nil, fmt.Errorf("list payloads: %w", err)
	}
	catids, err := s.db.PayloadCATIDs()
	if err != nil {
		if strict {
			return nil, fmt.Errorf("payload catids: %w", err)
		}
		// Degrade to no CATIDs rather than failing the whole catalog sync — the
		// edge just won't auto-fill expected_catid this round.
		log.Printf("core_handler: payload catids for catalog: %v", err)
		catids = map[int64]string{}
	}
	lines, err := s.db.PayloadManifestLines()
	if err != nil {
		if strict {
			return nil, fmt.Errorf("payload manifest lines: %w", err)
		}
		// Degrade as the CATIDs do: rows with no lines. The Edge's load
		// modal then offers the payload as a one-line template of itself.
		log.Printf("core_handler: payload manifest lines for catalog: %v", err)
		lines = nil
	}
	infos := catalogRows(payloads, catids, lines)
	digest, err := protocol.CatalogDigest(infos)
	if err != nil {
		return nil, fmt.Errorf("digest catalog: %w", err)
	}
	return &protocol.CatalogPayloadsResponse{Payloads: infos, Digest: digest}, nil
}

// CatalogRow is one payload's catalog row and its carrier rows, built by the
// builders the feeds use (catalogRows, payloadBinTypeRows), so an answer that
// carries them holds exactly what the next catalog feed and node list carry for
// it, and the Edge's conditional upsert of the next feed writes nothing new.
// Strict, as the feed path is: any failed read fails it. It reads the
// plant-wide sets the feeds read (the cat ids, the manifest lines, the carrier
// pairs) and keeps this payload's: it runs once per part made at the floor
// (wired to service.PayloadService.CatalogRow at the composition root).
// Pinned by TestCatalogRow_IsTheFeedsRow.
func (s *CoreDataService) CatalogRow(code string) (protocol.CatalogPayloadInfo, []protocol.PayloadBinTypeInfo, error) {
	p, err := s.db.GetPayloadByCode(code)
	if err != nil {
		return protocol.CatalogPayloadInfo{}, nil, fmt.Errorf("read payload %s: %w", code, err)
	}
	catids, err := s.db.PayloadCATIDs()
	if err != nil {
		return protocol.CatalogPayloadInfo{}, nil, fmt.Errorf("payload catids: %w", err)
	}
	lines, err := s.db.PayloadManifestLines()
	if err != nil {
		return protocol.CatalogPayloadInfo{}, nil, fmt.Errorf("payload manifest lines: %w", err)
	}
	pairs, err := s.db.ListPayloadBinTypeMappings()
	if err != nil {
		return protocol.CatalogPayloadInfo{}, nil, fmt.Errorf("list payload bin types: %w", err)
	}
	var own [][2]string
	for _, pair := range pairs {
		if pair[0] == p.Code {
			own = append(own, pair)
		}
	}
	return catalogRows([]*payloads.Payload{p}, catids, lines)[0], payloadBinTypeRows(own), nil
}

// catalogRows is the catalog's rows: one per payload, in the order read, with
// its CATID and its manifest template lines (part, ratio, description; the
// line and part ids and the part's cat id stay on Core). A payload with no
// lines carries none, nil, so its row encodes as it did before the lines rode
// the catalog. Pinned by TestCatalogRows_CarryManifestLines.
func catalogRows(ps []*payloads.Payload, catids map[int64]string, lines map[int64][]*payloads.ManifestItem) []protocol.CatalogPayloadInfo {
	infos := make([]protocol.CatalogPayloadInfo, len(ps))
	for i, p := range ps {
		manifest := domain.ProtocolLines(lines[p.ID])
		infos[i] = protocol.CatalogPayloadInfo{
			ID: p.ID, Name: p.Code, Code: p.Code,
			Description: p.Description,
			UOPCapacity: p.UOPCapacity,
			CATID:       catids[p.ID],
			Manifest:    manifest,
		}
	}
	return infos
}
