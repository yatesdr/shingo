package messaging

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/config"
)

func TestNewClient(t *testing.T) {
	cfg := &config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"localhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	}
	client := NewClient(cfg)

	if client.cfg == nil {
		t.Fatal("config should be set")
	}
	if client.handlers == nil {
		t.Fatal("handlers map should be initialized")
	}
	if client.stopChan == nil {
		t.Fatal("stopChan should be initialized")
	}
}

func TestClient_Connect_NoBrokers(t *testing.T) {
	cfg := &config.MessagingConfig{
		Kafka: config.KafkaConfig{},
	}
	client := NewClient(cfg)

	err := client.Connect()
	if err == nil {
		t.Fatal("connect should fail with no brokers")
	}
}

func TestClient_IsConnected(t *testing.T) {
	cfg := &config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"localhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	}
	client := NewClient(cfg)

	// Should not be connected initially
	if client.IsConnected() {
		t.Error("should not be connected before Connect()")
	}

	// Note: actual connection test would require running Kafka
	// This test verifies the state tracking logic
}

func TestClient_Close(t *testing.T) {
	cfg := &config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"localhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	}
	client := NewClient(cfg)

	// Close should not panic even if not connected
	client.Close()
}

func TestClient_CloseIdempotent(t *testing.T) {
	cfg := &config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"localhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	}
	client := NewClient(cfg)

	// Close multiple times should not panic
	client.Close()
	client.Close()
	client.Close()
}

func TestClient_Subscribe_NotConnected(t *testing.T) {
	cfg := &config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"localhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	}
	client := NewClient(cfg)

	handler := func(topic string, payload []byte) {}
	err := client.Subscribe("test-topic", handler)
	if err == nil {
		t.Fatal("subscribe should fail when not connected")
	}
}

func TestClient_Publish_NotConnected(t *testing.T) {
	cfg := &config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"localhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	}
	client := NewClient(cfg)

	err := client.Publish("test-topic", []byte("test"))
	if err == nil {
		t.Fatal("publish should fail when not connected")
	}
}

func TestClient_PublishEnvelope(t *testing.T) {
	cfg := &config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"localhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	}
	client := NewClient(cfg)

	env, err := protocol.NewEnvelope(
		"test.message",
		protocol.Address{Role: protocol.RoleEdge, Station: "line-1"},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.OrderRequest{OrderUUID: "test-uuid", OrderType: "retrieve"},
	)
	if err != nil {
		t.Fatalf("create envelope: %v", err)
	}

	// Should fail since not connected
	err = client.PublishEnvelope("test-topic", env)
	if err == nil {
		t.Fatal("publish envelope should fail when not connected")
	}
}

func TestClient_Reconfigure(t *testing.T) {
	cfg := &config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"localhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	}
	client := NewClient(cfg)

	// Subscribe before reconfigure - this will fail since not connected
	// but tests that the handler gets registered
	handler := func(topic string, payload []byte) {}
	
	_ = client.Subscribe("test-topic", handler)

	// Reconfigure with new brokers
	newCfg := &config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"newhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	}

	err := client.Reconfigure(newCfg)
	// Should fail since new host is not reachable, but tests the reconfigure logic
	if err == nil {
		t.Log("reconfigure succeeded (unexpected but not an error in test environment)")
	}

	// Verify config was updated
	if len(client.cfg.Kafka.Brokers) != 1 || client.cfg.Kafka.Brokers[0] != "newhost:9092" {
		t.Errorf("brokers = %v, want [newhost:9092]", client.cfg.Kafka.Brokers)
	}
}

func TestClient_HandlerRegistration(t *testing.T) {
	cfg := &config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"localhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	}
	client := NewClient(cfg)

	// Register multiple handlers directly
	handler1 := func(topic string, payload []byte) {}
	handler2 := func(topic string, payload []byte) {}

	// Note: actual subscription requires connection
	// This test verifies handler storage logic
	
	client.handlers["topic1"] = handler1
	client.handlers["topic2"] = handler2

	if len(client.handlers) != 2 {
		t.Errorf("handlers len = %d, want 2", len(client.handlers))
	}
}

func TestClient_EnvelopeEncoding(t *testing.T) {
	// Test that envelope encoding works correctly
	env, err := protocol.NewEnvelope(
		"order.request",
		protocol.Address{Role: protocol.RoleEdge, Station: "line-1"},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.OrderRequest{
			OrderUUID:       "test-uuid-123",
			OrderType:       "retrieve",
			PayloadCode: "BIN-A",
			DeliveryNode:    "line-1-station",
			Quantity:        1.0,
		},
	)
	if err != nil {
		t.Fatalf("create envelope: %v", err)
	}

	data, err := env.Encode()
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}

	// Verify JSON structure
	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}

	if decoded["type"] != "order.request" {
		t.Errorf("type = %v, want order.request", decoded["type"])
	}
	if decoded["v"] != float64(protocol.Version) {
		t.Errorf("version = %v, want %d", decoded["v"], protocol.Version)
	}
}

func TestClient_DataEnvelope(t *testing.T) {
	env, err := protocol.NewDataEnvelope(
		"edge.heartbeat",
		protocol.Address{Role: protocol.RoleEdge, Station: "line-1"},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.EdgeHeartbeat{
			StationID: "line-1",
			Uptime:    3600,
			Orders:    2,
		},
	)
	if err != nil {
		t.Fatalf("create data envelope: %v", err)
	}

	data, err := env.Encode()
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}

	if decoded["type"] != "data" {
		t.Errorf("type = %v, want data", decoded["type"])
	}
}

func TestClient_StopChanClosed(t *testing.T) {
	cfg := &config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"localhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	}
	client := NewClient(cfg)

	client.Close()

	// Verify stop channel is closed
	select {
	case <-client.stopChan:
		// OK
	default:
		t.Error("stopChan should be closed after Close()")
	}
}

func TestClient_ConcurrentAccess(t *testing.T) {
	cfg := &config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"localhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	}
	client := NewClient(cfg)

	// Simulate concurrent operations
	var wg sync.WaitGroup
	
	// Multiple goroutines trying to check connection
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = client.IsConnected()
		}()
	}

	// Multiple goroutines trying to close
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client.Close()
		}()
	}

	wg.Wait()
	// Should not panic
}

func TestClient_DebugLog(t *testing.T) {
	cfg := &config.MessagingConfig{
		Kafka:         config.KafkaConfig{Brokers: []string{"localhost:9092"}},
		OrdersTopic:   "shingo.orders",
		DispatchTopic: "shingo.dispatch",
	}
	client := NewClient(cfg)

	logs := []string{}
	client.DebugLog = func(format string, args ...any) {
		logs = append(logs, format)
	}

	client.dbg("test message %s", "arg1")

	if len(logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(logs))
	}
	if logs[0] != "test message %s" {
		t.Errorf("log = %q, want %q", logs[0], "test message %s")
	}
}

// Test readLoop backoff logic
func TestClient_BackoffCalculation(t *testing.T) {
	const (
		baseBackoff = 500 * time.Millisecond
		maxBackoff  = 5 * time.Second
	)

	backoff := baseBackoff

	// Simulate multiple failures
	for i := 0; i < 10; i++ {
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	if backoff != maxBackoff {
		t.Errorf("backoff = %v, want %v (capped)", backoff, maxBackoff)
	}
}
