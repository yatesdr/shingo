package engine

import (
	"shingo/protocol"
	"shingo/protocol/clock"
	"shingoedge/store/processes"
)

// goAtRequest is a REQUEST saying go at its position: it clears a pause or an
// off there and stamps the position's last go (processes.GoReorder). A person's
// REQUEST only: the sweep's own ask (the autoreorder trigger) is not a go, and
// is never made at a held position. Called where the keeper's re-arm is, once
// the request has passed every refusal, so a refused REQUEST says nothing. A
// failed write is logged and does not fail the request.
func (e *Engine) goAtRequest(node *processes.Node, trigger string) {
	if trigger == protocol.EpisodeTriggerAutoreorder {
		return
	}
	if err := processes.GoReorder(e.db.DB, node.ID, clock.Now()); err != nil {
		e.logFn("auto-reorder: node %s: the go on REQUEST: %v", node.Name, err)
	}
}
