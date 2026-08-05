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
	"shingocore/config"
	"shingocore/countgroup"
	"shingocore/dispatch"
	"shingocore/engine"
	"shingocore/fleet"
	"shingocore/fleet/seerrds"
	"shingocore/messaging"
	"shingocore/messaging/middleware"
	"shingocore/rds"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/robotconfidence"
	"shingocore/www"
)

// Build stamp, fed by -ldflags at build time from the shingo-core Makefile
// and install-core.sh (git describe / git rev-parse). Left at these defaults
// a binary cannot say which commit it is, which is what made the five boots
// on 2026-07-24 impossible to tie to a deploy.
var (
	Version = "dev"
	Commit  = "unknown"
)

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
		fmt.Println("shingocore", Version, "("+Commit+")")
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
	fmt.Println("  countgroup    Advanced-zone occupancy changes")
	fmt.Println()
	fmt.Println("--log-debug gates the FILE only. What reaches stderr (journald under")
	fmt.Println("systemd) is logging.stderr_subsystems in the YAML; the browser log UI")
	fmt.Println("shows every subsystem regardless.")
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

// applyLogGate narrows debuglog's stderr mirror to the configured allow-list.
//
// Runs after config load, so the handful of lines emitted during early boot
// still reach the journal unconditionally — that window is where a fatal
// config or DB error lands, and it is a few lines, not a firehose.
//
// The effective list is logged because the allow-list is default-deny: a
// subsystem added later is silently absent from the journal until someone
// opts it in, and this line is where that becomes visible.
func applyLogGate(dbg *debuglog.Logger, cfg *config.Config) {
	allow := cfg.Logging.ResolveStderrSubsystems()
	dbg.SetStderrSubsystems(allow)
	if allow == nil {
		log.Printf("shingocore: debug log mirroring ALL subsystems to stderr (logging.stderr_subsystems: all)")
		return
	}
	if len(allow) == 0 {
		log.Printf("shingocore: debug log stderr mirror disabled (ring buffer and log UI unaffected)")
		return
	}
	log.Printf("shingocore: debug log mirroring to stderr: %s", strings.Join(allow, ","))
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

	// Build stamp first, before anything that can fatal. Deliberately not
	// beside the "ready" line: a boot that dies during config or DB open
	// never reaches ready, and those are exactly the boots worth attributing
	// to a commit (five restarts in fifteen minutes, 2026-07-24).
	log.Printf("shingocore: starting version=%s commit=%s config=%s", Version, Commit, flags.configPath)
	// Hand the stamp to the web layer so the Core Health strip can show it
	// beside uptime — "0d 00h 14m" next to a red verdict reads as "it broke
	// right after a restart" with no correlation work.
	www.SetBuildInfo(Version, Commit, time.Now())

	dbg := mustInitDebugLog(flags.fileFilter)
	defer dbg.Close()

	cfg := mustLoadConfig(flags.configPath)
	applyLogGate(dbg, cfg)
	if cfg.Sim.Enabled {
		simGuard() // sim_enabled.go (sim build) / sim_disabled.go (!sim build)
	}
	maybeResetDB(flags.resetDB, cfg)

	// ── Database ────────────────────────────────────────────────────────
	db := mustOpenDatabase(cfg)
	defer db.Close()

	// ── Fleet backend ───────────────────────────────────────────────────
	// Sim mode swaps the SEER RDS adapter for the in-memory simulator
	// (newSimBackend lives in sim_enabled.go; the !sim build returns an
	// error and is never reached because simGuard already fatals above).
	// Production builds always use the seerrds adapter.
	var fleetAdapter fleet.TrackingBackend
	if cfg.Sim.Enabled {
		sb, err := newSimBackend(context.Background(), cfg)
		if err != nil {
			log.Fatalf("shingocore: sim fleet backend: %v", err)
		}
		fleetAdapter = sb
	} else {
		fleetAdapter = seerrds.New(seerrds.Config{
			BaseURL:      cfg.RDS.BaseURL,
			Timeout:      cfg.RDS.Timeout,
			PollInterval: cfg.RDS.PollInterval,
			FaultGrace:   cfg.RDS.FaultGrace,
			DebugLog:     dbg.Func("rds"),
		})
	}
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
		// Not fatal, and not the end of it: EnsureConnected (wired after the
		// ingestor subscribe below) keeps retrying in the background so a boot
		// where Kafka is not yet ready — a box reboot where the co-located broker
		// is slower to come up than core — recovers on its own instead of running
		// Kafka-dead until a manual restart.
		log.Printf("shingocore: messaging connect failed (%v); will retry in the background", err)
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
	// Skipped entirely in sim mode — there is no RDS to poll (brief T1.1).
	if !cfg.Sim.Enabled {
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

	// Futility detector (observe-only). Logs through the DEFAULT logger, not
	// debuglog: when this fires it is the loudest thing in the journal by
	// design, and the debug stream is gated by logging.stderr_subsystems.
	if fd := cfg.Dispatch.Futility; fd.Enabled {
		eng.Dispatcher().EnableFutilityDetector(dispatch.FutilityConfig{
			Enabled:       fd.Enabled,
			Threshold:     fd.Threshold,
			Window:        fd.Window,
			AlertThrottle: fd.AlertThrottle,
		}, log.Printf)
		log.Printf("shingocore: futility detector armed (observe-only) — %d futile terminals per tuple in %s, repeats suppressed for %s",
			fd.Threshold, fd.Window, fd.AlertThrottle)
	}

	// ── Protocol ingestor (inbound from ShinGo Edge) ───────────────────
	coreHandler := messaging.NewCoreHandler(db, msgClient, cfg.Messaging.StationID, cfg.Messaging.DispatchTopic, eng.Dispatcher())
	coreHandler.DebugLog = dbg.Func("core_handler")
	coreHandler.StaleEdgeThreshold = cfg.Messaging.StaleEdgeThreshold
	coreHandler.Start()
	defer coreHandler.Stop()

	// ── Subject router (Data sub-dispatch) ─────────────────────────────
	// The dispatch table itself lives in routers.go so a test can build it;
	// see the header there. CoreDataService is constructed at this composition
	// root rather than buried inside NewCoreHandler so the wiring is grep-able
	// from one place.
	coreDataService := messaging.NewCoreDataService(db, coreHandler, service.EpochAnnounce{
		Topic:       cfg.Messaging.DispatchTopic,
		CoreStation: cfg.Messaging.StationID,
	})
	// Wire the UOP-threshold monitor so loader-config threshold changes
	// reset debounce timers and bucket-applied events drive
	// re-evaluation. Engine.Start() has already constructed the monitor
	// and kicked its startup-sweep goroutine.
	coreDataService.SetThresholdMonitor(eng.ThresholdMonitor())

	subjectRouter, err := buildSubjectRouter(coreDataService)
	if err != nil {
		// Second line of defence: TestSubjectRouter_CoversEveryInboundSubject
		// fails the build first. Reaching this means a binary was shipped
		// without the suite, and at boot it is still fatal.
		log.Fatalf("shingocore: %v — composition root is incomplete", err)
	}
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
	coreDataService.StartDowntimeProjection()

	ingestor := protocol.NewIngestor(func(_ *protocol.RawHeader) bool { return true })
	ingestor.DebugLog = dbg.Func("protocol")
	if cfg.Messaging.SigningKey != "" {
		ingestor.SigningKey = []byte(cfg.Messaging.SigningKey)
		log.Printf("shingocore: envelope signing enabled")
	}

	// ── Protocol router (envelope Type dispatch) ───────────────────────
	// The dispatch table lives in routers.go; see the header there.
	protoRouter, err := buildProtocolRouter(
		coreHandler,
		subjectRouter,
		middleware.NewInboxDedup(db, dbg.Func("inbox_dedup")),
		dbg.Func("core_handler"),
	)
	if err != nil {
		// Second line of defence, as above:
		// TestProtocolRouter_CoversEveryEnvelopeType fails the build first.
		log.Fatalf("shingocore: %v — composition root is incomplete", err)
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
	// If the broker was not reachable at boot, the Connect above failed and this
	// Subscribe only recorded the handler without starting a reader. EnsureConnected
	// keeps retrying and, once the broker is up, starts the reader (and the writer)
	// and restores this subscription — so core never sits Kafka-dead after a reboot.
	// No-op when already connected.
	msgClient.EnsureConnected()

	// ── Outbox drainer (outbound to ShinGo Edge) ───────────────────────
	drainer := messaging.NewOutboxDrainer(db, msgClient, cfg.Messaging.OutboxDrainInterval)
	drainer.DebugLog = dbg.Func("outbox")
	drainer.Start()
	defer drainer.Stop()

	// ── Inbox retention ────────────────────────────────────────────────
	// The outbox's purge rides Drainer.run() every hundredth cycle; the
	// inbox had none at all, which is the only reason this loop exists. It
	// is a symmetry fix, not a capacity fix: 5,525 rows and 1.2 MB after
	// 123 days is roughly 3.5 MB a year and will never be a problem. Daily
	// is far more often than the volume needs and costs nothing.
	inboxStop := make(chan struct{})
	defer close(inboxStop)
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-inboxStop:
				return
			case <-t.C:
				n, err := db.PurgeOldInbox(store.InboxRetentionPeriod)
				if err != nil {
					log.Printf("shingocore: purge old inbox: %v", err)
				} else if n > 0 {
					log.Printf("shingocore: purged %d inbox record(s) older than %s", n, store.InboxRetentionPeriod)
				}
			}
		}
	}()

	// ── Robot localization confidence ──────────────────────────────────
	// Collection itself rides the engine's existing 2-second robot poll (see
	// engine_robot_confidence.go). What lives here is the housekeeping the
	// write path depends on: today's partitions must exist before anything
	// tries to insert into them, and the aggregates have to be computed while
	// the raw rows they read are still present.
	if cfg.RobotConfidence.Enabled {
		if err := db.EnsureRobotConfidencePartitions(time.Now().UTC()); err != nil {
			// Not fatal. A missing partition costs the confidence samples for
			// the day and nothing else; taking the whole of Core down over a
			// telemetry side-table would be wildly out of proportion.
			log.Printf("shingocore: robot confidence: ensure partitions: %v", err)
		}
		rcStop := make(chan struct{})
		defer close(rcStop)
		go func() {
			// NOT A 24-HOUR TICKER, AND THIS IS THE WHOLE POINT. The first
			// version of this loop was one, matching the inbox-retention loop
			// beside it. It would never have fired: Springfield's Core
			// restarted fifteen times in seven days — a mean process life of
			// about eleven hours — and a 24-hour ticker on an 11-hour process
			// never reaches its first tick. The aggregates would have stayed
			// empty forever while the raw rows they derive from expired at 14
			// days, on a collector that otherwise looked perfectly healthy.
			//
			// So the schedule is driven by the DATABASE's state, not by this
			// process's uptime: "is there a completed day with samples and no
			// aggregates" is a question a restart cannot reset. The hourly
			// tick just decides how soon after UTC midnight the answer gets
			// acted on; the boot pass below covers everything missed while
			// Core was down or between ticks.
			const rcInterval = time.Hour
			t := time.NewTicker(rcInterval)
			defer t.Stop()

			pass := func() {
				now := time.Now().UTC()
				if err := db.EnsureRobotConfidencePartitions(now); err != nil {
					log.Printf("shingocore: robot confidence: ensure partitions: %v", err)
				}

				// ROLL UP BEFORE DROPPING, and note that the order is
				// explicit rather than merely lucky. The aggregates are
				// permanent and the raw rows behind them are not: once a
				// partition is gone its day can never be recomputed, so a
				// drop that ran first would silently publish a hole.
				results, err := db.CatchUpRobotConfidence(now,
					cfg.RobotConfidence.RawRetentionDays, robotconfidence.RollUpConfig{
						SnapTolerance: cfg.RobotConfidence.SnapToleranceMetres,
						BaselineDays:  cfg.RobotConfidence.BaselineDays,
						Coverage:      robotconfidence.DefaultCoverage,
					})
				if err != nil {
					log.Printf("shingocore: robot confidence: roll-up: %v", err)
				}
				for _, res := range results {
					log.Printf("shingocore: robot confidence roll-up %s", res)
				}

				if n, err := db.DropOldRobotConfidencePartitions(
					cfg.RobotConfidence.RawRetentionDays, now); err != nil {
					log.Printf("shingocore: robot confidence: drop raw partitions: %v", err)
				} else if n > 0 {
					log.Printf("shingocore: dropped %d robot confidence partition(s) older than %d days",
						n, cfg.RobotConfidence.RawRetentionDays)
				}
				if n, err := db.DropOldRobotConfidenceLowPartitions(
					cfg.RobotConfidence.LowConfidenceRetentionDays, now); err != nil {
					log.Printf("shingocore: robot confidence: drop low partitions: %v", err)
				} else if n > 0 {
					log.Printf("shingocore: dropped %d low-confidence partition(s) older than %d days",
						n, cfg.RobotConfidence.LowConfidenceRetentionDays)
				}
			}

			// The boot pass is what actually makes this restart-proof. It is
			// also cheap when there is nothing to do: PendingDays answers with
			// two indexed existence checks per day of retention and stops.
			pass()
			for {
				select {
				case <-rcStop:
					return
				case <-t.C:
					pass()
				}
			}
		}()
	}

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
