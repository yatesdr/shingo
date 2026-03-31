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

func (s *LifecycleService) CreateInboundOrder(stationID string, p *protocol.OrderRequest) (*store.Order, string, *lifecycleError) {
	payloadCode := p.PayloadCode
	payloadDesc := p.PayloadDesc
	if p.RetrieveEmpty && p.OrderType == OrderTypeRetrieve {
		payloadDesc = "retrieve_empty"
	}
	order := &store.Order{
		EdgeUUID:     p.OrderUUID,
		StationID:    stationID,
		OrderType:    p.OrderType,
		Status:       StatusPending,
		Quantity:     p.Quantity,
		SourceNode:   p.SourceNode,
		DeliveryNode: p.DeliveryNode,
		Priority:     p.Priority,
		PayloadDesc:  payloadDesc,
		PayloadCode:  payloadCode,
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
	if err := s.db.UpdateOrderStatus(order.ID, StatusPending, "order received"); err != nil {
		log.Printf("dispatch: update order %d status to pending: %v", order.ID, err)
	}
	s.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, p.OrderType, payloadCode, p.DeliveryNode)
	return order, payloadCode, nil
}

func (s *LifecycleService) CreateStorageWaybillOrder(stationID string, p *protocol.OrderStorageWaybill) (*store.Order, *lifecycleError) {
	order := &store.Order{
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
	if err := s.db.UpdateOrderStatus(order.ID, StatusPending, "store order received"); err != nil {
		log.Printf("dispatch: update order %d status to pending: %v", order.ID, err)
	}
	s.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, p.OrderType, "", p.SourceNode)
	return order, nil
}

func (s *LifecycleService) CreateIngestStoreOrder(stationID string, p *protocol.OrderIngestRequest) (*store.Order, string, *lifecycleError) {
	tmpl, err := s.db.GetPayloadByCode(p.PayloadCode)
	if err != nil {
		return nil, "", lifecycleErr("payload_error", fmt.Sprintf("payload %q not found", p.PayloadCode), err)
	}
	bin, err := s.db.GetBinByLabel(p.BinLabel)
	if err != nil {
		return nil, "", lifecycleErr("bin_error", fmt.Sprintf("bin %q not found", p.BinLabel), err)
	}
	if len(p.Manifest) > 0 {
		manifest := store.BinManifest{Items: make([]store.ManifestEntry, len(p.Manifest))}
		for i, item := range p.Manifest {
			manifest.Items[i] = store.ManifestEntry{CatID: item.PartNumber, Quantity: item.Quantity}
		}
		manifestJSON, _ := json.Marshal(manifest)
		if err := s.binManifest.SetForProduction(bin.ID, string(manifestJSON), p.PayloadCode, tmpl.UOPCapacity); err != nil {
			return nil, "", lifecycleErr("internal_error", err.Error(), err)
		}
	} else {
		// NOTE: SetBinManifestFromTemplate is a store-level convenience that
		// resolves the payload template and builds manifest JSON internally.
		// It bypasses BinManifestService intentionally — template resolution
		// is a data concern, not a lifecycle concern. If audit logging on this
		// path becomes a requirement, add a SetFromTemplate wrapper to the
		// service layer.
		if err := s.db.SetBinManifestFromTemplate(bin.ID, p.PayloadCode, 0); err != nil {
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
	order := &store.Order{
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
	if err := s.db.UpdateOrderStatus(order.ID, StatusPending, "ingest order received"); err != nil {
		log.Printf("dispatch: update order %d status to pending: %v", order.ID, err)
	}
	if err := s.db.ClaimBin(bin.ID, order.ID); err != nil {
		log.Printf("dispatch: claim bin %d for order %d: %v", bin.ID, order.ID, err)
	}
	s.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, OrderTypeStore, p.PayloadCode, "")
	return order, p.PayloadCode, nil
}

func (s *LifecycleService) CancelOrder(order *store.Order, stationID, reason string) {
	if order.VendorOrderID != "" && order.Status != StatusConfirmed && order.Status != StatusFailed && order.Status != StatusCancelled {
		if err := s.backend.CancelOrder(order.VendorOrderID); err != nil {
			log.Printf("dispatch: cancel vendor order %s: %v", order.VendorOrderID, err)
			s.dbg("cancel fleet error: vendor_id=%s: %v", order.VendorOrderID, err)
		} else {
			s.dbg("cancel fleet ok: vendor_id=%s", order.VendorOrderID)
		}
	}
	s.db.UnclaimOrderBins(order.ID)
	s.db.DeleteOrderBins(order.ID)
	if err := s.db.UpdateOrderStatus(order.ID, StatusCancelled, reason); err != nil {
		log.Printf("dispatch: update order %d status to cancelled: %v", order.ID, err)
	}
	s.emitter.EmitOrderCancelled(order.ID, order.EdgeUUID, stationID, reason)
}

func (s *LifecycleService) ConfirmReceipt(order *store.Order, stationID, receiptType string, finalCount int64) (bool, error) {
	if order.CompletedAt != nil {
		s.dbg("delivery receipt: uuid=%s already completed at %s", order.EdgeUUID, order.CompletedAt.UTC().Format(time.RFC3339))
		return false, nil
	}
	if err := s.db.UpdateOrderStatus(order.ID, StatusConfirmed, fmt.Sprintf("receipt: %s, count: %d", receiptType, finalCount)); err != nil {
		log.Printf("dispatch: update order %d status to confirmed: %v", order.ID, err)
	}
	if err := s.db.CompleteOrder(order.ID); err != nil {
		return false, err
	}
	s.emitter.EmitOrderCompleted(order.ID, order.EdgeUUID, stationID)
	return true, nil
}

func (s *LifecycleService) PrepareRedirect(order *store.Order, newDeliveryNode string) (*store.Node, *store.Node, error) {
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
	if err := s.db.UpdateOrderStatus(order.ID, StatusSourcing, fmt.Sprintf("redirecting to %s", newDeliveryNode)); err != nil {
		log.Printf("dispatch: update order %d status to sourcing: %v", order.ID, err)
	}
	return sourceNode, newDest, nil
}
