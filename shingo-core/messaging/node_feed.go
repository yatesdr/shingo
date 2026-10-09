package messaging

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/store/nodes"
	"shingocore/store/scene"
)

// node_feed.go — the node list, the scene and the catalog: answered on request
// (boot, register, NodeStructureChanged, a Kafka reconnect, an older Edge's
// two-minute re-ask) and sent as feeds when a heartbeat's digest differs. One
// builder per value serves both paths, so the Digest on a reply is the digest
// the feed compares.
//
// THE TWO PATHS DEGRADE DIFFERENTLY. A reply keeps today's posture: a failed
// read of a slice the Edge holds only in memory (maintained marks, bin types,
// the scene) still sends the rest. The feed path is strict: any failed read
// fails the build, the key is left out of the ack, and nothing is sent — a
// digest of a partial read would confirm a copy nobody read. A degraded reply
// carries the digest of what it does carry, so the next heartbeat sees the
// difference and heals it.

// HandleNodeListRequest answers an Edge's node-list sync. req carries the
// scene revision the Edge already holds (empty from an older binary); see
// sceneSlices for what that decides.
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
	maintained, merr := s.db.MaintainedGroupIDs()
	if merr != nil {
		if strict {
			return nil, fmt.Errorf("list maintained groups: %w", merr)
		}
		log.Printf("core_handler: list maintained groups for %s: %v", stationID, merr)
	}
	infos := nodeInfos(nodeList, stationScoped, maintained)

	// Loader refactor cutover: include the Core-owned loader config as a sibling
	// slice so Edge's persistent cache receives it atomically with the topology.
	//
	// Sending the node list WITHOUT loaders is not "degraded but safe" — the Edge
	// cannot distinguish an absent Loaders field from "no loaders configured", and
	// ReplaceCoreLoaders(nil) truncates all five cache tables. Send nothing; the
	// Edge keeps its last-known-good cache and the next heartbeat asks again.
	loaderInfos, lerr := s.db.BuildLoaderInfos()
	if lerr != nil {
		return nil, fmt.Errorf("build loader infos: %w", lerr)
	}
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
	var payloadBinTypes []protocol.PayloadBinTypeInfo
	for _, p := range pbtPairs {
		payloadBinTypes = append(payloadBinTypes, protocol.PayloadBinTypeInfo{PayloadCode: p[0], BinTypeCode: p[1]})
	}

	digest, err := protocol.NodesDigest(infos, loaderInfos, payloadBinTypes)
	if err != nil {
		return nil, fmt.Errorf("digest node list: %w", err)
	}
	return &protocol.NodeListResponse{
		Nodes:           infos,
		Loaders:         loaderInfos,
		PayloadBinTypes: payloadBinTypes,
		Digest:          digest,
	}, nil
}

// nodeInfos projects the node rows for the wire: a station-scoped list names
// group children "Group.Child"; the plant-wide fallback sends the roots and the
// children of node groups.
func nodeInfos(nodeList []*nodes.Node, stationScoped bool, maintained map[int64]bool) []protocol.NodeInfo {
	var infos []protocol.NodeInfo
	if stationScoped {
		for _, n := range nodeList {
			name := n.Name
			if n.ParentID != nil && !n.IsSynthetic && n.ParentName != "" {
				name = n.ParentName + "." + n.Name
			}
			infos = append(infos, protocol.NodeInfo{Name: name, NodeType: n.NodeTypeCode, Maintained: maintained[n.ID]})
		}
		return infos
	}
	nodeMap := make(map[int64]*nodes.Node, len(nodeList))
	for _, n := range nodeList {
		nodeMap[n.ID] = n
	}
	for _, n := range nodeList {
		if n.ParentID == nil {
			infos = append(infos, protocol.NodeInfo{Name: n.Name, NodeType: n.NodeTypeCode, Maintained: maintained[n.ID]})
		} else if !n.IsSynthetic {
			if parent, ok := nodeMap[*n.ParentID]; ok && parent.NodeTypeCode == protocol.NodeClassNGRP {
				infos = append(infos, protocol.NodeInfo{
					Name: parent.Name + "." + n.Name, NodeType: n.NodeTypeCode, Maintained: maintained[n.ID],
				})
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

// buildScene reads the scene for the feed path. Its digest is the existing
// revision, a hash of the rows; a failed read of either half fails the build.
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
func (s *CoreDataService) sendNodeFeed(station string, held map[string]string) {
	now := clock.Now()
	ne, ok := s.feedCurrent(station, protocol.FeedNodes, now)
	if !ok {
		return
	}
	resp := *ne.value.(*protocol.NodeListResponse)
	heldRevision := held[protocol.FeedScene]
	if se, ok := s.feedCurrent(station, protocol.FeedScene, now); ok {
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

// HandleCatalogPayloadsRequest answers an Edge's catalog sync.
func (s *CoreDataService) HandleCatalogPayloadsRequest(env *protocol.Envelope) {
	log.Printf("core_handler: catalog payloads request from %s", env.Src.Station)
	resp, err := s.buildCatalog(false)
	if err != nil {
		log.Printf("core_handler: catalog for %s: %v", env.Src.Station, err)
		return
	}
	s.resp.replyData(env, protocol.SubjectCatalogPayloadsResponse, resp)
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
	infos := make([]protocol.CatalogPayloadInfo, len(payloads))
	for i, p := range payloads {
		infos[i] = protocol.CatalogPayloadInfo{
			ID: p.ID, Name: p.Code, Code: p.Code,
			Description: p.Description,
			UOPCapacity: p.UOPCapacity,
			CATID:       catids[p.ID],
		}
	}
	digest, err := protocol.CatalogDigest(infos)
	if err != nil {
		return nil, fmt.Errorf("digest catalog: %w", err)
	}
	return &protocol.CatalogPayloadsResponse{Payloads: infos, Digest: digest}, nil
}
