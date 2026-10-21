//go:build docker

package scenarios

import (
	"shingo/protocol"
	"shingo/protocol/router"

	"shingocore/asks"
	coremessaging "shingocore/messaging"
	corestore "shingocore/store"
	"shingocore/www"
)

// registerAsks wires the ask pair on a scenario's Core data subjects as Core's
// composition root does (cmd/shingocore buildAskServer, buildSubjectRouter):
// an edge.ask is answered through the door bodies (www.Doors.AskDoors), with
// the home clear's replay kept in coreDB's inbox, and the answer goes on
// Core's outbox (CoreHandler.ReplyAnswer) for the Bus to carry back.
//
// The Edge learns the kinds from Core's heartbeat ack: pass the returned
// kinds to the scenario's BinsPush (AnswerAsks), whose ack stands in for it.
// A scenario helper, not the harness's: it reaches Core's www package, which
// the harness module does not build against.
func registerAsks(subjects *router.SubjectRouter, coreDB *corestore.DB, doors *www.Doors, h *coremessaging.CoreHandler) []string {
	s := asks.New(coreDB, h.ReplyAnswer)
	for _, a := range doors.AskDoors() {
		s.Register(a.Kind, a.Door, a.Replay)
	}
	router.RegisterSubject(subjects, protocol.SubjectEdgeAsk, s.Handle)
	return s.Kinds()
}

// dataSubjects routes TypeData on a scenario's Core router to a subject
// router and returns it, for a scenario that routes no data subject of its
// own yet.
func dataSubjects(r *router.Router[string]) *router.SubjectRouter {
	subjects := router.NewSubject()
	router.Register(r, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) {
		subjects.Dispatch(env, p)
	})
	return subjects
}
