# WarPath Features — Phases 1 & 2

Quick reference for the 8 features added in this release. Each section covers routes, data flow, and debugging tips.

---

## F1: Auto-populate Nodes from RDS Bins

**Route:** `POST /nodes/sync-rds` (auth required)

**Flow:** Calls `rds.GetBinDetails()` → for each bin, checks `GetNodeByRDSLocation` and `GetNodeByName` → creates missing nodes with `NodeType=storage`, `Capacity=1`, `Enabled=true`.

**Files:** `www/handlers_nodes.go:handleNodeSyncRDS`, `store/nodes.go:GetNodeByRDSLocation`

**Debug:** If no nodes are created, check that RDS is reachable (try `/rds` explorer first). Duplicate bins are safely skipped — match is by `rds_location` column OR `name`.

---

## F2: PayloadType CRUD

**Routes:**
| Method | Path | Handler |
|--------|------|---------|
| GET | `/payload-types` | `handlePayloadTypes` (page) |
| POST | `/payload-types/create` | `handlePayloadTypeCreate` |
| POST | `/payload-types/update` | `handlePayloadTypeUpdate` |
| POST | `/payload-types/delete` | `handlePayloadTypeDelete` |
| GET | `/api/payload-types` | `apiListPayloadTypes` (public JSON) |

**Table:** `payload_types` — columns: `id`, `name` (unique), `description`, `form_factor`, `default_manifest_json`, `created_at`, `updated_at`

**Form factors:** `tote`, `shelf`, `kd_container`, `other`

**Files:** `store/payload_types.go`, `www/handlers_payload_types.go`, `www/templates/payload_types.html`

**Debug:** `default_manifest_json` defaults to `{}` if left blank. Delete will fail if payloads reference this type (FK constraint).

---

## F3: Payload CRUD & Assignment

**Routes:**
| Method | Path | Handler |
|--------|------|---------|
| GET | `/payloads` | `handlePayloads` (page) |
| POST | `/payloads/create` | `handlePayloadCreate` |
| POST | `/payloads/update` | `handlePayloadUpdate` |
| POST | `/payloads/delete` | `handlePayloadDelete` |
| GET | `/api/payloads` | `apiListPayloads` (public JSON) |
| GET | `/api/payloads/detail?id=X` | `apiGetPayload` (public JSON) |

**Table:** `payloads` — columns: `id`, `payload_type_id` (FK), `node_id` (nullable FK), `status`, `notes`, `created_at`, `updated_at`

**Statuses:** `empty`, `available`, `in_transit`, `at_line`, `hold`

**Joined query:** `ListPayloads` / `GetPayload` JOIN `payload_types` + LEFT JOIN `nodes` to populate `PayloadTypeName`, `FormFactor`, `NodeName`. The `scanPayload` function takes a `withJoins bool` parameter.

**Files:** `store/payloads.go`, `www/handlers_payloads.go`, `www/templates/payloads.html`, `www/helpers.go:payloadStatusColor`

**Debug:** `node_id` is nullable — uses `sql.NullInt64` in the scanner. Badge CSS classes follow the pattern `badge-{status}` — add corresponding styles in `style.css` if custom colors are needed.

---

## F4: Manifest Management

**Routes:**
| Method | Path | Auth | Handler |
|--------|------|------|---------|
| GET | `/api/payloads/manifest?id=X` | public | `apiListManifest` |
| POST | `/api/payloads/manifest/create` | required | `apiCreateManifestItem` |
| POST | `/api/payloads/manifest/update` | required | `apiUpdateManifestItem` |
| POST | `/api/payloads/manifest/delete` | required | `apiDeleteManifestItem` |

**Table:** `manifest_items` — columns: `id`, `payload_id` (FK, ON DELETE CASCADE), `part_number`, `quantity`, `production_date` (nullable), `lot_code` (nullable), `notes`, `created_at`

**Data format:** All manifest endpoints use JSON request/response bodies (not form posts). Example create body:
```json
{"payload_id": 1, "part_number": "PART-A", "quantity": 10, "production_date": "2025-01-15", "lot_code": "LOT-42", "notes": ""}
```

**UI:** Manifest is loaded via `fetch()` when clicking a payload row in the payloads table. Add/delete controls appear only when authenticated.

**Files:** `store/manifest_items.go`, `www/handlers_payloads.go` (bottom half), `www/templates/payloads.html` (modal section)

**Debug:** `production_date` and `lot_code` use `sql.NullString` — empty strings are stored as SQL NULL. Cascade delete means deleting a payload removes all its manifest items automatically.

---

## F5: Bin Occupancy Sync

**Route:** `GET /api/bins/status` (public JSON)

**Flow:** Fetches `/binDetails` from RDS + all WarPath nodes → builds side-by-side comparison → flags discrepancies.

**Response format:**
```json
[
  {"bin_id": "BIN-01", "node_name": "BIN-01", "rds_filled": true, "in_warpath": true, "discrepancy": ""},
  {"bin_id": "BIN-99", "node_name": "", "rds_filled": false, "in_warpath": false, "discrepancy": "rds_only"},
  {"bin_id": "OLD-BIN", "node_name": "OLD-BIN", "rds_filled": null, "in_warpath": true, "discrepancy": "warpath_only"}
]
```

**Discrepancy values:** `""` (matched), `"rds_only"` (bin in RDS but no WarPath node), `"warpath_only"` (WarPath node with `rds_location` not found in RDS)

**UI:** "Check RDS Bins" button on the nodes page opens a modal with color-coded table (yellow = RDS only, red = WarPath only).

**Files:** `www/handlers_nodes.go:apiBinOccupancy`, `www/templates/nodes.html` (bin modal + JS at bottom)

---

## F6: Robot Controls

**Routes (all auth required, JSON body):**
| Route | Body | RDS Call |
|-------|------|----------|
| `POST /api/robots/dispatchable` | `{"vehicle_id":"R1", "type":"dispatchable"}` | `POST /dispatchable` |
| `POST /api/robots/redo-failed` | `{"vehicle_id":"R1"}` | `POST /redoFailedOrder` |
| `POST /api/robots/manual-finish` | `{"vehicle_id":"R1"}` | `POST /manualFinished` |

**Dispatch types:** `"dispatchable"`, `"undispatchable_unignore"` (pause), `"undispatchable_ignore"` (pause + ignore)

**UI:** Control buttons appear in the robot detail modal when authenticated. `currentRobotVehicle` JS var is set on modal open. Manual Finish requires a `confirm()` dialog.

**Files:** `www/handlers_robots.go`, `www/templates/robots.html`

---

## F7: Order Controls

**Routes (all auth required, JSON body):**
| Route | Body | Effect |
|-------|------|--------|
| `POST /api/orders/terminate` | `{"order_id": 123}` | Terminates RDS order (if exists) + sets local status to `cancelled` |
| `POST /api/orders/priority` | `{"order_id": 123, "priority": 5}` | Updates RDS priority (if RDS order exists) + updates local `priority` column |

**Flow for terminate:** Fetches order → if `rds_order_id` is set, calls `rds.TerminateOrder` → updates local status to `cancelled` with audit detail `"cancelled by {username}"`.

**UI:** Controls card appears on the order detail page (`/orders/detail?id=X`) below the detail/timeline cards. Terminate button is hidden for terminal statuses. Page auto-reloads on success.

**Files:** `www/handlers_orders.go`, `store/orders.go:UpdateOrderPriority`, `www/templates/orders.html`

---

## F8: Scene/Area Zone Sync

**Route:** `POST /nodes/sync-scene` (auth required)

**Flow:** Calls `rds.GetScene()` → builds a map of `point → area.Name` from `scene.Areas[].Points[]` → for each WarPath node that has `rds_location` set but `zone` empty, sets `zone` to the matching area name.

**RDS type fix:** `Scene.Areas` changed from `[]any` to `[]Area` where `Area{Name string, Points []string}`.

**Files:** `rds/types.go:Area`, `www/handlers_nodes.go:handleSceneSync`, `www/templates/nodes.html`

**Debug:** Only updates nodes with empty zones — won't overwrite manually-set zones. If zones don't populate, verify that the RDS scene response actually includes area → point mappings (check via `/rds` explorer, GET `/scene`).

---

## Database Schema Additions

Three new tables added to both `schema_sqlite.go` and `schema_postgres.go`:

```
payload_types  →  payloads  →  manifest_items
                     ↓
                   nodes (nullable FK)
```

Key differences between SQLite and PostgreSQL:
- PKs: `INTEGER PRIMARY KEY AUTOINCREMENT` vs `BIGSERIAL PRIMARY KEY`
- FKs: `INTEGER` vs `BIGINT`
- Timestamps: `TEXT DEFAULT datetime('now','localtime')` vs `TIMESTAMPTZ DEFAULT NOW()`
- JSON: `TEXT DEFAULT '{}'` vs `JSONB DEFAULT '{}'`
- Floats: `REAL` vs `DOUBLE PRECISION`

All new tables are created via the schema `CREATE TABLE IF NOT EXISTS` pattern — safe to run against existing databases.

---

## Nav & Template Summary

**New nav links** (auth-gated, in `layout.html`): Payload Types, Payloads

**New templates:** `payload_types.html`, `payloads.html`

**Modified templates:** `robots.html` (control buttons), `orders.html` (terminate/priority controls), `nodes.html` (3 sync buttons + bin modal)

**New template func:** `payloadStatusColor` in `helpers.go` — returns badge CSS class for payload statuses.
