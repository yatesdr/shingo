package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/messaging"
	"shingoedge/store"
	"shingoedge/www"
)

func main() {
	// Strip --log-debug / -log-debug from os.Args before flag.Parse,
	// so bare --log-debug (no value) and --log-debug=FILTER both work.
	var fileFilter []string // nil = no file; []string{} = all; populated = specific
	debugFlag := false
	var filteredArgs []string
	for _, arg := range os.Args[1:] {
		switch {
		case arg == "--log-debug" || arg == "-log-debug":
			debugFlag = true
			fileFilter = []string{} // all subsystems
		case strings.HasPrefix(arg, "--log-debug=") || strings.HasPrefix(arg, "-log-debug="):
			debugFlag = true
			val := arg[strings.Index(arg, "=")+1:]
			if val == "" {
				fileFilter = []string{}
			} else {
				fileFilter = strings.Split(val, ",")
			}
		default:
			filteredArgs = append(filteredArgs, arg)
		}
	}
	os.Args = append(os.Args[:1], filteredArgs...)

	configPath := flag.String("config", "shingoedge.yaml", "path to config file")
	port := flag.Int("port", 0, "HTTP port (overrides config)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: shingoedge [flags]\n\nFlags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), "  --log-debug[=FILTER]\n")
		fmt.Fprintf(flag.CommandLine.Output(), "        Enable debug log file. FILTER is optional comma-separated subsystems:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "        engine, plc, orders, changeover, kafka, edge_handler,\n")
		fmt.Fprintf(flag.CommandLine.Output(), "        heartbeat, outbox, reporter, protocol\n")
	}
	flag.Parse()

	if debugFlag {
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}

	// Create debug logger (ring buffer always active; file only with --log-debug)
	dbg, err := debuglog.New(1000, fileFilter)
	if err != nil {
		log.Fatalf("debug log: %v", err)
	}
	defer dbg.Close()

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
		AppConfig:   cfg,
		ConfigPath:  *configPath,
		DB:          db,
		LogFunc:     log.Printf,
		DebugLogger: dbg,
	})
	eng.Start()
	defer eng.Stop()

	// Ensure Kafka GroupID is set (unique per edge so each gets all messages)
	if cfg.Messaging.Kafka.GroupID == "" {
		cfg.Messaging.Kafka.GroupID = cfg.KafkaGroupID()
	}

	// Set up messaging
	msgClient := messaging.NewClient(&cfg.Messaging)
	msgClient.DebugLog = messaging.DebugLogFunc(dbg.Func("kafka"))
	if cfg.Messaging.SigningKey != "" {
		msgClient.SigningKey = []byte(cfg.Messaging.SigningKey)
		log.Printf("shingoedge: envelope signing enabled")
	}
	defer msgClient.Close()

	// Wire send function and reconnect unconditionally — they self-gate on connection state
	dataSender := messaging.NewDataSender(msgClient, cfg.Messaging.OrdersTopic, nil)
	dataSender.DebugLog = messaging.DebugLogFunc(dbg.Func("heartbeat"))
	eng.SetSendFunc(func(env *protocol.Envelope) error {
		return dataSender.PublishEnvelope(env, "core data sync")
	})
	eng.SetKafkaReconnectFunc(msgClient.Reconnect)

	// Start outbox drainer unconditionally — it checks IsConnected() each cycle
	// and skips when Kafka is unavailable, but messages still accumulate in the
	// outbox and will drain once the connection is established.
	drainer := messaging.NewOutboxDrainer(db, msgClient, &cfg.Messaging)
	drainer.DebugLog = messaging.DebugLogFunc(dbg.Func("outbox"))
	drainer.Start()
	defer drainer.Stop()

	// Production reporter uses the outbox for delivery — always start it
	stationID := cfg.StationID()
	reporter := messaging.NewProductionReporter(db, stationID)
	reporter.DebugLog = messaging.DebugLogFunc(dbg.Func("reporter"))
	eng.Events.SubscribeTypes(func(evt engine.Event) {
		if delta, ok := evt.Payload.(engine.CounterDeltaEvent); ok {
			reporter.RecordDelta(delta.JobStyleID, delta.Delta)
		}
	}, engine.EventCounterDelta)
	reporter.Start()
	defer reporter.Stop()

	// Connect to Kafka — if unavailable, the outbox drainer and reporter still
	// run (they self-gate). Subscription and heartbeat require a live connection.
	if err := msgClient.Connect(); err != nil {
		log.Printf("messaging connect: %v (outbox drainer active, will drain when connected)", err)
	} else {
		// Protocol ingestor (inbound from ShinGo Core)
		edgeHandler := messaging.NewEdgeHandler(eng.OrderManager(), func(nodes []protocol.NodeInfo) {
			eng.SetCoreNodes(nodes)
		})
		edgeHandler.DebugLog = messaging.DebugLogFunc(dbg.Func("edge_handler"))
		ingestor := protocol.NewIngestor(edgeHandler, func(hdr *protocol.RawHeader) bool {
			return hdr.Dst.Station == stationID || hdr.Dst.Station == protocol.StationBroadcast
		})
		ingestor.DebugLog = dbg.Func("protocol")
		if cfg.Messaging.SigningKey != "" {
			ingestor.SigningKey = []byte(cfg.Messaging.SigningKey)
		}
		if err := msgClient.Subscribe(cfg.Messaging.DispatchTopic, func(data []byte) {
			ingestor.HandleRaw(data)
		}); err != nil {
			log.Printf("protocol ingestor subscribe: %v", err)
		} else {
			log.Printf("protocol ingestor listening on %s (station=%s)", cfg.Messaging.DispatchTopic, stationID)
		}

		// Heartbeater (registration + periodic heartbeat)
		hb := messaging.NewHeartbeater(msgClient, stationID, "dev", []string{cfg.LineID}, cfg.Messaging.OrdersTopic, func() int {
			return db.CountActiveOrders()
		})
		hb.DebugLog = messaging.DebugLogFunc(dbg.Func("heartbeat"))
		hb.Start()
		defer hb.Stop()

		// Wire node sync so edge UI can trigger a re-request
		eng.SetNodeSyncFunc(hb.RequestNodeSync)

		// Wire payload catalog sync
		edgeHandler.SetPayloadCatalogHandler(func(entries []protocol.CatalogPayloadInfo) {
			eng.HandlePayloadCatalog(entries)
		})
		edgeHandler.SetOrderStatusHandler(func(items []protocol.OrderStatusSnapshot) {
			eng.HandleOrderStatusSnapshots(items)
		})
		eng.SetCatalogSyncFunc(hb.RequestCatalogSync)
		edgeHandler.SetRegisterRequestHandler(func() {
			hb.SendRegister()
			if err := eng.StartupReconcile(); err != nil {
				log.Printf("startup reconcile after register request: %v", err)
			}
		})

		if err := eng.StartupReconcile(); err != nil {
			log.Printf("initial startup reconcile: %v", err)
		}
	}

	// Set up HTTP server
	router, stopWeb := www.NewRouter(eng, dbg)
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
