package messaging

import (
	"log"

	"shingo/protocol"
)

// supply_refusal_handler.go — Core's side of the supplier→customer channel.
//
// Core is a ROUTER here plus a historian, and deliberately nothing more. It does
// not gate on a refusal, compute from it, or feed it to the threshold monitor: a
// person saying "there are none in my racks" is evidence about the world, not an
// instruction to the dispatcher.
//
// NN5: this handler, its protocol.CoreInboundSubjects() entry and its
// RegisterSubject call are one change. An entry without a handler is a Core that
// does not start, and TestSubjectRouter_CoversEveryInboundSubject fails the
// build in milliseconds rather than at a plant.

// HandleSupplyRefusal stores one refusal message and fans it back out.
//
// STORE THEN BROADCAST, in that order, and the order matters. The broadcast is
// what makes the cell's screen light up; the store is what makes the fact
// survive. Broadcasting first would let a storage failure produce a refusal that
// every operator saw and no record contains — and this message exists precisely
// because it is the one inventory fact with no other source.
//
// REBROADCAST TO EVERY EDGE, not to a resolved addressee. Routing would need
// Core to answer "which edges host cells short of this payload", which means
// retaining process identity through PayloadsForLoader or joining on
// inbound_source — a column that may hold a node GROUP name, where a wrong
// answer sends the message to the wrong cell. Broadcasting removes the question:
// each edge already receives every payload of this kind and filters locally, on
// the same predicate the supplier's own endpoint enforced before accepting the
// refusal. sourcing.state is broadcast for exactly this reason.
//
// The sender gets its own message back. That is intended and harmless: every
// apply path is idempotent, and it means a single-edge line exercises the
// identical code path a multi-edge one will, from day one, rather than leaving
// the cross-edge path untested until the day it matters.
func (s *CoreDataService) HandleSupplyRefusal(env *protocol.Envelope, st *protocol.SupplyRefusalState) {
	if st == nil || st.LoaderNode == "" || st.PayloadCode == "" {
		log.Printf("core_handler: supply refusal with no loader_node/payload_code from %s — dropped",
			env.Src.Station)
		return
	}
	if err := s.db.ApplySupplyRefusal(*st, env.Src.Station); err != nil {
		log.Printf("core_handler: apply supply refusal %s/%s action=%s from %s: %v — NOT broadcast, "+
			"because a refusal every operator saw and no record contains is worse than one nobody saw",
			st.LoaderNode, st.PayloadCode, st.Action, env.Src.Station, err)
		return
	}
	// Stored, so the refusals digest Core quotes has moved: drop the memo so
	// the next heartbeat compares against the new set, not one up to ten
	// seconds old.
	s.feeds.drop(protocol.FeedRefusals)
	// Now fan out. sendData is fire-and-forget by design on this seam. A
	// broadcast that does not land heals from the stored row: every Edge
	// quotes the digest of its own refusal table on each heartbeat, and an
	// Edge whose digest differs from Core's is sent Core's open set
	// (supply.refusal_snapshot, feeds.go). A lost broadcast degrades to "the
	// cell finds out within a heartbeat or two" rather than "never".
	s.resp.sendData(protocol.SubjectSupplyRefusalState, protocol.StationBroadcast, st)
}
