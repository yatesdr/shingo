# <picture><source media="(prefers-color-scheme: dark)" srcset="docs/assets/shingo-banner-dark.svg"><source srcset="docs/assets/shingo-banner-light.svg"><img alt="Shingo" src="docs/assets/shingo-banner-dark.svg"></picture>

Verified against: ae059495

Material tracking and automated transport system for manufacturing plants. Shingo manages the flow of bins and materials between warehouse storage and production line stations using autonomous mobile robots. Named from the Japanese 信号 ("signal" / "instruction"), Shingo bridges the robot fleet manager and production processes, coordinating material requests and delivery.

## Concepts

### Bin-Centric Tracking

Shingo tracks material at the bin level. A **bin** is a physical container (tote, pallet, shelf unit) identified by a unique label (e.g. `SHG:0042`). Bins move between **nodes** — fixed floor locations such as storage slots, staging areas, and line-side positions. The bin record carries its manifest, remaining production capacity, and confirmation state. All dispatch and inventory logic operates on bins.

### Payload Templates

A **payload** defines what a bin should contain: a parts list and a unit-of-production (UOP) capacity. Payloads are templates. When a bin is loaded with material, it is assigned a payload code; the operator confirms the manifest; and the bin becomes eligible for automated retrieval.

### Automated Dispatch

Operators request material by payload type. Shingo Core locates the oldest eligible bin (FIFO), dispatches a robot to retrieve it, and tracks the order through delivery and operator confirmation. Storage orders work in reverse — the system selects the optimal storage slot automatically.

### Changeover Automation

When a production line switches styles, Shingo orchestrates the material changeover automatically. Core aborts in-flight orders on affected nodes, A/B cycles material slots between old and new styles, dispatches fresh material, and confirms completion. Bins at lineside are left in place when the payload is shared between styles.

### Plant Integration

Edge integrates with plant-floor PLCs through **WarLink**, its PLC client. Reads drive consumption tracking — reporting-point counters tick whenever the line produces a unit. Writes drive plant-side signaling from the same tag surface.

### Decoupled Architecture

Core and edge communicate asynchronously over Kafka. Each edge station operates independently; if connectivity to core is lost, the edge continues tracking local state and queues outbound messages for later delivery. They exchange JSON envelopes on two topics, `shingo.orders` (Edge to Core) and `shingo.dispatch` (Core to Edge); [docs/wire-protocol.md](docs/wire-protocol.md) is the contract.

## Philosophy

- **System-directed retrieval.** Operators request material by type; the system handles sourcing, routing, and delivery.
- **Engineered depletion.** Bins are loaded so all parts deplete together after a known number of production cycles. A single counter — UOP remaining — describes consumption state.
- **FIFO enforcement.** The oldest material is always retrieved first, enforced automatically by the storage and retrieval logic.
- **Operator-confirmed manifests.** A bin becomes eligible for automated retrieval when an operator confirms what was loaded into it. Confirmation is the verification step; there is no scan-at-pickup gate.
- **Vendor-agnostic fleet integration.** The fleet backend is abstracted behind an interface. The current implementation targets Seer RDS; other vendors can be added without changes to the dispatch layer.

## Where it runs

Shingo is live at two plants, Springfield and Hopkinsville. At each plant:

- **Shingo Core** runs on a Linux server and keeps its state in PostgreSQL. Core and the Edges meet on a Kafka broker.
- **Shingo Edge** runs on a Raspberry Pi at each production line, with its own SQLite database, serving that line's operator screens.
- The robots are an AMR fleet managed by **Seer RDS**; Core is the only thing that talks to it.
- Each Edge reads and writes its line's PLC through **WarLink**.

## Modules

<!-- generated: go.work -->

| Module | Go module | Description |
|---|---|---|
| [**integration**](integration/) | `shingo/integration` | End-to-end tests: Edge and Core wired in one test process, their envelopes routed between them without a broker. |
| [**protocol**](protocol/) | `shingo/protocol` | The wire contract and the infrastructure both servers run: the JSON envelope, message types and payloads, statuses, TTL expiry, and the outbox, clock, debuglog, auth and migrate packages. |
| [**shared**](shared/) | `shingo/shared` | What Core and Edge must present or answer identically: UI tokens, status classes and JS utilities, cross-surface answers and cross-module test fixtures; also the repo's Go tooling under scripts/. |
| [**shingo-core**](shingo-core/) | `shingocore` | Central server. Receives orders from edge stations, resolves source and destination nodes, dispatches to the robot fleet, and tracks fulfillment. PostgreSQL. |
| [**shingo-edge**](shingo-edge/) | `shingoedge` | Shop-floor client. Runs at each production line: tracks PLC counters, keeps lineside material, runs the operator screens, and talks to Core over Kafka. |

<!-- /generated -->

## Run it locally

```sh
make dev        # the sim stack in Docker: Postgres, Kafka, Core and two Edges, simulated robots and PLCs
make dev-seed   # seed the sim plant (plants/master.yaml)
# Core: http://localhost:8083   Edges: http://localhost:8081, http://localhost:8082
```

Docker is the only prerequisite. [README.dev.md](README.dev.md) covers tuning, the knobs and troubleshooting; [plants/README.md](plants/README.md) the sim plants.

## Deploy to a plant

The installers build from source at a release tag, install the unit and its log and journal retention, and restart the service:

```sh
sudo bash install-core.sh --tag vX.Y.Z    # on the Core server, first
sudo bash install-edge.sh --tag vX.Y.Z    # then on each Edge
```

First-time setup, configuration and the operator pages are in [shingo-core/README.md](shingo-core/README.md) and [shingo-edge/README.md](shingo-edge/README.md); what the Core installer puts on the host is in [shingo-core/deploy/README.md](shingo-core/deploy/README.md).

## Releases and versioning

Core and Edge share one version, `v0.MINOR.PATCH`: a **MINOR** release adds a migration, changes the wire protocol or adds or removes a config key (migrate first and ship both binaries together, Core before the Edges); a **PATCH** release needs neither. Every change writes its lines under `## Unreleased` in [CHANGELOG.md](CHANGELOG.md), opened by a `Deploy:` line, and the gate fails without them. Each merge to `main` then cuts a release by itself: the number is computed from the changes, and the GitHub release carries both binaries for every platform, their `SHA256SUMS`, and that changelog section as its notes. A plant installs a tag when the owner says (the installers above), and both Logs pages (`/diagnostics`) show the running version and commit.

## Documentation

Every doc in `docs/`, `shingo-core/docs/` and `shingo-edge/docs/`, by its first heading.

<!-- generated: docs index -->

### Shared

| Document | File |
|---|---|
| [Bin Loader / Unloader](docs/bin-loader-unloader-architecture.md) | `docs/bin-loader-unloader-architecture.md` |
| [Bin Transit State](docs/bin-transit-state.md) | `docs/bin-transit-state.md` |
| [Data Model](docs/data-model.md) | `docs/data-model.md` |
| [Ground rules](docs/ground-rules.md) | `docs/ground-rules.md` |
| [Lanes — mouths, digs, admission and liveness](docs/lanes.md) | `docs/lanes.md` |
| [Node Naming Standards](docs/node-naming-standards.md) | `docs/node-naming-standards.md` |
| [Order-Building & Source Dispatch](docs/order-builder-dispatch.md) | `docs/order-builder-dispatch.md` |
| [Order Status Lifecycle](docs/order-lifecycle.md) | `docs/order-lifecycle.md` |
| [Order state machine — bypasses and the lint guard](docs/order-state-machine/transitions.md) | `docs/order-state-machine/transitions.md` |
| [Queue releasers](docs/queue-releasers.md) | `docs/queue-releasers.md` |
| [Seer RDS HTTP API Reference](docs/rds-api-reference.md) | `docs/rds-api-reference.md` |
| [Bin & Slot Reservations](docs/reservations.md) | `docs/reservations.md` |
| [When code may be promoted to a shared layer](docs/shared-layer-promotion.md) | `docs/shared-layer-promotion.md` |
| [Storage / handoff protections — mechanism map](docs/storage-protections.md) | `docs/storage-protections.md` |
| [Sweeps, monitors and re-evaluation paths](docs/sweeps-and-monitors.md) | `docs/sweeps-and-monitors.md` |
| [Terminology Reference](docs/terminology.md) | `docs/terminology.md` |
| [UOP Architecture](docs/uop-architecture.md) | `docs/uop-architecture.md` |
| [UOP-Threshold Replenishment](docs/uop-threshold-replenishment.md) | `docs/uop-threshold-replenishment.md` |
| [Shingo Wire Protocol Specification](docs/wire-protocol.md) | `docs/wire-protocol.md` |

### UI style guide

| Document | File |
|---|---|
| [Shingo UI Style Guide](docs/ui-style-guide.md) | `docs/ui-style-guide.md` |
| [The three surfaces](docs/ui-style-guide/02-the-three-surfaces.md) | `docs/ui-style-guide/02-the-three-surfaces.md` |
| [Code organization](docs/ui-style-guide/03-code-organization.md) | `docs/ui-style-guide/03-code-organization.md` |
| [Design tokens](docs/ui-style-guide/04-design-tokens.md) | `docs/ui-style-guide/04-design-tokens.md` |
| [Status indicators](docs/ui-style-guide/05-status-indicators.md) | `docs/ui-style-guide/05-status-indicators.md` |
| [Data visualization](docs/ui-style-guide/06-data-visualization.md) | `docs/ui-style-guide/06-data-visualization.md` |
| [Visual principles](docs/ui-style-guide/07-visual-principles.md) | `docs/ui-style-guide/07-visual-principles.md` |
| [Icons](docs/ui-style-guide/08-icons.md) | `docs/ui-style-guide/08-icons.md` |
| [Glyphs: slots, bins and streams](docs/ui-style-guide/09-glyphs-slots-bins-and-streams.md) | `docs/ui-style-guide/09-glyphs-slots-bins-and-streams.md` |
| [Modals](docs/ui-style-guide/10-modals.md) | `docs/ui-style-guide/10-modals.md` |
| [Dialog UX — confirmation, prompt, toast](docs/ui-style-guide/11-dialog-ux-confirmation-prompt-toast.md) | `docs/ui-style-guide/11-dialog-ux-confirmation-prompt-toast.md` |
| [Buttons](docs/ui-style-guide/12-buttons.md) | `docs/ui-style-guide/12-buttons.md` |
| [Forms](docs/ui-style-guide/13-forms.md) | `docs/ui-style-guide/13-forms.md` |
| [Placement boxes — the box is the form](docs/ui-style-guide/14-placement-boxes-the-box-is-the-form.md) | `docs/ui-style-guide/14-placement-boxes-the-box-is-the-form.md` |
| [The Nodes page — tiles, groups, sections](docs/ui-style-guide/15-the-nodes-page-tiles-groups-sections.md) | `docs/ui-style-guide/15-the-nodes-page-tiles-groups-sections.md` |
| [Desktop composer patterns](docs/ui-style-guide/16-desktop-composer-patterns-u9.md) | `docs/ui-style-guide/16-desktop-composer-patterns-u9.md` |
| [JavaScript primitives](docs/ui-style-guide/17-javascript-primitives.md) | `docs/ui-style-guide/17-javascript-primitives.md` |
| [Timestamps — plant-local, one clock](docs/ui-style-guide/18-timestamps-plant-local-one-clock.md) | `docs/ui-style-guide/18-timestamps-plant-local-one-clock.md` |
| [Templates and composition](docs/ui-style-guide/19-templates-and-composition.md) | `docs/ui-style-guide/19-templates-and-composition.md` |
| [CSS conventions](docs/ui-style-guide/20-css-conventions.md) | `docs/ui-style-guide/20-css-conventions.md` |
| [Tabs](docs/ui-style-guide/22-tabs.md) | `docs/ui-style-guide/22-tabs.md` |
| [Drift detection](docs/ui-style-guide/24-drift-detection.md) | `docs/ui-style-guide/24-drift-detection.md` |
| [Event handling — delegated actions](docs/ui-style-guide/28-event-handling-delegated-actions.md) | `docs/ui-style-guide/28-event-handling-delegated-actions.md` |
| [Mission detail — stages](docs/ui-style-guide/31-mission-detail-stages.md) | `docs/ui-style-guide/31-mission-detail-stages.md` |
| [Settings pages](docs/ui-style-guide/33-settings-pages.md) | `docs/ui-style-guide/33-settings-pages.md` |

### Shingo Core

| Document | File |
|---|---|
| [API Reference](shingo-core/docs/api-reference.md) | `shingo-core/docs/api-reference.md` |
| [Architecture](shingo-core/docs/architecture.md) | `shingo-core/docs/architecture.md` |
| [Shingo → CMS Integration](shingo-core/docs/cms-integration.md) | `shingo-core/docs/cms-integration.md` |
| [Configuration Reference](shingo-core/docs/configuration.md) | `shingo-core/docs/configuration.md` |
| [Wall Display Platform](shingo-core/docs/dashboard-platform.md) | `shingo-core/docs/dashboard-platform.md` |
| [PostgreSQL Setup](shingo-core/docs/postgresql-setup.md) | `shingo-core/docs/postgresql-setup.md` |
| [UI Guide](shingo-core/docs/ui-guide.md) | `shingo-core/docs/ui-guide.md` |

### Shingo Edge

| Document | File |
|---|---|
| [Backup and Restore](shingo-edge/docs/backup-restore.md) | `shingo-edge/docs/backup-restore.md` |
| [UI Guide](shingo-edge/docs/ui-guide.md) | `shingo-edge/docs/ui-guide.md` |

### Development

| Document | File |
|---|---|
| [The engine test harness](docs/dev-env/engine-test-harness.md) | `docs/dev-env/engine-test-harness.md` |
| [The Kanban rig](docs/dev-env/kanban-rig.md) | `docs/dev-env/kanban-rig.md` |
| [Designing a Sim Plant](docs/dev-env/sim-plant-design.md) | `docs/dev-env/sim-plant-design.md` |
| [The Shingo Sim (local dev environment)](docs/dev-env/sim.md) | `docs/dev-env/sim.md` |
| [Test population ledger](docs/testing/population-ledger.md) | `docs/testing/population-ledger.md` |

<!-- /generated -->

## Contributing

[docs/ground-rules.md](docs/ground-rules.md) holds the rules a change is held to:

- **The gate.** `bash scripts/gate.sh` on every change, `bash scripts/gate.sh full` before a push; CI runs the same steps.
- **Where things go.** `docs/` for anything naming both Core and Edge or a wire contract, `<module>/docs/` for one binary's operator and dev material; design records stay outside the repo.
- **Symbols, not lines.** Docs and comments cite symbols and tests, never line numbers; the gate fails a citation that no longer resolves. Tables the code already states are generated (`make docs-gen`).
- **The changelog.** Every change lands with its line under `## Unreleased` in operator language, opened by the `Deploy:` line.

## License

Proprietary. See [LICENSE](LICENSE).
