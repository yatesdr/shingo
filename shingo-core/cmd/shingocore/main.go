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
	"strings"
	"syscall"
	"time"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingocore/config"
	"shingocore/countgroup"
	"shingocore/engine"
	"shingocore/fleet/seerrds"
	"shingocore/messaging"
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
		if fileFilter != nil && len(fileFilter) > 0 {
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
	inboxDedup := messaging.NewInboxDedup(coreHandler, db)
	inboxDedup.DebugLog = dbg.Func("core_handler")
	ingestor := protocol.NewIngestor(inboxDedup, func(_ *protocol.RawHeader) bool { return true })
	ingestor.DebugLog = dbg.Func("protocol")
	if cfg.Messaging.SigningKey != "" {
		ingestor.SigningKey = []byte(cfg.Messaging.SigningKey)
		log.Printf("shingocore: envelope signing enabled")
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
