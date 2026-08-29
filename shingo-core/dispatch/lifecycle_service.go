package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/dispatch/binresolver"
	"shingocore/fleet"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

type lifecycleError struct {
	Code   string
	Detail string
	Err    error
}

func (e *lifecycleError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Detail
}

func lifecycleErr(code, detail string, err error) *lifecycleError {
	return &lifecycleError{Code: code, Detail: detail, Err: err}
}

type LifecycleService struct {
	db          *store.DB
	backend     fleet.Backend
	emitter     Emitter
	resolver    NodeResolver
	binManifest *service.BinManifestService
	debug       func(string, ...any)

	// futility is the rate-per-tuple detector (futility.go). nil = disabled,
	// which is the default until a plant turns it on in YAML; every method on
	// it is nil-safe.
	futility *FutilityDetector

	// serves answers whether Core can carry out a kind of order, so intake can
	// refuse one it cannot before writing anything down. It is the planner's
	// own registry, reached through a closure because the planner is built
	// after this service. nil means every type is admitted, which is what a
	// LifecycleService constructed on its own in a test gets.
	serves func(protocol.OrderType) bool
}

func newLifecycleService(db *store.DB, backend fleet.Backend, emitter Emitter, resolver NodeResolver, binManifest *service.BinManifestService, debug func(string, ...any)) *LifecycleService {
	return &LifecycleService{db: db, backend: backend, emitter: emitter, resolver: resolver, binManifest: binManifest, debug: debug}
}

func (s *LifecycleService) dbg(format string, args ...any) {
	if s.debug != nil {
		s.debug(format, args...)
	}
}

func (s *LifecycleService) CreateInboundOrder(stationID string, p *protocol.OrderRequest) (*orders.Order, string, *lifecycleError) {
	payloadCode := p.PayloadCode
	// Wire-protocol normalization: edge may send OrderTypeRetrieve + RetrieveEmpty=true.
	// Promote that pair to the canonical OrderTypeRetrieveEmpty so downstream code
	// dispatches on a single field. Preserves p.PayloadDesc as the operator's note;
	// it used to be clobbered with the literal string "retrieve_empty" here.
	orderType := p.OrderType
	if p.RetrieveEmpty && p.OrderType == OrderTypeRetrieve {
		orderType = OrderTypeRetrieveEmpty
	}
	// Refuse a kind of order Core cannot carry out, here, before anything
	// exists.
	//
	// The type used to be taken verbatim all the way into the INSERT. Nothing
	// looked at it until the planner went to pick a handler, found none, and
	// failed the order — by which point the row was written, its history had
	// two entries, and order.received had been announced for an order that was
	// never going to move. Springfield has one of these from June: a `store`
	// order, a word that was never a kind of order at all, sitting in the table
	// as a failure that looks like something was attempted.
	//
	// Asked of the planner rather than a list kept here. A list would be a copy
	// of the planner's map maintained by hand, and the first thing to disagree
	// with it would be a planner registered at runtime — which is a supported
	// thing to do, so a hardcoded list would quietly break it.
	//
	// AFTER the retrieve_empty promotion above, deliberately: an older Edge
	// sends retrieve plus a flag, and it is the promoted value that has to be
	// servable, because the promoted value is what gets planned.
	if s.serves != nil && !s.serves(orderType) {
		return nil, "", lifecycleErr(string(protocol.TermUnknownType),
			fmt.Sprintf("unknown order type: %s", orderType), nil)
	}
	// A move carries one bin, always: one robot, one bin, one bin_id on the row.
	// The Edge will send a larger count — its own screens let an operator type
	// one — and we copied it verbatim, so a move could be stored, displayed and
	// confirmed as "4" while moving a single bin. Nothing in Core branches on
	// the number; the one thing it does is get printed on the order screen, so
	// the whole effect of the old value was to tell a person something untrue.
	//
	// Floored for moves ONLY. On a retrieve the count is the Edge's to declare —
	// the batch path reads it to decide how many separate orders to create, and
	// the Edge declares it back on confirm.
	quantity := p.Quantity
	if orderType == OrderTypeMove {
		quantity = 1
	}
	// Intake site 1 of 3 for the demand grain. Stamped from the envelope here,
	// where the sender's statement is in hand; never inferred later from a NULL.
	originID, originClass := classifyInboundOrigin(p.OriginID, p.OriginClass, stationID, p.OrderUUID)
	order := &orders.Order{
		EdgeUUID:        p.OrderUUID,
		StationID:       stationID,
		OrderType:       orderType,
		Status:          StatusPending,
		Quantity:        quantity,
		SourceNode:      p.SourceNode,
		DeliveryNode:    p.DeliveryNode,
		Priority:        p.Priority,
		PayloadDesc:     p.PayloadDesc,
		PayloadCode:     payloadCode,
		SkipAutoConfirm: p.SkipAutoConfirm,
		// Stage 4: stamp the sourcing intent as data at intake (label→data
		// carve-out) so the finder + scanner read it, not the type.
		SourceIntent: SourceIntentForType(orderType),
		OriginID:     originID,
		OriginClass:  originClass,
	}
	if lerr := s.admitOrder(order); lerr != nil {
		return nil, "", lerr
	}
	// Emitted by the CALLER, not by admitOrder, and deliberately with the RAW
	// wire values: p.OrderType rather than the promoted orderType, and
	// p.DeliveryNode rather than the possibly-resolved order.DeliveryNode. Those
	// are what the sender said, and this event reports receipt of a request. A
	// Core-originated order has no sender and emits its own event with its own
	// values, which is why this does not belong inside the shared body.
	s.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, p.OrderType, payloadCode, p.DeliveryNode)
	// The Edge that sent this request already has a row for it — it created the
	// row before sending. Projecting it back would be Core telling a station
	// about its own order.
	//
	// It is projected anyway, deliberately, and this is the option the plan
	// called worth taking: it gives the projection path real production traffic
	// before it becomes load-bearing, on orders that are low-volume and already
	// visible, where a bug shows up as a redundant update rather than as a
	// missing order. The applier is an idempotent upsert by UUID, so the second
	// arrival is a no-op — and if it is NOT a no-op, that is precisely the defect
	// worth finding here rather than after the cutover.
	s.projectOrder(order)
	return order, payloadCode, nil
}

// projectOrder pushes an order's row down to the station that owns it.
//
// SCOPE IS DEFINED BY admitOrder, on purpose. Every order admitted through that
// body projects, and nothing else does — compound children and complex orders
// stay out, because a child is a step of a parent the Edge already knows about
// and complex orders were excluded by name. Tying the scope to a function rather
// than to a list means a new door that wants projection gets it by routing
// through admission, which is where it belongs anyway.
func (s *LifecycleService) projectOrder(order *orders.Order) {
	if order == nil || order.StationID == "" {
		return
	}
	s.emitter.ProjectOrder(order.StationID, ProjectionFor(order))
}

// admitOrder is the wire-free middle of order intake: validate the payload,
// resolve a synthetic delivery node, insert the row, and mark it pending. It
// takes a fully-built *orders.Order because that is already the shape every
// caller has — a parallel spec struct would be a second field list to keep in
// step with the first, which is the failure mode TestCensus_OrdersTableInsertStatements
// exists to prevent one layer down.
//
// It is extracted so a Core-originated order can be admitted through exactly the
// same body as a wire-originated one, without synthesizing a fake OrderRequest.
// Note what stays OUTSIDE: wire normalization, origin classification (its own
// comment says it is stamped "where the sender's statement is in hand", and a
// Core-authored order has no sender), and the received event.
//
// This is NOT the only way an order comes into existence in Core — see
// TestCensus_OrderCreationPaths for the seven other writers. Anything that hangs
// off this function covers what routes through it and nothing else.
func (s *LifecycleService) admitOrder(order *orders.Order) *lifecycleError {
	destNode, lerr := s.checkOrderRefs(order)
	if lerr != nil {
		return lerr
	}
	resolvedAt, lerr := s.resolveSyntheticDestination(order, destNode)
	if lerr != nil {
		return lerr
	}
	if err := s.db.CreateOrder(order); err != nil {
		return lifecycleErr("internal_error", err.Error(), err)
	}
	// THE SELECTOR'S OWN CLOCK READING, WRITTEN AFTER THE ROW EXISTS. There is no
	// earlier seam: resolution rewrites a field on a struct that has no id yet, so
	// the stamp cannot ride the INSERT without threading a diagnostic column
	// through Order, SelectCols and ScanOrders — the hot read path — for one
	// reader that consults it once per burial. orphan_aged_at set the precedent.
	//
	// Logged and swallowed. This is the burial tripwire's input, not the order's
	// business, and an order that has already been created must not fail because a
	// diagnostic write did.
	if !resolvedAt.IsZero() {
		if err := s.db.StampDestinationResolved(order.ID, resolvedAt); err != nil {
			log.Printf("dispatch: stamp destination-resolved for order %d: %v "+
				"(the burial tripwire falls back to fleet-commit for this order)", order.ID, err)
		}
	}
	// The pending→pending status write that stood here is gone. The order was
	// already at pending — the INSERT set it — and the write's only product was
	// the history row saying the order began, which orders.Create now writes in
	// the INSERT's own transaction for every door. Keeping it would have put a
	// second `pending` row on every wire-intake order.
	return nil
}

// checkOrderRefs answers whether the things an order names actually exist.
//
// It reads and reports; it writes nothing and changes nothing on the order.
// That is what makes it safe for any door to call, which is the point of it
// being separate from the resolution step below.
//
// It hands back the delivery node it looked up, so a caller that goes on to
// resolve a synthetic destination does not pay for the same query twice. A
// blank delivery node is not an error — a complex order's last dropoff is
// legitimately deferred sometimes — so a nil node with no error means "nothing
// was named, so there was nothing to check".
func (s *LifecycleService) checkOrderRefs(order *orders.Order) (*nodes.Node, *lifecycleError) {
	if order.PayloadCode != "" {
		if _, err := s.db.GetPayloadByCode(order.PayloadCode); err != nil {
			return nil, lifecycleErr("payload_error", fmt.Sprintf("payload %q not found", order.PayloadCode), err)
		}
	}
	if order.DeliveryNode == "" {
		return nil, nil
	}
	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		return nil, lifecycleErr("invalid_node", fmt.Sprintf("delivery node %q not found", order.DeliveryNode), err)
	}
	return destNode, nil
}

// resolveSyntheticDestination points a delivery node that names a GROUP at one
// of its children, by rewriting the order's delivery node.
//
// WIRE INTAKE ONLY, and the last line is why. It ASSIGNS to
// order.DeliveryNode. A complex order has already chosen the concrete nodes its
// robot will visit and stored them in its steps, and its final dropoff is
// rewritten from this same field at dispatch — so running this on a complex
// order would re-aim a robot mid-route, quietly, in a way that reads as a
// routing bug rather than a validation one.
//
// That is the whole reason this is not part of checkOrderRefs. The two look
// like one job at intake and are not: one asks whether a thing exists, the
// other changes where an order is going.
//
// ── IT RETURNS WHEN IT LOOKED ─────────────────────────────────────────────
//
// A zero time means no choice was made here — not synthetic, no resolver, or a
// full group left to queue. A non-zero time is the instant the store-slot
// selector approved this destination, and it is the only moment in the system at
// which that guard was consulted for this order. admitOrder writes it down
// because the burial tripwire has to be able to ask "could the selector have
// seen this claim", and until this column existed it was reduced to comparing
// against the fleet-commit — an event that can trail the choice by minutes.
func (s *LifecycleService) resolveSyntheticDestination(order *orders.Order, destNode *nodes.Node) (time.Time, *lifecycleError) {
	if destNode == nil || !destNode.IsSynthetic || s.resolver == nil {
		return time.Time{}, nil
	}
	requested := order.DeliveryNode

	// ── MG4-2: THE BUILT-BUT-NIL PARAMETER GETS ITS CONSUMER ────────────────
	//
	// binTypeID has been threaded through ResolveStore since the resolver had a
	// per-child Allowed Bins check, and every caller passed nil — so a check that
	// existed was never exercised. The level keeper is the first caller that
	// knows the answer: its ask carries an origin, and that episode names exactly
	// one carrier type.
	//
	// TYPED PLACEMENT IS NOT COSMETIC HERE. A group declaring "four 45x58 and two
	// 45x48" has positions that accept one and not the other; an untyped resolve
	// picks the emptiest slot and can put a 45x48 where only a 45x58 fits, which
	// the floor discovers when the robot arrives. It also makes MG4-1's level cap
	// evaluate per type rather than on the group total, which is the tighter and
	// more correct of the two readings.
	//
	// A FAILED READ RESOLVES UNTYPED rather than refusing. That is the behaviour
	// every order had before this line existed, so the failure mode is "no worse
	// than yesterday" rather than a placement that does not happen.
	var binTypeID *int64
	if order.OriginID != "" {
		id, terr := s.db.MaintainedBinTypeIDForOrigin(order.OriginID)
		if terr != nil {
			s.dbg("intake: maintained bin type for origin %s unreadable (%v) — resolving untyped",
				order.OriginID, terr)
		} else {
			binTypeID = id
		}
	}

	result, err := s.resolver.Resolve(destNode, binresolver.ResolveModeStore, order.PayloadCode, binTypeID, digAskerFor(order))
	if err != nil {
		// A full group (ResolutionCapacity — "no available slot in node group
		// X") must NOT fail the operator's action. Leave the synthetic
		// destination on the order and create it: planMove resolves a concrete
		// child at dispatch time, and CheckDropoffCapacity parks it in `queued`
		// until a slot frees — the same queue-don't-fail contract every other
		// dropoff path already honors. Structural/transient failures (no
		// enabled children, DB error) still hard-fail so a real misconfiguration
		// surfaces to the operator instead of queueing forever.
		if class, _ := classifyResolutionError(err); class != ResolutionCapacity {
			return time.Time{}, lifecycleErr("resolution_failed", fmt.Sprintf("cannot resolve synthetic node %s: %v", requested, err), err)
		}

		// ── MG6-1: TRY THE OVERFLOW ─────────────────────────────────────────
		//
		// A maintained group at its level refuses the push. If somebody named an
		// overflow destination for it, the carrier goes there instead of parking.
		//
		// CORE-SIDE ONLY. Edge keeps naming the group unconditionally — it has no
		// level to read and should not grow one — so this is Core answering "where
		// does this actually go" at admission, which is the same shape as every
		// other placement decision in the system.
		//
		// ONE HOP, NOT A CHAIN. An overflow whose own group is full parks; it does
		// not consult ITS overflow. A chain is a loop with extra steps the first
		// time two groups name each other, and "the carrier went three groups away
		// from where anybody expected" is worse than a park an operator can see.
		//
		// NO MID-ROUTE RE-AIM, EVER. This runs at ADMISSION, before a robot has
		// been told anything. A push already in flight that arrives at a
		// just-topped group parks at its dropoff with a named cause — it does not
		// get redirected underneath the robot carrying it.
		if node, stamp, ok := s.tryOverflow(order, destNode); ok {
			s.dbg("intake: %s at level — overflowing to %s", requested, node)
			order.DeliveryNode = node
			return stamp, nil
		}

		s.dbg("intake: synthetic %s full — creating order against group so it queues: %v", requested, err)
		// NO STAMP, and it is the honest answer rather than an omission: nothing was
		// chosen. planMove resolves this order's destination at dispatch, where the
		// selector runs close enough to the commit that the fallback comparison is
		// the right one.
		return time.Time{}, nil
	}
	s.dbg("resolved synthetic %s -> %s", requested, result.Node.Name)
	order.DeliveryNode = result.Node.Name
	return clock.Now().UTC(), nil
}

// resolveIngestBin finds the bin an ingest should manifest.
//
// Two callers, two ways to identify the bin:
//   - Manual / HTTP ingest carries a real BinLabel (an operator scanned the
//     tote), so we look it up by name directly.
//   - Headless produce-finalize (Edge operator_produce.go) ships a BLANK label
//     plus the SourceNode. Edge knows the contents (payload + UOP) but tracks the
//     active bin by id, not label (loaded_bin_label was retired), so it can't
//     name the tote — it tells Core which node it's parked at and lets Core
//     resolve identity from location. That's the same look-by-node/group Core
//     already uses for consume (FindEmptyCompatible*); the ingest was the lone
//     path still demanding a label. This completes the "bin label resolved by
//     core from node contents" contract Edge has documented since 2026-04-30.
func (s *LifecycleService) resolveIngestBin(p *protocol.OrderIngestRequest) (*bins.Bin, *lifecycleError) {
	// Explicit bin id wins: the release-time produce manifest (Fix D) pins
	// the departing bin by the id Core seeded at delivery, because node-based
	// resolution can land on the freshly-indexed tote by processing time.
	if p.BinID != 0 {
		bin, err := s.db.GetBin(p.BinID)
		if err != nil || bin == nil {
			return nil, lifecycleErr("bin_error", fmt.Sprintf("ingest bin id %d not found", p.BinID), err)
		}
		return bin, nil
	}
	if p.BinLabel != "" {
		bin, err := s.db.GetBinByLabel(p.BinLabel)
		if err != nil {
			return nil, lifecycleErr("bin_error", fmt.Sprintf("bin %q not found", p.BinLabel), err)
		}
		return bin, nil
	}
	if p.SourceNode == "" {
		return nil, lifecycleErr("bin_error", "ingest carries neither bin_label nor source_node",
			errors.New("ingest: no bin identity"))
	}
	node, err := s.db.GetNodeByDotName(p.SourceNode)
	if err != nil || node == nil {
		return nil, lifecycleErr("invalid_node", fmt.Sprintf("ingest source node %q not found", p.SourceNode), err)
	}
	atNode, err := s.db.ListBinsByNode(node.ID)
	if err != nil {
		return nil, lifecycleErr("bin_error", fmt.Sprintf("list bins at node %q failed", p.SourceNode), err)
	}
	if len(atNode) == 0 {
		return nil, lifecycleErr("bin_error", fmt.Sprintf("no bin parked at node %q to ingest", p.SourceNode),
			errors.New("ingest: empty node"))
	}
	// A node can transiently hold the outgoing full and an incoming empty
	// mid-swap; manifest the one whose payload Edge just reported. Fall back to
	// the only/first bin (the freshly-filled produce bin carries no core-side
	// payload until this very ingest sets it, so the match misses on purpose).
	if p.PayloadCode != "" {
		for _, b := range atNode {
			if b.PayloadCode == p.PayloadCode {
				return b, nil
			}
		}
	}
	return atNode[0], nil
}

// ApplyIngestManifest records a produce-finalize on the target bin: an audited
// inventory manifest write (manifest + UOP + confirm via BinManifestService).
// It creates no order and dispatches nothing — ingest is manifest-only. Returns
// a lifecycleError on failure, nil on success.
func (s *LifecycleService) ApplyIngestManifest(p *protocol.OrderIngestRequest) *lifecycleError {
	tmpl, err := s.db.GetPayloadByCode(p.PayloadCode)
	if err != nil {
		return lifecycleErr("payload_error", fmt.Sprintf("payload %q not found", p.PayloadCode), err)
	}
	bin, binErr := s.resolveIngestBin(p)
	if binErr != nil {
		return binErr
	}
	// Set the manifest AND confirm it in ONE transaction: a confirm failure must
	// not leave a counted-but-unconfirmed bin. manifest_confirmed is a hard gate
	// for a full bin to be a drain/retrieve source, so a stranded unconfirmed bin
	// is invisible to kanban. The epoch bump's return value is discarded because
	// this Core-internal path has no Edge response to thread it through — but the
	// bump announces itself, so the station is told either way. This used to say
	// the Edge relearned "on its next periodic bin-state refresh"; there was no
	// such refresh and nothing polled.
	if len(p.Manifest) > 0 {
		manifest := bins.Manifest{Items: make([]bins.ManifestEntry, len(p.Manifest))}
		for i, item := range p.Manifest {
			manifest.Items[i] = bins.ManifestEntry{CatID: item.PartNumber, Quantity: item.Quantity}
		}
		manifestJSON, _ := json.Marshal(manifest)
		// Use the operator-measured count Edge captured at finalize time
		// (carried in p.Quantity == runtime.RemainingUOP from produce_plan.go),
		// not tmpl.UOPCapacity. UOP is assembly-normalized: a finalized bin may
		// hold fewer than capacity cycles when the operator finalizes early or
		// the run wrapped on a non-multiple-of-capacity count. Falls back to
		// tmpl.UOPCapacity only if the wire value is missing (transitional Edge
		// builds and the no-Quantity test fixtures).
		uop := int(p.Quantity)
		if uop <= 0 {
			uop = tmpl.UOPCapacity
		}
		if err := s.binManifest.RecordProducedBin(bin.ID, string(manifestJSON), p.PayloadCode, uop, p.ProducedAt); err != nil {
			return lifecycleErr("internal_error", err.Error(), err)
		}
	} else {
		// Item 19 of the bin-as-truth refactor: route through the audited
		// BinManifestService so the 0→capacity initial fill surfaces in
		// bin_uop_ledger. Pre-Item-19 this path called the lower-level
		// SetBinManifestFromTemplate directly, bypassing audit; the resulting
		// timeline gap made forensics confusing because freshly-loaded bins
		// appeared in bin_uop_ledger only at the first downstream delta.
		if err := s.binManifest.RecordProducedBinFromTemplate(bin.ID, p.PayloadCode, nil, p.ProducedAt); err != nil {
			return lifecycleErr("internal_error", err.Error(), err)
		}
	}

	loadedAtLabel := p.ProducedAt
	if loadedAtLabel == "" {
		loadedAtLabel = "(server time)"
	}
	s.dbg("ingest: manifest recorded + confirmed bin=%d payload=%s at %s loaded_at=%s",
		bin.ID, p.PayloadCode, p.SourceNode, loadedAtLabel)
	return nil
}

// CancelOrder and ConfirmReceipt now live in lifecycle.go and route
// through the transition() driver against protocol.validTransitions.
// They preserve their original signatures for caller compatibility.

func (s *LifecycleService) PrepareRedirect(order *orders.Order, newDeliveryNode string) (*nodes.Node, *nodes.Node, error) {
	if order.VendorOrderID != "" {
		if err := s.backend.CancelOrder(order.VendorOrderID); err != nil {
			log.Printf("dispatch: cancel for redirect %s: %v", order.VendorOrderID, err)
		}
	}
	newDest, err := s.db.GetNodeByDotName(newDeliveryNode)
	if err != nil {
		return nil, nil, err
	}
	if err := s.db.UpdateOrderDeliveryNode(order.ID, newDeliveryNode); err != nil {
		log.Printf("dispatch: update order %d delivery_node: %v", order.ID, err)
	}
	order.DeliveryNode = newDeliveryNode
	if order.SourceNode == "" {
		return nil, nil, errors.New("no source node for redirect")
	}
	sourceNode, err := s.db.GetNodeByDotName(order.SourceNode)
	if err != nil {
		return nil, nil, err
	}
	if err := s.MoveToSourcing(order, "system", fmt.Sprintf("redirecting to %s", newDeliveryNode)); err != nil {
		log.Printf("dispatch: redirect order %d to sourcing: %v", order.ID, err)
	}
	return sourceNode, newDest, nil
}

// tryOverflow resolves a maintained group's configured overflow destination.
// Reports whether it found one and where.
//
// ── THE RESIDUALS, STATED RATHER THAN SOLVED ────────────────────────────────
//
// NO OVERFLOW CONFIGURED: false, and the push parks holding its bin. That is
// backpressure into whatever was pushing — an unloader that cannot put its
// carrier down stops draining — which is uncomfortable and is the honest
// consequence of telling a group to hold exactly four carriers and giving it
// nowhere to send the fifth. Blank is a real answer, not a missing one.
//
// THE OVERFLOW IS ITSELF FULL: false, same park. See the one-hop note above.
//
// THE OVERFLOW NAMES A NODE THAT DOES NOT EXIST, or one that cannot be resolved
// for a structural reason: false, and the push parks. A misconfigured overflow
// must not be able to FAIL an order that would otherwise have queued perfectly
// well — the operator's action succeeds either way, and the only difference is
// where the carrier ends up.
//
// It re-resolves rather than reusing anything from the first attempt, because
// the overflow is a different group with its own algorithm, its own children,
// and possibly its own level.
func (s *LifecycleService) tryOverflow(order *orders.Order, group *nodes.Node) (string, time.Time, bool) {
	overflow := s.db.GetNodeProperty(group.ID, nodes.PropOverflowDestination)
	if overflow == "" {
		return "", time.Time{}, false
	}
	dest, err := s.db.GetNodeByDotName(overflow)
	if err != nil || dest == nil {
		s.dbg("intake: overflow %q of %s does not resolve (%v) — parking instead",
			overflow, group.Name, err)
		return "", time.Time{}, false
	}
	if dest.ID == group.ID {
		// A group naming itself is the one-hop rule's degenerate case, and it is
		// worth refusing explicitly: without this it would re-resolve the same
		// full group and return the same capacity error, which reads as a
		// mysterious no-op rather than a configuration mistake.
		s.dbg("intake: overflow of %s names itself — parking instead", group.Name)
		return "", time.Time{}, false
	}

	var binTypeID *int64
	if order.OriginID != "" {
		if id, terr := s.db.MaintainedBinTypeIDForOrigin(order.OriginID); terr == nil {
			binTypeID = id
		}
	}
	result, err := s.resolver.Resolve(dest, binresolver.ResolveModeStore, order.PayloadCode,
		binTypeID, digAskerFor(order))
	if err != nil || result == nil || result.Node == nil {
		s.dbg("intake: overflow %s of %s has no room either (%v) — parking",
			overflow, group.Name, err)
		return "", time.Time{}, false
	}
	return result.Node.Name, clock.Now().UTC(), true
}
