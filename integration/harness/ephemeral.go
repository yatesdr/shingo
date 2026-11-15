package harness

import (
	"errors"
	"sync"

	"shingo/protocol"
)

// EphemeralTopic stands in for the ephemeral Kafka topic (protocol/ephemeral.go)
// the way memPublisher stands in for the durable ones: what is published is the
// signed bytes production would put on the topic, and what a reader is handed
// goes through the real decode on the far side.
//
// It keeps the topic's two properties a scenario can see: one partition, so
// one order for every reader; and readers with no consumer group that open at
// the LAST offset, so a reader is handed only what is published after it opened
// and a reader opened after an outage is handed nothing from it. Delivery is
// synchronous, on the publisher's goroutine. Nothing expires off the log: the
// scenario reads it whole.
type EphemeralTopic struct {
	key []byte

	mu      sync.Mutex
	log     []EphemeralMessage
	readers map[*EphemeralReader]struct{}
}

// EphemeralMessage is one message on the topic: its offset, its header as the
// reader's decode sees it, and the bytes as published.
type EphemeralMessage struct {
	Offset int
	Header protocol.RawHeader
	Raw    []byte
}

// NewEphemeralTopic is an empty topic whose publishers sign with signingKey,
// as the messaging clients do (nil: unsigned).
func NewEphemeralTopic(signingKey []byte) *EphemeralTopic {
	return &EphemeralTopic{key: signingKey, readers: map[*EphemeralReader]struct{}{}}
}

// Publish encodes and signs env, appends it, and hands it to every open reader.
func (t *EphemeralTopic) Publish(env *protocol.Envelope) error {
	raw, err := env.Encode()
	if err != nil {
		return err
	}
	if len(t.key) > 0 {
		if raw, err = protocol.Sign(raw, t.key); err != nil {
			return err
		}
	}
	hdr, err := protocol.ParseHeader(raw, t.key)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.log = append(t.log, EphemeralMessage{Offset: len(t.log), Header: *hdr, Raw: raw})
	readers := make([]*EphemeralReader, 0, len(t.readers))
	for r := range t.readers {
		readers = append(readers, r)
	}
	t.mu.Unlock()
	for _, r := range readers {
		r.deliver(raw)
	}
	return nil
}

// Messages is every message published so far, in offset order.
func (t *EphemeralTopic) Messages() []EphemeralMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]EphemeralMessage(nil), t.log...)
}

// EphemeralReader is one open reader of the topic.
type EphemeralReader struct {
	topic   *EphemeralTopic
	deliver func([]byte)
	// From is the offset the reader opened at: the topic's length then.
	From int
}

// Reader opens a reader at the topic's last offset that hands each message
// published from now on to deliver.
func (t *EphemeralTopic) Reader(deliver func([]byte)) *EphemeralReader {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := &EphemeralReader{topic: t, deliver: deliver, From: len(t.log)}
	t.readers[r] = struct{}{}
	return r
}

// Close stops the reader. Idempotent.
func (r *EphemeralReader) Close() {
	r.topic.mu.Lock()
	delete(r.topic.readers, r)
	r.topic.mu.Unlock()
}

// EphemeralLink is an Edge engine's link onto a topic (engine.EphemeralLink):
// Start opens a reader at the topic's last offset delivering to Deliver, Stop
// closes it, Publish appends. Opens counts the readers it opened.
type EphemeralLink struct {
	Topic   *EphemeralTopic
	Deliver func([]byte)

	mu     sync.Mutex
	reader *EphemeralReader
	opens  int
}

// Start opens a reader; the topic name is the one the Edge was offered.
func (l *EphemeralLink) Start(string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reader == nil {
		l.reader = l.Topic.Reader(l.Deliver)
		l.opens++
	}
	return nil
}

// Stop closes the reader.
func (l *EphemeralLink) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reader != nil {
		l.reader.Close()
		l.reader = nil
	}
}

// Publish appends env to the topic, and refuses while the link is closed, as
// the messaging client's does.
func (l *EphemeralLink) Publish(env *protocol.Envelope) error {
	l.mu.Lock()
	open := l.reader != nil
	l.mu.Unlock()
	if !open {
		return errors.New("harness: the ephemeral link is not open")
	}
	return l.Topic.Publish(env)
}

// Opens is how many readers the link has opened.
func (l *EphemeralLink) Opens() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opens
}
