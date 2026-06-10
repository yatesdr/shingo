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

dev-seed: ## Seed the demo plant then restart edge to pick up new runtime states
	# --build so an edited seeddev / demo.yaml is always picked up (the seed
	# image is separate from core/edge and easy to leave stale otherwise).
	# Restart edge after seed so it re-reads the freshly seeded runtime states
	# (the engine caches them at startup; seed writes into SQLite behind it).
	$(COMPOSE) run --build --rm seed
	$(COMPOSE) restart edge

dev-logs: ## Tail core + edge logs
	$(COMPOSE) logs -f core edge

dev-rates: ## Fill/starve check on the demo plant (run after editing it; no Docker needed)
	cd shingo-core && go run ./cmd/simcalc -plant ../plants/demo.yaml -edge ../shingo-edge/shingoedge.dev.yaml

dev-rates-solve: ## Derive balanced tick rates. Override: make dev-rates-solve ARGS="-line-rate 8 -transit 15m"
	cd shingo-core && go run ./cmd/simcalc -solve -plant ../plants/demo.yaml $(ARGS)

# dev-wipe target added in Phase 4 (T4.5).
