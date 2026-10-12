package messaging

import (
	"net"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"shingocore/config"
)

// A broker that accepts and never answers does not hold Connect: ensureTopics
// gives up after the broker-probe timeout. Connect holds the client's lock
// across it, so an unbounded wait stalled every reader of that lock (the health
// strip, the config page, the outbox drainer).
func TestEnsureTopics_ASilentBrokerDoesNotHoldIt(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // held open, never answered
		}
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn := kafka.NewConn(raw, "", 0)
	defer conn.Close()

	c := NewClient(&config.MessagingConfig{Kafka: config.KafkaConfig{DialTimeout: 300 * time.Millisecond}})
	done := make(chan struct{})
	go func() {
		c.ensureTopics(conn, "orders")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ensureTopics still waiting on a silent broker after 5 s; want it bounded by the 300 ms probe timeout")
	}
}
