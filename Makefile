# Local dev environment — SIMULATION ONLY, not for production.
# Quickstart: make dev && make dev-seed   (see README.dev.md, added in T5.3)
COMPOSE := docker compose -f docker-compose.dev.yml

.PHONY: dev-build dev dev-down dev-reset dev-seed dev-logs dev-rates dev-rates-solve

dev-build: ## Build the three sim binaries into images
	$(COMPOSE) build

dev: dev-build ## Bring up postgres + kafka + core + edge
	$(COMPOSE) up -d postgres kafka core edge
	$(COMPOSE) ps

dev-down: ## Stop services (keep data volumes)
	$(COMPOSE) down

dev-reset: ## Stop and delete volumes (fresh DBs on next up)
	$(COMPOSE) down -v

dev-seed: ## Seed the demo plant then restart core+edge to pick up the seeded registry + runtime states
	# --build so an edited seeddev / demo.yaml is always picked up (the seed
	# image is separate from core/edge and easy to leave stale otherwise).
	# Restart BOTH services: the seed writes behind the running engines. Edge
	# caches runtime states at startup and the seed writes them into SQLite
	# behind it; Core's threshold monitor does a one-shot startup sweep ~3s after
	# boot, and the seed writes demand_registry (the C-push threshold bindings)
	# behind it — so a registry seeded after that sweep stays UNMONITORED until
	# Core restarts (the loop never gets a loop-below-threshold signal). The
	# compose deps force core to start before the seed (edge depends on core,
	# seed depends on edge), so the sweep always predates the seed. Restarting
	# core re-runs the sweep against the populated registry. Mirrors production,
	# where a deploy restarts Core after an out-of-band registry write
	# (migrateloaders). See threshold_monitor.go startupSweep / Resync.
	$(COMPOSE) run --build --rm seed
	$(COMPOSE) restart core edge

dev-logs: ## Tail core + edge logs
	$(COMPOSE) logs -f core edge

dev-rates: ## Fill/starve check on the demo plant (run after editing it; no Docker needed)
	cd shingo-core && go run ./cmd/simcalc -plant ../plants/demo.yaml -edge ../shingo-edge/shingoedge.dev.yaml

dev-rates-solve: ## Derive balanced tick rates. Override: make dev-rates-solve ARGS="-line-rate 8 -transit 15m"
	cd shingo-core && go run ./cmd/simcalc -solve -plant ../plants/demo.yaml $(ARGS)

dev-fleet: ## Estimate the AMR fleet the plant needs. Override: make dev-fleet ARGS="-transit 15m -util 0.7"
	cd shingo-core && go run ./cmd/simcalc -fleet -plant ../plants/demo.yaml -edge ../shingo-edge/shingoedge.dev.yaml $(ARGS)

# dev-wipe target added in Phase 4 (T4.5).
