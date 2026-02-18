# Changelog

## [v0.0.3] - 2026-02-18

### Breaking Changes
- Wire protocol: `material_code` renamed to `payload_type_code` in `order_request` messages. ShinGo Edge clients must update in lockstep.
- SSE event `inventory-update` renamed to `payload-update`.
- API endpoint `/api/nodes/inventory` now returns payload objects instead of inventory items.

### Added
- **Payload-centric dispatch model** — payloads are discrete physical containers that move between nodes; manifests describe their contents.
- `payloads` table: `claimed_by` (order FK) and `delivered_at` columns for dispatch tracking.
- `orders` table: `payload_type_id` and `payload_id` columns linking orders to the payload model.
- `corrections` table: `payload_id`, `manifest_item_id`, `cat_id`, `description` columns for manifest-based corrections.
- Store methods: `ClaimPayload`, `UnclaimPayload`, `MovePayload`, `FindSourcePayloadFIFO`, `FindStorageDestinationForPayload`, `ListPayloadsByClaimedOrder`.
- CRUD pages for Payload Types and Payloads (`/payload-types`, `/payloads`).
- Manifest-based corrections UI: add item, remove item, adjust quantity against a payload's manifest.
- `PayloadChangedEvent` for event-driven payload tracking (replaces `InventoryChangedEvent`).
- CRUD API endpoints for nodes, orders, robots, payload types, and payloads.

### Changed
- Dispatcher sources payloads via FIFO (`ORDER BY delivered_at ASC`) instead of inventory items.
- Engine `handleOrderCompleted` moves payloads between nodes instead of removing/adding inventory.
- NodeState layer uses `PayloadItem` instead of `InventoryItem`; manager methods rewritten for payload operations.
- All templates updated: dashboard, orders, nodes, nodestate, corrections show payload data.
- Nav bar updated: Materials page replaced by Payload Types and Payloads pages.

### Removed
- Materials UI page and handler (`/materials`, `handlers_materials.go`).
- `InventoryChangedEvent` and `InventoryItem` types (replaced by payload equivalents).

## [v0.0.1] - 2026-02-17

### Added
- Initial release: ShinGo central dispatch server.
- Dual-database support (SQLite for dev, PostgreSQL for prod).
- SEER RDS Core HTTP client with order status poller.
- MQTT/Kafka messaging with typed envelope protocol.
- Dual-layer node state (Redis reads, SQL writes).
- EventBus engine with reactive wiring.
- FIFO dispatch with RDS order creation.
- Web UI: Dashboard, Nodes, Orders, Robots, Materials, Corrections, Diagnostics, RDS Explorer, Config, Login.
- Cross-platform builds (linux/darwin amd64+arm64, windows amd64).
- GitHub Actions CI/CD with release on version tags.
