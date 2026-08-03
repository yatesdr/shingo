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
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingo/protocol/router"
	"shingoedge/backup"
	"shingoedge/config"
	"shingoedge/countgroup"
	"shingoedge/engine"
	"shingoedge/messaging"
	"shingoedge/store"
	"shingoedge/store/counters"
	"shingoedge/uop"
	"shingoedge/www"
)

// Build stamp, fed by -ldflags from install-edge.sh (git describe /
// git rev-parse), mirroring shingo-core.
//
// This one is not just a log line: Version is what the heartbeat reports and
// what Core persists in edge_registry.version, so until 2026-07-25 — when the
// value was the hardcoded literal "dev" at the NewHeartbeater call below —
// every edge in every plant registered as "dev" and the column answered
// nothing.
var (
	Version = "dev"
	Commit  = "unknown"
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

// goSafe runs fn in a new goroutine with a defer recover() that
// logs panics with stack traces instead of crashing the process.
// Use for fire-and-forget background work whose failure should be
// logged but not fatal. Sticking the name through helps grep
// targeted panics out of journald.
func goSafe(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC %s: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
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

// ensureIdentity gives this edge an identity if nobody has given it one.
//
// THIS REPLACES A REFUSAL, AND THE REFUSAL WAS RIGHT ABOUT THE WRONG THING.
//
// What it refused was `plant-a.line-1` — Namespace + "." + LineID, composed
// from two struct defaults. Nobody ever typed it, every unconfigured edge in
// the fleet produced the identical string, and two of them did not collide:
// they took turns owning one registry row while the clause that let them do it
// erased the evidence there had been two. Downstream, one of the pair received
// no dispatch traffic at all — a single-partition consumer group keyed on that
// same string — while heartbeating and registering normally.
//
// Refusing to boot closed that. It also closed something that was never the
// defect: an edge being able to come up AT ALL without a human first fetching a
// value out of Core. That property is what let whoever was holding the SD card
// deploy an edge, and taking it away moved a distributed-identity concept onto
// the shop floor, where the job is to make the line run.
//
// THE DISTINCTION IS COLLISION versus CREATION. The hazard was never that a box
// could bring a station into existence; it was that it could bring the SAME one
// into existence as another box, because the value was DERIVED rather than
// DRAWN. Sixty-four random bits cannot reach another station's row. So an edge
// may introduce itself — and Core marks the row unclaimed until a human says
// what it is (registry.Introduce, edge_registry.claimed_at).
//
// WHAT THIS STILL CANNOT DO, and must not: claim to be an EXISTING station.
// Replacement hardware takes the dead box's uid — read off Core, or restored
// from its backup — and that is a human act by construction, because a minted
// value is new by definition. That case is the entire reason the uid is
// re-issuable, and self-introduction does not touch it.
//
// THE MINT IS PERSISTED IMMEDIATELY, which is the part that is easy to get
// wrong. An edge that minted on every boot would introduce a new station on
// every restart and fill Core with abandoned rows, each holding a shift of
// work. Save() writes the whole struct in place over the existing file — the
// same call the config pages make — so it works despite /etc/shingo being
// root-owned, because the file itself is shingo-owned. If that write fails the
// edge still runs, and says loudly that its identity will not survive a
// restart; refusing there would re-create the outage this function removes.
func ensureIdentity(cfg *config.Config, configPath string) {
	if cfg.StationID() != "" {
		return
	}

	uid := config.NewStationUID()
	if uid == "" {
		log.Fatal("shingoedge: REFUSING TO START — this edge has no identity and could not " +
			"mint one (crypto/rand failed). There is no safe fallback: the previous default " +
			"composed plant-a.line-1 from two struct fields, which every unconfigured edge in " +
			"the fleet produced identically and which is the duplicate-station defect this " +
			"exists to end.")
	}

	cfg.Lock()
	cfg.StationUID = uid
	cfg.Unlock()

	if err := cfg.Save(configPath); err != nil {
		log.Printf("shingoedge: WARNING — minted station uid %s but could NOT write it to %s: %v\n"+
			"  This edge is running and Core will show it as an unclaimed station.\n"+
			"  IT WILL MINT A DIFFERENT UID ON THE NEXT RESTART and appear as a second station.\n"+
			"  Fix the config file's permissions, or set station_uid by hand, before restarting.",
			uid, configPath, err)
		return
	}

	log.Printf("shingoedge: no station_uid configured — MINTED %s and wrote it to %s.\n"+
		"  This edge is running now. Core lists it as UNCLAIMED until somebody says what it is:\n"+
		"    NEW station          → name it on Core's Stations page.\n"+
		"    REPLACING a station  → put THAT station's uid on this box instead, so its history\n"+
		"                           stays attached, then delete this unclaimed row.",
		uid, configPath)
}

func mustOpenDatabase(path string) *store.DB {
	db, err := store.Open(path)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	return db
}

func startHTTPServer(addr string, handler http.Handler) *http.Server {
	// IdleTimeout reaps stale keep-alive slots so SSE goroutines don't
	// pile up on rapid tab navigation. WriteTimeout is intentionally
	// unset because SSE responses are long-lived writes by design.
	srv := &http.Server{Addr: addr, Handler: handler, IdleTimeout: 120 * time.Second}
	go func() {
		log.Printf("ShinGo Edge listening on %s", addr)
		for {
			err := srv.ListenAndServe()
			if err == http.ErrServerClosed {
				return
			}
			// Log and retry instead of log.Fatalf — a transient listener
			// error shouldn't kill the whole process. The supervisor
			// (Phase 1) handles real fatal cases; the retry handles
			// transient binds (port reuse during quick restart, brief
			// network blip on docker container restart).
			log.Printf("http server: %v — retrying in 5s", err)
			time.Sleep(5 * time.Second)
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
func setupKafkaSubscribers(eng *engine.Engine, msgClient *messaging.Client, cfg *config.Config, dbg *debuglog.Logger, stationID, instanceID string, db *store.DB, cgHandler *countgroup.Handler) {
	edgeHandler := messaging.NewEdgeHandler(eng.OrderManager())
	edgeHandler.DebugLog = messaging.DebugLogFunc(dbg.Func("edge_handler"))
	dataDbg := dbg.Func("edge_handler")
	ingestor := protocol.NewIngestor(func(hdr *protocol.RawHeader) bool {
		return hdr.Dst.Station == stationID || hdr.Dst.Station == protocol.StationBroadcast
	})
	ingestor.DebugLog = dbg.Func("protocol")
	if cfg.Messaging.SigningKey != "" {
		ingestor.SigningKey = []byte(cfg.Messaging.SigningKey)
	}

	// ── Heartbeater (built early so subject-router closures can capture it) ──
	hb := messaging.NewHeartbeater(msgClient, stationID, Version, instanceID, cfg.Messaging.OrdersTopic, func() int {
		return db.CountActiveOrders()
	})
	hb.DebugLog = messaging.DebugLogFunc(dbg.Func("heartbeat"))
	// Q-034: attach the PLC-grouped cell catalog to every register so Core can
	// auto-derive cells (no manual cell setup). Read errors degrade to no
	// catalog — registration must still succeed.
	hb.CatalogFn = func() []protocol.CellCatalogEntry {
		pts, err := db.ListReportingPoints()
		if err != nil {
			log.Printf("heartbeat: cell catalog load failed: %v", err)
			return nil
		}
		return messaging.BuildCellCatalog(pts)
	}

	// ── Subject router (Data sub-dispatch) ─────────────────────────────
	// Every protocol.SubjectX is registered against the closure that
	// drives the corresponding Edge subsystem (engine method, heartbeater
	// resync trigger, countgroup command, etc). Pre-router this was done
	// via nine EdgeHandler.SetXHandler setters; the router is now the
	// registration surface and EdgeHandler holds only order-channel
	// state.
	subjectRouter := router.NewSubject()
	router.RegisterSubject(subjectRouter, protocol.SubjectEdgeRegistered, func(_ *protocol.Envelope, reg *protocol.EdgeRegistered) {
		log.Printf("edge_handler: registration acknowledged: station=%s msg=%s", reg.StationID, reg.Message)
		if eng.Uptime() > 30 {
			hb.RequestNodeSync()
			hb.RequestCatalogSync()
		}
		// Sync manual_swap claims to Core's demand registry. Pre-side-cycle
		// this also called StartupSweepManualSwap to seed empty-in orders
		// at every loader — unnecessary now that empty-ins are driven by
		// the UOP-threshold C-push (SubjectLoopBelowThreshold) or by
		// operator staging.
		// Auto-push unloaders: catch any window that became free (or
		// supply that arrived) while Edge was offline. No-op for kanban-
		// driven consume manual_swap claims (AutoPush=false). Mirrors
		// the loader-side reasoning that demand triggers fire while we
		// were unreachable, so a startup pass keeps the queue current.
		goSafe("engine-SweepPushUnloaders", func() { eng.SweepPushUnloaders() })
		goSafe("engine-SweepPushLoaders", func() { eng.SweepPushLoaders() })
	})
	router.RegisterSubject(subjectRouter, protocol.SubjectEdgeHeartbeatAck, func(_ *protocol.Envelope, ack *protocol.EdgeHeartbeatAck) {
		log.Printf("edge_handler: heartbeat ack: station=%s server_ts=%s", ack.StationID, ack.ServerTS)
	})
	router.RegisterSubject(subjectRouter, protocol.SubjectNodeListResponse, func(_ *protocol.Envelope, resp *protocol.NodeListResponse) {
		log.Printf("edge_handler: received node list (%d nodes, %d loaders, %d payload-bin-types)", len(resp.Nodes), len(resp.Loaders), len(resp.PayloadBinTypes))
		eng.SetCoreNodes(resp.Nodes)
		eng.SetCoreLoaders(resp.Loaders)
		eng.SetPayloadBinTypes(resp.PayloadBinTypes)
	})
	router.RegisterSubject(subjectRouter, protocol.SubjectProductionReportAck, func(_ *protocol.Envelope, ack *protocol.ProductionReportAck) {
		log.Printf("edge_handler: production report ack: station=%s accepted=%d", ack.StationID, ack.Accepted)
	})
	router.RegisterSubject(subjectRouter, protocol.SubjectCatalogPayloadsResponse, func(_ *protocol.Envelope, resp *protocol.CatalogPayloadsResponse) {
		log.Printf("edge_handler: received payload catalog (%d entries)", len(resp.Payloads))
		eng.HandlePayloadCatalog(resp.Payloads)
	})
	router.RegisterSubject(subjectRouter, protocol.SubjectOrderStatusResponse, func(_ *protocol.Envelope, resp *protocol.OrderStatusResponse) {
		eng.HandleOrderStatusSnapshots(resp.Orders)
		// The healing half: orders Core holds for this station that we did not
		// ask about, because we have no row for them. A dropped projection is
		// repaired here and nowhere else.
		eng.HandleUnlistedOrders(resp.Unlisted)
	})
	router.RegisterSubject(subjectRouter, protocol.SubjectTagVerifyResponse, func(_ *protocol.Envelope, resp *protocol.TagVerifyResponse) {
		if resp.Match {
			log.Printf("edge_handler: tag verify: uuid=%s match=true detail=%s", resp.OrderUUID, resp.Detail)
		} else {
			log.Printf("edge_handler: tag verify: uuid=%s match=false expected=%s detail=%s", resp.OrderUUID, resp.Expected, resp.Detail)
		}
	})
	router.RegisterSubject(subjectRouter, protocol.SubjectEdgeRegisterRequest, func(_ *protocol.Envelope, req *protocol.EdgeRegisterRequest) {
		log.Printf("edge_handler: core requested re-registration: %s", req.Reason)
		hb.SendRegister()
		if err := eng.StartupReconcile(); err != nil {
			log.Printf("startup reconcile after register request: %v", err)
		}
	})
	router.RegisterSubject(subjectRouter, protocol.SubjectEdgeStale, func(_ *protocol.Envelope, stale *protocol.EdgeStale) {
		log.Printf("edge_handler: WARNING: core marked this edge as stale: %s — re-registering", stale.Message)
		hb.SendRegister()
		if err := eng.StartupReconcile(); err != nil {
			log.Printf("startup reconcile after stale notification: %v", err)
		}
	})
	router.RegisterSubject(subjectRouter, protocol.SubjectNodeStructureChanged, func(_ *protocol.Envelope, changed *protocol.NodeStructureChanged) {
		log.Printf("edge_handler: node structure changed: node=%s action=%s — refreshing node cache",
			changed.NodeName, changed.Action)
		hb.RequestNodeSync()
	})
	// Kanban demand signals from Core's wiring_kanban driver. PRODUCE-role signals
	// are NO LONGER handled: the legacy bin-count produce trigger is retired — a
	// produce loader is supplied by the UOP-threshold C-push (SubjectLoopBelowThreshold,
	// below) or operator staging, never a bin-count floor. CONSUME-role signals fire
	// the unloader U1 to pull a freshly-arrived full (MaybeCreateUnloaderFullIn); the
	// withLoaderBudget seam dedups against the operator-release trigger by in-flight
	// count. Core still emits produce DemandSignals on bin movements; the Edge drops
	// them here harmlessly.
	router.RegisterSubject(subjectRouter, protocol.SubjectDemandSignal, func(_ *protocol.Envelope, s *protocol.DemandSignal) {
		log.Printf("edge_handler: demand signal: node=%s payload=%s role=%s reason=%s",
			s.CoreNodeName, s.PayloadCode, s.Role, s.Reason)
		if s.Role == protocol.ClaimRoleConsume {
			eng.MaybeCreateUnloaderFullIn(s.PayloadCode)
		}
	})
	// UOP-threshold replenishment: Core observes combined in-loop UOP
	// (bins + buckets) per payload and signals here when a monitored
	// (loader, payload) drops below threshold. Edge responds in
	// HandleLoopBelowThreshold, which sizes the ask from the signalled
	// current/threshold and fires L1 through the withLoaderBudget seam.
	// The seam's own in-flight count under the loader mutex is the dedup —
	// there is no separate dedup step on this path.
	router.RegisterSubject(subjectRouter, protocol.SubjectLoopBelowThreshold, func(_ *protocol.Envelope, s *protocol.LoopBelowThresholdSignal) {
		log.Printf("edge_handler: loop below threshold: core_node=%s payload=%s current=%d threshold=%d reason=%s",
			s.CoreNodeName, s.PayloadCode, s.CurrentUOP, s.Threshold, s.Reason)
		eng.HandleLoopBelowThreshold(s)
	})
	if cgHandler != nil {
		router.RegisterSubject(subjectRouter, protocol.SubjectCountGroupCommand, func(_ *protocol.Envelope, cmd *protocol.CountGroupCommand) {
			cgHandler.OnCommand(*cmd)
		})
	}
	// Item 11: SEND PARTIAL BACK pickup notification. Fires when Core's
	// rds.Poller observes the robot finished the pickup block — Edge
	// flushes the released bin's accumulator and clears the runtime's
	// active order so subsequent ticks attribute cleanly.
	router.RegisterSubject(subjectRouter, protocol.SubjectBinPickedUp, func(_ *protocol.Envelope, bp *protocol.BinPickedUp) {
		log.Printf("edge_handler: bin_picked_up: order=%s bin=%d at=%s",
			bp.OrderUUID, bp.BinID, bp.Location)
		eng.HandleBinPickedUp(bp.OrderUUID, bp.BinID, bp.Location)
	})
	// UOP adjustment from Core Bins record-count action. Admin sets
	// the absolute UOP value; Edge writes it directly to the runtime
	// cache and emits EventUOPAdjusted for SSE operator screen refresh.
	router.RegisterSubject(subjectRouter, protocol.SubjectUOPAdjustment, func(_ *protocol.Envelope, adj *protocol.UOPAdjustment) {
		log.Printf("edge_handler: uop_adjustment: node=%s bin=%d new=%d actor=%s",
			adj.CoreNodeName, adj.BinID, adj.NewRemaining, adj.Actor)
		eng.HandleUOPAdjustment(*adj)
	})
	// Sourceability feed from Core: the verdict per (process, style) — what each
	// process can change over to. Edge persists it to SQLite so the changeover
	// picker reads it with no Core round-trip. Registered here in the same place
	// the subject joins EdgeInboundSubjects, so the boot coverage assertion holds.
	router.RegisterSubject(subjectRouter, protocol.SubjectSourcingState, func(_ *protocol.Envelope, r *protocol.SourcingStateReport) {
		eng.HandleSourcingState(*r)
	})
	// Supply refusals, broadcast by Core to every edge. Registered here in the
	// same place the subject joins EdgeInboundSubjects, so the boot coverage
	// assertion holds. Each edge stores the whole set and filters locally — a
	// loader card shows its own, a cell shows the ones it has an outstanding
	// call for.
	router.RegisterSubject(subjectRouter, protocol.SubjectSupplyRefusalState, func(_ *protocol.Envelope, st *protocol.SupplyRefusalState) {
		eng.HandleSupplyRefusalState(*st)
	})
	// A whole order row pushed down by Core, for an order this Edge did not
	// create. Without it a Core-authored order is invisible here: nothing on the
	// board, and the delivery handler falls back to binding the bin alone.
	// Registered here in the same place the subject joins EdgeInboundSubjects, so
	// the boot coverage assertion holds.
	router.RegisterSubject(subjectRouter, protocol.SubjectOrderProjected, func(_ *protocol.Envelope, p *protocol.OrderProjection) {
		if _, err := eng.ApplyOrderProjection(*p); err != nil {
			log.Printf("edge_handler: order projection %s: %v", p.OrderUUID, err)
		}
	})
	// SubjectCountGroupCommand may be skipped above when cgHandler is nil
	// (countgroup is an optional feature). The boot-time coverage assertion
	// below is gated on the same condition so a non-countgroup edge doesn't
	// fail to start over an unregistered optional subject.
	for _, s := range protocol.EdgeInboundSubjects() {
		if s == protocol.SubjectCountGroupCommand && cgHandler == nil {
			continue
		}
		if !subjectRouter.Has(s) {
			log.Fatalf("shingoedge: subject router missing handler for %s — composition root is incomplete", s)
		}
	}

	// ── Protocol router (envelope Type dispatch) ───────────────────────
	// Every envelope Type is registered against either the EdgeHandler
	// order-channel method or, for TypeData, a closure that delegates to
	// the SubjectRouter built above.
	protoRouter := router.New[string]()
	router.Register(protoRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) {
		dataDbg("data subject=%s from=%s", p.Subject, env.Src.Station)
		subjectRouter.Dispatch(env, p)
	})
	// Edge sends these order-channel types to Core but never receives
	// them. Register as inline no-ops so the Phase 3.5 startup assertion
	// (every type in protocol.AllTypes() has a handler) is satisfied
	// without a junk MessageHandler implementation. The "no handler
	// registered" router log makes accidental inbound order-channel
	// traffic visible if it ever shows up.
	router.Register(protoRouter, protocol.TypeOrderRequest, func(*protocol.Envelope, *protocol.OrderRequest) {})
	router.Register(protoRouter, protocol.TypeOrderCancel, func(*protocol.Envelope, *protocol.OrderCancel) {})
	router.Register(protoRouter, protocol.TypeOrderReceipt, func(*protocol.Envelope, *protocol.OrderReceipt) {})
	router.Register(protoRouter, protocol.TypeOrderRedirect, func(*protocol.Envelope, *protocol.OrderRedirect) {})
	router.Register(protoRouter, protocol.TypeComplexOrderRequest, func(*protocol.Envelope, *protocol.ComplexOrderRequest) {})
	router.Register(protoRouter, protocol.TypeOrderRelease, func(*protocol.Envelope, *protocol.OrderRelease) {})
	router.Register(protoRouter, protocol.TypeOrderIngest, func(*protocol.Envelope, *protocol.OrderIngestRequest) {})
	router.Register(protoRouter, protocol.TypeOrderAck, edgeHandler.HandleOrderAck)
	router.Register(protoRouter, protocol.TypeOrderWaybill, edgeHandler.HandleOrderWaybill)
	router.Register(protoRouter, protocol.TypeOrderUpdate, edgeHandler.HandleOrderUpdate)
	router.Register(protoRouter, protocol.TypeOrderDelivered, edgeHandler.HandleOrderDelivered)
	router.Register(protoRouter, protocol.TypeOrderError, edgeHandler.HandleOrderError)
	router.Register(protoRouter, protocol.TypeOrderCancelled, edgeHandler.HandleOrderCancelled)
	router.Register(protoRouter, protocol.TypeOrderStaged, edgeHandler.HandleOrderStaged)
	router.Register(protoRouter, protocol.TypeOrderSkipped, edgeHandler.HandleOrderSkipped)
	for _, t := range protocol.AllTypes() {
		if !protoRouter.Has(t) {
			log.Fatalf("shingoedge: protocol router missing handler for envelope type %s — composition root is incomplete", t)
		}
	}
	protoRouter.LogRegistration(log.Printf)
	ingestor.Dispatch = func(env *protocol.Envelope) {
		protoRouter.Dispatch(env, env.Type)
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

	// Wrap msgClient.Reconnect so a UI-triggered reconnect (operator saves
	// Messaging config) also re-fires the Heartbeater's startup-only sends.
	// Heartbeater.Start() does register / node.list_request / catalog
	// request exactly once at process start; if Kafka was unreachable then
	// (or the client got wedged during a long outage), those one-shots
	// failed and the periodic loop only re-sends node_list+catalog every
	// 2 min — never register — leaving Edge silently unregistered until
	// manual restart. Witnessed at Shelbyville 2026-05-21. This makes the
	// "Save Messaging" button a self-heal action.
	eng.SetKafkaReconnectFunc(func() error {
		if err := msgClient.Reconnect(); err != nil {
			return err
		}
		log.Printf("kafka reconnect: re-firing heartbeater startup sends (register, node list, catalog)")
		hb.SendRegister()
		hb.RequestNodeSync()
		hb.RequestCatalogSync()
		return nil
	})

	hb.Start()
	// Note: hb.Stop() is not deferred here — it lives for the process lifetime
	// and is cleaned up by the Kafka client close.

	// Item 3: auto-fire bucket backfill when Core is fresh. Detects
	// "Core has zero buckets for this station and Edge has rows" —
	// idempotent re-runs return false once Core is populated. Best
	// effort; failures (Core unreachable at boot, partial responses)
	// just log and defer to the next startup or to the admin endpoint.
	goSafe("engine-autoBackfill", func() {
		needed, err := eng.BucketBackfillNeeded()
		if err != nil {
			log.Printf("auto-backfill: probe: %v", err)
			return
		}
		if !needed {
			return
		}
		emitted, err := eng.BackfillBucketsForStation(true)
		if err != nil {
			log.Printf("auto-backfill: %v", err)
			return
		}
		log.Printf("auto-backfill: seeded %d bucket deltas to Core", emitted)
	})

	eng.SetNodeSyncFunc(hb.RequestNodeSync)
	eng.SetCatalogSyncFunc(hb.RequestCatalogSync)
	log.Printf("kanban: demand-signal handler wired — consume->MaybeCreateUnloaderFullIn (produce-role signals are received and dropped by design; produce supply is the UOP-threshold C-push or operator staging)")
	log.Printf("kanban: loop-below-threshold handler wired — C-push signals route to HandleLoopBelowThreshold")

	if err := eng.StartupReconcile(); err != nil {
		log.Printf("initial startup reconcile: %v", err)
	}

	// Mark subscribers wired so /status reports the operational
	// truth. Without this flag the only signal an operator has is
	// "I see order updates in the HMI" — a much later proxy.
	eng.MarkSubscribersWired()
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

	// ── Flags & restore ─────────────────────────────────────────────────
	flags := parseFlags()

	// Build stamp first, before anything that can fatal — same reasoning as
	// shingo-core: a boot that dies during config or DB open is exactly the
	// boot worth attributing to a commit.
	log.Printf("shingoedge: starting version=%s commit=%s config=%s", Version, Commit, flags.configPath)

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
	if cfg.Sim.Enabled {
		simGuard() // sim_enabled.go (sim build) / sim_disabled.go (!sim build)
	}

	// ── Identity ────────────────────────────────────────────────────────
	// Resolved here, once, ahead of everything that consumes it — the Kafka
	// consumer group, the ingest filter, the envelope source address and the
	// backup manifest are all this one value, and the previous arrangement
	// derived it in four places at three points in the startup sequence.
	//
	// instanceID identifies this RUN. Drawn once, here, because
	// setupKafkaSubscribers runs a second time on the Kafka-retry path below
	// and a fresh value there would read to Core as a lease move.
	instanceID := config.NewInstanceID()
	ensureIdentity(cfg, flags.configPath)
	stationID := cfg.StationID()

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
		// Sim mode injects the fake WarLink client (nil otherwise → real HTTP
		// client). sim_enabled.go / sim_disabled.go (T3.1).
		Warlink: simWarlinkClient(cfg),
	})
	eng.Start()
	defer eng.Stop()

	// Sim-only startup (poller + readiness gate + sim operator). engine.Start()
	// left the WarLink poller stopped because the dev config sets
	// warlink.enabled=false; sim mode starts it explicitly against the
	// injected fake (blocker S1). The wlClient is passed to wire the
	// readiness gate (G3).
	if cfg.Sim.Enabled {
		wlClient := eng.WarlinkClient()        // returns the injected fake
		startSimSubsystems(eng, cfg, wlClient) // sim_enabled.go / sim_disabled.go (no-op)
	}

	// ── Backup service ─────────────────────────────────────────────────
	backupSvc := backup.NewService(db, cfg, flags.configPath, "dev", log.Printf)
	backupSvc.Start()
	defer backupSvc.Stop()

	// ── Messaging (Kafka) ───────────────────────────────────────────────
	// UNCONDITIONAL. The old `if GroupID == ""` guard read a field that could
	// be non-empty only because a previous run of this very line had written a
	// derived value into the yaml (KafkaConfig.GroupID carried a yaml tag and
	// Config.Save marshals everything). Both live plants have that persisted
	// pin on line 22 of their Edge config. The field is runtime-only now, so
	// this assignment is the only thing that ever sets it.
	cfg.Messaging.Kafka.GroupID = cfg.KafkaGroupID()
	log.Printf("shingoedge: kafka consumer group=%s (derived from station identity)", cfg.Messaging.Kafka.GroupID)
	msgClient := messaging.NewClient(&cfg.Messaging)
	// Partition key = this edge's identity. Inert today (topics are created
	// with NumPartitions=1) and inert under the writer's default balancer,
	// which is why the balancer moved to Hash alongside it — see
	// messaging.Client.PartitionKey.
	msgClient.PartitionKey = stationID
	msgClient.DebugLog = messaging.DebugLogFunc(dbg.Func("kafka"))
	if cfg.Messaging.SigningKey != "" {
		msgClient.SigningKey = []byte(cfg.Messaging.SigningKey)
		log.Printf("shingoedge: envelope signing enabled")
	}
	defer msgClient.Close()

	// Inject the Kafka IsConnected closure so /status can report
	// kafka_connected without a hard engine→messaging dep.
	eng.SetKafkaConnFunc(msgClient.IsConnected)

	// ── Data sender & outbox drainer ────────────────────────────────────
	dataSender := messaging.NewDataSender(msgClient, cfg.Messaging.OrdersTopic, nil)
	dataSender.DebugLog = messaging.DebugLogFunc(dbg.Func("heartbeat"))
	eng.SetSendFunc(func(env *protocol.Envelope) error {
		return dataSender.PublishEnvelope(env, "core data sync")
	})
	// Kafka reconnect wiring lives in setupKafkaSubscribers where the
	// Heartbeater is in scope — the wrapper re-fires register/node-sync/
	// catalog-sync after Reconnect, self-healing the "Kafka-unreachable-
	// at-startup wedges Edge silently" case.

	// Outbox drainer — runs unconditionally, drains when connected
	drainer := messaging.NewOutboxDrainer(db, msgClient, &cfg.Messaging)
	drainer.DebugLog = messaging.DebugLogFunc(dbg.Func("outbox"))
	drainer.Start()
	defer drainer.Stop()

	// ── Production reporter ────────────────────────────────────────────
	reporter := messaging.NewProductionReporter(db, stationID)
	reporter.DebugLog = messaging.DebugLogFunc(dbg.Func("reporter"))
	eng.Events.SubscribeTypes(func(evt engine.Event) {
		if pr, ok := evt.Payload.(engine.ProducedReportEvent); ok {
			reporter.RecordDelta(pr.PayloadCode, pr.Delta)
		}
	}, engine.EventProducedReport)
	reporter.Start()
	defer reporter.Stop()

	// ── UOP mutator (Phase 1: accumulator wrapper) ─────────────────────
	// Accumulates per-bin / per-bucket UOP changes from the PLC tick
	// path and the operator release path; flushes through the same
	// outbox as the production reporter on a 5s cadence plus the
	// release-click / loader-confirm / A/B-flip flush triggers. Core
	// applies the deltas authoritatively to bins.uop_remaining /
	// lineside_buckets via InventoryDeltaService. Phase 3 will grow
	// this Mutator with intent verbs (Consumed, Produced, CaptureToLineside,
	// etc.) — wiring here does not change.
	uopMutator := uop.New(db, stationID, db, db, db)
	uopMutator.SetDebugLog(uop.DebugLogFunc(dbg.Func("inventory_delta")))
	eng.SetInventoryDeltaSink(uopMutator)
	uopMutator.Start()
	defer uopMutator.Stop()

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
	//
	// Background retry-with-backoff: if Connect fails at boot, Edge
	// would otherwise run "deaf to inbound messages" until a process
	// restart. The retry loop self-heals on Kafka availability changes.
	//
	// /status exposes kafka_connected and subscribers_wired so
	// operators can see the deaf-but-running state. Log loudly at
	// startup so operators notice from the boot log too.
	if msgClient.IsConnected() {
		setupKafkaSubscribers(eng, msgClient, cfg, dbg, stationID, instanceID, db, cgHandler)
	} else {
		log.Printf("WARNING messaging not connected at boot — Edge will run deaf to inbound (orders, demand, stale) until Kafka is reachable. Outbox drainer is active and will flush when connected.")
		goSafe("kafka-connect-retry", func() {
			backoff := 5 * time.Second
			for {
				if err := msgClient.Connect(); err != nil {
					log.Printf("kafka connect failed: %v — retrying in %s; edge still DEAF to inbound messages", err, backoff)
					time.Sleep(backoff)
					if backoff < 60*time.Second {
						backoff *= 2
						if backoff > 60*time.Second {
							backoff = 60 * time.Second
						}
					}
					continue
				}
				log.Printf("kafka connect succeeded — wiring subscribers")
				setupKafkaSubscribers(eng, msgClient, cfg, dbg, stationID, instanceID, db, cgHandler)
				return
			}
		})
	}

	// ── WAL checkpoint ticker ──────────────────────────────────────────
	// PRAGMA wal_checkpoint(TRUNCATE) once per hour keeps the WAL file
	// from growing unbounded under sustained writes on SD-card storage.
	// Cheap operation, well-understood SQLite primitive.
	walStop := make(chan struct{})
	defer close(walStop)
	goSafe("store-wal-checkpoint", func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-walStop:
				return
			case <-ticker.C:
				if err := db.CheckpointWAL(); err != nil {
					log.Printf("WAL checkpoint: %v", err)
				}
			}
		}
	})

	// ── Retention ticker ───────────────────────────────────────────────
	// One pass over the counting ladder, four times a day:
	//
	//   counter_snapshots  raw poll rows           14 days   counters.SnapshotRetention
	//   hourly_counts      per process/style/hour  90 days   counters.HourlyRetention
	//   daily_counts       per process/style/day   permanent counters.RollUpDaily
	//
	// ORDER IS LOAD-BEARING AT THE TOP AND CONVENIENT AT THE BOTTOM. Rolling
	// up before purging hourly is the obvious sequence, but it is NOT what
	// makes it safe — counters.PurgeRolledUpHourly refuses to delete an hour
	// whose day is not already in daily_counts, so a failed rollup deletes
	// nothing no matter when it is called. Each step therefore logs and
	// continues rather than aborting the pass; a broken rollup must not stop
	// counter_snapshots being purged. The VACUUM genuinely does want to be
	// last, so it sees every page all three purges freed.
	//
	// counter_snapshots is the table with a real growth driver: roughly one
	// row per part produced, 472 per counter per calendar day, and no purge
	// until now. Unconfirmed jumps survive that window at any age because
	// they are the operator's popover.
	//
	// A DEDICATED TICKER, NOT A SECOND COUNTER ON THE OUTBOX DRAINER.
	// PurgeOldOutbox rides Drainer.run() every hundredth cycle, and bolting
	// this on there is the cheaper edit — but it would put a counters
	// concern inside the messaging drainer, where the next person changing
	// the drain interval silently changes the retention cadence too. Six
	// hours is four passes a day against a 14-day window: the exact cutoff
	// moment does not matter, only that the backlog is bounded.
	//
	// The VACUUM is the other half. auto_vacuum is NONE, so the first
	// purge's ~147k deleted rows go to the freelist and the file does not
	// shrink until something rebuilds it; VacuumIfFragmented does that once
	// and then stops firing on its own (see the constant's comment).
	//
	// THE FIRST PASS ON AN EXISTING PLANT IS IRREVERSIBLE AND NEEDS AN EXPORT
	// FIRST. Everything else in this change deletes only rows Core already
	// holds — but Core's cell_part_events archive starts 2026-06-08 and
	// Springfield's counter_snapshots start 2026-04-22, so 34.8% of the rows
	// on that Pi exist in exactly one place. The first purge after this
	// deploys removes them. Take a copy before the first tick: the Edge is
	// stopped during install-edge.sh, so a plain file copy of
	// /var/lib/shingo-edge/shingoedge.db alongside the install is enough, and
	// it must happen on the deploy, not afterwards.
	//
	// THE HOURLY PURGE IS INSIDE THAT SENTENCE, WITH ONE QUALIFIER. It deletes
	// only rows daily_counts already holds the total for, so no production
	// disappears in aggregate — but the HOUR-LEVEL resolution does, and for
	// dates before Core's 2026-06-08 archive start there is no second copy of
	// it anywhere. Springfield's first pass takes 78 such rows, all from
	// 2026-04-22..27. The same one file copy covers it; no separate step.
	//
	// Nothing here enforces that, deliberately — a switch to disable a purge
	// is a switch someone leaves off. What the schedule does give is room: the
	// ticker fires on the tick and not at boot, so the first pass is six hours
	// after the edge comes back up.
	//
	// One thing this ticker is NOT accounting for: deploy/db-migration.sh's
	// per-table count(*) diff. That gate compares a stopped database against
	// its own copy — Step 1 refuses to run while a shingoedge process is
	// alive, and install-edge.sh stops the unit before calling it — so this
	// purge cannot run inside the gate's window and cannot fail it.
	//
	// NO NEW VACUUM, AND THAT IS A DECISION. VacuumIfFragmented's 20% freelist
	// threshold already covers both new cases. The hourly purge is far too
	// small to move it — 78 rows, ~6 KB, measured on the restored Springfield
	// database — so it correctly never fires on that account. Hopkinsville's
	// standing 14.83 MB freelist (70.2% of a 21.13 MB file, measured 2026-07-30)
	// is over the threshold and will be reclaimed on its first tick after this
	// deploys, without anything new being written. Incremental vacuum was the
	// alternative and is rejected: switching auto_vacuum NONE -> INCREMENTAL on
	// an existing file needs a full VACUUM anyway to build the pointer-map
	// pages, so it costs the same one-time rebuild AND then charges every
	// future write on the SD card for ptrmap maintenance — a recurring cost to
	// avoid a rebuild that the threshold already makes a once-ever event. It
	// also never defragments, which is the half that matters on flash.
	bucketLoc := engine.BucketLocation(cfg.Timezone)
	retentionStop := make(chan struct{})
	defer close(retentionStop)
	goSafe("store-retention", func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-retentionStop:
				return
			case <-ticker.C:
				cutoff := counters.CutoffDate(time.Now(), bucketLoc, counters.HourlyRetention)

				if n, err := counters.RollUpDaily(db.DB, cutoff); err != nil {
					log.Printf("retention: roll up daily counts: %v", err)
				} else if n > 0 {
					log.Printf("retention: rolled %d daily count rows up from hourly_counts", n)
				}

				if n, err := counters.PurgeRolledUpHourly(db.DB, cutoff); err != nil {
					log.Printf("retention: purge hourly counts: %v", err)
				} else if n > 0 {
					log.Printf("retention: purged %d hourly counts before %s (already rolled up)", n, cutoff)
				}

				if n, err := counters.PurgeOldSnapshots(db.DB, counters.SnapshotRetention); err != nil {
					log.Printf("retention: purge counter snapshots: %v", err)
				} else if n > 0 {
					log.Printf("retention: purged %d counter snapshots older than %s", n, counters.SnapshotRetention)
				}

				vacuumed, err := db.VacuumIfFragmented(store.VacuumFreeFraction)
				if err != nil {
					log.Printf("retention: vacuum: %v", err)
				} else if vacuumed {
					log.Printf("retention: vacuumed the database (free pages exceeded %.0f%% of the file)", store.VacuumFreeFraction*100)
				}
			}
		}
	})

	// ── HTTP server & shutdown ──────────────────────────────────────────
	h, router, stopWeb := www.NewRouter(eng, dbg, backupSvc)

	// ── Plant-claims publisher (sourceability feed, Edge → Core) ───────
	// Publishes the plant-spec claim set so Core can mirror what every
	// process can source. Start publishes one full snapshot immediately
	// (boot), then periodic snapshots every few minutes (late-joiner
	// rebuild). The spec-change hook re-publishes on every style/claim
	// edit via the coalesced claim-sync signal.
	plantClaims := messaging.NewPlantClaimsPublisher(db, stationID)
	plantClaims.DebugLog = messaging.DebugLogFunc(dbg.Func("plant_claims"))
	h.SetPlantSpecChangeHook(func() {
		if err := plantClaims.PublishChanged(); err != nil {
			log.Printf("plant_claims: spec-change publish: %v", err)
		}
	})
	plantClaims.Start()
	defer plantClaims.Stop()

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
