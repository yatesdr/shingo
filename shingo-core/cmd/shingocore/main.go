// main.go — ShinGo Core composition root.
//
// This is the single place where all subsystems are created, configured,
// and wired together. Nothing runs until main() stitches the pieces.
//
// Startup sequence (main):
//   flags → debug log → config → DB → fleet adapter → messaging →
//   engine → protocol ingestor → outbox drainer → web server → shutdown
//
// Helper functions are ordered to match that sequence.
// Each helper is prefixed must*/maybe* to signal whether it can fail.
//
// To find where a subsystem is created, search for its constructor name
// (e.g. engine.New, protocol.NewIngestor, messaging.NewOutboxDrainer).

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embed the IANA tz database so PLANT_TIMEZONE (Q-004)
	// resolves on any host regardless of OS tzdata — air-gapped single-binary
	// deploys (Proxmox VMs) can't rely on system zoneinfo being present.

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingo/protocol/router"
	"shingocore/config"
	"shingocore/countgroup"
	"shingocore/engine"
	"shingocore/fleet/seerrds"
	"shingocore/messaging"
	"shingocore/messaging/middleware"
	"shingocore/rds"
	"shingocore/store"
	"shingocore/www"
)

var Version = "dev"

// coreFlags holds parsed command-line flags.
type coreFlags struct {
	configPath string
	resetDB    bool
	fileFilter []string // nil = no file; empty = all subsystems; populated = specific
}

// parseFlags handles the custom --log-debug stripping and standard flag parsing.
// Exits on --help or --version.
func parseFlags() coreFlags {
	filteredArgs, fileFilter := debuglog.ParseDebugFlag(os.Args[1:])
	os.Args = append(os.Args[:1], filteredArgs...)

	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "shingocore.yaml", "path to config file")
	resetDB := flag.Bool("reset-db", false, "wipe database before starting (requires confirmation)")
	showHelp := flag.Bool("help", false, "show help")
	flag.Parse()

	if *showHelp {
		printUsage()
		os.Exit(0)
	}
	if *showVersion {
		fmt.Println("shingocore", Version)
		os.Exit(0)
	}

	return coreFlags{configPath: *configPath, resetDB: *resetDB, fileFilter: fileFilter}
}

func printUsage() {
	fmt.Println("Usage: shingocore [options]")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  --config PATH         config file path (default: shingocore.yaml)")
	fmt.Println("  --reset-db            wipe database before starting (requires confirmation)")
	fmt.Println("  --version             show version")
	fmt.Println("  --log-debug[=FILTER]  enable debug log to shingo-debug.log")
	fmt.Println("                        FILTER: comma-separated subsystems (default: all)")
	fmt.Println("  --help                show this help")
	fmt.Println()
	fmt.Println("Debug subsystems:")
	fmt.Println("  rds           Fleet manager (Seer RDS) HTTP requests/responses")
	fmt.Println("  kafka         Kafka connect, publish, subscribe, receive")
	fmt.Println("  dispatch      Order lifecycle: request routing, fleet dispatch")
	fmt.Println("  protocol      Protocol envelope decode/encode")
	fmt.Println("  outbox        Outbox drain cycles and delivery")
	fmt.Println("  core_handler  Inbound message handler dispatch")
	fmt.Println("  engine        Engine wiring, vendor status changes")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  shingocore --log-debug              # all subsystems to file")
	fmt.Println("  shingocore --log-debug=rds           # only RDS to file")
	fmt.Println("  shingocore --log-debug=rds,dispatch  # RDS + dispatch to file")
}

func mustInitDebugLog(fileFilter []string) *debuglog.Logger {
	dbg, err := debuglog.New(1000, fileFilter)
	if err != nil {
		log.Fatalf("debug log: %v", err)
	}
	if dbg.FileEnabled() {
		if len(fileFilter) > 0 {
			log.Printf("shingocore: debug log enabled (file: shingo-debug.log, subsystems: %s)", strings.Join(fileFilter, ","))
		} else {
			log.Printf("shingocore: debug log enabled (file: shingo-debug.log, all subsystems)")
		}
	}
	return dbg
}

func mustLoadConfig(path string) *config.Config {
	cfg, err := config.Load(path)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	return cfg
}

func maybeResetDB(resetDB bool, cfg *config.Config) {
	if !resetDB {
		return
	}
	fmt.Fprintf(os.Stderr, "WARNING: This will permanently delete all data in the database.\n")
	fmt.Fprintf(os.Stderr, "Type 'yes' to confirm: ")
	var answer string
	fmt.Scanln(&answer)
	if answer != "yes" {
		fmt.Fprintln(os.Stderr, "Aborted.")
		os.Exit(1)
	}
	if err := store.ResetDatabase(&cfg.Database); err != nil {
		log.Fatalf("reset database: %v", err)
	}
	log.Printf("shingocore: database reset complete")
}

func mustOpenDatabase(cfg *config.Config) *store.DB {
	db, err := store.Open(&cfg.Database)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	log.Printf("shingocore: database open (postgres)")
	return db
}

func startHTTPServer(addr string, handler http.Handler) *http.Server {
	srv := &http.Server{
		Addr:        addr,
		Handler:     handler,
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		log.Printf("shingocore: web server listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("web server: %v", err)
		}
	}()
	return srv
}

func awaitShutdown(srv *http.Server, stopWeb func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Printf("shingocore: shutting down...")
	stopWeb()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)

	log.Printf("shingocore: stopped")
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			log.Printf("PANIC main: %v\n%s", r, stack)
			// Persistent crash file (when SHINGO_PANIC_LOG is set by systemd unit)
			if path := os.Getenv("SHINGO_PANIC_LOG"); path != "" {
				if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
					fmt.Fprintf(f, "%s component=main panic: %v\n%s\n---\n",
						time.Now().UTC().Format(time.RFC3339Nano),
						r, stack)
					f.Close()
				}
			}
			os.Exit(1)
		}
	}()
	// IMPORTANT: this recover only catches panics on the main goroutine
	// — composition root, awaitShutdown, signal handling. Panics in
	// other goroutines bypass this and are caught by the Go runtime +
	// systemd supervisor + journald capture. SHINGO_PANIC_LOG path is
	// set by the systemd unit.

	// ── Flags & config ──────────────────────────────────────────────────
	flags := parseFlags()

	dbg := mustInitDebugLog(flags.fileFilter)
	defer dbg.Close()

	cfg := mustLoadConfig(flags.configPath)
	maybeResetDB(flags.resetDB, cfg)

	// ── Database ────────────────────────────────────────────────────────
	db := mustOpenDatabase(cfg)
	defer db.Close()

	// ── Fleet backend (Seer RDS adapter) ────────────────────────────────
	fleetAdapter := seerrds.New(seerrds.Config{
		BaseURL:      cfg.RDS.BaseURL,
		Timeout:      cfg.RDS.Timeout,
		PollInterval: cfg.RDS.PollInterval,
		DebugLog:     dbg.Func("rds"),
	})
	if err := fleetAdapter.Ping(); err == nil {
		log.Printf("shingocore: fleet backend connected (%s)", fleetAdapter.Name())
	} else {
		log.Printf("shingocore: fleet backend not available (%v)", err)
	}

	// ── Messaging (Kafka) ───────────────────────────────────────────────
	msgClient := messaging.NewClient(&cfg.Messaging)
	msgClient.DebugLog = dbg.Func("kafka")
	if cfg.Messaging.SigningKey != "" {
		msgClient.SigningKey = []byte(cfg.Messaging.SigningKey)
	}
	if err := msgClient.Connect(); err != nil {
		log.Printf("shingocore: messaging connect failed (%v)", err)
	} else {
		log.Printf("shingocore: messaging connected (kafka)")
	}
	defer msgClient.Close()

	// ── Engine ──────────────────────────────────────────────────────────
	eng := engine.New(engine.Config{
		AppConfig:  cfg,
		ConfigPath: flags.configPath,
		DB:         db,
		Fleet:      fleetAdapter,
		MsgClient:  msgClient,
		DebugLog:   dbg.Func("engine"),
	})

	// ── Count-group runner (advanced-zone light alerts) ────────────────
	// Uses a dedicated short-timeout RDS client separate from the 10s
	// fleet adapter so one slow response can't back up N poll cycles.
	// Always register the builder so the Traffic UI can add groups at
	// runtime. Runner.Start() is a no-op if no groups are enabled.
	{
		cgTimeout := cfg.CountGroups.RDSTimeout
		if cgTimeout <= 0 {
			cgTimeout = 400 * time.Millisecond
		}
		cgClient := rds.NewClient(cfg.RDS.BaseURL, cgTimeout)
		cgClient.DebugLog = dbg.Func("countgroup")
		eng.SetCountGroupRunner(func(em countgroup.Emitter) *countgroup.Runner {
			return countgroup.NewRunner(cfg.CountGroups, cgClient, em, log.Printf)
		})
	}

	eng.Start()
	defer eng.Stop()

	eng.Dispatcher().DebugLog = dbg.Func("dispatch")

	// ── Protocol ingestor (inbound from ShinGo Edge) ───────────────────
	coreHandler := messaging.NewCoreHandler(db, msgClient, cfg.Messaging.StationID, cfg.Messaging.DispatchTopic, eng.Dispatcher())
	coreHandler.DebugLog = dbg.Func("core_handler")
	coreHandler.StaleEdgeThreshold = cfg.Messaging.StaleEdgeThreshold
	coreHandler.Start()
	defer coreHandler.Stop()

	// ── Subject router (Data sub-dispatch) ─────────────────────────────
	// Every Subject Core handles is registered explicitly here against
	// a CoreDataService method — same shape as cmd/shingoedge/main.go's
	// SubjectRouter wiring. CoreDataService is constructed at this
	// composition root rather than buried inside NewCoreHandler so the
	// dispatch table is grep-able from one place.
	coreDataService := messaging.NewCoreDataService(db, coreHandler)
	// Wire the UOP-threshold monitor so claim-sync threshold changes
	// reset debounce timers and bucket-applied events drive
	// re-evaluation. Engine.Start() has already constructed the monitor
	// and kicked its startup-sweep goroutine.
	coreDataService.SetThresholdMonitor(eng.ThresholdMonitor())

	subjectRouter := router.NewSubject()
	router.RegisterSubject(subjectRouter, protocol.SubjectEdgeRegister, coreDataService.HandleEdgeRegister)
	router.RegisterSubject(subjectRouter, protocol.SubjectEdgeHeartbeat, coreDataService.HandleEdgeHeartbeat)
	router.RegisterSubjectBare(subjectRouter, protocol.SubjectNodeListRequest, coreDataService.HandleNodeListRequest)
	router.RegisterSubject(subjectRouter, protocol.SubjectProductionReport, coreDataService.HandleProductionReport)
	router.RegisterSubject(subjectRouter, protocol.SubjectTagVerifyRequest, coreDataService.HandleTagVerifyRequest)
	router.RegisterSubjectBare(subjectRouter, protocol.SubjectCatalogPayloadsRequest, coreDataService.HandleCatalogPayloadsRequest)
	router.RegisterSubject(subjectRouter, protocol.SubjectNodeStateRequest, coreDataService.HandleNodeStateRequest)
	router.RegisterSubject(subjectRouter, protocol.SubjectOrderStatusRequest, coreDataService.HandleOrderStatusRequest)
	router.RegisterSubject(subjectRouter, protocol.SubjectClaimSync, coreDataService.HandleClaimSync)
	router.RegisterSubject(subjectRouter, protocol.SubjectCountGroupAck, coreDataService.HandleCountGroupAck)
	router.RegisterSubject(subjectRouter, protocol.SubjectBinUOPDelta, coreDataService.HandleBinUOPDelta)
	router.RegisterSubject(subjectRouter, protocol.SubjectLinesideBucketDelta, coreDataService.HandleLinesideBucketDelta)
	router.RegisterSubject(subjectRouter, protocol.SubjectProductionTick, coreDataService.HandleProductionTick)
	// Fan projected ticks out to the engine event bus so the SSE layer can
	// rebroadcast them as cell-heartbeat (Phase E). Set before the projection
	// worker starts so it reads the emitter race-free.
	coreDataService.SetCellTickEmitter(func(station string, processID, styleID int64, recordedAt time.Time) {
		eng.Events.Emit(engine.Event{Type: engine.EventCellTick, Payload: engine.CellTickEvent{
			Station: station, ProcessID: processID, StyleID: styleID, RecordedAt: recordedAt,
		}})
	})
	// Launch the async cell_part_events projection worker + partition manager
	// (plan §12). Must follow registration; the handler only enqueues.
	coreDataService.StartHeartbeatProjection()
	for _, s := range protocol.CoreInboundSubjects() {
		if !subjectRouter.Has(s) {
			log.Fatalf("shingocore: subject router missing handler for %s — composition root is incomplete", s)
		}
	}

	ingestor := protocol.NewIngestor(func(_ *protocol.RawHeader) bool { return true })
	ingestor.DebugLog = dbg.Func("protocol")
	if cfg.Messaging.SigningKey != "" {
		ingestor.SigningKey = []byte(cfg.Messaging.SigningKey)
		log.Printf("shingocore: envelope signing enabled")
	}

	// ── Protocol router (envelope Type dispatch) ───────────────────────
	// Each envelope Type registers directly against coreHandler.HandleX
	// (or, for TypeData, a closure into subjectRouter.Dispatch). The 8
	// order-channel Types share the inbox-dedup middleware via UseFor;
	// TypeData and the reply-channel Types pass through ungated (the
	// order-channel scoping matches the legacy InboxDedup decorator
	// contract this middleware replaced).
	protoRouter := router.New[string]()
	dedupMW := middleware.NewInboxDedup(db, dbg.Func("inbox_dedup"))
	protoRouter.UseFor(dedupMW,
		protocol.TypeOrderRequest,
		protocol.TypeOrderCancel,
		protocol.TypeOrderReceipt,
		protocol.TypeOrderRedirect,
		protocol.TypeOrderStorageWaybill,
		protocol.TypeComplexOrderRequest,
		protocol.TypeOrderRelease,
		protocol.TypeOrderIngest,
	)
	dataDbg := dbg.Func("core_handler")
	router.Register(protoRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) {
		dataDbg("data: subject=%s body_size=%d from=%s", p.Subject, len(p.Body), env.Src.Station)
		subjectRouter.Dispatch(env, p)
	})
	router.Register(protoRouter, protocol.TypeOrderRequest, coreHandler.HandleOrderRequest)
	router.Register(protoRouter, protocol.TypeOrderCancel, coreHandler.HandleOrderCancel)
	router.Register(protoRouter, protocol.TypeOrderReceipt, coreHandler.HandleOrderReceipt)
	router.Register(protoRouter, protocol.TypeOrderRedirect, coreHandler.HandleOrderRedirect)
	router.Register(protoRouter, protocol.TypeOrderStorageWaybill, coreHandler.HandleOrderStorageWaybill)
	router.Register(protoRouter, protocol.TypeComplexOrderRequest, coreHandler.HandleComplexOrderRequest)
	router.Register(protoRouter, protocol.TypeOrderRelease, coreHandler.HandleOrderRelease)
	router.Register(protoRouter, protocol.TypeOrderIngest, coreHandler.HandleOrderIngest)
	// Core sends these reply-channel types to Edge but never receives
	// them. Register as inline no-ops so the Phase 3.5 startup assertion
	// (every type in protocol.AllTypes() has a handler) is satisfied
	// without inventing a junk MessageHandler implementation. The
	// "no handler registered" router log makes accidental inbound
	// reply-channel traffic visible if it ever shows up.
	router.Register(protoRouter, protocol.TypeOrderAck, func(*protocol.Envelope, *protocol.OrderAck) {})
	router.Register(protoRouter, protocol.TypeOrderWaybill, func(*protocol.Envelope, *protocol.OrderWaybill) {})
	router.Register(protoRouter, protocol.TypeOrderUpdate, func(*protocol.Envelope, *protocol.OrderUpdate) {})
	router.Register(protoRouter, protocol.TypeOrderDelivered, func(*protocol.Envelope, *protocol.OrderDelivered) {})
	router.Register(protoRouter, protocol.TypeOrderError, func(*protocol.Envelope, *protocol.OrderError) {})
	router.Register(protoRouter, protocol.TypeOrderCancelled, func(*protocol.Envelope, *protocol.OrderCancelled) {})
	router.Register(protoRouter, protocol.TypeOrderStaged, func(*protocol.Envelope, *protocol.OrderStaged) {})
	router.Register(protoRouter, protocol.TypeOrderSkipped, func(*protocol.Envelope, *protocol.OrderSkipped) {})
	for _, t := range protocol.AllTypes() {
		if !protoRouter.Has(t) {
			log.Fatalf("shingocore: protocol router missing handler for envelope type %s — composition root is incomplete", t)
		}
	}
	protoRouter.LogRegistration(log.Printf)
	ingestor.Dispatch = func(env *protocol.Envelope) {
		protoRouter.Dispatch(env, env.Type)
	}
	if err := msgClient.Subscribe(cfg.Messaging.OrdersTopic, func(_ string, data []byte) {
		ingestor.HandleRaw(data)
	}); err != nil {
		log.Printf("shingocore: protocol ingestor subscribe failed: %v", err)
	} else {
		log.Printf("shingocore: protocol ingestor listening on %s", cfg.Messaging.OrdersTopic)
	}

	// ── Outbox drainer (outbound to ShinGo Edge) ───────────────────────
	drainer := messaging.NewOutboxDrainer(db, msgClient, cfg.Messaging.OutboxDrainInterval)
	drainer.DebugLog = dbg.Func("outbox")
	drainer.Start()
	defer drainer.Stop()

	// ── Web server ─────────────────────────────────────────────────────
	handler, stopWeb, err := www.NewRouter(eng, dbg)
	if err != nil {
		log.Fatalf("shingocore: build router: %v", err)
	}
	addr := fmt.Sprintf("%s:%d", cfg.Web.Host, cfg.Web.Port)
	srv := startHTTPServer(addr, handler)

	// ── Ready — wait for shutdown signal ────────────────────────────────
	log.Printf("shingocore: ready")

	awaitShutdown(srv, stopWeb)
}
