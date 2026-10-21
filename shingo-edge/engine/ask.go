package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/lockcheck"
	"shingoedge/internal/lockorder"
	"shingoedge/service"
)

// ask.go — the Edge's half of the ask pair (protocol/ask.go): a question that
// wants Core's answer, sent on the Edge outbox, answered on Core's outbox as a
// reply whose CorID is the ask's envelope ID.
//
// THE WAITER IS MEMORY ONLY. A tap blocks for a short wait; past it, the tap
// returns ErrAskPending and the screen says it was sent and is waiting. The
// answer still arrives, and whatever the waiter does or does not do, the
// answer is APPLIED BY ITS KIND (HandleEdgeAnswer): its rows go into the bins
// table, and the kind's own appliers (askAppliers) take its fact. A reply that
// lands after an Edge restart therefore heals the state the tap changed rather
// than dying with the waiter. There is no pending table.
//
// AN ASK THAT IS NEVER ANSWERED ENDS. Core answers an ask that reaches it late
// (AnswerExpired). One that never reaches Core (the Edge outbox could not
// publish it before Core stops answering it) is forgotten here AskTTL plus
// askForgetSlack after it was sent: its waiter, if any, was long gone.
//
// ONLY WHAT CORE ANSWERS IS ASKED. The heartbeat ack names the kinds this Core
// answers (EdgeHeartbeatAck.Asks); CoreAnswers(kind) is false for any other,
// and the act is refused (ErrCoreDoesNotAnswer): there is no other way to
// Core. A Core older than this Edge answers no kind; Core deploys first.

// ErrAskPending is an ask whose answer did not arrive within the wait. It was
// sent; the answer will be applied when it comes.
var ErrAskPending = errors.New("sent; Core has not answered yet")

// ErrCoreDoesNotAnswer is an act refused before anything was sent: the last
// heartbeat ack did not name its kind (no ack yet, or a Core older than this
// Edge). Wrapped with the kind by doesNotAnswer.
var ErrCoreDoesNotAnswer = errors.New("Core does not answer it")

// doesNotAnswer is the refusal of an ask of kind that the last ack did not
// name: nothing was sent.
func doesNotAnswer(kind string) error {
	// OWNER-WORDS
	return fmt.Errorf("Core does not answer the %s ask yet: %w", kind, ErrCoreDoesNotAnswer)
}

// askForgetSlack is how long past an ask's expiry the Edge keeps its pending
// entry: Core's late answer (AnswerExpired) still crosses the broker.
const askForgetSlack = 10 * time.Second

// pendingAsk is one ask sent and not yet answered.
type pendingAsk struct {
	kind    string
	expires time.Time
	done    chan *protocol.EdgeAnswer // buffered 1; nil once a waiter gave up
	// late is what the tap that sent it does with an answer that comes after
	// its wait (AskLate), in this process only: the tap's own follow-up, for
	// which the answer alone does not carry the node. Nil for none. An answer
	// after a restart has no late; its kind's applier is what heals.
	late func(*protocol.EdgeAnswer)
}

// AskLate is an Ask whose tap keeps a follow-up for an answer that comes
// after the wait: late runs on that answer, after it is applied by its kind.
func (e *Engine) AskLate(kind string, body any, wait time.Duration, late func(*protocol.EdgeAnswer)) (*protocol.EdgeAnswer, error) {
	return e.ask(kind, body, wait, late)
}

// askBook is every ask sent and not yet answered, by envelope ID, and the
// kinds the last ack said Core answers.
type askBook struct {
	mu      lockcheck.Mutex[lockorder.Asks]
	pending map[string]*pendingAsk
	kinds   map[string]bool
}

// askAppliers is what each kind's answer does on this Edge beyond its rows,
// waiter or none. Registered by the doors as they move onto the ask (one
// release of the HTTP arm beside it); a kind with none applies its rows only.
type askApplier func(e *Engine, answer *protocol.EdgeAnswer)

// setAskKinds takes the kinds the last heartbeat ack advertised. nil (an ack
// without the field) is a Core that answers no asks.
func (e *Engine) setAskKinds(kinds []string) {
	set := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		set[k] = true
	}
	e.asks.mu.Lock()
	e.asks.kinds = set
	e.asks.mu.Unlock()
}

// CoreAnswers reports whether the last heartbeat ack named kind: whether an
// ask of it will be answered.
func (e *Engine) CoreAnswers(kind string) bool {
	e.asks.mu.Lock()
	defer e.asks.mu.Unlock()
	return e.asks.kinds[kind]
}

// Ask sends one ask of kind with body and waits up to wait for its answer.
// The answer is returned as Core gave it, already applied (rows written,
// the kind's applier run). ErrAskPending when wait passes first: the ask is
// out and its answer will be applied on arrival. An error otherwise means it
// was not sent.
func (e *Engine) Ask(kind string, body any, wait time.Duration) (*protocol.EdgeAnswer, error) {
	return e.ask(kind, body, wait, nil)
}

func (e *Engine) ask(kind string, body any, wait time.Duration, late func(*protocol.EdgeAnswer)) (*protocol.EdgeAnswer, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode %s ask: %w", kind, err)
	}
	env, err := protocol.NewDataEnvelope(protocol.SubjectEdgeAsk,
		protocol.Address{Role: protocol.RoleEdge, Station: e.cfg.StationID()},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.EdgeAsk{Kind: kind, Body: raw})
	if err != nil {
		return nil, fmt.Errorf("build %s ask: %w", kind, err)
	}
	data, err := env.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode %s ask envelope: %w", kind, err)
	}
	p := &pendingAsk{kind: kind, expires: env.ExpiresAt.Add(askForgetSlack), done: make(chan *protocol.EdgeAnswer, 1), late: late}
	e.asks.mu.Lock()
	if e.asks.pending == nil {
		e.asks.pending = map[string]*pendingAsk{}
	}
	e.forgetExpiredLocked(clock.Now())
	e.asks.pending[env.ID] = p
	e.asks.mu.Unlock()
	// The pending entry is in before the row: an answer cannot beat its entry.
	if _, err := e.db.EnqueueOutbox(data, protocol.SubjectEdgeAsk); err != nil {
		e.asks.mu.Lock()
		delete(e.asks.pending, env.ID)
		e.asks.mu.Unlock()
		return nil, fmt.Errorf("enqueue %s ask: %w", kind, err)
	}
	select {
	case a := <-p.done:
		return a, nil
	case <-time.After(wait):
		e.asks.mu.Lock()
		done := p.done
		p.done = nil
		e.asks.mu.Unlock()
		// An answer that landed between the timer and the lock is in the
		// buffer, and from here on none can be: it is this tap's.
		select {
		case a := <-done:
			return a, nil
		default:
			return nil, ErrAskPending
		}
	}
}

// forgetExpiredLocked drops the pending entries past their forget time.
func (e *Engine) forgetExpiredLocked(now time.Time) {
	for id, p := range e.asks.pending {
		if now.After(p.expires) {
			delete(e.asks.pending, id)
		}
	}
}

// HandleEdgeAnswer takes Core's answer to an ask (edge.answer). It is applied
// by its kind first — its rows into the bins table, then the kind's applier —
// whether or not a waiter is alive, and then handed to the waiter if one is.
// An answer for an ask this Edge does not hold (sent before a restart, or a
// replayed copy of one already taken) is applied all the same.
func (e *Engine) HandleEdgeAnswer(env *protocol.Envelope, answer *protocol.EdgeAnswer) {
	if answer == nil {
		return
	}
	e.ApplyNodeBinsRows(answer.Rows)
	if apply := e.askAppliers[answer.Kind]; apply != nil {
		apply(e, answer)
	}
	if env == nil || env.CorID == "" {
		return
	}
	e.asks.mu.Lock()
	p := e.asks.pending[env.CorID]
	delete(e.asks.pending, env.CorID)
	var done chan *protocol.EdgeAnswer
	var late func(*protocol.EdgeAnswer)
	if p != nil {
		done, late = p.done, p.late
	}
	e.asks.mu.Unlock()
	switch {
	case done != nil:
		done <- answer
	case late != nil:
		late(answer)
	}
}

// PendingAsks is how many asks are out without an answer, for /status.
func (e *Engine) PendingAsks() int {
	e.asks.mu.Lock()
	defer e.asks.mu.Unlock()
	e.forgetExpiredLocked(clock.Now())
	return len(e.asks.pending)
}

// AskWait is how long a tap blocks for Core's answer before it returns
// ErrAskPending and the screen says the request is out. Measured on the sim
// rig (laptop, the case plant, 2026-10-20): over 55 LOAD/count/CLEAR taps that
// Core answered, the tap's round trip was p50 155 ms, p95 201 ms, max 218 ms,
// five times under the one-second bar. 2 s keeps headroom for a busy broker;
// re-measured at a plant after deploy, beside the bins push cost.
const AskWait = 2 * time.Second

// askInto turns an answer into the kind's response: decoded into T when Core
// did the act, else a *CoreRefusal with the status, the sentence, the rows and
// (a count's) epoch refresh. The rows were already applied by HandleEdgeAnswer;
// applying a refusal's rows again (Engine.refused) changes nothing.
func askInto[T any](a *protocol.EdgeAnswer) (*T, error) {
	if a == nil {
		return nil, fmt.Errorf("no answer")
	}
	if a.OK() {
		var out T
		if len(a.Body) > 0 {
			if err := json.Unmarshal(a.Body, &out); err != nil {
				return nil, fmt.Errorf("decode %s answer: %w", a.Kind, err)
			}
		}
		return &out, nil
	}
	r := &CoreRefusal{Status: a.Status, Sentence: a.Sentence, Rows: a.Rows}
	if len(a.Body) > 0 {
		var body struct {
			EpochRefresh *protocol.BinEpochRefresh `json:"epoch_refresh"`
		}
		if err := json.Unmarshal(a.Body, &body); err == nil {
			r.EpochRefresh = body.EpochRefresh
		}
	}
	return nil, r
}

// askOrRefuse sends req as an ask of kind and decodes its answer, when Core
// answers that kind; refused before sending otherwise (doesNotAnswer).
// ErrAskPending means the ask is out and unanswered after AskWait.
func askOrRefuse[T any](e *Engine, kind string, req any) (*T, error) {
	if !e.CoreAnswers(kind) {
		return nil, doesNotAnswer(kind)
	}
	a, err := e.Ask(kind, req, AskWait)
	if err != nil {
		return nil, err
	}
	return askInto[T](a)
}

// askAppliersByKind is every kind's applier: what its answer does on this Edge
// beyond its rows, applied on arrival whether or not the tap still waits, so
// each is idempotent (an answer may be replayed).
func askAppliersByKind() map[string]askApplier {
	return map[string]askApplier{
		// The reseat: with no carrier at the node, the seat's carrier left; a
		// carrier there was bound by its landing (applyReseat).
		protocol.AskReseat: applyReseat,
		// A count refused for an ended generation carries the carrier's current
		// stamp: the seat adopts it, as from a bin epoch refresh. A waiting tap
		// re-sends its count under it (declareCount); a late one leaves the
		// count to the operator.
		protocol.AskBinCount: func(e *Engine, a *protocol.EdgeAnswer) {
			if _, err := askInto[BinCountResponse](a); err != nil {
				var r *CoreRefusal
				if errors.As(err, &r) && r.EpochRefresh != nil {
					e.HandleBinEpochRefresh(*r.EpochRefresh)
				}
			}
		},
	}
}

// askLateOrRefuse is askOrRefuse for a tap with a follow-up: after an answer
// within the wait the caller runs it at once; for an answer that comes after
// AskWait, late runs it then, with the answer decoded as askInto does.
func askLateOrRefuse[T any](e *Engine, kind string, req any, late func(*T, error)) (*T, error) {
	if !e.CoreAnswers(kind) {
		return nil, doesNotAnswer(kind)
	}
	a, err := e.AskLate(kind, req, AskWait, func(a *protocol.EdgeAnswer) { late(askInto[T](a)) })
	if err != nil {
		return nil, err
	}
	return askInto[T](a)
}

// ── The read-asks ───────────────────────────────────────────────────────────
//
// A read that is a computed answer at the moment of a decision rides the ask
// pair like a write, with AskWait for its answer. An answer still pending is
// the site's "cannot check now", as an unreachable Core was over HTTP: each
// site fails closed on it. A Core that does not answer the kind is the same.

// preflightAsker is service.PreflightCorePoster over the preflight read-ask
// (protocol.AskPreflight): the changeover start's inventory gate and the
// composer's confirm sheet ask Core through it.
type preflightAsker struct{ e *Engine }

// Available reports whether Core answers the preflight ask.
func (p preflightAsker) Available() bool { return p.e.CoreAnswers(protocol.AskPreflight) }

// SetPreflightSource replaces the preflight ask as the changeover preflight's
// source: for an Edge running without a Core (the integration harness, which
// answers from the Edge's side alone), as SetReleasePointSource does for the
// release points. Call before Start.
func (e *Engine) SetPreflightSource(src service.PreflightCorePoster) {
	e.preflightChecker = service.NewPreflightChecker(e.db, src, e.cfg.StationID())
}

// PreflightInventory asks Core which of payloads has no free bin. Core reads
// the station from the ask's envelope. An error is "not checked": a refusal,
// a pending answer (ErrAskPending) or an ask that could not be sent.
func (p preflightAsker) PreflightInventory(station string, payloads []string) (*service.PreflightCoreResult, error) {
	a, err := p.e.Ask(protocol.AskPreflight, protocol.PreflightRequest{Station: station, Payloads: payloads}, AskWait)
	if err != nil {
		return nil, fmt.Errorf("preflight: %w", err)
	}
	// The wire shape, with absent a pointer so a Core too old to send it is
	// told apart from one that has every payload somewhere.
	wire, err := askInto[struct {
		Missing   []string                         `json:"missing"`
		Absent    *[]string                        `json:"absent"`
		Available []protocol.PreflightAvailability `json:"available"`
	}](a)
	if err != nil {
		return nil, fmt.Errorf("preflight: %w", err)
	}
	out := &service.PreflightCoreResult{Missing: wire.Missing}
	if wire.Absent != nil {
		out.Absent, out.AbsentKnown = *wire.Absent, true
	}
	out.Available = make([]service.PreflightCoreAvailability, len(wire.Available))
	for i, av := range wire.Available {
		out.Available[i] = service.PreflightCoreAvailability{PayloadCode: av.PayloadCode, BinCount: av.BinCount}
	}
	return out, nil
}

// HeldPartials is Core's list of a part's held partials, oldest held first,
// through the held partials read-ask (protocol.AskHeldPartials, waiting
// AskWait): the carriers a finish-later claim's REQUEST offers the operator.
// An error — a Core that does not answer the kind, a refusal, an answer still
// pending — is "cannot check now", and the REQUEST goes on without the picker,
// Core choosing the oldest, as when Core could not be read.
func (e *Engine) HeldPartials(payloadCode string) ([]protocol.HeldPartial, error) {
	if !e.CoreAnswers(protocol.AskHeldPartials) {
		return nil, fmt.Errorf("held partials: %w", doesNotAnswer(protocol.AskHeldPartials))
	}
	a, err := e.Ask(protocol.AskHeldPartials, protocol.HeldPartialsRequest{PayloadCode: payloadCode}, AskWait)
	if err != nil {
		return nil, fmt.Errorf("held partials: %w", err)
	}
	list, err := askInto[[]protocol.HeldPartial](a)
	if err != nil {
		return nil, fmt.Errorf("held partials: %w", err)
	}
	return *list, nil
}
