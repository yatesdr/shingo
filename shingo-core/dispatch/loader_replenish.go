package dispatch

import (
	"fmt"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/store/loaders"
	"shingocore/store/orders"
)

// loader_replenish.go — Core originating a loader's replenishment itself.
//
// THIS IS THE LIVE ORIGINATION PATH. Core notices a loop is below threshold and
// works out how many carriers and which windows and creates the orders, all on
// this side. The Edge's half was deleted, not left dormant — the receiver, the
// sizing arithmetic and the park-and-replay machinery are gone.
//
// The split they replaced is why the loader over-ordered on 2026-07-31: Core
// decided a loop was low, the Edge decided how much that meant, and the two
// halves counted different things while only one of them could see the plant.
//
// The caller is engine/threshold_monitor.go, in fireSignalCached. It is the one
// call site, which is what the build-it-unreached-then-switch-one-line approach
// bought — but the switch has been thrown, so read this file as production.
// It is the third of the loader triad, alongside loader_source.go and
// loader_place.go.

// ReplenishRequest names a loop that has fallen below its threshold.
type ReplenishRequest struct {
	// StationID is the Edge station the resulting orders belong to. Not a return
	// address — Core originates these — but every order carries a station and the
	// Edge board filters on it.
	StationID string
	// LoaderID is the loader to replenish.
	LoaderID int64
	// PayloadCode is the part that ran low.
	PayloadCode string
	// MemberNode is the specific member node the reading came from, or "". It
	// routes a dedicated loader's carrier to the position that actually reported
	// low rather than to whichever position sorts first.
	MemberNode string
	// Threshold and CurrentUOP are the reading that triggered this.
	Threshold  int
	CurrentUOP int
	// PerBinCapacity is how many units of the payload fit in one carrier, from
	// the payload catalog. Zero means the catalog has no answer, and the request
	// is refused rather than guessed at.
	PerBinCapacity int
	// OriginID and OriginClass attach the orders to the demand episode that
	// prompted them. Core mints the episode locally, so unlike the wire path
	// there is no sender's claim to classify — these are used as given.
	OriginID    string
	OriginClass string
}

// ReplenishResult is what happened, in enough detail to explain a quiet run.
//
// A result with Created empty is the COMMON case and not a failure: the loader
// is already full, or every window has something on the way, or the loop was not
// really below its threshold by the time this ran. Each of those is named
// separately so a reader is never left inferring which.
type ReplenishResult struct {
	// Want is how many carriers the loop needed, before any bounding. It is the
	// sizing answer, not a plan.
	Want int
	// Outstanding is how many carriers this demand episode had already ordered
	// and not yet had delivered (or cancelled) when the decision was made. It is
	// subtracted from Want. Reported because "created nothing" and "created
	// nothing because it already asked for four" are different runs, and only one
	// of them is worth a second look.
	Outstanding int
	// Created is the orders actually made, one per window.
	Created []*orders.Order
	// HeldBy names each window that could not take a carrier, and why. A window
	// with a bin already at it, or an order already inbound, appears here.
	HeldBy map[string]string
	// Skipped explains a run that created nothing for a reason that is not
	// per-window: at threshold, no capacity figure, loader serves no such
	// payload. Empty when the per-window detail in HeldBy is the whole story.
	Skipped string
}

// LoaderReplenishConfig is the loader configuration ReplenishLoader decides
// from, already read. Passed in rather than read here so the decision is
// testable against a shape without a database, and so the caller controls how
// often the config is fetched.
type LoaderReplenishConfig struct {
	Layout        string
	FunnelWindows bool
	Homes         []loaders.Home
	// NodeNames maps each home's position node id to its name.
	NodeNames map[int64]string
	Payloads  []loaders.Payload
	// Replenishment is how this loader is meant to be fed: threshold-driven, or
	// operator-driven. An operator-driven loader is refused here — see
	// ReplenishLoader.
	Replenishment string
	// InboundSource is where the empty carriers are retrieved FROM. Blank means
	// the loader is fed directly — by press, forklift or reach truck — and Core
	// must not create carrier pulls for it at all.
	InboundSource string
	// BufferDest is a staging group of empties for this loader. When set it is
	// used INSTEAD of InboundSource, with no fallback if it runs dry. See
	// emptySource for why that precedence is copied rather than chosen.
	BufferDest string
}

// emptySource answers where this loader's empty carriers are pulled from.
//
// The buffer wins outright when one is configured, with NO fallback to the
// inbound market if it is empty. That is not a preference — it reproduces
// loaderEmptySource on the Edge, which is what the plant runs today. A loader
// with a buffer configured would silently start sourcing from somewhere else the
// day Core took over ordering, which is a physical change to where a robot
// drives, delivered by a refactor.
//
// The known gap comes with it: nothing checks whether the buffer group actually
// holds an unclaimed empty, so a dry buffer produces orders that cannot source.
// The Edge has the same gap and records it in the same place. Fixing it is a
// change to both sides.
func (c LoaderReplenishConfig) emptySource() string {
	if c.BufferDest != "" {
		return c.BufferDest
	}
	return c.InboundSource
}

// LoadReplenishConfig assembles the configuration ReplenishLoader decides from.
//
// Split from the decision so the decision stays testable against a shape with no
// database in the loop, and so a caller replenishing several payloads at one
// loader can read the config once.
//
// Returns (zero, false, nil) for a loader that does not exist or is archived —
// an absent loader is a normal answer at a decision point, not a failure. A read
// error is a failure and is returned as one: deciding replenishment against
// config Core could not read is how a loader gets fed from the wrong place.
func (d *Dispatcher) LoadReplenishConfig(loaderID int64) (LoaderReplenishConfig, bool, error) {
	if loaderID <= 0 {
		return LoaderReplenishConfig{}, false, nil
	}
	cfg, err := d.db.GetLoaderConfig(loaderID)
	if err != nil {
		return LoaderReplenishConfig{}, false, fmt.Errorf("loader %d config: %w", loaderID, err)
	}
	if cfg == nil || cfg.Loader.ArchivedAt != nil {
		return LoaderReplenishConfig{}, false, nil
	}
	names, err := d.db.LoaderMemberNodeNames(loaderID)
	if err != nil {
		return LoaderReplenishConfig{}, false, fmt.Errorf("loader %d member nodes: %w", loaderID, err)
	}
	return LoaderReplenishConfig{
		Layout:        cfg.Loader.Layout,
		FunnelWindows: cfg.Loader.FunnelWindows,
		Homes:         cfg.Homes,
		NodeNames:     names,
		Payloads:      cfg.Payloads,
		Replenishment: cfg.Loader.Replenishment,
		InboundSource: cfg.Loader.InboundSource,
		BufferDest:    cfg.Loader.BufferDest,
	}, true, nil
}

// episodeOutstanding counts the live orders a demand episode has already
// created — the carriers it is still waiting on.
//
// A blank origin returns zero with no read at all, and that is not merely an
// optimisation. orders.origin_id is a UUID column that rejects "", so the query
// would ERROR rather than match nothing, and the fail-closed caller would then
// refuse to replenish every unattributed loader in the plant.
func (d *Dispatcher) episodeOutstanding(originID string) (int, error) {
	if originID == "" {
		return 0, nil
	}
	return d.db.CountLiveOrdersByOrigin(originID)
}

// ReplenishLoader decides and creates a loader's replenishment orders.
//
// The order of the decision is deliberate and each step exists because skipping
// it caused a real failure:
//
//  1. SIZE the ask from the reading. What the loop needs, rounded up to whole
//     carriers, with a broken (negative) reading sized from zero.
//  2. SUBTRACT WHAT THIS DEMAND ALREADY ASKED FOR. The episode's own live orders
//     are carriers already coming; asking again for the same ones is how a dry
//     market turned into 241 duplicate orders (see below).
//  3. RESOLVE the windows. Which nodes may take a carrier and how many may be
//     inbound at once — the never-2N budget.
//  4. BOUND the ask by the windows. want = min(sizing, free windows). The sizing
//     answer alone is what over-ordered: it says what the loop needs with no idea
//     how many places there are to put it.
//  5. CHECK EACH WINDOW at decision time, and skip the blocked ones by name.
//
// One order per window, never two at the same window: a window holds one
// physical carrier.
//
// STEP 2 IS THE CONTRACT THE EDGE USED TO HOLD, and losing it in the cutover is
// the whole of Springfield 2026-08-03. The Edge's seam sized against
// `CurrentUOP + inFlight*capacity` — it projected the orders it had already
// created into the total and asked for the remainder. Core replaced that with a
// per-window capacity check, which reads `status != 'queued'` and therefore
// cannot see an order that has been created but has not yet been able to source.
// A loader whose empty market was dry accumulated 241 identical queued
// retrieve_empty orders at one window, roughly one a minute, because to that
// check every one of them was the first.
//
// Carriers, not UOP, but the same arithmetic: BinsToReachThreshold already
// converts the reading into whole carriers, and an outstanding order IS one
// carrier. Subtracting them there is the same projection stated in the unit the
// decision is actually made in.
//
// It returns an error only when something genuinely went wrong — a failed write,
// a loader that could not be admitted. "Nothing to do" comes back as a result
// with an empty Created and a reason, because a caller that treats quiet as
// failure will log alarms all shift.
func (d *Dispatcher) ReplenishLoader(req ReplenishRequest, cfg LoaderReplenishConfig) (ReplenishResult, error) {
	res := ReplenishResult{HeldBy: map[string]string{}}

	// A loader with no identity has no configuration to decide from. Legacy
	// bindings carry no loader id, and a request built from one would silently
	// resolve against an empty config and create nothing while looking like it
	// tried.
	if req.LoaderID <= 0 {
		res.Skipped = "request names no loader (a legacy binding with no loader id); nothing to replenish against"
		return res, nil
	}

	// AN OPERATOR-DRIVEN LOADER IS NOT AUTOMATICALLY REPLENISHED, and this guard
	// is the Core half of one that already exists on the Edge — where it has been
	// quietly doing real work.
	//
	// The combination that reaches here is legal and derivable: a produce loader
	// set to operator replenishment with a leftover threshold on one of its
	// payloads. Core's registry derivation keeps that threshold (it only zeroes
	// the consume case), and the loader-config validation refuses only
	// consume+threshold, so the monitor fires at such a loader today. The Edge
	// swallows it. Without this, Core taking over ordering would turn a signal
	// nobody acts on into robot-delivered carriers arriving at a loader whose
	// whole configuration says a person stages it.
	if cfg.Replenishment == loaders.ReplenishmentOperator {
		res.Skipped = "loader is operator-driven: a person stages it, so no carriers are ordered automatically"
		d.dbg("loader_replenish loader=%d payload=%s: operator-driven, not replenishing", req.LoaderID, req.PayloadCode)
		return res, nil
	}

	// A loader nobody retrieves for. Blank source is a real, supported
	// configuration — the loader is fed by hand — and creating a carrier pull for
	// it would send a robot to fetch from nowhere.
	if cfg.emptySource() == "" {
		res.Skipped = "loader has no inbound source or buffer: it is fed directly, so Shingo pulls no carriers for it"
		return res, nil
	}

	want, outcome, detail := BinsToReachThreshold(req.Threshold, req.CurrentUOP, req.PerBinCapacity)
	if outcome != SizingOK {
		res.Skipped = detail
		d.dbg("loader_replenish loader=%d payload=%s outcome=%s: %s", req.LoaderID, req.PayloadCode, outcome, detail)
		return res, nil
	}
	res.Want = want

	// What this demand already has coming, subtracted from what it is about to
	// ask for.
	//
	// An unattributed request (no episode) gets no such bound, and that is stated
	// rather than defaulted: without an origin there is no way to tell this
	// request's outstanding orders from anyone else's, and guessing would mean
	// suppressing a legitimate ask on someone else's evidence.
	outstanding, err := d.episodeOutstanding(req.OriginID)
	if err != nil {
		// Fail CLOSED — the same posture as the capacity gate one step down, and
		// for the same reason. If the outstanding orders cannot be read, creating
		// more is exactly the failure this read exists to prevent.
		res.Skipped = "could not read what this demand already ordered; not adding to it"
		d.dbg("loader_replenish loader=%d payload=%s: read outstanding for origin %s: %v",
			req.LoaderID, req.PayloadCode, req.OriginID, err)
		return res, nil
	}
	res.Outstanding = outstanding
	if remaining := want - outstanding; remaining < want {
		if remaining <= 0 {
			res.Skipped = fmt.Sprintf("this demand already has %d carrier(s) outstanding for a need of %d; nothing more to order until they land or die",
				outstanding, want)
			d.dbg("loader_replenish loader=%d payload=%s: want=%d already-outstanding=%d — nothing to add",
				req.LoaderID, req.PayloadCode, want, outstanding)
			return res, nil
		}
		want = remaining
	}

	if NegativeCurrentUOP(req.CurrentUOP) {
		// Logged, not suppressed. A negative total means material moved off the
		// books, which is exactly when the loop needs stock — but the number that
		// produced this ask is known-bad and the record should say so.
		d.dbg("loader_replenish loader=%d payload=%s currentUOP=%d is negative — sized the gap from 0 (ledger is off the books; ordering continues)",
			req.LoaderID, req.PayloadCode, req.CurrentUOP)
	}

	targets, budget := loaders.DeliveryTargets(loaders.DeliveryTargetsInput{
		Layout:        cfg.Layout,
		FunnelWindows: cfg.FunnelWindows,
		Homes:         cfg.Homes,
		NodeNames:     cfg.NodeNames,
		Payloads:      cfg.Payloads,
		Member:        req.MemberNode,
		PayloadCode:   req.PayloadCode,
	})
	if len(targets) == 0 || budget <= 0 {
		res.Skipped = fmt.Sprintf("loader %d has no delivery target for payload %q", req.LoaderID, req.PayloadCode)
		d.dbg("loader_replenish loader=%d payload=%s: no target", req.LoaderID, req.PayloadCode)
		return res, nil
	}

	// THE LOOP IS THE BOUND, and it is worth saying so because a separate
	// min(want, budget) clamp reads like the safety here and is not: it cannot
	// fire. DeliveryTargets returns a budget equal to its target count in every
	// shape that exists — spread gives every window and the window count, funnel
	// and dedicated each give one node and 1 — so iterating targets already caps
	// creation at the budget. A clamp above this loop would be unreachable code
	// dressed as a guard, which is worse than no guard, because the next reader
	// trusts it.
	//
	// What actually bounds the ask, in order: the target list caps it at the
	// number of places a carrier can go, `want` stops early when the loop needs
	// fewer than that, and the per-window check below removes the ones that
	// cannot take one right now.
	for _, t := range targets {
		if len(res.Created) >= want {
			break
		}
		// Decision-time capacity, per window. Pass 0 for the exclude-order id:
		// there is no order of ours yet to exclude, which is the whole difference
		// between deciding and retrying.
		if blocked, block := CheckDropoffCapacity(d.db, t.NodeName, 0); blocked {
			res.HeldBy[t.NodeName] = string(block.Cause)
			continue
		}
		// A carrier has already been ASKED FOR here and the ask has not been given
		// up. The check above cannot see it while it sits `queued` — that
		// blindness is exactly what produced the duplicates — and it is asked from
		// ANY origin on purpose: two payloads at one shared-window loader are two
		// separate episodes that cannot see each other, so an episode-scoped
		// question here would let both put a carrier on the same window. "One
		// order per window" has to mean one order, not one per asker.
		//
		// ASKS, not arrivals. A swap's evac leg names this home as its delivery
		// node but never asked for a carrier — it is a return trip. Counting it as
		// an outstanding ask is what deadlocked SMN_030 for 8h57m on 2026-08-05:
		// the evac held the window, so the carrier its own supply sibling was
		// waiting for could not be ordered. Returns are covered by
		// CheckDropoffCapacity's in-flight arm above, which is the physical
		// question; this is the logical one. See
		// orders.CountLiveCarrierRequestsByDeliveryNode.
		live, lerr := d.db.CountLiveCarrierRequestsByDeliveryNode(t.NodeName)
		if lerr != nil {
			// Fail closed, same as every other unreadable occupancy question.
			res.HeldBy[t.NodeName] = "window-check-failed"
			d.dbg("loader_replenish loader=%d payload=%s window=%s: count live carrier requests: %v",
				req.LoaderID, req.PayloadCode, t.NodeName, lerr)
			continue
		}
		if live > 0 {
			res.HeldBy[t.NodeName] = "window-order-open"
			continue
		}
		order, err := d.admitReplenishOrder(req, cfg, t)
		if err != nil {
			// Stop rather than press on. A failure here is a write failure or a
			// refused reference, and the windows after this one would fail the same
			// way; returning what was created keeps the caller's picture true.
			d.dbg("loader_replenish loader=%d payload=%s window=%s: %v", req.LoaderID, req.PayloadCode, t.NodeName, err)
			return res, err
		}
		res.Created = append(res.Created, order)
		d.queueOrderInternal(order, req.StationID, req.PayloadCode)
	}

	// The decision record. want= is what the loop needed, targets= how many places
	// there were to put it, created= what was made, held= how many windows refused
	// one. A run where want greatly exceeds targets is the over-ordering shape
	// being correctly refused, and it should be visible as such rather than
	// looking like a loader that under-delivered.
	d.dbg("loader_replenish loader=%d payload=%s want=%d budget=%d targets=%d created=%d held=%d",
		req.LoaderID, req.PayloadCode, res.Want, budget, len(targets), len(res.Created), len(res.HeldBy))
	return res, nil
}

// admitReplenishOrder builds and admits one carrier pull.
//
// It goes through admitOrder, the same body wire-originated orders go through,
// rather than writing a row itself — so a Core-originated order gets the same
// reference checks, the same synthetic-destination resolution, and the same
// insert. A second writer here is exactly the drift the order-writer
// consolidation removed one layer down.
func (d *Dispatcher) admitReplenishOrder(req ReplenishRequest, cfg LoaderReplenishConfig, t loaders.Target) (*orders.Order, error) {
	order, err := d.AdmitCoreAsk(CoreAskSpec{
		UUIDPrefix:   "core-l1-",
		StationID:    req.StationID,
		SourceNode:   cfg.emptySource(),
		DeliveryNode: t.NodeName,
		OriginID:     req.OriginID,
		OriginClass:  req.OriginClass,
	})
	if err != nil {
		return nil, fmt.Errorf("admit replenishment for loader %d at %s: %w", req.LoaderID, t.NodeName, err)
	}
	return order, nil
}

// CoreAskSpec is one Core-authored carrier pull: everything that differs between
// the two things Core asks for on its own initiative.
//
// TWO CALLERS, ONE DOOR. The loader replenishment loop and the maintained-group
// level keeper both mint retrieve_empty orders nobody on the Edge requested. What
// they share — the order shape, the admit, the projection, the origin-class rule
// — is everything except the six fields below, and a second copy of that body is
// exactly the drift the order-writer consolidation removed one layer down.
type CoreAskSpec struct {
	// UUIDPrefix marks where the order came from, readably, without a join.
	// Edge-authored UUIDs are bare; Core's say so at a glance and in a log line.
	// "core-l1-" is the loader loop, "core-mnt-" the level keeper.
	UUIDPrefix   string
	StationID    string
	SourceNode   string
	DeliveryNode string
	// PayloadDesc is the human line on the Edge board card. The carrier is
	// generic so PayloadCode stays blank (below), which leaves the card with
	// nothing to render — this is what it renders instead ("empty 45x58x32").
	PayloadDesc string
	OriginID    string
	OriginClass string
}

// AdmitCoreAsk builds and admits one Core-authored carrier pull.
//
// It goes through admitOrder, the same body wire-originated orders go through,
// rather than writing a row itself — so a Core-originated order gets the same
// reference checks, the same synthetic-destination resolution, and the same
// insert.
func (d *Dispatcher) AdmitCoreAsk(spec CoreAskSpec) (*orders.Order, error) {
	order := &orders.Order{
		EdgeUUID:     spec.UUIDPrefix + uuid.New().String(),
		StationID:    spec.StationID,
		OrderType:    OrderTypeRetrieveEmpty,
		Status:       StatusPending,
		Quantity:     1,
		SourceNode:   spec.SourceNode,
		DeliveryNode: spec.DeliveryNode,
		// No payload code, deliberately. An empty carrier is generic: the part
		// binds when the loader fills it. Stamping the payload here would tag a
		// carrier that is supposed to be untagged, and that tag then picks the
		// robot — the same rule lookupPayloadMeta states from the Edge side.
		PayloadCode:  "",
		PayloadDesc:  spec.PayloadDesc,
		SourceIntent: SourceIntentForType(OrderTypeRetrieveEmpty),
		// Origin attaches locally. Unlike the wire path there is no sender's claim
		// to classify: Core opened this episode and knows what it is.
		OriginID:    spec.OriginID,
		OriginClass: spec.OriginClass,
	}
	// Origin class, stated rather than defaulted. An order carrying an episode id
	// IS attached by definition, and leaving the class blank would stamp every
	// Core-originated ask as an orphan — turning the demand-grain bucket that
	// exists to find lost attributions into a bucket containing nothing but
	// correctly-attributed orders.
	//
	// A caller that supplies neither is genuinely unattributed, and orphan is the
	// honest reading rather than a guess that keeps the bucket quiet.
	if order.OriginClass == "" {
		if order.OriginID != "" {
			order.OriginClass = protocol.OriginClassAttached
		} else {
			order.OriginClass = protocol.OriginClassOrphan
		}
	}
	if lerr := d.lifecycle.admitOrder(order); lerr != nil {
		if lerr.Err != nil {
			return nil, lerr.Err
		}
		return nil, fmt.Errorf("%s: %s", lerr.Code, lerr.Detail)
	}
	// THE case the projection exists for. Nobody on the Edge asked for this
	// order, so nobody there has a row for it; without the projection the
	// operator watches a robot arrive at a window with nothing on the board to
	// say why.
	d.lifecycle.projectOrder(order)
	return order, nil
}

// QueueCoreAsk puts an admitted Core ask into the fulfillment scanner's retry
// set. Separate from AdmitCoreAsk because the loader loop queues per window
// inside its own loop and wants the admit failure and the queue failure
// distinguishable.
func (d *Dispatcher) QueueCoreAsk(order *orders.Order, stationID string) {
	d.queueOrderInternal(order, stationID, "")
}
