// Package asks is Core's half of the ask pair (protocol/ask.go): it takes an
// Edge's edge.ask, routes it by kind to the door body that answers it, and
// sends the answer back on Core's outbox as a reply to the ask.
//
// The door bodies are the retired HTTP doors' own, extracted wire-free
// (www.Doors); this package knows nothing of any door. The composition root registers each kind's body
// (cmd/shingocore), and a census there fences the list against
// protocol.AskKinds.
//
// Three things an ask needs that a door does not:
//   - an expired ask is answered, not dropped (AnswerExpired): the ingestor
//     hands it here (protocol.Ingestor.OnExpired) and the tile waiting on it
//     ends with "nothing was done";
//   - a kind whose repeat must not act twice (Replay) is answered, on a
//     repeat of the same envelope, by re-sending the first answer: the ask's
//     inbox row records its answer's outbox row (store.RecordInboundReply);
//   - the answer carries Core's current bins rows for the nodes the act
//     touched (Result.Rows), which the Edge writes into its table first.
package asks

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"

	"shingo/protocol"
)

// Result is a door's answer, in the door's own terms: its status (the HTTP
// status the door answers with), the sentence a person reads when it is not
// OK, its response body, and the rows for the nodes it touched.
type Result struct {
	Status   int
	Sentence string
	Body     any
	Rows     []protocol.NodeBinsRow
}

// Door answers one kind. station is the asking Edge's (the envelope's source,
// which the transport carried); body is the kind's request, encoded.
type Door func(station string, body json.RawMessage) Result

// Store is what the replay needs of Core's database.
type Store interface {
	RecordInboundMessage(msgID, msgType, stationID string) (bool, error)
	RecordInboundReply(msgID string, outboxID int64) error
	InboundReply(msgID string) (int64, bool, error)
	ReplayOutbox(id int64) (bool, error)
}

// Reply sends an answer to the ask in env and returns its outbox row's id.
type Reply func(env *protocol.Envelope, answer *protocol.EdgeAnswer) (int64, error)

// Server routes asks by kind.
type Server struct {
	mu     sync.RWMutex
	doors  map[string]Door
	replay map[string]bool
	db     Store
	reply  Reply
}

// New returns a server answering through reply, with db for the replay.
func New(db Store, reply Reply) *Server {
	return &Server{doors: map[string]Door{}, replay: map[string]bool{}, db: db, reply: reply}
}

// Register makes door the answer to kind. replay marks a kind whose repeat
// must be answered by re-sending its first answer rather than acting again.
func (s *Server) Register(kind string, door Door, replay bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doors[kind] = door
	if replay {
		s.replay[kind] = true
	}
}

// Kinds is every kind this Core answers, sorted: what its heartbeat ack
// advertises, so an Edge asks only what it will be answered.
func (s *Server) Kinds() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.doors))
	for k := range s.doors {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Handle answers one ask.
func (s *Server) Handle(env *protocol.Envelope, ask *protocol.EdgeAsk) {
	if env == nil || ask == nil {
		return
	}
	s.mu.RLock()
	door, known := s.doors[ask.Kind]
	replay := s.replay[ask.Kind]
	s.mu.RUnlock()
	if !known {
		s.send(env, ask.Kind, &protocol.EdgeAnswer{Kind: ask.Kind, Status: protocol.AnswerUnknownKind,
			// OWNER-WORDS
			Sentence: fmt.Sprintf("Core does not answer %q — nothing was done", ask.Kind)})
		return
	}
	if replay {
		if done := s.replayed(env); done {
			return
		}
	}
	res := door(env.Src.Station, ask.Body)
	answer := &protocol.EdgeAnswer{Kind: ask.Kind, Status: res.Status, Sentence: res.Sentence, Rows: res.Rows}
	if res.Body != nil {
		b, err := json.Marshal(res.Body)
		if err != nil {
			log.Printf("asks: encode %s answer for %s: %v", ask.Kind, env.Src.Station, err)
		} else {
			answer.Body = b
		}
	}
	id, ok := s.send(env, ask.Kind, answer)
	if ok && replay {
		if err := s.db.RecordInboundReply(env.ID, id); err != nil {
			log.Printf("asks: record the answer to %s ask %s: %v — a repeat will be acted on again (the door refuses a repeat it can tell)", ask.Kind, env.ID, err)
		}
	}
}

// replayed takes a replay kind's inbox row. true when this envelope was seen
// before and its first answer was sent again; a repeat whose first answer was
// never recorded (Core stopped between the act and the answer) is acted on
// again, and the door's own checks refuse what it can tell was done.
func (s *Server) replayed(env *protocol.Envelope) bool {
	inserted, err := s.db.RecordInboundMessage(env.ID, protocol.SubjectEdgeAsk, env.Src.Station)
	if err != nil {
		log.Printf("asks: record ask %s from %s: %v — answering it", env.ID, env.Src.Station, err)
		return false
	}
	if inserted {
		return false
	}
	id, ok, err := s.db.InboundReply(env.ID)
	if err != nil || !ok {
		return false
	}
	sent, err := s.db.ReplayOutbox(id)
	if err != nil || !sent {
		log.Printf("asks: replay answer %d to ask %s: sent=%v err=%v — answering it again", id, env.ID, sent, err)
		return false
	}
	log.Printf("asks: ask %s from %s seen before — its answer (outbox %d) sent again, nothing acted on", env.ID, env.Src.Station, id)
	return true
}

// AnswerExpired answers an ask that reached Core after its ExpiresAt: nothing
// was done. Every other expired message is left to the ingestor's drop.
func (s *Server) AnswerExpired(env *protocol.Envelope) {
	if env == nil || env.Type != protocol.TypeData {
		return
	}
	var data protocol.Data
	if err := json.Unmarshal(env.Payload, &data); err != nil || data.Subject != protocol.SubjectEdgeAsk {
		return
	}
	var ask protocol.EdgeAsk
	if err := json.Unmarshal(data.Body, &ask); err != nil {
		return
	}
	// A late repeat of a replay kind whose first copy was acted on is answered
	// with that first answer: "nothing was done" would be false.
	s.mu.RLock()
	replay := s.replay[ask.Kind]
	s.mu.RUnlock()
	if replay && s.db != nil {
		if id, ok, err := s.db.InboundReply(env.ID); err == nil && ok {
			if sent, rerr := s.db.ReplayOutbox(id); rerr == nil && sent {
				return
			}
		}
	}
	s.send(env, ask.Kind, &protocol.EdgeAnswer{Kind: ask.Kind, Status: protocol.AnswerExpired,
		// OWNER-WORDS
		Sentence: "the request reached Core too late — nothing was done"})
}

func (s *Server) send(env *protocol.Envelope, kind string, answer *protocol.EdgeAnswer) (int64, bool) {
	if s.reply == nil {
		return 0, false
	}
	id, err := s.reply(env, answer)
	if err != nil {
		log.Printf("asks: answer %s ask %s from %s: %v", kind, env.ID, env.Src.Station, err)
		return 0, false
	}
	return id, true
}
