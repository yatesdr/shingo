# Local dev environment — SIMULATION ONLY, not for production.
# Quickstart: make dev && make dev-seed   (see README.dev.md, added in T5.3)
COMPOSE := docker compose -f docker-compose.dev.yml

.PHONY: dev-build dev dev-down dev-reset dev-seed dev-logs dev-rates dev-carriers dev-rates-solve

dev-build: ## Build the sim binaries into images (INCLUDING the tools profile)
	# --profile tools is load-bearing, not thoroughness. `compose build` without it
	# builds core/edge/edge2 and SKIPS seed/seed-edge2/migrate-loaders, and seeddev
	# carries ITS OWN COPY of the migration list. On 2026-09-06 a stale seeder image
	# survived a teardown and applied two migrations under a retired numbering on top
	# of a freshly migrated database — pushing it past the chain it was supposed to be
	# on, with no symptom but a version number inside a line that reads like success.
	# One build for all six images is what makes that unrepeatable.
	$(COMPOSE) --profile tools build

dev: dev-build ## Bring up postgres + kafka + core + edge
	# Keeps the existing anchor if there is one — restarting a stack must not
	# re-anchor its clock, because the rows already in the volumes are stamped in
	# the current frame. Only dev-reset mints a new one. See scripts/sim-anchor.sh.
	bash scripts/sim-anchor.sh ensure
	$(COMPOSE) up -d postgres kafka core edge
	$(COMPOSE) ps

dev-down: ## Stop services (keep data volumes)
	$(COMPOSE) down

dev-reset: ## Stop and delete volumes (fresh DBs + a fresh sim anchor on next up)
	$(COMPOSE) down -v
	# Fresh volumes are the ONLY safe moment to re-anchor: no rows survive carrying
	# simulated stamps in the old frame, so simulated time can restart at today
	# without landing before data that already exists.
	bash scripts/sim-anchor.sh mint

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

dev-rates: ## Fill/starve AND carrier-deadlock check on the demo plant (no Docker needed)
	# BOTH CHECKS, AND THE SECOND ONE IS THE ONE THAT BITES. simcalc answers two
	# different questions and only the first ran here:
	#
	#   default    per-PAYLOAD balance — is each part made as fast as it is drawn?
	#   -carriers  per-POOL empty-bin balance — can the loop physically circulate?
	#
	# A plant passes the first and jams on the second, which is exactly what
	# carriers.go was written for ("THE CHECK THAT WOULD HAVE SAVED THE
	# TWEAKING"). demo.yaml reports "SUSTAINS" from the payload check while the
	# carrier check reports WILL JAM on SYN_MARKET — a 0.45 bins/min empty
	# deficit that drains the pool regardless of how many carriers are seeded.
	# Measured 2026-09-06, and the rig reached exactly that state: 13 STANDARD-SM
	# carriers all full, zero empty, PRESS-2 stalled, and the retrieve_empty
	# orders feeding it queued for the rest of the run.
	#
	# The carrier check EXITS NON-ZERO on WILL JAM, so this target now fails on a
	# plant that will deadlock. That is the point — a verdict nobody runs is a
	# verdict nobody has.
	cd shingo-core && go run ./cmd/simcalc -plant ../plants/demo.yaml -edge ../shingo-edge/shingoedge.dev.yaml
	cd shingo-core && go run ./cmd/simcalc -carriers -plant ../plants/demo.yaml -edge ../shingo-edge/shingoedge.dev.yaml

dev-carriers: ## Carrier/empty-pool deadlock check alone (the half that exits non-zero)
	cd shingo-core && go run ./cmd/simcalc -carriers -plant ../plants/demo.yaml -edge ../shingo-edge/shingoedge.dev.yaml

dev-rates-solve: ## Derive balanced tick rates. Override: make dev-rates-solve ARGS="-line-rate 8 -transit 15m"
	cd shingo-core && go run ./cmd/simcalc -solve -plant ../plants/demo.yaml $(ARGS)

dev-fleet: ## Estimate the AMR fleet the plant needs. Override: make dev-fleet ARGS="-transit 15m -util 0.7"
	cd shingo-core && go run ./cmd/simcalc -fleet -plant ../plants/demo.yaml -edge ../shingo-edge/shingoedge.dev.yaml $(ARGS)

# dev-wipe target added in Phase 4 (T4.5).

# ── The pre-push gate ────────────────────────────────────────────────
#
# The gate itself lives in scripts/gate.sh, not here, because `make` is not
# installed on the Windows dev host or in its WSL distro — a Makefile-only
# gate would be one nobody there can run, which is the same failure it exists
# to fix. These targets are for anyone who does have make.
#
# See scripts/gate.sh for what it runs and why gofmt goes first.
.PHONY: gate gate-fmt gate-vet gate-lint gate-test

gate: ## Everything CI enforces except the docker suites
	@bash scripts/gate.sh

gate-fmt:
	@bash scripts/gate.sh fmt

gate-vet:
	@bash scripts/gate.sh vet

gate-lint:
	@bash scripts/gate.sh lint

gate-test:
	@bash scripts/gate.sh test
