package engine

import (
	"fmt"
	"slices"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
)

// loader_empties_ask.go — an operator's empties at a loader, asked of Core.
//
// Core sizes and places them (protocol.AskLoaderEmpties: dispatch.ReplenishLoader's
// operator origin, the windows chosen by plan.LoaderWindows, each order born
// through AdmitCoreAsk), so the count of what is in flight, the windows already
// spoken for and the resident bins are read where they are written. The orders
// reach this Edge as projections (ApplyOrderProjection), as every Core-authored
// order does. Nothing here creates an order.

// askLoaderEmpties asks Core for count empties at dl for payload, routed to
// member on a dedicated loader, attributed to origin, waiting AskWait. The
// answer is what Core made and, per window it did not order to, why. A Core
// that does not answer the kind is refused here: there is no Edge path left
// that orders an operator's empty on its own count.
func (e *Engine) askLoaderEmpties(dl *domain.Loader, payload domain.PayloadCode, count int, member domain.NodeID, origin orders.Origin) (*protocol.LoaderEmptiesResponse, error) {
	if !e.CoreAnswers(protocol.AskLoaderEmpties) {
		return nil, fmt.Errorf("Core does not answer the %s ask yet", protocol.AskLoaderEmpties)
	}
	a, err := e.Ask(protocol.AskLoaderEmpties, protocol.LoaderEmptiesRequest{
		LoaderKey: string(dl.ID()), PayloadCode: string(payload), MemberNode: string(member),
		Count: count, OriginID: origin.ID, OriginClass: origin.Class,
	}, AskWait)
	if err != nil {
		return nil, err
	}
	return askInto[protocol.LoaderEmptiesResponse](a)
}

// noEmptiesSentence is the operator's answer when Core made none: why, from
// Core's Skipped, or each window Core did not order to with its cause.
func noEmptiesSentence(node string, res *protocol.LoaderEmptiesResponse) string {
	if res.Skipped != "" {
		// OWNER-WORDS
		return fmt.Sprintf("node %s: no empty ordered — %s", node, res.Skipped)
	}
	held := make([]string, 0, len(res.HeldBy))
	for window, cause := range res.HeldBy {
		held = append(held, window+" "+cause)
	}
	slices.Sort(held)
	// OWNER-WORDS
	return fmt.Sprintf("node %s: no empty ordered — every window is spoken for (%s)", node, strings.Join(held, ", "))
}
