// main.go — ShinGo Edge composition root.
//
// This is the single place where all edge subsystems are created,
// configured, and wired together. Nothing runs until main() stitches
// the pieces.
//
// Startup sequence (main):
//   flags → restore check → debug log → config → DB → engine →
//   backup service → messaging → data sender → outbox drainer →
//   production reporter → Kafka subscribers → HTTP server → shutdown
//
// Kafka wiring lives in setupKafkaSubscribers() because it requires
// a live connection. If Connect fails, the outbox drainer still runs
// and will drain when connectivity returns.
//
// The file also contains the interactive restore flow (--restore flag)
// and its prompt helpers at the bottom.

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
	"shingoedge/countgroup"
	"shingoedge/engine"
	"shingoedge/messaging"
	"shingoedge/store"
	"shingoedge/www"
)

// edgeFlags holds parsed command-line flags.
type edgeFlags struct {
	configPath  string
	port        int
	restoreMode bool
	debugFlag   bool
	fileFilter  []string // nil = no file; empty = all subsystems; populated = specific
}

// parseFlags handles the custom --log-debug stripping and standard flag parsing.
func parseFlags() edgeFlags {
	filteredArgs, fileFilter := debuglog.ParseDebugFlag(os.Args[1:])
	debugFlag := fileFilter != nil
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

	return edgeFlags{
		configPath:  *configPath,
		port:        *port,
		restoreMode: *restoreMode,
		debugFlag:   debugFlag,
		fileFilter:  fileFilter,
	}
}

func mustInitDebugLog(fileFilter []string) *debuglog.Logger {
	dbg, err := debuglog.New(1000, fileFilter)
	if err != nil {
		log.Fatalf("debug log: %v", err)
	}
	return dbg
}

func mustLoadConfig(path string, portOverride int) *config.Config {
	cfg, err := config.Load(path)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if portOverride > 0 {
		cfg.Web.Port = portOverride
	}
	return cfg
}

func mustOpenDatabase(path string) *store.DB {
	db, err := store.Open(path)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	return db
}

func startHTTPServer(addr string, handler http.Handler) *http.Server {
	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		log.Printf("ShinGo Edge listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()
	return srv
}

// setupKafkaSubscribers wires protocol ingestor, heartbeater, and all handler
// callbacks that require a live Kafka connection. Called only when Connect succeeds.
//
// cgHandler may be nil — countgroup is an optional feature. If non-nil, the
// handler's MarkStarted() is called after the Kafka subscribe succeeds, which
// enables the heartbeat writer (deadman). See countgroup/handler.go for the
// `started` guard rationale.
func setupKafkaSubscribers(eng *engine.Engine, msgClient *messaging.Client, cfg *config.Config, dbg *debuglog.Logger, stationID string, db *store.DB, cgHandler *countgroup.Handler) {
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
		// Kafka subscription is live — flip the countgroup started flag
		// so the heartbeat writer can begin. Before this moment the
		// heartbeat is intentionally suppressed so the PLC deadman
		// trips ON during the startup window (fail-safe).
		if cgHandler != nil {
			cgHandler.MarkStarted()
			log.Printf("countgroup: subscription confirmed, heartbeat enabled")
		}
	}

	// ── Heartbeater (registration + periodic heartbeat) ────────────────
	hb := messaging.NewHeartbeater(msgClient, stationID, "dev", []string{cfg.LineID}, cfg.Messaging.OrdersTopic, func() int {
		return db.CountActiveOrders()
	})
	hb.DebugLog = messaging.DebugLogFunc(dbg.Func("heartbeat"))
	hb.Start()
	// Note: hb.Stop() is not deferred here — it lives for the process lifetime
	// and is cleaned up by the Kafka client close.

	// ── Handler callbacks (catalog, status, registration) ───────────────
	eng.SetNodeSyncFunc(hb.RequestNodeSync)

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
		// Sync manual_swap claims to Core's demand registry. Pre-side-cycle
		// this also called StartupSweepManualSwap to seed empty-in orders
		// at every loader — unnecessary now that empty-ins are driven by
		// line REQUESTs through MaybeCreateLoaderEmptyIn.
		go eng.SendClaimSync()
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
	if cgHandler != nil {
		edgeHandler.SetCountGroupCommandHandler(func(cmd protocol.CountGroupCommand) {
			cgHandler.OnCommand(cmd)
		})
	}

	if err := eng.StartupReconcile(); err != nil {
		log.Printf("initial startup reconcile: %v", err)
	}
}

func awaitShutdown(srv *http.Server, stopWeb func()) {
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
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("http server shutdown: %v", err)
	}
}

func main() {
	// ── Flags & restore ─────────────────────────────────────────────────
	flags := parseFlags()

	if flags.debugFlag {
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}

	if flags.restoreMode {
		if err := runInteractiveRestore(flags.configPath); err != nil {
			log.Fatalf("interactive restore: %v", err)
		}
	}
	if err := backup.ApplyPendingRestore(flags.configPath, log.Printf); err != nil {
		log.Fatalf("apply pending restore: %v", err)
	}

	// ── Debug log & config ─────────────────────────────────────────────
	dbg := mustInitDebugLog(flags.fileFilter)
	defer dbg.Close()

	cfg := mustLoadConfig(flags.configPath, flags.port)

	// ── Database ────────────────────────────────────────────────────────
	db := mustOpenDatabase(cfg.DatabasePath)
	defer db.Close()

	// ── Engine ──────────────────────────────────────────────────────────
	eng := engine.New(engine.Config{
		AppConfig:   cfg,
		ConfigPath:  flags.configPath,
		DB:          db,
		LogFunc:     log.Printf,
		DebugLogger: dbg,
	})
	eng.Start()
	defer eng.Stop()

	// ── Backup service ─────────────────────────────────────────────────
	backupSvc := backup.NewService(db, cfg, flags.configPath, "dev", log.Printf)
	backupSvc.Start()
	defer backupSvc.Stop()

	// ── Messaging (Kafka) ───────────────────────────────────────────────
	if cfg.Messaging.Kafka.GroupID == "" {
		cfg.Messaging.Kafka.GroupID = cfg.KafkaGroupID()
	}
	msgClient := messaging.NewClient(&cfg.Messaging)
	msgClient.DebugLog = messaging.DebugLogFunc(dbg.Func("kafka"))
	if cfg.Messaging.SigningKey != "" {
		msgClient.SigningKey = []byte(cfg.Messaging.SigningKey)
		log.Printf("shingoedge: envelope signing enabled")
	}
	defer msgClient.Close()

	// ── Data sender & outbox drainer ────────────────────────────────────
	dataSender := messaging.NewDataSender(msgClient, cfg.Messaging.OrdersTopic, nil)
	dataSender.DebugLog = messaging.DebugLogFunc(dbg.Func("heartbeat"))
	eng.SetSendFunc(func(env *protocol.Envelope) error {
		return dataSender.PublishEnvelope(env, "core data sync")
	})
	eng.SetKafkaReconnectFunc(msgClient.Reconnect)

	// Outbox drainer — runs unconditionally, drains when connected
	drainer := messaging.NewOutboxDrainer(db, msgClient, &cfg.Messaging)
	drainer.DebugLog = messaging.DebugLogFunc(dbg.Func("outbox"))
	drainer.Start()
	defer drainer.Stop()

	// ── Production reporter ────────────────────────────────────────────
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

	// ── Count-group handler (advanced-zone light alerts) ────────────────
	// Constructed before Kafka connect so the heartbeat writer can start
	// immediately (but gated by `started` until subscription confirms).
	// Handler is nil if feature disabled / no bindings — setupKafkaSubscribers
	// tolerates nil and simply doesn't register the handler.
	var cgHandler *countgroup.Handler
	var cgHeartbeat *countgroup.HeartbeatWriter
	if len(cfg.CountGroups.Bindings) > 0 {
		cgHandler = countgroup.New(cfg.CountGroups, eng.PLCManager(), eng.SendCountGroupAck, log.Printf)
		cgHeartbeat = countgroup.NewHeartbeatWriter(cgHandler, log.Printf)
		cgHeartbeat.Start()
		defer cgHeartbeat.Stop()
		log.Printf("countgroup: edge handler active (%d bindings)", len(cfg.CountGroups.Bindings))
	}

	// ── Kafka connect & subscribe ───────────────────────────────────────
	if err := msgClient.Connect(); err != nil {
		log.Printf("messaging connect: %v (outbox drainer active, will drain when connected)", err)
	} else {
		setupKafkaSubscribers(eng, msgClient, cfg, dbg, stationID, db, cgHandler)
	}

	// ── HTTP server & shutdown ──────────────────────────────────────────
	router, stopWeb := www.NewRouter(eng, dbg, backupSvc)
	addr := fmt.Sprintf("%s:%d", cfg.Web.Host, cfg.Web.Port)
	srv := startHTTPServer(addr, router)

	awaitShutdown(srv, stopWeb)
}

// ── Interactive restore flow (--restore flag) ───────────────────────

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

// ── Prompt helpers ──────────────────────────────────────────────────

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
