package messaging

import (
	"log"
)

// InboundHandler is called for each decoded inbound message.
type InboundHandler interface {
	HandleOrderRequest(env *Envelope, req OrderRequest)
	HandleOrderCancel(env *Envelope, req OrderCancel)
	HandleDeliveryReceipt(env *Envelope, req DeliveryReceipt)
	HandleRedirectRequest(env *Envelope, req RedirectRequest)
}

// Consumer subscribes to the orders topic and routes messages to the handler.
type Consumer struct {
	client  *Client
	topic   string
	handler InboundHandler
}

func NewConsumer(client *Client, topic string, handler InboundHandler) *Consumer {
	return &Consumer{
		client:  client,
		topic:   topic,
		handler: handler,
	}
}

func (c *Consumer) Start() error {
	return c.client.Subscribe(c.topic, c.handleMessage)
}

func (c *Consumer) handleMessage(_ string, payload []byte) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		log.Printf("consumer: decode error: %v", err)
		return
	}

	switch p := env.Payload.(type) {
	case OrderRequest:
		c.handler.HandleOrderRequest(env, p)
	case OrderCancel:
		c.handler.HandleOrderCancel(env, p)
	case DeliveryReceipt:
		c.handler.HandleDeliveryReceipt(env, p)
	case RedirectRequest:
		c.handler.HandleRedirectRequest(env, p)
	default:
		log.Printf("consumer: unhandled payload type: %T", p)
	}
}
