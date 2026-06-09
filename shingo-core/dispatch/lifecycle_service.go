package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
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
	order := &orders.Order{
		EdgeUUID:        p.OrderUUID,
		StationID:       stationID,
		OrderType:       orderType,
		Status:          StatusPending,
		Quantity:        p.Quantity,
		SourceNode:      p.SourceNode,
		DeliveryNode:    p.DeliveryNode,
		Priority:        p.Priority,
		PayloadDesc:     p.PayloadDesc,
		PayloadCode:     payloadCode,
		SkipAutoConfirm: p.SkipAutoConfirm,
	}
	if payloadCode != "" {
		if _, err := s.db.GetPayloadByCode(payloadCode); err != nil {
			return nil, "", lifecycleErr("payload_error", fmt.Sprintf("payload %q not found", payloadCode), err)
		}
	}
	if p.DeliveryNode != "" {
		destNode, err := s.db.GetNodeByDotName(p.DeliveryNode)
		if err != nil {
			return nil, "", lifecycleErr("invalid_node", fmt.Sprintf("delivery node %q not found", p.DeliveryNode), err)
		}
		if destNode.IsSynthetic && s.resolver != nil {
			result, err := s.resolver.Resolve(destNode, OrderTypeStore, payloadCode, nil)
			if err != nil {
				// A full group (ResolutionCapacity — "no available slot in node
				// group X") must NOT fail the operator's action. Leave the
				// synthetic destination on the order and create it: planMove
				// resolves a concrete child at dispatch time, and
				// CheckDropoffCapacity parks it in `queued` until a slot frees —
				// the same queue-don't-fail contract every other dropoff path
				// already honors. Structural/transient failures (no enabled
				// children, DB error) still hard-fail so a real misconfiguration
				// surfaces to the operator instead of queueing forever.
				if class, _ := classifyResolutionError(err); class != ResolutionCapacity {
					return nil, "", lifecycleErr("resolution_failed", fmt.Sprintf("cannot resolve synthetic node %s: %v", p.DeliveryNode, err), err)
				}
				s.dbg("intake: synthetic %s full — creating order against group so it queues: %v", p.DeliveryNode, err)
			} else {
				s.dbg("resolved synthetic %s -> %s", p.DeliveryNode, result.Node.Name)
				order.DeliveryNode = result.Node.Name
			}
		}
	}
	if err := s.db.CreateOrder(order); err != nil {
		return nil, "", lifecycleErr("internal_error", err.Error(), err)
	}
	if err := s.db.UpdateOrderStatus(order.ID, string(StatusPending), "order received"); err != nil {
		log.Printf("dispatch: update order %d status to pending: %v", order.ID, err)
	}
	s.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, p.OrderType, payloadCode, p.DeliveryNode)
	return order, payloadCode, nil
}

func (s *LifecycleService) CreateStorageWaybillOrder(stationID string, p *protocol.OrderStorageWaybill) (*orders.Order, *lifecycleError) {
	order := &orders.Order{
		EdgeUUID:    p.OrderUUID,
		StationID:   stationID,
		OrderType:   p.OrderType,
		Status:      StatusPending,
		SourceNode:  p.SourceNode,
		PayloadDesc: p.PayloadDesc,
	}
	if err := s.db.CreateOrder(order); err != nil {
		return nil, lifecycleErr("internal_error", err.Error(), err)
	}
	if err := s.db.UpdateOrderStatus(order.ID, string(StatusPending), "store order received"); err != nil {
		log.Printf("dispatch: update order %d status to pending: %v", order.ID, err)
	}
	s.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, p.OrderType, "", p.SourceNode)
	return order, nil
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

func (s *LifecycleService) CreateIngestStoreOrder(stationID string, p *protocol.OrderIngestRequest) (*orders.Order, string, *lifecycleError) {
	tmpl, err := s.db.GetPayloadByCode(p.PayloadCode)
	if err != nil {
		return nil, "", lifecycleErr("payload_error", fmt.Sprintf("payload %q not found", p.PayloadCode), err)
	}
	bin, binErr := s.resolveIngestBin(p)
	if binErr != nil {
		return nil, "", binErr
	}
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
		// Epoch return is discarded here — the lifecycle service is
		// running a Core-internal ingest path with no Edge response to
		// thread the new epoch through. Edge picks up the new epoch on
		// its next periodic bin-state refresh.
		if _, err := s.binManifest.SetForProduction(bin.ID, string(manifestJSON), p.PayloadCode, uop); err != nil {
			return nil, "", lifecycleErr("internal_error", err.Error(), err)
		}
	} else {
		// Item 19 of the bin-as-truth refactor: route through
		// BinManifestService.SetFromTemplate so the 0→capacity initial
		// fill audits via bin_uop_audit. Pre-Item-19 this path called
		// *store.DB.SetBinManifestFromTemplate directly, bypassing
		// audit; the resulting timeline gap made forensics confusing
		// because freshly-loaded bins appeared in bin_uop_audit only
		// at the first downstream delta — missing the load itself.
		if _, err := s.binManifest.SetFromTemplate(bin.ID, p.PayloadCode, 0); err != nil {
			return nil, "", lifecycleErr("internal_error", err.Error(), err)
		}
	}
	if err := s.binManifest.Confirm(bin.ID, p.ProducedAt); err != nil {
		log.Printf("dispatch: confirm bin %d manifest: %v", bin.ID, err)
	}
	// loaded_at is resolved and stored by Confirm/ConfirmManifest above
	// (server time when ProducedAt is empty); log the requested value
	// rather than recomputing a second, possibly-divergent timestamp.
	loadedAtLabel := p.ProducedAt
	if loadedAtLabel == "" {
		loadedAtLabel = "(server time)"
	}
	s.dbg("ingest: set manifest on bin=%d, payload=%s, loaded_at=%s", bin.ID, p.PayloadCode, loadedAtLabel)

	// Swap-mode produce ships ManifestOnly: the bin's count is now recorded, and
	// the swap is already carrying that bin to the supermarket. Minting a store
	// order here would fight the swap for the (claimed) bin, and the routeless
	// order would let FindStorageDestination relocate it onto some other node
	// (incl. another press output). Stop at the manifest; simple-mode produce
	// (no swap) falls through to the real store move.
	if p.ManifestOnly {
		s.dbg("ingest: manifest-only bin=%d at %s — swap carries the move, no store order", bin.ID, p.SourceNode)
		return nil, p.PayloadCode, nil
	}

	order := &orders.Order{
		EdgeUUID:    p.OrderUUID,
		StationID:   stationID,
		OrderType:   OrderTypeStore,
		Status:      StatusPending,
		Quantity:    p.Quantity,
		SourceNode:  p.SourceNode,
		PayloadDesc: fmt.Sprintf("ingest %s bin %s", p.PayloadCode, p.BinLabel),
		BinID:       &bin.ID,
	}
	if err := s.db.CreateOrder(order); err != nil {
		return nil, "", lifecycleErr("internal_error", err.Error(), err)
	}
	if err := s.db.UpdateOrderStatus(order.ID, string(StatusPending), "ingest order received"); err != nil {
		log.Printf("dispatch: update order %d status to pending: %v", order.ID, err)
	}
	if err := s.db.ClaimBin(bin.ID, order.ID); err != nil {
		log.Printf("dispatch: claim bin %d for order %d: %v", bin.ID, order.ID, err)
	}
	s.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, OrderTypeStore, p.PayloadCode, "")
	return order, p.PayloadCode, nil
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
