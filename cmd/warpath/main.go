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

	"github.com/redis/go-redis/v9"

	"warpath/config"
	"warpath/engine"
	"warpath/messaging"
	"warpath/nodestate"
	"warpath/rds"
	"warpath/store"
	"warpath/www"
)

var Version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "warpath.yaml", "path to config file")
	flag.Parse()

	if *showVersion {
		fmt.Println("warpath", Version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// Database
	db, err := store.Open(&cfg.Database)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	log.Printf("warpath: database open (%s)", cfg.Database.Driver)

	// Redis
	redisClient := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Address,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Printf("warpath: redis not available (%v), running without cache", err)
	} else {
		log.Printf("warpath: redis connected (%s)", cfg.Redis.Address)
	}
	cancel()
	defer redisClient.Close()

	// Node state manager
	redisStore := nodestate.NewRedisStore(redisClient)
	nodeStateMgr := nodestate.NewManager(db, redisStore)
	nodeStateMgr.SyncRedisFromSQL()

	// RDS client
	rdsClient := rds.NewClient(cfg.RDS.BaseURL, cfg.RDS.Timeout)
	if ping, err := rdsClient.Ping(); err == nil {
		log.Printf("warpath: RDS Core connected (%s %s)", ping.Product, ping.Version)
	} else {
		log.Printf("warpath: RDS Core not available (%v)", err)
	}

	// Messaging client
	msgClient := messaging.NewClient(&cfg.Messaging)
	if err := msgClient.Connect(); err != nil {
		log.Printf("warpath: messaging connect failed (%v)", err)
	} else {
		log.Printf("warpath: messaging connected (%s)", cfg.Messaging.Backend)
	}
	defer msgClient.Close()

	// Engine
	eng := engine.New(engine.Config{
		AppConfig:  cfg,
		ConfigPath: *configPath,
		DB:         db,
		RDSClient:  rdsClient,
		NodeState:  nodeStateMgr,
		MsgClient:  msgClient,
	})
	eng.Start()
	defer eng.Stop()

	// Messaging consumer (inbound from WarDrop)
	consumer := messaging.NewConsumer(msgClient, cfg.Messaging.OrdersTopic, eng.Dispatcher())
	if err := consumer.Start(); err != nil {
		log.Printf("warpath: consumer start failed: %v", err)
	}

	// Outbox drainer (outbound to WarDrop)
	drainer := messaging.NewOutboxDrainer(db, msgClient, cfg.Messaging.OutboxDrainInterval)
	drainer.Start()
	defer drainer.Stop()

	// Web server
	handler, stopWeb := www.NewRouter(eng)

	addr := fmt.Sprintf("%s:%d", cfg.Web.Host, cfg.Web.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	go func() {
		log.Printf("warpath: web server listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("web server: %v", err)
		}
	}()

	log.Printf("warpath: ready")

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Printf("warpath: shutting down...")
	stopWeb()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)

	log.Printf("warpath: stopped")
}
