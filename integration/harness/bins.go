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
// WHEN: on change only. At the start (StartBinsPush); at the end of every Bus
// pump (Bus.SetBinsPush: PumpAll and PumpEdgeOutbox), after the messages are
// delivered, as Core's push follows the change that moved a bin; on a
// scenario's explicit Bus.PushBins; and with AnswerAsks. Nothing pushes in
// between. The Edge's decisions read the table, never Core, so they read the
// last push, as a plant Edge does: a scenario that writes Core's bins directly
// (a fixture write, a bin moved by hand) and needs the Edge to decide from that
// write calls Bus.PushBins, or pumps, first.
//
// A 20 ms ticker pushed as well until 2026-11-24, and this comment said it
// covered what a scenario writes to Core directly. It was 80% of all pushes
// and changed no order the Edge made (R26): with it off over 5 runs of every
// test that starts a push (228 cells a run, the T19 probe, 2026-11-23) no
// verdict moved and no scenario that writes Core directly failed for want of
// it.
type BinsPush struct {
	t    *testing.T
	core *coremessaging.CoreDataService
	edge *edgeengine.Engine
	mu   sync.Mutex
	// asks is the ask kinds the push's ack says Core answers (AnswerAsks):
	// none until a scenario wires Core's asks (the scenarios' registerAsks).
	asks []string
	// rec is the cell's scenario recorder (RecordEnv), nil when it is off;
	// last is the digest of the snapshot the push before applied, which says
	// whether a push changed the Edge's copy.
	rec  *Recorder
	last string
}

// AnswerAsks makes every later ack say Core answers kinds, as a Core whose
// ask server answers them does, and acks once now.
func (p *BinsPush) AnswerAsks(kinds []string) {
	p.mu.Lock()
	p.asks = kinds
	p.mu.Unlock()
	p.push("answer-asks")
}

// StartBinsPush pushes coreDB's bins to edge now and returns the push, for a
// Bus to push after each pump (Bus.SetBinsPush). It starts no goroutine.
func StartBinsPush(t *testing.T, coreDB *corestore.DB, edge *edgeengine.Engine) *BinsPush {
	t.Helper()
	p := &BinsPush{t: t, core: coremessaging.NewCoreDataService(coreDB, nil, service.EpochAnnounce{}), edge: edge,
		rec: RecorderFor(t)}
	p.push("start")
	return p
}

// Push builds and applies one snapshot for the Edge's current watch list. A
// failed build logs and leaves the copy as it was.
func (p *BinsPush) Push() { p.push("direct") }

// push is Push for caller: the recorder counts pushes by who asked for them
// (the start, a pump, a scenario's explicit Bus.PushBins, AnswerAsks).
func (p *BinsPush) push(caller string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rec != nil {
		start := time.Now()
		var digest string
		p.apply(&digest)
		p.rec.push(caller, digest != "" && digest != p.last, time.Since(start))
		if digest != "" {
			p.last = digest
		}
		return
	}
	p.apply(nil)
}

// apply is one push's body; the caller holds p.mu. digest, when not nil, gets
// the digest of the snapshot it applied ("" when the push failed).
func (p *BinsPush) apply(digest *string) {
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
	if digest != nil {
		*digest = snap.Digest
	}
	feeds := p.edge.FeedDigests()
	p.edge.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: feeds, BinWatch: &listDigest, Asks: p.asks})
}
