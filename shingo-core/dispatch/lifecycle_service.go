package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

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
		EdgeUUID:     p.OrderUUID,
		StationID:    stationID,
		OrderType:    orderType,
		Status:       StatusPending,
		Quantity:     p.Quantity,
		SourceNode:   p.SourceNode,
		DeliveryNode: p.DeliveryNode,
		Priority:     p.Priority,
		PayloadDesc:  p.PayloadDesc,
		PayloadCode:  payloadCode,
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
				return nil, "", lifecycleErr("resolution_failed", fmt.Sprintf("cannot resolve synthetic node %s: %v", p.DeliveryNode, err), err)
			}
			s.dbg("resolved synthetic %s -> %s", p.DeliveryNode, result.Node.Name)
			order.DeliveryNode = result.Node.Name
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

func (s *LifecycleService) CreateIngestStoreOrder(stationID string, p *protocol.OrderIngestRequest) (*orders.Order, string, *lifecycleError) {
	tmpl, err := s.db.GetPayloadByCode(p.PayloadCode)
	if err != nil {
		return nil, "", lifecycleErr("payload_error", fmt.Sprintf("payload %q not found", p.PayloadCode), err)
	}
	bin, err := s.db.GetBinByLabel(p.BinLabel)
	if err != nil {
		return nil, "", lifecycleErr("bin_error", fmt.Sprintf("bin %q not found", p.BinLabel), err)
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
		if err := s.binManifest.SetForProduction(bin.ID, string(manifestJSON), p.PayloadCode, uop); err != nil {
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
		if err := s.binManifest.SetFromTemplate(bin.ID, p.PayloadCode, 0); err != nil {
			return nil, "", lifecycleErr("internal_error", err.Error(), err)
		}
	}
	if err := s.binManifest.Confirm(bin.ID, p.ProducedAt); err != nil {
		log.Printf("dispatch: confirm bin %d manifest: %v", bin.ID, err)
	}
	loadedAt := p.ProducedAt
	if loadedAt == "" {
		loadedAt = time.Now().UTC().Format("2006-01-02 15:04:05")
	}
	s.dbg("ingest: set manifest on bin=%d, payload=%s, loaded_at=%s", bin.ID, p.PayloadCode, loadedAt)
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
