package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/domain"
)

// node_feed.go — applying Core's node list and catalog, whether they came as
// a reply to an ask or as a feed Core sent because a heartbeat's digest
// differed. The two arrive on the same subjects and apply the same way.

// ApplyNodeList applies one node-list message from Core.
//
// A DIGEST EQUAL TO THE ONE HELD APPLIES NONE OF THE VALUES IT COVERS.
// Each of them costs something on every call — SetCoreNodes bumps the plant
// generation and rebuilds every station view, SetCoreLoaders rewrites the
// loader cache — and an identical list changes nothing they produce. A reply
// with no digest (an older Core) applies as it always did.
//
// The scene is applied on every message: its names ride every reply, and the
// geometry store already ignores a revision it holds.
//
// The digest is recorded only once the loader cache write succeeded, so a
// failed apply leaves the old digest, the next heartbeat differs, and Core
// sends again. An empty loader set from a successful read is a value like any
// other: it applies and its digest is held.
func (e *Engine) ApplyNodeList(resp *protocol.NodeListResponse) {
	if resp.Digest == "" || resp.Digest != e.heldDigest(protocol.FeedNodes) {
		e.SetCoreNodes(resp.Nodes)
		e.reportRefusedSetups()
		loadersErr := e.SetCoreLoaders(resp.Loaders)
		e.setBinTypeFamilies(resp.Family)
		e.SetPayloadBinTypes(resp.PayloadBinTypes)
		if loadersErr == nil && resp.Digest != "" {
			e.holdFeed(protocol.FeedNodes, resp.Digest, "")
		}
	}
	e.SetSceneGraph(resp.ScenePoints, resp.SceneEdges)
	// The same two slices, second consumer: replaced only when the response
	// carries the whole scene with its revision.
	e.SetSceneGeometry(resp.SceneRevision, resp.ScenePoints, resp.SceneEdges)
	if resp.SceneRevision != "" {
		e.holdFeed(protocol.FeedScene, e.SceneRevision(), "")
	}
	// The groups and loaders this reply carried may move the bins watch list;
	// reading it rebuilds it, and a moved list is published at once.
	if e.binWatchSource != nil {
		e.binWatchSource()
	}
}

// ApplyCatalog applies one catalog message from Core and, when it carries a
// digest and the rows landed, holds that digest.
func (e *Engine) ApplyCatalog(resp *protocol.CatalogPayloadsResponse) {
	if err := e.HandlePayloadCatalog(resp.Payloads); err == nil && resp.Digest != "" {
		e.holdFeed(protocol.FeedCatalog, resp.Digest, "")
	}
}

// reportRefusedSetups logs every saved setup the leg rule refuses against the
// node list just applied (domain.CoreNodeKinds.LegRefusal): each live claim's
// legs, each routing row, and each recipe's legs where Core picks. Saved before
// the rule, they still run; the claim editor shows the same sentences on the
// fields. One line each, so the owner can see how the plant is set up. A list
// that says nothing (an older Core, none heard) reports nothing.
func (e *Engine) reportRefusedSetups() {
	lines := e.refusedSetups()
	if len(lines) == 0 {
		return
	}
	log.Printf("engine: %d saved setup(s) name a node the leg rule refuses; they still run, and the editor "+
		"refuses the next save until each is changed:", len(lines))
	for _, l := range lines {
		log.Printf("engine:   %s", l)
	}
}

// refusedSetups is the report's lines: one per refused claim leg, routing row
// and recipe leg, by process, then style and claim, routing set, recipes.
func (e *Engine) refusedSetups() []string {
	kinds := e.CoreNodeKinds()
	if e.db == nil || kinds.Empty() {
		return nil
	}
	procs, err := e.db.ListProcesses()
	if err != nil {
		log.Printf("engine: report of saved setups: list processes: %v", err)
		return nil
	}
	var lines []string
	for _, p := range procs {
		styles, err := e.db.ListStylesByProcess(p.ID)
		if err != nil {
			log.Printf("engine: report of saved setups: styles of %s: %v", p.Name, err)
			continue
		}
		for _, st := range styles {
			claims, err := e.db.ListStyleNodeClaims(st.ID)
			if err != nil {
				log.Printf("engine: report of saved setups: claims of %s / %s: %v", p.Name, st.Name, err)
				continue
			}
			for _, c := range claims {
				for _, f := range kinds.RefusedLegs(domain.InputFromClaim(c)) {
					lines = append(lines, fmt.Sprintf("claim %s / %s at %s: %s: %s",
						p.Name, st.Name, c.CoreNodeName, f.Field, f.Message))
				}
			}
		}
		rows, err := e.db.ListRoutingNodes(p.ID)
		if err != nil {
			log.Printf("engine: report of saved setups: routing set of %s: %v", p.Name, err)
		}
		for _, r := range rows {
			if msg := kinds.LegRefusal(r.Role, r.CoreNodeName); msg != "" {
				lines = append(lines, fmt.Sprintf("routing set of %s: %s row: %s", p.Name, r.Role, msg))
			}
		}
		presets, err := e.db.ListFlowPresets(p.ID, false)
		if err != nil {
			log.Printf("engine: report of saved setups: recipes of %s: %v", p.Name, err)
		}
		for _, fp := range presets {
			cells, err := domain.ParsePresetShape(fp.FlowJSON)
			if err != nil {
				continue // a malformed recipe is refused by its own read
			}
			for _, r := range domain.RefusedPresetLegs(cells, kinds) {
				lines = append(lines, fmt.Sprintf("recipe %s v%d of %s, cell %d: %s: %s",
					fp.Name, fp.Version, p.Name, r.Cell, r.Field, r.Message))
			}
		}
	}
	return lines
}
