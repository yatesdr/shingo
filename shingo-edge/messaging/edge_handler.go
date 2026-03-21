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

	orderMgr           *orders.Manager
	onCoreNodes        func([]protocol.NodeInfo)
	onPayloadCatalog   func([]protocol.CatalogPayloadInfo)
	onRegisterReq      func()

	DebugLog DebugLogFunc
}

// NewEdgeHandler creates a handler for inbound core messages.
func NewEdgeHandler(orderMgr *orders.Manager, onCoreNodes func([]protocol.NodeInfo)) *EdgeHandler {
	return &EdgeHandler{orderMgr: orderMgr, onCoreNodes: onCoreNodes}
}

// SetRegisterRequestHandler sets a callback for when core requests re-registration.
func (h *EdgeHandler) SetRegisterRequestHandler(fn func()) {
	h.onRegisterReq = fn
}

// SetPayloadCatalogHandler sets a callback for when the payload catalog is received from core.
func (h *EdgeHandler) SetPayloadCatalogHandler(fn func([]protocol.CatalogPayloadInfo)) {
	h.onPayloadCatalog = fn
}


func (h *EdgeHandler) HandleData(env *protocol.Envelope, p *protocol.Data) {
	h.DebugLog.log("data subject=%s from=%s", p.Subject, env.Src.Station)
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
			h.onCoreNodes(resp.Nodes)
		}
	case protocol.SubjectProductionReportAck:
		var ack protocol.ProductionReportAck
		if err := json.Unmarshal(p.Body, &ack); err != nil {
			log.Printf("edge_handler: decode production report ack: %v", err)
			return
		}
		log.Printf("edge_handler: production report ack: station=%s accepted=%d", ack.StationID, ack.Accepted)
	case protocol.SubjectCatalogPayloadsResponse:
		var resp protocol.CatalogPayloadsResponse
		if err := json.Unmarshal(p.Body, &resp); err != nil {
			log.Printf("edge_handler: decode payload catalog response: %v", err)
			return
		}
		log.Printf("edge_handler: received payload catalog (%d entries)", len(resp.Payloads))
		if h.onPayloadCatalog != nil {
			h.onPayloadCatalog(resp.Payloads)
		}
	case protocol.SubjectTagVerifyResponse:
		var resp protocol.TagVerifyResponse
		if err := json.Unmarshal(p.Body, &resp); err != nil {
			log.Printf("edge_handler: decode tag verify response: %v", err)
			return
		}
		if resp.Match {
			log.Printf("edge_handler: tag verify: uuid=%s match=true detail=%s", resp.OrderUUID, resp.Detail)
		} else {
			log.Printf("edge_handler: tag verify: uuid=%s match=false expected=%s detail=%s", resp.OrderUUID, resp.Expected, resp.Detail)
		}
	case protocol.SubjectEdgeRegisterRequest:
		var req protocol.EdgeRegisterRequest
		if err := json.Unmarshal(p.Body, &req); err != nil {
			log.Printf("edge_handler: decode register request: %v", err)
			return
		}
		log.Printf("edge_handler: core requested re-registration: %s", req.Reason)
		if h.onRegisterReq != nil {
			h.onRegisterReq()
		}
	case protocol.SubjectEdgeStale:
		var stale protocol.EdgeStale
		if err := json.Unmarshal(p.Body, &stale); err != nil {
			log.Printf("edge_handler: decode stale notification: %v", err)
			return
		}
		log.Printf("edge_handler: WARNING: core marked this edge as stale: %s", stale.Message)
	default:
		log.Printf("edge_handler: unhandled data subject: %s", p.Subject)
	}
}

func (h *EdgeHandler) HandleOrderAck(env *protocol.Envelope, p *protocol.OrderAck) {
	h.DebugLog.log("order_ack uuid=%s shingo_id=%d", p.OrderUUID, p.ShingoOrderID)
	log.Printf("edge_handler: order ack: uuid=%s shingo_id=%d", p.OrderUUID, p.ShingoOrderID)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, orders.ReplyAck, "", "", p.SourceNode); err != nil {
		log.Printf("edge_handler: handle ack for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderWaybill(env *protocol.Envelope, p *protocol.OrderWaybill) {
	h.DebugLog.log("order_waybill uuid=%s waybill=%s", p.OrderUUID, p.WaybillID)
	log.Printf("edge_handler: order waybill: uuid=%s waybill=%s", p.OrderUUID, p.WaybillID)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, orders.ReplyWaybill, p.WaybillID, p.ETA, ""); err != nil {
		log.Printf("edge_handler: handle waybill for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderUpdate(env *protocol.Envelope, p *protocol.OrderUpdate) {
	h.DebugLog.log("order_update uuid=%s status=%s", p.OrderUUID, p.Status)
	log.Printf("edge_handler: order update: uuid=%s status=%s", p.OrderUUID, p.Status)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, orders.ReplyUpdate, "", p.ETA, p.Detail); err != nil {
		log.Printf("edge_handler: handle update for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderDelivered(env *protocol.Envelope, p *protocol.OrderDelivered) {
	h.DebugLog.log("order_delivered uuid=%s at=%s", p.OrderUUID, p.DeliveredAt)
	log.Printf("edge_handler: order delivered: uuid=%s at=%s", p.OrderUUID, p.DeliveredAt)
	if err := h.orderMgr.HandleDeliveredWithExpiry(p.OrderUUID, p.DeliveredAt.Format(time.RFC3339), p.StagedExpireAt); err != nil {
		log.Printf("edge_handler: handle delivered for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderError(env *protocol.Envelope, p *protocol.OrderError) {
	h.DebugLog.log("order_error uuid=%s code=%s", p.OrderUUID, p.ErrorCode)
	log.Printf("edge_handler: order error: uuid=%s code=%s detail=%s", p.OrderUUID, p.ErrorCode, p.Detail)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, orders.ReplyError, "", "", p.Detail); err != nil {
		log.Printf("edge_handler: handle error for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderCancelled(env *protocol.Envelope, p *protocol.OrderCancelled) {
	h.DebugLog.log("order_cancelled uuid=%s reason=%s", p.OrderUUID, p.Reason)
	log.Printf("edge_handler: order cancelled: uuid=%s reason=%s", p.OrderUUID, p.Reason)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, orders.ReplyCancelled, "", "", p.Reason); err != nil {
		log.Printf("edge_handler: handle cancelled for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderStaged(env *protocol.Envelope, p *protocol.OrderStaged) {
	h.DebugLog.log("order_staged uuid=%s detail=%s", p.OrderUUID, p.Detail)
	log.Printf("edge_handler: order staged: uuid=%s detail=%s", p.OrderUUID, p.Detail)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, orders.ReplyStaged, "", "", p.Detail); err != nil {
		log.Printf("edge_handler: handle staged for %s: %v", p.OrderUUID, err)
	}
}
