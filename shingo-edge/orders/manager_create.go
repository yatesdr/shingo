package orders

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingoedge/release"
	"shingoedge/store/orders"
)

// RetrieveSpec is what a retrieve is created from: the order's facts, named,
// in place of a row of positionals.
type RetrieveSpec struct {
	// Process is the Edge's process node the order is for (nil for none).
	Process *int64
	// Delivery is where the carrier goes.
	Delivery string
	// Source names the supermarket node group (or specific node) Core pulls
	// the bin from. Empty falls back to Core's global FIFO search (legacy
	// behaviour). For bin_loader manual_swap claims this MUST be
	// claim.InboundSource — otherwise the planner happily pulls a
	// payload-matching empty/full bin from anywhere in the system, including
	// the empty-tote return area instead of the configured supermarket. See
	// planTransport in shingo-core/dispatch/planning_service.go, and its
	// source finder, for the resolver branch.
	Source string
	// Staging is written on this Edge's row alone, where the orders page shows
	// it; Core reads none.
	Staging string
	// Payload is the part; empty derives it from the process node's claim
	// (lookupPayloadMeta).
	Payload string
	// Quantity is carried verbatim: the manual order page's count, 1 for every
	// other door.
	Quantity int64
	// Empty asks for an empty carrier rather than a full of the part.
	Empty bool
	// AutoConfirm confirms the delivery on this Edge (handleDelivered);
	// SkipAutoConfirm asks Core's sweep not to (protocol.OrderRequest).
	AutoConfirm, SkipAutoConfirm bool
	// Origin is REQUIRED. There is no unattributed twin of this any more — see
	// the Origin type, and see specimen (c) for what the twin cost.
	Origin Origin
}

// CreateRetrieveOrder creates a new retrieve order and enqueues it to the
// outbox.
func (m *Manager) CreateRetrieveOrder(s RetrieveSpec) (*orders.Order, error) {
	pull := PullFull
	if s.Empty {
		pull = PullEmpty
	}
	return m.createRetrieveOrder(s, pull)
}

// CreateTopsAndBottomsPull creates SEND BACK's pull: tops & bottoms of payload
// (the running style's part) from source (the claim's inbound source) to
// delivery (the press's outside position), quantity 1, on the process node
// given (the outside's own: it never touches the press's runtime slots).
// Auto-confirmed: the outside has no tile and no operator receives it. Core
// plans it as an empty at exactly that height (retrieve_tops_bottoms).
func (m *Manager) CreateTopsAndBottomsPull(processNodeID *int64, delivery, source, payload string, origin Origin) (*orders.Order, error) {
	return m.createRetrieveOrder(RetrieveSpec{Process: processNodeID, Delivery: delivery, Source: source,
		Payload: payload, Quantity: 1, AutoConfirm: true, Origin: origin}, PullTopsAndBottoms)
}

// createRetrieveOrder is the one body. pull is what it brings (Pull.wire): it
// decides the row's type, the wire's type and flag, the log line and the
// created event, so no caller and no line here names a type of its own.
func (m *Manager) createRetrieveOrder(s RetrieveSpec, pull Pull) (*orders.Order, error) {
	orderUUID := uuid.New().String()
	orderType, retrieveEmpty := pull.wire()

	payloadDesc, payloadCode := m.lookupPayloadMeta(s.Process, s.Payload)

	orderID, err := m.db.CreateOrder(orderUUID, orderType,
		s.Process, retrieveEmpty,
		s.Quantity, s.Delivery, s.Staging, s.Source, s.AutoConfirm, payloadCode,
		s.Origin.ID, s.Origin.Class)
	if err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderRequest, &protocol.OrderRequest{
		OrderUUID:       orderUUID,
		OrderType:       orderType,
		PayloadDesc:     payloadDesc,
		PayloadCode:     payloadCode,
		RetrieveEmpty:   retrieveEmpty,
		Quantity:        s.Quantity,
		DeliveryNode:    s.Delivery,
		SourceNode:      s.Source,
		StagingNode:     s.Staging,
		SkipAutoConfirm: s.SkipAutoConfirm,
		OriginID:        s.Origin.ID,
		OriginClass:     s.Origin.Class,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s delivery=%s", orderType, orderID, orderUUID, s.Delivery)
	m.emitter.EmitOrderCreated(orderID, orderUUID, orderType, nil, s.Process)

	return m.db.GetOrder(orderID)
}

// CreateMoveOrder creates a new move order (e.g., quality hold).
// autoConfirm threads through to the order row so Manager.handleDelivered
// can self-confirm instead of stranding the order at "delivered" when no
// operator station is wired up to confirm manually.
// origin is REQUIRED; see the Origin type.
func (m *Manager) CreateMoveOrder(processNodeID *int64, quantity int64, sourceNode, deliveryNode string, autoConfirm bool, origin Origin) (*orders.Order, error) {
	return m.createMoveOrder(processNodeID, quantity, sourceNode, deliveryNode, "", nil, autoConfirm, false, namedBin{}, origin)
}

// CreateMoveOrderWithPayloadCode is CreateMoveOrder with an explicit payload
// code instead of falling back to the active claim's primary payload. The
// manual_swap loader / unloader case needs this: a claim can list multiple
// allowed_payload_codes and the operator picks one at LoadBin time. Without
// threading that pick through to L2 / U2, the side-cycle move ends up tagged
// with claim.PayloadCode (the primary), and operator station tiles — which
// filter active orders by payload_code per card — show no in-transit state on
// the loaded payload's tile and may render unrelated tiles as queued via the
// no-payload-code fallback in operator-render.js / operator-modal.js.
// origin is REQUIRED; see the Origin type.
func (m *Manager) CreateMoveOrderWithPayloadCode(processNodeID *int64, quantity int64, sourceNode, deliveryNode, payloadCode string, autoConfirm bool, origin Origin) (*orders.Order, error) {
	return m.createMoveOrder(processNodeID, quantity, sourceNode, deliveryNode, payloadCode, nil, autoConfirm, false, namedBin{}, origin)
}

// CreateMoveOrderWithUOP creates a move order and threads remainingUOP into the
// protocol envelope so Core can atomically clear/sync the bin manifest on claim.
// autoConfirm mirrors CreateMoveOrder so operator-initiated moves at a
// manual_swap node can self-confirm on delivery.
// origin is REQUIRED; see the Origin type.
//
// PASS THE PAYLOAD WHEN THE CALLER KNOWS IT. This used to hardcode "", which
// sends createMoveOrder to lookupPayloadMeta's backfill — and that resolver
// prefers the TARGET style mid-changeover, because its subject is normally an
// order that does not exist yet. A move carrying a carrier already standing on
// a cell is the opposite case: the carrier is the subject, and it got stamped
// with the incoming style's part. Empty still backfills, for callers with no
// better answer.
func (m *Manager) CreateMoveOrderWithUOP(processNodeID *int64, quantity int64, sourceNode, deliveryNode, payloadCode string, remainingUOP *int, autoConfirm bool, origin Origin) (*orders.Order, error) {
	return m.createMoveOrder(processNodeID, quantity, sourceNode, deliveryNode, payloadCode, remainingUOP, autoConfirm, false, namedBin{}, origin)
}

// CreateMoveOrderCarrying moves the carrier standing on sourceNode and names
// it by what it CARRIES: carried is the bin's own payload as Core reported it,
// and a blank means an empty carrier and stays blank.
//
// Every other move variant back-fills a blank payload from the node's claim,
// and mid-changeover that claim is the TARGET style's (resolveClaimForNode,
// TargetStyleFirst). A carrier already standing on a cell is the opposite case:
// an empty spare going back to its supply would go out tagged with the
// incoming part, and Core's store restriction for that part would judge where
// it may land. So this one says exactly what the bin is. Auto-confirmed: no
// operator receives a carrier going back to where it came from.
func (m *Manager) CreateMoveOrderCarrying(processNodeID *int64, sourceNode, deliveryNode, carried string, origin Origin) (*orders.Order, error) {
	return m.createMoveOrder(processNodeID, 1, sourceNode, deliveryNode, carried, nil, true, true, namedBin{}, origin)
}

// CreateMoveOrderForBin is CreateMoveOrderCarrying for one bin: binID is Core's
// id for the bin standing on sourceNode, as the read that decided the move saw
// it. Core lifts that bin and no other, and only while it stands on sourceNode;
// once it has left, the move ends skipped (protocol.OrderRequest.BinID). For a
// move decided from a read of what stands on the source, which must not carry
// away whatever lands there after.
func (m *Manager) CreateMoveOrderForBin(processNodeID *int64, sourceNode, deliveryNode, carried string, binID int64, origin Origin) (*orders.Order, error) {
	return m.createMoveOrder(processNodeID, 1, sourceNode, deliveryNode, carried, nil, true, true, namedBin{id: binID}, origin)
}

// namedBin is the bin a move names and the hold it asks for: id 0 names none
// (protocol.OrderRequest.BinID); holdBy "" asks for no hold
// (protocol.OrderRequest.QualityHoldBy).
type namedBin struct {
	id     int64
	holdBy string
}

// CreateMoveOrderWithPayloadCodeForBin is CreateMoveOrderWithPayloadCode for
// one bin: binID is Core's id for the bin standing on sourceNode, as the read
// that decided the move saw it (0 names none). Core refuses the move when that
// bin is not there ("not at that source"), lifts it and no other, and ends the
// move skipped once it has left.
func (m *Manager) CreateMoveOrderWithPayloadCodeForBin(processNodeID *int64, quantity int64, sourceNode, deliveryNode, payloadCode string, binID int64, autoConfirm bool, origin Origin) (*orders.Order, error) {
	return m.createMoveOrder(processNodeID, quantity, sourceNode, deliveryNode, payloadCode, nil, autoConfirm, false, namedBin{id: binID}, origin)
}

// CreateHoldMoveOrder is the move that takes one bin to quality containment and asks
// Core for its hold in the same act: Core checks the bin stands on sourceNode,
// sets its hold marker attributed to heldBy and births the move in one
// transaction, or refuses all three (protocol.OrderRequest.QualityHoldBy).
func (m *Manager) CreateHoldMoveOrder(processNodeID *int64, sourceNode, deliveryNode, payloadCode string, binID int64, heldBy string, origin Origin) (*orders.Order, error) {
	return m.createMoveOrder(processNodeID, 1, sourceNode, deliveryNode, payloadCode, nil, true, false, namedBin{id: binID, holdBy: heldBy}, origin)
}

// CreateMoveOrderCarryingTo is CreateMoveOrderCarrying for a carrier delivered
// to a line: the line's own confirm policy applies, since an operator may be
// there to receive it.
func (m *Manager) CreateMoveOrderCarryingTo(processNodeID *int64, sourceNode, deliveryNode, carried string, autoConfirm bool, origin Origin) (*orders.Order, error) {
	return m.createMoveOrder(processNodeID, 1, sourceNode, deliveryNode, carried, nil, autoConfirm, true, namedBin{}, origin)
}

// createMoveOrder is the one body behind every move variant.
//
// They were four copies of the same twenty lines differing in one field each,
// which is how a change lands in three of them: the origin plumbing would have
// been exactly that change. Collapsing them first makes "every move order can
// carry an origin" true by construction rather than by inspection.
//
// carried says payloadCode is the carrier's own and is not back-filled when
// blank (CreateMoveOrderCarrying). bin names the one bin the move is for, none
// when zero (CreateMoveOrderForBin), and the hold it asks for (CreateHoldMoveOrder).
func (m *Manager) createMoveOrder(processNodeID *int64, quantity int64,
	sourceNode, deliveryNode, payloadCode string, remainingUOP *int,
	autoConfirm, carried bool, bin namedBin, origin Origin) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	var payloadDesc string
	if carried {
		payloadDesc = m.payloadDesc(payloadCode)
	} else {
		payloadDesc, payloadCode = m.lookupPayloadMeta(processNodeID, payloadCode)
	}

	orderID, err := m.db.CreateOrder(orderUUID, TypeMove,
		processNodeID, false,
		quantity, deliveryNode, "", sourceNode, autoConfirm, payloadCode,
		origin.ID, origin.Class)
	if err != nil {
		return nil, fmt.Errorf("create move order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderRequest, &protocol.OrderRequest{
		OrderUUID:     orderUUID,
		OrderType:     TypeMove,
		PayloadDesc:   payloadDesc,
		PayloadCode:   payloadCode,
		Quantity:      quantity,
		DeliveryNode:  deliveryNode,
		SourceNode:    sourceNode,
		RemainingUOP:  remainingUOP,
		OriginID:      origin.ID,
		OriginClass:   origin.Class,
		BinID:         bin.id,
		QualityHoldBy: bin.holdBy,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s source=%s delivery=%s payload=%s remainingUOP=%v bin=%d hold=%q origin=%s",
		TypeMove, orderID, orderUUID, sourceNode, deliveryNode, payloadCode, remainingUOP, bin.id, bin.holdBy, origin.ID)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeMove, nil, processNodeID)
	return m.db.GetOrder(orderID)
}

// CreateComplexOrder creates a new multi-step complex order and enqueues
// it to the outbox. The order is created with auto_confirm=false: it
// requires an operator HMI press to transition delivered → confirmed.
// Use this for deliveries whose destination is at the lineside, where an
// operator can inspect the bin. For deliveries to the supermarket /
// outbound staging (no operator present), use
// CreateComplexOrderWithAutoConfirm instead. deliveryNode is stored on
// the order for downstream logic (e.g., handleOrderCompleted uses it to
// determine which payload to reset on completion).
//
// processNodeName is the dot-name of the line node the order belongs to
// (typically claim.CoreNodeName). Threaded through to
// ComplexOrderRequest.ProcessNode so Core picks the line bin for
// order.BinID and targets it at release-time fallback. Pass "" when the
// order has no distinct line node — Core falls back to source-node
// behavior.
// origin is REQUIRED; see the Origin type.
func (m *Manager) CreateComplexOrder(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, origin Origin) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, false, "", "", "", origin)
}

// CreateComplexOrderWithAutoConfirm creates an auto-confirm complex order.
// Used for orders whose destination is the supermarket / outbound staging,
// where there is no operator to press CONFIRM. The order auto-transitions
// delivered → confirmed in handleDelivered the moment the fleet reports
// FINISHED, eliminating the FINISHED → CONFIRMED race window where the
// scanner can re-claim a delivered bin and the late confirm clobbers state
// (the SMN_001 / SMN_002 teleport bug, plant-test 2026-04-27).
// origin is REQUIRED; see the Origin type.
func (m *Manager) CreateComplexOrderWithAutoConfirm(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, origin Origin) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, true, "", "", "", origin)
}

// origin is REQUIRED; see the Origin type.
func (m *Manager) CreateComplexOrderWithPayload(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, autoConfirm bool, payloadCode string, origin Origin) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, autoConfirm, payloadCode, "", "", origin)
}

// CreateComplexOrderSibling creates a complex order and records the
// two-robot swap sibling UUID on the outbound ComplexOrderRequest, so Core
// can pair the legs at intake — before the removal leg's synchronous
// dispatch claims the line bin. siblingUUID is the *other* leg's edge UUID,
// or "" for non-swap / first-created legs.
// origin is REQUIRED; see the Origin type.
func (m *Manager) CreateComplexOrderSibling(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, autoConfirm bool, payloadCode, siblingUUID string, origin Origin) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, autoConfirm, payloadCode, siblingUUID, "", origin)
}

// CreateComplexOrderPaired creates one leg of a swap whose BOTH uuids were
// minted before either create, so each leg can name its partner.
//
// ── WHY THE UUID COMES FROM THE CALLER ────────────────────────────────────
//
// createComplexOrder used to mint the uuid itself, which made the first leg
// created structurally unable to name a sibling that did not exist yet: it
// went in with siblingUUID == "", and everything downstream that pairs legs
// had to cope with a half-linked pair. The deleted swap-hold gate's fail-open at
// sibUUID == "" was one such coping mechanism, and SYNTH-round2's finding is
// that it made
// CREATION ORDER a correctness input — the changeover path creates the filler
// first, the produce path the supply, and no single ordering is safe once both
// legs are held.
//
// Minting both up front removes the window entirely: neither leg is ever
// unpaired, and creation order stops mattering. Core's intake back-link
// becomes redundant-but-harmless rather than load-bearing.
//
// ORDERING STILL MATTERS FOR THE OUTBOX, and differently: the outbox drains
// strictly ORDER BY id, so the leg created first is the leg Core sees first.
// That is about which robot is asked for first, not about pairing, and the
// callers keep their existing order for that reason.
func (m *Manager) CreateComplexOrderPaired(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, autoConfirm bool, payloadCode, siblingUUID, orderUUID string, origin Origin) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, autoConfirm, payloadCode, siblingUUID, orderUUID, origin)
}

// NewOrderUUID mints an order uuid. Exported so a caller creating a PAIR can
// mint both before either create — see CreateComplexOrderPaired.
func NewOrderUUID() string { return uuid.New().String() }

func (m *Manager) createComplexOrder(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, autoConfirm bool, payloadOverride, siblingUUID, presetUUID string, origin Origin) (*orders.Order, error) {
	// Caller-supplied when this leg is half of a pre-minted pair; minted here
	// for every single-order path, which is most of them.
	orderUUID := presetUUID
	if orderUUID == "" {
		orderUUID = uuid.New().String()
	}

	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return nil, fmt.Errorf("marshal steps: %w", err)
	}

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, payloadOverride)

	orderID, err := m.db.CreateOrder(orderUUID, TypeComplex,
		processNodeID, false,
		quantity, deliveryNode, "", "", autoConfirm, payloadCode,
		origin.ID, origin.Class)
	if err != nil {
		return nil, fmt.Errorf("create complex order: %w", err)
	}

	// The release facts are the steps' answers to what a release of this leg
	// moves (release.Facts), computed once, here, where the steps are authored.
	// A leg with no steps stores none: the release reads it as it reads any
	// order with no plan (the curtain gates its process node).
	if len(steps) == 0 {
		if err := m.db.UpdateOrderStepsJSON(orderID, string(stepsJSON)); err != nil {
			return nil, fmt.Errorf("store steps: %w", err)
		}
	} else {
		facts, err := release.FactsFromSteps(steps, processNodeName).Encode()
		if err != nil {
			return nil, err
		}
		if err := m.db.UpdateOrderStepsAndFacts(orderID, string(stepsJSON), facts); err != nil {
			return nil, fmt.Errorf("store steps: %w", err)
		}
	}

	// Routing comes from the claim, resolved the same way and at the same
	// moment as the payload — see lookupRouting for why this is not a
	// parameter.
	keyRoute := m.lookupRouting(processNodeID)

	env, envErr := m.sender.build(protocol.TypeComplexOrderRequest, &protocol.ComplexOrderRequest{
		OrderUUID:        orderUUID,
		PayloadCode:      payloadCode,
		PayloadDesc:      payloadDesc,
		Quantity:         quantity,
		ProcessNode:      processNodeName,
		Steps:            steps,
		SiblingOrderUUID: siblingUUID,
		KeyRoute:         keyRoute,
		OriginID:         origin.ID,
		OriginClass:      origin.Class,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s steps=%d", TypeComplex, orderID, orderUUID, len(steps))
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeComplex, nil, processNodeID)

	return m.db.GetOrder(orderID)
}

// IngestDeclaration is a station's RELEASE FULL on a bin it counted below its
// pack, carried on the release-time ingest beside the counted Quantity
// (protocol.OrderIngestRequest.DeclaredQty, CalledBy, OrderID): Core books the
// count and then stamps the bin to Qty with a row naming who declared it. The
// zero value is no declaration, and the ingest carries none of the three keys.
type IngestDeclaration struct {
	Qty      int64  // the pack declared; 0 = none
	CalledBy string // who pressed RELEASE
	OrderID  string // the departing order's Edge UUID
}

// IngestEnvelope encodes a manifest-only ingest for a caller that writes the
// outbox row inside its own transaction: the release-time produce finalize,
// whose ledger row commits with it. No local order is minted (a local order
// there was a phantom the operator-abort fan-out later cancelled, and Core's
// manifest-only handler sends no reply to match it back); the swap's own order
// carries the bin. The row is delivered by the durable outbox: an ingest never
// expires and never dead-letters (protocol.TypeOrderIngest's TTL, the drainer's
// exemption). binID (0 = absent) pins the exact Core bin
// (protocol.OrderIngestRequest.BinID); binEpoch (0 = absent) fences a late
// ingest to the life of the bin it counted. declared is the station's
// declaration, when there is one.
func (m *Manager) IngestEnvelope(payloadCode, binLabel string, binID, binEpoch int64, sourceNode string, quantity int64, producedAt string, declared IngestDeclaration) ([]byte, string, error) {
	env, err := m.sender.build(protocol.TypeOrderIngest, &protocol.OrderIngestRequest{
		OrderUUID:   uuid.New().String(),
		BinID:       binID,
		BinEpoch:    binEpoch,
		PayloadCode: payloadCode,
		BinLabel:    binLabel,
		SourceNode:  sourceNode,
		Quantity:    quantity,
		ProducedAt:  producedAt,
		DeclaredQty: declared.Qty,
		CalledBy:    declared.CalledBy,
		OrderID:     declared.OrderID,
	})
	if err != nil {
		return nil, "", err
	}
	data, err := json.Marshal(env)
	if err != nil {
		return nil, "", fmt.Errorf("marshal envelope: %w", err)
	}
	return data, env.Type, nil
}

// HoldDroppedEnvelope encodes one dropped produce hold for Core
// (protocol.SubjectHoldDropped) and returns it with the outbox msg type, for a
// caller that writes the outbox row inside the transaction that dropped the
// hold (store.EnqueueOutboxIn), so the drop and its message commit together.
// The caller rings the drainer after its commit (messaging.NotifyEnqueued).
//
// The row never expires and never dead-letters (protocol.SubjectHoldDropped),
// and Core writes one hold_dropped row per envelope id, so a re-send writes
// nothing twice.
func (m *Manager) HoldDroppedEnvelope(d protocol.HoldDropped) ([]byte, string, error) {
	env, err := protocol.NewDataEnvelope(protocol.SubjectHoldDropped, m.sender.src(), m.sender.dst(), &d)
	if err != nil {
		return nil, "", fmt.Errorf("build hold-dropped envelope node %s: %w", d.Node, err)
	}
	data, err := env.Encode()
	if err != nil {
		return nil, "", fmt.Errorf("encode hold-dropped envelope node %s: %w", d.Node, err)
	}
	return data, protocol.SubjectHoldDropped, nil
}

// QueueIngestManifest sends the same manifest-only ingest through the
// fire-and-forget Queue path, for a caller with no transaction of its own: the
// manual ingest door (the www apiCreateIngestOrder handler). It carries no
// declaration.
func (m *Manager) QueueIngestManifest(payloadCode, binLabel string, binID, binEpoch int64, sourceNode string, quantity int64, manifest []protocol.IngestManifestItem, producedAt string) error {
	return m.sender.Queue(protocol.TypeOrderIngest, &protocol.OrderIngestRequest{
		OrderUUID:   uuid.New().String(),
		BinID:       binID,
		BinEpoch:    binEpoch,
		PayloadCode: payloadCode,
		BinLabel:    binLabel,
		SourceNode:  sourceNode,
		Quantity:    quantity,
		Manifest:    manifest,
		ProducedAt:  producedAt,
	})
}

// Origin attributes an order to the demand episode that asked for it.
//
// A VALUE, NOT TWO MORE POSITIONAL PARAMETERS. createMoveOrder already takes
// nine, and the tenth and eleventh would be two adjacent strings whose order
// nothing but the compiler could check — and the compiler cannot tell
// origin_id from origin_class.
//
// ── THERE IS NO "NOT STATED" ANY MORE ─────────────────────────────────────
//
// The zero value used to be a third state, defended as honest: "Core classifies
// those; saying nothing here is honest, where guessing would not be." The
// defence was sound and the outcome was not. Every door that was never wired
// took the zero — the HMI button, the sequential backfill, the changeover
// applier's legs, RequestEmptyBin's multi-step arm — and each of them WAS
// demand-serving. They reached Core carrying nothing, Core honestly stamped
// orphan, and the episode surface showed changeovers and thresholds only. Not
// mislabelled: absent. A zero value that means "nobody decided" is
// indistinguishable from "somebody decided nothing", and the whole of specimen
// (c) is the second one wearing the first one's clothes.
//
// So the type is TOTAL: exactly two constructors, and every create site names
// one. "Forgot" is now a compile error, because the unattributed wrappers that
// used to supply this silently are deleted and origin is a required argument.
// The remaining hole is a bare Origin{} literal, which Go will always let you
// write; a forbidigo fence closes it outside this package.
//
// NO_DEMAND MEANS "BELONGS TO NO DEMAND EPISODE BY CONSTRUCTION" — not "nobody
// wanted it". An API-commanded retrieve, a loader's owed outbound, a home
// consolidation: somebody or something wanted every one of those, and none of
// them will ever belong to a cell's episode. That is the distinction the class
// records, and it is what keeps the orphan bucket meaning "should have had one
// and lost it".
//
// AND A WRONG no_demand IS WORSE THAN A WRONG attached, which is the thing to
// hold on to when adding a site. An order labelled no_demand has ANSWERED the
// question and leaves the orphan bucket for good; an order labelled with the
// wrong episode is visibly wrong on a surface somebody reads. When a site is
// genuinely unclear, it is the no_demand answer that needs the argument.
type Origin struct {
	// ID is the demand episode. Empty for a no_demand order.
	ID string
	// Class is one of protocol.OriginClass*. Empty means the value was never
	// built through a constructor — see the fence above; nothing in the tree
	// should be able to produce it.
	Class string
}

// Attached is the origin of an order created to serve a demand episode.
func Attached(originID string) Origin {
	return Origin{ID: originID, Class: protocol.OriginClassAttached}
}

// NoDemand marks an order that is STRUCTURALLY originless — one nothing asked
// for, created because the system had a reason of its own.
//
// Stamped at the CREATE SITE, where it is known, and never inferred later.
// Without it, `origin_id IS NULL` selects every opportunistic stage and every
// admin action along with the actual lost origins: a haystack with the needle
// in it. This is what keeps the orphan bucket meaning something.
func NoDemand() Origin {
	return Origin{Class: protocol.OriginClassNoDemand}
}
