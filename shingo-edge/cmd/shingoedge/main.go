package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/messaging"
	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/www"
)

func main() {
	configPath := flag.String("config", "shingoedge.yaml", "path to config file")
	debug := flag.Bool("debug", false, "enable debug logging")
	port := flag.Int("port", 0, "HTTP port (overrides config)")
	flag.Parse()

	if *debug {
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	if *port > 0 {
		cfg.Web.Port = *port
	}

	// Open database
	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	// Create and start engine
	eng := engine.New(engine.Config{
		AppConfig:  cfg,
		ConfigPath: *configPath,
		DB:         db,
		LogFunc:    log.Printf,
		Debug:      *debug,
	})
	eng.Start()
	defer eng.Stop()

	// Ensure Kafka GroupID is set (unique per edge so each gets all messages)
	if cfg.Messaging.Kafka.GroupID == "" {
		cfg.Messaging.Kafka.GroupID = cfg.KafkaGroupID()
	}

	// Set up messaging
	msgClient := messaging.NewClient(&cfg.Messaging)
	defer msgClient.Close()
	if err := msgClient.Connect(); err != nil {
		log.Printf("messaging connect: %v (will retry via outbox)", err)
	} else {
		// Start outbox drainer
		drainer := messaging.NewOutboxDrainer(db, msgClient, &cfg.Messaging)
		drainer.Start()
		defer drainer.Stop()

		// Protocol ingestor (inbound from ShinGo Core)
		stationID := cfg.StationID()
		edgeHandler := messaging.NewEdgeHandler(eng.OrderManager(), func(names []string) {
			eng.SetCoreNodes(names)
		})
		ingestor := protocol.NewIngestor(edgeHandler, func(hdr *protocol.RawHeader) bool {
			return hdr.Dst.Station == stationID || hdr.Dst.Station == protocol.StationBroadcast
		})
		if err := msgClient.Subscribe(cfg.Messaging.DispatchTopic, func(data []byte) {
			ingestor.HandleRaw(data)
		}); err != nil {
			log.Printf("protocol ingestor subscribe: %v", err)
		} else {
			log.Printf("protocol ingestor listening on %s (station=%s)", cfg.Messaging.DispatchTopic, stationID)
		}

		// Heartbeater (registration + periodic heartbeat)
		hb := messaging.NewHeartbeater(msgClient, stationID, "dev", []string{cfg.LineID}, cfg.Messaging.OrdersTopic)
		hb.Start()
		defer hb.Stop()

		// Wire node sync so edge UI can trigger a re-request
		eng.SetNodeSyncFunc(hb.RequestNodeSync)

		// Production reporter (accumulates deltas, sends periodic reports to core)
		reporter := messaging.NewProductionReporter(msgClient, db, stationID, cfg.Messaging.OrdersTopic)
		eng.Events.SubscribeTypes(func(evt engine.Event) {
			if delta, ok := evt.Payload.(engine.CounterDeltaEvent); ok {
				reporter.RecordDelta(delta.JobStyleID, delta.Delta)
			}
		}, engine.EventCounterDelta)
		reporter.Start()
		defer reporter.Stop()
	}

	// Set up HTTP server
	router, stopWeb := www.NewRouter(eng)
	defer stopWeb()

	addr := fmt.Sprintf("%s:%d", cfg.Web.Host, cfg.Web.Port)
	server := &http.Server{Addr: addr, Handler: router}

	// Start HTTP server
	go func() {
		log.Printf("ShinGo Edge listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")

	// Stop SSE event hub first so long-lived connections close
	stopWeb()

	// Graceful HTTP shutdown with 10s deadline
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("http server shutdown: %v", err)
	}
}
