package messaging

import (
	"log"
	"time"

	"shingo/protocol"
)

type DataSender struct {
	client *Client
	topic  string
	stopCh <-chan struct{}

	DebugLog DebugLogFunc
}

func NewDataSender(client *Client, topic string, stopCh <-chan struct{}) *DataSender {
	return &DataSender{client: client, topic: topic, stopCh: stopCh}
}

func (s *DataSender) PublishEnvelope(env *protocol.Envelope, label string) error {
	var err error
	backoff := 2 * time.Second
	for attempt := 0; attempt < 3; attempt++ {
		if err = s.client.PublishEnvelope(s.topic, env); err == nil {
			return nil
		}
		log.Printf("data sender: %s attempt %d failed: %v (retrying in %s)", label, attempt+1, err, backoff)
		select {
		case <-s.stopCh:
			return err
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return err
}
