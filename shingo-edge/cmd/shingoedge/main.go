package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingoedge/backup"
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
	restoreMode := flag.Bool("restore", false, "interactive restore from backup storage before starting")
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

	if *restoreMode {
		if err := runInteractiveRestore(*configPath); err != nil {
			log.Fatalf("interactive restore: %v", err)
		}
	}

	if err := backup.ApplyPendingRestore(*configPath, log.Printf); err != nil {
		log.Fatalf("apply pending restore: %v", err)
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

	backupSvc := backup.NewService(db, cfg, *configPath, "dev", log.Printf)
	backupSvc.Start()
	defer backupSvc.Stop()

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
			reporter.RecordDelta(delta.StyleID, delta.Delta)
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
		// Re-sync on registration ack only if we've been running for a while
		// (startup already sends these; avoid triple-send at boot)
		edgeHandler.SetRegisteredHandler(func() {
			if eng.Uptime() > 30 {
				hb.RequestNodeSync()
				hb.RequestCatalogSync()
			}
			// Sync manual_swap claims to Core's demand registry, then sweep
			// to create initial orders for any payloads missing demand.
			// On re-registration (uptime > 30), only ClaimSync — the sweep
			// is unnecessary because tryAutoRequest dedup would no-op, and
			// repeated delete+insert on demand_registry is churn on flaky links.
			go func() {
				eng.SendClaimSync()
				if eng.Uptime() <= 30 {
					eng.StartupSweepManualSwap()
				}
			}()
		})
		edgeHandler.SetRegisterRequestHandler(func() {
			hb.SendRegister()
			if err := eng.StartupReconcile(); err != nil {
				log.Printf("startup reconcile after register request: %v", err)
			}
		})
		edgeHandler.SetNodeStructureChangedHandler(func() {
			hb.RequestNodeSync()
		})
		edgeHandler.SetDemandSignalHandler(func(signal *protocol.DemandSignal) {
			go eng.HandleDemandSignal(signal)
		})

		if err := eng.StartupReconcile(); err != nil {
			log.Printf("initial startup reconcile: %v", err)
		}
	}

	// Set up HTTP server
	router, stopWeb := www.NewRouter(eng, dbg, backupSvc)
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

	// Force exit on second signal
	go func() {
		<-sigCh
		log.Println("Forced shutdown")
		os.Exit(1)
	}()

	// Stop SSE event hub first so long-lived connections close
	stopWeb()

	// Graceful HTTP shutdown with 10s deadline
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("http server shutdown: %v", err)
	}
}

func runInteractiveRestore(configPath string) error {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("ShinGo Edge interactive restore")
	fmt.Println("Provide minimal backup storage settings to restore this machine before startup.")

	stationID, err := promptNonEmpty(reader, "Station ID")
	if err != nil {
		return err
	}
	endpoint, err := promptNonEmpty(reader, "S3 Endpoint URL")
	if err != nil {
		return err
	}
	bucket, err := promptNonEmpty(reader, "Bucket")
	if err != nil {
		return err
	}
	region, err := promptWithDefault(reader, "Region", "us-east-1")
	if err != nil {
		return err
	}
	accessKey, err := promptNonEmpty(reader, "Access Key")
	if err != nil {
		return err
	}
	secretKey, err := promptNonEmpty(reader, "Secret Key")
	if err != nil {
		return err
	}
	usePathStyle, err := promptYesNo(reader, "Use path-style S3", true)
	if err != nil {
		return err
	}
	insecureSkip, err := promptYesNo(reader, "Skip TLS verification", false)
	if err != nil {
		return err
	}

	s3cfg := config.BackupS3Config{
		Endpoint:              endpoint,
		Bucket:                bucket,
		Region:                region,
		AccessKey:             accessKey,
		SecretKey:             secretKey,
		UsePathStyle:          usePathStyle,
		InsecureSkipTLSVerify: insecureSkip,
	}

	storage, err := backup.NewS3Storage(s3cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := storage.Test(ctx, stationID); err != nil {
		return fmt.Errorf("storage test failed: %w", err)
	}
	fmt.Println("Connection test succeeded.")

	backups, err := backup.ListBackupsWithConfig(ctx, s3cfg, stationID)
	if err != nil {
		return err
	}
	if len(backups) == 0 {
		return fmt.Errorf("no backups found for station %q", stationID)
	}
	if len(backups) > 20 {
		backups = backups[:20]
	}
	fmt.Println("Available backups:")
	for i, item := range backups {
		created := item.CreatedAt
		if created == nil {
			created = item.LastModified
		}
		when := "unknown"
		if created != nil {
			when = created.UTC().Format(time.RFC3339)
		}
		fmt.Printf("  %d. %s  %s  %s\n", i+1, when, humanBytes(item.Size), item.Key)
	}
	selectionText, err := promptNonEmpty(reader, "Select backup number")
	if err != nil {
		return err
	}
	selection, err := strconv.Atoi(selectionText)
	if err != nil || selection < 1 || selection > len(backups) {
		return fmt.Errorf("invalid selection")
	}
	selected := backups[selection-1]

	confirm, err := promptNonEmpty(reader, "Type the station ID to confirm restore")
	if err != nil {
		return err
	}
	if confirm != stationID {
		return fmt.Errorf("confirmation station ID did not match")
	}

	restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer restoreCancel()
	if err := backup.RestoreNow(restoreCtx, configPath, s3cfg, stationID, selected.Key); err != nil {
		return err
	}
	fmt.Println("Restore completed successfully. Launching ShinGo Edge...")
	return nil
}

func promptNonEmpty(reader *bufio.Reader, label string) (string, error) {
	for {
		fmt.Printf("%s: ", label)
		text, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		text = strings.TrimSpace(text)
		if text != "" {
			return text, nil
		}
	}
}

func promptWithDefault(reader *bufio.Reader, label, def string) (string, error) {
	fmt.Printf("%s [%s]: ", label, def)
	text, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return def, nil
	}
	return text, nil
}

func promptYesNo(reader *bufio.Reader, label string, def bool) (bool, error) {
	defText := "y/N"
	if def {
		defText = "Y/n"
	}
	for {
		fmt.Printf("%s [%s]: ", label, defText)
		text, err := reader.ReadString('\n')
		if err != nil {
			return false, err
		}
		text = strings.ToLower(strings.TrimSpace(text))
		if text == "" {
			return def, nil
		}
		if text == "y" || text == "yes" {
			return true, nil
		}
		if text == "n" || text == "no" {
			return false, nil
		}
	}
}

func humanBytes(v int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(v)
	idx := 0
	for size >= 1024 && idx < len(units)-1 {
		size /= 1024
		idx++
	}
	if idx == 0 {
		return fmt.Sprintf("%d %s", v, units[idx])
	}
	return fmt.Sprintf("%.1f %s", size, units[idx])
}
