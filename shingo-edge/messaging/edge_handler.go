package messaging

import (
	"encoding/json"
	"log"
	"time"

	"shingoedge/orders"
	"shingo/protocol"
)

// EdgeHandler handles inbound protocol messages on the dispatch topic.
// It delegates order reply messages to the orders.Manager.
type EdgeHandler struct {
	protocol.NoOpHandler

	orderMgr    *orders.Manager
	onCoreNodes func([]string)
}

// NewEdgeHandler creates a handler for inbound core messages.
func NewEdgeHandler(orderMgr *orders.Manager, onCoreNodes func([]string)) *EdgeHandler {
	return &EdgeHandler{orderMgr: orderMgr, onCoreNodes: onCoreNodes}
}

func (h *EdgeHandler) HandleData(env *protocol.Envelope, p *protocol.Data) {
	switch p.Subject {
	case protocol.SubjectEdgeRegistered:
		var reg protocol.EdgeRegistered
		if err := json.Unmarshal(p.Body, &reg); err != nil {
			log.Printf("edge_handler: decode edge registered body: %v", err)
			return
		}
		log.Printf("edge_handler: registration acknowledged: station=%s msg=%s", reg.StationID, reg.Message)
	case protocol.SubjectEdgeHeartbeatAck:
		var ack protocol.EdgeHeartbeatAck
		if err := json.Unmarshal(p.Body, &ack); err != nil {
			log.Printf("edge_handler: decode heartbeat ack body: %v", err)
			return
		}
		log.Printf("edge_handler: heartbeat ack: station=%s server_ts=%s", ack.StationID, ack.ServerTS)
	case protocol.SubjectNodeListResponse:
		var resp protocol.NodeListResponse
		if err := json.Unmarshal(p.Body, &resp); err != nil {
			log.Printf("edge_handler: decode node list response: %v", err)
			return
		}
		log.Printf("edge_handler: received node list (%d nodes)", len(resp.Nodes))
		if h.onCoreNodes != nil {
			names := make([]string, len(resp.Nodes))
			for i, n := range resp.Nodes {
				names[i] = n.Name
			}
			h.onCoreNodes(names)
		}
	default:
		log.Printf("edge_handler: unhandled data subject: %s", p.Subject)
	}
}

func (h *EdgeHandler) HandleOrderAck(env *protocol.Envelope, p *protocol.OrderAck) {
	log.Printf("edge_handler: order ack: uuid=%s shingo_id=%d", p.OrderUUID, p.ShingoOrderID)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, "ack", "", "", p.SourceNode); err != nil {
		log.Printf("edge_handler: handle ack for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderWaybill(env *protocol.Envelope, p *protocol.OrderWaybill) {
	log.Printf("edge_handler: order waybill: uuid=%s waybill=%s", p.OrderUUID, p.WaybillID)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, "waybill", p.WaybillID, p.ETA, ""); err != nil {
		log.Printf("edge_handler: handle waybill for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderUpdate(env *protocol.Envelope, p *protocol.OrderUpdate) {
	log.Printf("edge_handler: order update: uuid=%s status=%s", p.OrderUUID, p.Status)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, "update", "", p.ETA, p.Detail); err != nil {
		log.Printf("edge_handler: handle update for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderDelivered(env *protocol.Envelope, p *protocol.OrderDelivered) {
	log.Printf("edge_handler: order delivered: uuid=%s at=%s", p.OrderUUID, p.DeliveredAt)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, "delivered", "", "", p.DeliveredAt.Format(time.RFC3339)); err != nil {
		log.Printf("edge_handler: handle delivered for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderError(env *protocol.Envelope, p *protocol.OrderError) {
	log.Printf("edge_handler: order error: uuid=%s code=%s detail=%s", p.OrderUUID, p.ErrorCode, p.Detail)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, "error", "", "", p.Detail); err != nil {
		log.Printf("edge_handler: handle error for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderCancelled(env *protocol.Envelope, p *protocol.OrderCancelled) {
	log.Printf("edge_handler: order cancelled: uuid=%s reason=%s", p.OrderUUID, p.Reason)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, "cancelled", "", "", p.Reason); err != nil {
		log.Printf("edge_handler: handle cancelled for %s: %v", p.OrderUUID, err)
	}
}
