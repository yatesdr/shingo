package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
)

// operator_demand_unloader.go — the unloader's U1 full-in, asked of Core.
//
// Core sizes and places a release's full (protocol.AskLoaderRetrieve, the
// side-cycle origin: dispatch.ReplenishLoader subtracts the part's live fulls
// across the unloader's windows, stops for a full of the part standing on a
// target window, and orders to a free window under its per-loader lock). The
// order reaches this Edge as a projection. The Edge decides nothing about the
// unloader's windows: it says when to ask, which is a trigger it alone sees
// (a release, CLEAR, PUSH EMPTY, the U2's pickup and landing, the startup
// sweep).
//
// AN ASK THAT DOES NOT WAIT. A trigger is an act of its own (a release, a
// CLEAR), and the U1 is its side effect at another node, so the trigger
// sends the ask and returns (AskLate with no wait, the clear_home
// precedent). Until Core's answer comes, the unloader's first window on this
// Edge carries the pending note on the board (UnloaderAskNote); the answer, whenever it
// comes, clears it. An ask Core cannot be sent (the last ack does not name
// the kind) leaves the note too: Core has not answered either way, and the
// next trigger asks again.

// unloaderAskKey is one pending U1 ask, by the window whose card says so and
// the parts it names, in order, joined.
type unloaderAskKey struct {
	window string
	parts  string
}

// MaybeCreateUnloaderFullIn (U1 of the side-cycle model) is the consumer-side
// counterpart to the loader side's L1: it asks Core for a full FG bin at the
// unloader serving payloadCode, for the operator to process.
//
// U2 (empty-out from the unloader to the supermarket) fires when the unloader
// operator taps CLEAR — driven off the clear itself (ClearBin →
// createUnloaderEmptyOut), not off this U1 completing, so a press/forklift-fed
// drain with no U1 still drains.
//
// Caller: the release commit's produce arm (release_commit.go), when
// release.PlanLeg sets LegPlan.U1: a produce, non-supply leg whose disposition
// mode is capture_lineside. A station's produce RELEASE never reaches it: the
// HMI posts a bare body there (releasedFullBody in operator-release.js). What
// does is a changeover evac at count 0, whose disposition evacDispositionForTask
// synthesises as capture_lineside (it then asks for a full of the evac order's
// part, the outgoing one), the sim's auto-operator (releaseAsPair), and any API
// client that posts capture_lineside on a produce node. The consume
// DemandSignal caller is gone (2026-08) with the kanban demand-signal route;
// fulls landing at FG storage no longer auto-trigger a U1.
//
// LOCAL FIRST. The released payload is usually already covered — in an auto_push
// unloader the pickup gate pulled its next full before the release arrived — and
// then Core's answer can only be "already coming". unloaderPullsCovered answers
// that from the order table for this payload alone, and the ask is sent only
// when a pull is possible (TestPinFold_ProduceRelease).
func (e *Engine) MaybeCreateUnloaderFullIn(payloadCode string) {
	loader, err := e.loaderStore.LoaderForPayload(domain.PayloadCode(payloadCode), domain.RoleConsume)
	if err != nil || loader == nil {
		return
	}
	if e.unloaderPullsCovered(loader, []domain.PayloadCode{domain.PayloadCode(payloadCode)}, "") {
		e.debugFn("side-cycle: unloader %s — a full-in of %s is already in flight; release offers nothing", loader.ID(), payloadCode)
		return
	}
	e.offerUnloaderFulls(loader, []domain.PayloadCode{domain.PayloadCode(payloadCode)})
}

// offerUnloaderFulls asks Core, in one ask, for a full of each payload offered
// that the unloader serves (a release's full-in: the side-cycle origin, its
// parts in the order given), and returns without waiting for the answer.
//
// Callers: the unloader's own auto_push gates (rePushOwnUnloader and
// rePushOwnUnloaderIfUncovered: CLEAR, PUSH EMPTY, the U2's pickup, the U2's
// landing), the startup sweep (pushUnloaders), and, one payload at a time,
// MaybeCreateUnloaderFullIn on a produce release that carries capture_lineside.
//
// Cost: one ask per unloader, however many payloads (a SQLite outbox row, no
// wait), after one process-node read per window until this Edge's is found
// (ownWindow); nothing at all for an unloader with no inbound source or none
// of the payloads, and no ask for one with no window here. Core answers it in
// one decision: 16 statements for a twenty-payload one-window unloader, the
// same as for one part, where twenty one-part asks cost 225
// (TestSideCycleParts_TwentyPartsAtOneWindowIsOneAsk).
func (e *Engine) offerUnloaderFulls(loader *domain.Loader, payloads []domain.PayloadCode) {
	if loader == nil {
		return
	}
	if loader.InboundSource() == "" {
		// Forklift/press-fed drain: no AMR source to pull a full from — the operator
		// (reach truck) feeds the windows directly. Core gives such an unloader no
		// release's full, so nothing is asked. The empty-out on clear
		// (outbound_dest) is independent and still fires.
		e.debugFn("side-cycle: unloader %s has no inbound_source — fed directly, no U1 asked", loader.ID())
		return
	}
	served := make([]string, 0, len(payloads))
	for _, p := range payloads {
		if p != "" && loader.ServesPayload(p) {
			served = append(served, string(p))
		}
	}
	if len(served) == 0 {
		return // nothing the unloader serves: no read, no ask
	}
	window, ok := e.ownWindow(loader)
	if !ok {
		return
	}
	e.askUnloaderFulls(loader, window, served)
}

// ownWindow is the first of the unloader's windows this Edge holds a process
// node for: where its board shows the pending note. ok is false for an
// unloader none of whose windows is this Edge's.
//
// ONE PLANT, SEVERAL EDGES. Core sends every loader to every Edge, and each
// Edge holds process nodes only for its own windows, so a sweep here walks
// other Edges' unloaders too (on the two-Edge sim stack, 2026-09-07, edge1
// walked edge2's FGN_L2_01 520 times an hour). Such an unloader is that Edge's
// to ask for: an ask from here would be answered with an order attributed to
// this station at a window no operator here works. A read error is treated as
// "not ours" too, and logged: it asks for nothing rather than for another
// Edge's window. One process-node read per window, until one is found; no
// Core round trip.
func (e *Engine) ownWindow(loader *domain.Loader) (string, bool) {
	for _, w := range loader.DeliveryNodes() {
		node, err := e.db.GetProcessNodeByCoreNodeName(string(w))
		switch {
		case err == nil && node != nil:
			return string(w), true
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			e.logFn("side-cycle: unloader %s window %s: read its process node: %v — not asked from here", loader.ID(), w, err)
			return "", false
		}
	}
	e.debugFn("side-cycle: unloader %s has no window on this Edge — another Edge's to ask for", loader.ID())
	return "", false
}

// askUnloaderFulls sends one release's full-in ask for parts at the unloader,
// with the board's pending note on window until Core answers it.
func (e *Engine) askUnloaderFulls(loader *domain.Loader, window string, parts []string) {
	key := unloaderAskKey{window: window, parts: strings.Join(parts, ",")}
	// The note is up before the ask is out, so an answer cannot beat it.
	e.setUnloaderAsk(key, true)
	if !e.CoreAnswers(protocol.AskLoaderRetrieve) {
		e.logFn("side-cycle: unloader %s parts %v: %v — the board says it is not answered", loader.ID(), parts, doesNotAnswer(protocol.AskLoaderRetrieve))
		return
	}
	req := protocol.LoaderRetrieveRequest{
		LoaderKey: string(loader.ID()), Direction: protocol.LoaderRetrieveFull, Origin: protocol.LoaderAskSideCycle,
		Payloads: parts, Count: 1,
		// NO_DEMAND: the U1 is EVENT-driven (a full appeared, a window freed),
		// has no process grain (it resolves a loader by payload) and wants a
		// fixed one; nothing's demand asked for this bin. If it were ever real
		// demand that would be a value here, not a redesign.
		OriginClass: orders.NoDemand().Class,
		// The operator processes the bin before the U2 fires: Core's sweep must
		// not confirm it on arrival.
		SkipAutoConfirm: true,
	}
	a, err := e.AskLate(protocol.AskLoaderRetrieve, req, 0, func(a *protocol.EdgeAnswer) {
		e.unloaderFullAnswered(loader, key, a)
	})
	switch {
	case err == nil:
		e.unloaderFullAnswered(loader, key, a)
	case errors.Is(err, ErrAskPending):
		// Out; the answer is taken when it comes (late above).
	default:
		e.logFn("side-cycle: unloader %s parts %v: ask not sent: %v", loader.ID(), parts, err)
	}
}

// unloaderFullAnswered is Core's answer to a release's full-in ask: the
// pending note goes, and what Core did with each part is logged. The orders
// themselves arrive as projections, ahead of the answer.
func (e *Engine) unloaderFullAnswered(loader *domain.Loader, key unloaderAskKey, a *protocol.EdgeAnswer) {
	e.setUnloaderAsk(key, false)
	res, err := askInto[protocol.LoaderRetrieveResponse](a)
	if err != nil {
		e.logFn("side-cycle: unloader %s parts %s: Core refused the U1: %v", loader.ID(), key.parts, err)
		return
	}
	if res.Skipped != "" {
		e.debugFn("side-cycle: unloader %s parts %s: no U1 — %s", loader.ID(), key.parts, res.Skipped)
	}
	for _, p := range res.Parts {
		if len(p.Created) > 0 {
			log.Printf("side-cycle: %d U1 order(s) by Core for unloader %s payload %s", len(p.Created), loader.ID(), p.PayloadCode)
			continue
		}
		e.debugFn("side-cycle: unloader %s payload %s: no U1 — %s", loader.ID(), p.PayloadCode,
			heldWhy(&protocol.LoaderRetrieveResponse{Skipped: p.Skipped, HeldBy: p.HeldBy}))
	}
}

// heldWhy is why Core made none: its Skipped, else each window held with its
// cause, in name order.
func heldWhy(res *protocol.LoaderRetrieveResponse) string {
	if res.Skipped != "" {
		return res.Skipped
	}
	held := make([]string, 0, len(res.HeldBy))
	for window, cause := range res.HeldBy {
		held = append(held, window+" "+cause)
	}
	slices.Sort(held)
	return "every window is spoken for (" + strings.Join(held, ", ") + ")"
}

// setUnloaderAsk puts up or takes down one pending U1 note, and nudges the
// boards when that changed what a board shows.
func (e *Engine) setUnloaderAsk(key unloaderAskKey, pending bool) {
	var changed bool
	if pending {
		_, had := e.unloaderAsks.LoadOrStore(key, struct{}{})
		changed = !had
	} else {
		_, changed = e.unloaderAsks.LoadAndDelete(key)
	}
	if changed {
		e.nudgeBoards()
	}
}

// UnloaderAskNote is the board's sentence for a window whose unloader has a
// U1 asked of Core and not answered (the station view's ask_pending), blank
// when none is out.
func (e *Engine) UnloaderAskNote(coreNodeName string) string {
	seen := map[string]bool{}
	var parts []string
	e.unloaderAsks.Range(func(k, _ any) bool {
		if key := k.(unloaderAskKey); key.window == coreNodeName {
			for _, p := range strings.Split(key.parts, ",") {
				if !seen[p] {
					seen[p] = true
					parts = append(parts, p)
				}
			}
		}
		return true
	})
	switch len(parts) {
	case 0:
		return ""
	case 1:
		// OWNER-WORDS
		return fmt.Sprintf("A full of %s is asked of Core; no answer yet", parts[0])
	}
	// OWNER-WORDS
	return fmt.Sprintf("Fulls of %d parts are asked of Core; no answer yet", len(parts))
}

// pushUnloaders is the unloaders' startup sweep: it walks every consume loader
// in the aggregate and asks Core, once per unloader, for a full of each allowed
// payload. Core's
// count makes it idempotent, so it is safe as a sweep: already-covered windows
// get nothing.
//
// A consume loader has a single mode: the window-queue DRAIN. It pulls a full
// whenever a window frees and one is waiting — operator-paced, one per window.
// Its stored replenishment is not read: Core refuses threshold on a consume
// loader (service.ErrConsumeThreshold), so every unloader is drained alike, and
// a release's full is not gated by it on Core either
// (TestPortedBudget_SideCycleIsNotGatedByReplenishment).
func (e *Engine) pushUnloaders() {
	loaders, err := e.loaderStore.Loaders(domain.RoleConsume)
	if err != nil {
		e.logFn("side-cycle: push-unloaders list: %v", err)
		return
	}
	for _, l := range loaders {
		e.offerUnloaderFulls(l, l.PayloadSet())
	}
}

// SweepPushUnloaders runs the consume auto-push sweep on Edge startup (after
// registration ack). Catches windows that became free while Edge was offline.
// The CAS guard serializes a re-register storm so concurrent sweeps don't stack.
func (e *Engine) SweepPushUnloaders() {
	if !e.sweepingUnloaders.CompareAndSwap(false, true) {
		return // a sweep is already running — a re-register storm must not stack them
	}
	defer e.sweepingUnloaders.Store(false)
	e.pushUnloaders()
}
