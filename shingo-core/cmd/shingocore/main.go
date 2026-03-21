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
	"shingocore/config"
	"shingo/protocol/debuglog"
	"shingocore/engine"
	"shingocore/fleet/seerrds"
	"shingocore/messaging"
	"shingocore/store"
	"shingocore/www"
)

var Version = "dev"

func main() {
	// Strip --log-debug / -log-debug from os.Args before flag.Parse,
	// because flag.String always requires a value argument but we want
	// bare --log-debug (no value) to mean "all subsystems".
	var fileFilter []string // nil = no file output
	var filteredArgs []string
	for _, arg := range os.Args[1:] {
		if arg == "--log-debug" || arg == "-log-debug" {
			fileFilter = []string{} // empty = all subsystems
			continue
		}
		if strings.HasPrefix(arg, "--log-debug=") || strings.HasPrefix(arg, "-log-debug=") {
			val := arg[strings.Index(arg, "=")+1:]
			fileFilter = strings.Split(val, ",")
			continue
		}
		filteredArgs = append(filteredArgs, arg)
	}
	os.Args = append(os.Args[:1], filteredArgs...)

	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "shingocore.yaml", "path to config file")
	resetDB := flag.Bool("reset-db", false, "wipe database before starting (requires confirmation)")
	showHelp := flag.Bool("help", false, "show help")
	flag.Parse()

	if *showHelp {
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
		os.Exit(0)
	}

	if *showVersion {
		fmt.Println("shingocore", Version)
		return
	}

	dbg, err := debuglog.New(1000, fileFilter)
	if err != nil {
		log.Fatalf("debug log: %v", err)
	}
	defer dbg.Close()

	if dbg.FileEnabled() {
		if fileFilter != nil && len(fileFilter) > 0 {
			log.Printf("shingocore: debug log enabled (file: shingo-debug.log, subsystems: %s)", strings.Join(fileFilter, ","))
		} else {
			log.Printf("shingocore: debug log enabled (file: shingo-debug.log, all subsystems)")
		}
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// Reset database if requested
	if *resetDB {
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

	// Database
	db, err := store.Open(&cfg.Database)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	log.Printf("shingocore: database open (postgres)")

	// Fleet backend (Seer RDS adapter)
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

	// Messaging client
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

	// Engine
	eng := engine.New(engine.Config{
		AppConfig:  cfg,
		ConfigPath: *configPath,
		DB:         db,
		Fleet:      fleetAdapter,
		MsgClient:  msgClient,
		DebugLog:   dbg.Func("engine"),
	})
	eng.Start()
	defer eng.Stop()

	// Inject debug log into dispatcher
	eng.Dispatcher().DebugLog = dbg.Func("dispatch")

	// Protocol ingestor (inbound from ShinGo Edge)
	coreHandler := messaging.NewCoreHandler(db, msgClient, cfg.Messaging.StationID, cfg.Messaging.DispatchTopic, eng.Dispatcher())
	coreHandler.DebugLog = dbg.Func("core_handler")
	coreHandler.Start()
	defer coreHandler.Stop()
	ingestor := protocol.NewIngestor(coreHandler, func(_ *protocol.RawHeader) bool { return true })
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

	// Outbox drainer (outbound to ShinGo Edge)
	drainer := messaging.NewOutboxDrainer(db, msgClient, cfg.Messaging.OutboxDrainInterval)
	drainer.DebugLog = dbg.Func("outbox")
	drainer.Start()
	defer drainer.Stop()

	// Web server
	handler, stopWeb := www.NewRouter(eng, dbg)

	addr := fmt.Sprintf("%s:%d", cfg.Web.Host, cfg.Web.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	go func() {
		log.Printf("shingocore: web server listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("web server: %v", err)
		}
	}()

	log.Printf("shingocore: ready")

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Printf("shingocore: shutting down...")
	stopWeb()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)

	log.Printf("shingocore: stopped")
}
