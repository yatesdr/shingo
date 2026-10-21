package harness

import (
	"sync"
	"testing"
	"time"

	"shingo/protocol"

	coremessaging "shingocore/messaging"
	"shingocore/service"
	corestore "shingocore/store"

	edgeengine "shingoedge/engine"
)

// BinsPush stands in for Core's bins feed in a scenario that runs no
// heartbeater and no feed push: it builds, from Core's database, the snapshot
// Core answers the Edge's watch list with (CoreDataService.NodeBinsRows, the
// rows Core's feed builder reads), applies it to the Edge as the copy, and
// takes the heartbeat ack that confirms it, so the Edge's decisions read a
// table they may decide from (engine BinsActable).
//
// WHEN: on every Bus pump (Bus.SetBinsPush), after the messages are delivered,
// as Core's push follows the change that moved a bin; and on a short ticker,
// for what a scenario writes to Core directly. The Edge's decisions read the
// table, never Core, so a scenario that changes Core and acts on the Edge at
// once acts on the table as a plant Edge would, up to one tick behind.
type BinsPush struct {
	t    *testing.T
	core *coremessaging.CoreDataService
	edge *edgeengine.Engine
	mu   sync.Mutex
}

// StartBinsPush pushes coreDB's bins to edge now and every interval until the
// test ends.
func StartBinsPush(t *testing.T, coreDB *corestore.DB, edge *edgeengine.Engine, interval time.Duration) *BinsPush {
	t.Helper()
	p := &BinsPush{t: t, core: coremessaging.NewCoreDataService(coreDB, nil, service.EpochAnnounce{}), edge: edge}
	p.Push()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				p.Push()
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
	return p
}

// Push builds and applies one snapshot for the Edge's current watch list. A
// failed build logs and leaves the copy as it was.
func (p *BinsPush) Push() {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := p.edge.StationService().WatchedBinNodes()
	if names == nil {
		names = []string{}
	}
	listDigest, err := protocol.BinWatchDigest(names)
	if err != nil {
		p.t.Logf("bins push: watch digest: %v", err)
		return
	}
	rows, err := p.core.NodeBinsRows(names)
	if err != nil {
		p.t.Logf("bins push: rows: %v", err)
		return
	}
	snap := protocol.NodeBinsSnapshot{ListDigest: listDigest, Rows: rows, Groups: []protocol.NodeBinsGroup{}}
	if snap.Digest, err = protocol.NodeBinsDigest(snap); err != nil {
		p.t.Logf("bins push: digest: %v", err)
		return
	}
	if err := p.edge.ApplyNodeBinsSnapshot(snap); err != nil {
		p.t.Logf("bins push: apply: %v", err)
		return
	}
	feeds := p.edge.FeedDigests()
	p.edge.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: feeds, BinWatch: &listDigest, Asks: AskKindsAnswered})
}

// AskKindsAnswered is the ask kinds the push's ack says Core answers: none.
// The harness carries no ask (no Core answers one here), so every Edge door
// takes its HTTP arm, and the read-asks fail closed or take the source a
// scenario set (SetPreflightSource, SetReleasePointSource).
var AskKindsAnswered []string
