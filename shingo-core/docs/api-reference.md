# API Reference

Verified against: 51489c78

The `/api` endpoints return JSON; the routes outside `/api` are pages and form posts. Protected endpoints require authentication via session cookie (log in through the web UI).

Base URL: `http://<host>:8083`

## Routes

<!-- generated: shingo-core/www/router.go routes -->

247 routes (124 behind `requireAuth`, 7 registered only in a `sim` build), from `NewRouter` in `shingo-core/www/router.go`. **Auth**: `login` is behind `h.requireAuth` (a session cookie from `/login`); `public` needs none. Grouped by area: everything outside `/api` under *Pages and forms*, then one section per first path segment under `/api`; within a section, by path then method.

### Pages and forms

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/` | `handleDashboard` | public |
| `GET` | `/admin/cells` | `handleCellsAdmin` | login |
| `POST` | `/bin-types/create` | `handleBinTypeCreate` | login |
| `POST` | `/bin-types/delete` | `handleBinTypeDelete` | login |
| `POST` | `/bin-types/update` | `handleBinTypeUpdate` | login |
| `GET` | `/bins` | `handleBins` | login |
| `POST` | `/bins/create` | `handleBinCreate` | login |
| `POST` | `/bins/retire` | `handleBinRetire` | login |
| `GET` | `/board` | (redirect to `/dashboards`) | public |
| `GET` | `/board/{kind}` | `handleBoardKindRedirect` | public |
| `GET` | `/config` | `handleConfig` | login |
| `POST` | `/config/password` | `handleConfigPassword` | login |
| `POST` | `/config/test-alert` | `handleConfigTestAlert` | login |
| `POST` | `/config/test-email` | `handleConfigTestEmail` | login |
| `GET` | `/cycle-time` | `handleCycleTime` | public |
| `GET` | `/dashboard-active-orders` | `handleDashboardActiveOrders` | public |
| `GET` | `/dashboard/{id}` | `handleWallDisplayMoved` | public |
| `GET` | `/dashboards` | `handleWallDisplaysMoved` | public |
| `GET` | `/demand-episodes` | `handleDemandEpisodes` | public |
| `GET` | `/demand-episodes/{originID}` | `handleDemandEpisodeDetail` | public |
| `GET` | `/diagnostics` | `handleDiagnostics` | login |
| `GET` | `/edges` | `handleEdgesAdmin` | login |
| `GET` | `/events` | `SSEHandler` | public |
| `ANY` | `/favicon.ico` | `staticAssets.File` | public |
| `GET` | `/fleet-explorer` | `handleFleetExplorer` | login |
| `GET` | `/heartbeat` | `handleHeartbeatKiosk` | public |
| `GET` | `/inventory` | `handleInventory` | public |
| `GET` | `/inventory/count-history` | `handleInventoryCountHistory` | public |
| `GET` | `/login` | `handleLoginPage` | public |
| `POST` | `/login` | `handleLogin` | public |
| `GET` | `/logout` | `handleLogout` | public |
| `GET` | `/material-flags` | `handleMaterialFlags` | public |
| `GET` | `/missions` | `handleMissions` | public |
| `GET` | `/missions/{orderID}` | `handleMissionDetail` | public |
| `GET` | `/nodes` | `handleNodes` | public |
| `POST` | `/nodes/create` | `handleNodeCreate` | login |
| `POST` | `/nodes/delete` | `handleNodeDelete` | login |
| `POST` | `/nodes/sync-fleet` | `handleNodeSyncFleet` | login |
| `POST` | `/nodes/sync-scene` | `handleSceneSync` | login |
| `POST` | `/nodes/update` | `handleNodeUpdate` | login |
| `GET` | `/orders` | `handleOrders` | public |
| `GET` | `/orders/detail` | `handleOrderDetail` | public |
| `GET` | `/orders/rows` | `handleOrdersRows` | public |
| `GET` | `/orphans` | `handleOrphans` | public |
| `GET` | `/overview` | `handleOverview` | public |
| `GET` | `/payloads` | `handlePayloadsPage` | login |
| `POST` | `/payloads/containment` | `handlePayloadContainment` | login |
| `POST` | `/payloads/create` | `handlePayloadCreate` | login |
| `POST` | `/payloads/delete` | `handlePayloadDelete` | login |
| `POST` | `/payloads/update` | `handlePayloadUpdate` | login |
| `GET` | `/robots` | `handleRobots` | public |
| `GET` | `/sourcing` | `handleSourcing` | login |
| `ANY` | `/static/*` | `staticAssets` | public |
| `GET` | `/test-orders` | `handleTestOrders` | login |
| `GET` | `/wall-display/{id}` | `handleWallDisplay` | public |
| `GET` | `/wall-displays` | `handleWallDisplaysMoved` | public |

### `/api/audit`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/audit/bin/{id}` | `apiAuditBinTimeline` | public |
| `GET` | `/api/audit/discrepancies` | `apiAuditDiscrepancies` | public |

### `/api/bin-types`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/bin-types` | `apiListBinTypes` | public |

### `/api/bins`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `POST` | `/api/bins/action` | `apiBinAction` | login |
| `GET` | `/api/bins/available` | `apiListAvailableBins` | public |
| `POST` | `/api/bins/bulk-action` | `apiBulkBinAction` | login |
| `POST` | `/api/bins/bulk-register` | `apiBulkRegisterBins` | login |
| `GET` | `/api/bins/by-node` | `apiBinsByNode` | public |
| `GET` | `/api/bins/detail` | `apiBinDetail` | public |
| `POST` | `/api/bins/request-move` | `apiRequestBinMove` | login |

### `/api/board`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/board/orders` | `handleBoardOrders` | public |

### `/api/buckets`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/buckets` | `apiBuckets` | public |

### `/api/cells`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/cells` | `apiCellsList` | public |
| `POST` | `/api/cells` | `apiCellUpsert` | login |
| `GET` | `/api/cells/catalog` | `apiCellsCatalog` | public |
| `GET` | `/api/cells/processes` | `apiCellProcesses` | login |
| `DELETE` | `/api/cells/{id}` | `apiCellDelete` | login |
| `GET` | `/api/cells/{id}/heartbeat` | `apiCellHeartbeat` | public |
| `GET` | `/api/cells/{id}/state` | `apiCellState` | public |
| `GET` | `/api/cells/{id}/stops` | `apiCellStops` | public |

### `/api/cms-health`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/cms-health` | `apiCMSHealth` | public |

### `/api/cms-transactions`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/cms-transactions` | `apiListCMSTransactions` | public |

### `/api/config`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `PUT` | `/api/config` | `apiConfigSave` | login |
| `POST` | `/api/config/test-database` | `apiConfigTestDatabase` | login |

### `/api/containment`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `POST` | `/api/containment/payload` | `apiSetPayloadContainment` | login |

### `/api/core`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/core/health` | `apiCoreHealth` | public |

### `/api/dashboards`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/dashboards` | `apiListDashboards` | public |
| `POST` | `/api/dashboards` | `apiCreateDashboard` | login |
| `DELETE` | `/api/dashboards/{id}` | `apiDeleteDashboard` | login |
| `GET` | `/api/dashboards/{id}` | `apiGetDashboard` | public |
| `PUT` | `/api/dashboards/{id}` | `apiUpdateDashboard` | login |
| `GET` | `/api/dashboards/{id}/cells` | `apiDashboardCells` | public |
| `GET` | `/api/dashboards/{id}/node-report` | `apiDashboardNodeReport` | public |

### `/api/dispatch`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/dispatch/anomalies` | `apiListTransitAnomalies` | public |
| `POST` | `/api/dispatch/clear-anomaly` | `apiClearTransitAnomaly` | login |
| `GET` | `/api/dispatch/preview-capacity` | `apiPreviewDropoffCapacity` | public |

### `/api/edges`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/edges` | `apiEdges` | login |
| `POST` | `/api/edges/claim` | `apiEdgeClaim` | login |
| `POST` | `/api/edges/enroll` | `apiEdgeEnroll` | login |
| `POST` | `/api/edges/rebind` | `apiEdgeRebind` | login |
| `POST` | `/api/edges/rename` | `apiEdgeRename` | login |
| `POST` | `/api/edges/reregister` | `apiEdgeReregister` | login |

### `/api/fire-alarm`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/fire-alarm/status` | `apiFireAlarmStatus` | login |
| `POST` | `/api/fire-alarm/trigger` | `apiFireAlarmTrigger` | login |

### `/api/fleet`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `POST` | `/api/fleet/proxy` | `apiFleetProxy` | login |
| `GET` | `/api/fleet/robot-groups` | `apiRobotGroups` | public |

### `/api/footprint`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/footprint` | `apiFootprint` | public |

### `/api/health`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/health` | `apiHealthCheck` | public |

### `/api/inventory`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/inventory/anomaly-summary` | `apiInventoryAnomalySummary` | public |
| `GET` | `/api/inventory/export` | `apiInventoryExport` | login |
| `GET` | `/api/inventory/invariant` | `apiInventoryInvariant` | public |
| `GET` | `/api/inventory/ledger-exceptions` | `apiInventoryLedgerExceptions` | public |
| `GET` | `/api/inventory/loop` | `apiInventoryLoop` | public |
| `GET` | `/api/inventory/rejected-deltas` | `apiInventoryRejectedDeltas` | public |

### `/api/loader`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `POST` | `/api/loader/calculate` | `apiCalculateThreshold` | login |
| `POST` | `/api/loader/create` | `apiCreateLoader` | login |
| `POST` | `/api/loader/create-two-stage` | `apiCreateTwoStageLoader` | login |
| `POST` | `/api/loader/delete` | `apiDeleteLoader` | login |
| `GET` | `/api/loader/list` | `apiListLoaders` | public |
| `POST` | `/api/loader/remove-home` | `apiRemoveLoaderHome` | login |
| `POST` | `/api/loader/remove-payload` | `apiRemoveLoaderPayload` | login |
| `POST` | `/api/loader/remove-quota` | `apiRemoveLoaderQuota` | login |
| `POST` | `/api/loader/reorder-homes` | `apiReorderLoaderHomes` | login |
| `POST` | `/api/loader/set-home` | `apiSetLoaderHome` | login |
| `POST` | `/api/loader/set-payload` | `apiSetLoaderPayload` | login |
| `POST` | `/api/loader/set-quota` | `apiSetLoaderQuota` | login |
| `POST` | `/api/loader/set-window-bin-types` | `apiSetWindowBinTypes` | login |
| `POST` | `/api/loader/update` | `apiUpdateLoader` | login |

### `/api/map`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/map/areas` | `apiSceneAreas` | public |
| `GET` | `/api/map/diffs` | `apiSceneDiffs` | public |
| `GET` | `/api/map/edges` | `apiSceneEdges` | public |
| `GET` | `/api/map/marks` | `apiSceneMarks` | public |
| `GET` | `/api/map/points` | `apiScenePoints` | public |
| `GET` | `/api/map/reflectors` | `apiSceneReflectors` | public |

### `/api/missions`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/missions` | `apiListMissions` | public |
| `GET` | `/api/missions/active` | `apiMissionsActive` | public |
| `GET` | `/api/missions/alerts` | `apiMissionsAlerts` | public |
| `GET` | `/api/missions/breakdown` | `apiMissionBreakdown` | public |
| `GET` | `/api/missions/dwell` | `apiMissionDwell` | public |
| `GET` | `/api/missions/failures` | `apiMissionFailures` | public |
| `GET` | `/api/missions/faults` | `apiMissionFaults` | public |
| `GET` | `/api/missions/stats` | `apiMissionStats` | public |
| `GET` | `/api/missions/stats/v2` | `apiMissionStatsV2` | public |
| `GET` | `/api/missions/timeseries` | `apiMissionTimeseries` | public |
| `GET` | `/api/missions/{orderID}` | `apiGetMission` | public |

### `/api/nodegroup`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `POST` | `/api/nodegroup/add-lane` | `apiAddLane` | login |
| `POST` | `/api/nodegroup/create` | `apiCreateNodeGroup` | login |
| `POST` | `/api/nodegroup/delete` | `apiDeleteNodeGroup` | login |
| `GET` | `/api/nodegroup/layout` | `apiGetGroupLayout` | login |
| `POST` | `/api/nodegroup/reorder-lane` | `apiReorderLaneSlots` | login |

### `/api/nodes`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/nodes` | `apiListNodes` | public |
| `GET` | `/api/nodes/bin-types` | `apiGetNodeBinTypes` | public |
| `POST` | `/api/nodes/bin-types` | `apiSetNodeBinTypes` | login |
| `GET` | `/api/nodes/detail` | `apiNodeDetail` | public |
| `GET` | `/api/nodes/inventory` | `apiNodePayloads` | public |
| `GET` | `/api/nodes/kanban` | `apiKanban` | public |
| `POST` | `/api/nodes/kanban/check-types` | `apiKanbanCheckTypes` | login |
| `GET` | `/api/nodes/kanban/families` | `apiKanbanFamilies` | public |
| `POST` | `/api/nodes/kanban/level` | `apiKanbanLevelSet` | login |
| `POST` | `/api/nodes/kanban/level/remove` | `apiKanbanLevelRemove` | login |
| `GET` | `/api/nodes/kanban/levels` | `apiKanbanLevels` | public |
| `POST` | `/api/nodes/kanban/pools` | `apiKanbanPoolsSet` | login |
| `POST` | `/api/nodes/kanban/settings` | `apiKanbanSettingsSet` | login |
| `POST` | `/api/nodes/kanban/sources` | `apiKanbanSourcesSet` | login |
| `POST` | `/api/nodes/kanban/supports` | `apiKanbanSupportsSet` | login |
| `GET` | `/api/nodes/lane-gate-points` | `apiLaneGatePoints` | public |
| `GET` | `/api/nodes/lane-waiting` | `apiLaneWaiting` | public |
| `GET` | `/api/nodes/occupancy` | `apiNodeOccupancy` | public |
| `GET` | `/api/nodes/process-options` | `apiKanbanProcessOptions` | public |
| `POST` | `/api/nodes/properties/delete` | `apiNodePropertyDelete` | login |
| `POST` | `/api/nodes/properties/set` | `apiNodePropertySet` | login |
| `POST` | `/api/nodes/reparent` | `apiReparentNode` | login |
| `POST` | `/api/nodes/{id}/display-name` | `apiNodeDisplayNameSet` | login |

### `/api/nodestate`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/nodestate` | `apiNodeState` | public |

### `/api/orders`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/orders` | `apiListOrders` | public |
| `GET` | `/api/orders/detail` | `apiGetOrder` | public |
| `GET` | `/api/orders/enriched` | `apiGetOrderEnriched` | public |
| `POST` | `/api/orders/hard-release` | `apiHardReleaseOrder` | login |
| `POST` | `/api/orders/priority` | `apiSetOrderPriority` | login |
| `POST` | `/api/orders/reissue-terminate` | `apiReissueTerminate` | login |
| `POST` | `/api/orders/spot` | `apiManualOrderSubmit` | login |
| `POST` | `/api/orders/terminate` | `apiTerminateOrder` | login |

### `/api/outbox`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/outbox/deadletters` | `apiListDeadLetterOutbox` | public |
| `POST` | `/api/outbox/replay` | `apiReplayOutbox` | login |

### `/api/parts`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/parts` | `apiListParts` | public |
| `POST` | `/api/parts/catid` | `apiSetPartCATID` | login |
| `GET` | `/api/parts/lookup` | `apiGetPart` | public |

### `/api/payloads`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/payloads` | `apiListPayloads` | public |
| `GET` | `/api/payloads/by-node` | `apiPayloadsByNode` | public |
| `POST` | `/api/payloads/confirm-manifest` | `apiConfirmManifest` | login |
| `GET` | `/api/payloads/detail` | `apiGetPayload` | public |
| `GET` | `/api/payloads/events` | `apiListPayloadEvents` | login |
| `GET` | `/api/payloads/manifest` | `apiListManifest` | public |
| `POST` | `/api/payloads/manifest/create` | `apiCreateManifestItem` | login |
| `POST` | `/api/payloads/manifest/delete` | `apiDeleteManifestItem` | login |
| `POST` | `/api/payloads/manifest/update` | `apiUpdateManifestItem` | login |
| `GET` | `/api/payloads/templates` | `apiListPayloads` | public |
| `GET` | `/api/payloads/templates/bin-types` | `apiGetPayloadBinTypes` | public |
| `POST` | `/api/payloads/templates/bin-types` | `apiSavePayloadBinTypes` | login |
| `GET` | `/api/payloads/templates/check-sequence` | `apiCheckLoadSequence` | login |
| `POST` | `/api/payloads/templates/create` | `apiCreatePayloadTemplate` | login |
| `POST` | `/api/payloads/templates/import` | `apiImportPayloadTemplates` | login |
| `GET` | `/api/payloads/templates/manifest` | `apiGetPayloadManifestTemplate` | public |
| `GET` | `/api/payloads/templates/sequences` | `apiListLoadSequences` | login |
| `POST` | `/api/payloads/templates/update` | `apiUpdatePayloadTemplate` | login |

### `/api/plant`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/plant/timezone` | `apiPlantTimezone` | public |

### `/api/reconciliation`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/reconciliation` | `apiReconciliation` | public |

### `/api/recovery`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/recovery/actions` | `apiListRecoveryActions` | public |
| `POST` | `/api/recovery/repair` | `apiRepairAnomaly` | login |

### `/api/robots`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/robots` | `apiRobotsStatus` | public |
| `POST` | `/api/robots/availability` | `apiRobotSetAvailability` | login |
| `GET` | `/api/robots/cleaning` | `apiRobotCleaning` | public |
| `POST` | `/api/robots/cleaning/clean-now` | `apiRobotCleanNow` | login |
| `POST` | `/api/robots/cleaning/opt-out` | `apiRobotCleaningOptOut` | login |
| `POST` | `/api/robots/cleaning/setup` | `apiRobotCleaningSetup` | login |
| `POST` | `/api/robots/control` | `apiRobotControl` | login |
| `GET` | `/api/robots/fleet` | `apiRobotsFleet` | public |
| `POST` | `/api/robots/force-complete` | `apiRobotForceComplete` | login |
| `GET` | `/api/robots/hours` | `apiRobotHours` | public |
| `GET` | `/api/robots/lane-change` | `apiLaneChange` | public |
| `GET` | `/api/robots/localization` | `apiLocalizationBoard` | public |
| `POST` | `/api/robots/move` | `apiRobotMoveTo` | login |
| `POST` | `/api/robots/retry` | `apiRobotRetryFailed` | login |

### `/api/sim`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `POST` | `/api/sim/cancel-fails` | `apiSimCancelFails` (sim build) | public |
| `POST` | `/api/sim/create-fails` | `apiSimCreateFails` (sim build) | public |
| `POST` | `/api/sim/robot/deck` | `apiSimRobotDeck` (sim build) | public |
| `POST` | `/api/sim/robot/fault` | `apiSimRobotFault` (sim build) | public |
| `GET` | `/api/sim/robots` | `apiSimRobots` (sim build) | public |
| `POST` | `/api/sim/speed` | `apiSimSetSpeed` (sim build) | public |
| `GET` | `/api/sim/status` | `apiSimStatus` (sim build) | public |

### `/api/sourceability`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/sourceability/events` | `apiSourceabilityEvents` | public |

### `/api/stations`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/stations` | `apiStations` | public |

### `/api/telemetry`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/telemetry/e-maint` | `apiEMaintRobotTelemetry` | public |
| `GET` | `/api/telemetry/e-maint/download` | `apiEMaintRobotTelemetryDownload` | public |

### `/api/test-commands`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/test-commands` | `apiTestCommandsList` | login |
| `POST` | `/api/test-commands/cancel` | `apiTestCommandCancel` | login |
| `GET` | `/api/test-commands/status` | `apiTestCommandStatus` | login |
| `POST` | `/api/test-commands/submit` | `apiTestCommandSubmit` | login |

### `/api/test-orders`

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/test-orders` | `apiTestOrdersList` | login |
| `POST` | `/api/test-orders/cancel` | `apiTestOrderCancel` | login |
| `GET` | `/api/test-orders/detail` | `apiTestOrderDetail` | login |
| `GET` | `/api/test-orders/direct` | `apiDirectOrdersList` | login |
| `POST` | `/api/test-orders/direct` | `apiDirectOrderSubmit` | login |
| `POST` | `/api/test-orders/direct/complex` | `apiDirectComplexOrderSubmit` | login |
| `POST` | `/api/test-orders/direct/receipt` | `apiDirectOrderReceipt` | login |
| `POST` | `/api/test-orders/direct/release` | `apiDirectOrderRelease` | login |
| `POST` | `/api/test-orders/receipt` | `apiTestOrderReceipt` | login |
| `GET` | `/api/test-orders/robots` | `apiTestRobots` | login |
| `GET` | `/api/test-orders/scene-points` | `apiTestScenePoints` | login |
| `POST` | `/api/test-orders/submit` | `apiTestOrderSubmit` | login |
| `POST` | `/api/test-orders/submit/complex` | `apiKafkaComplexOrderSubmit` | login |

<!-- /generated -->

## SSE Events

**Endpoint:** `GET /events` — server-sent events for live browser updates. No
authentication required. `EventHub.SetupEngineListeners` (`www/sse.go`) turns
engine events into broadcasts; a few handlers broadcast directly.

`?topics=a,b` narrows a connection to the named events
(`EventHub.AddClientFiltered`); without it a connection receives every event.
`robot-update` is coalesced per connection — a newer frame replaces an unsent
older one — and every other event is queued in order
(`TestSSE_CoalescingTopicKeepsOnlyNewest`, `TestSSE_StateChangesAreNotCoalesced`).

| Event | Data |
|---|---|
| `connected` | `{build, ts}`, sent once on connect. A changed `build` across a reconnect means Core restarted, and the page reloads. |
| `heartbeat` | `{build, ts}`, every 30 s. |
| `order-update` | `{type, order_id, …}`. One event name, several shapes, discriminated by `type`: `received`; `dispatched` (+`vendor_order_id`); `status_changed` (+`new_status`); `eta_update` (+`eta`); `completed`; `failed` (+`detail`); `cancelled` (+`reason`); `faulted` (+`detail`); `recovered` (+`robot_id`); `skipped` (+`detail`). The order key is `order_id`, not `id`. |
| `mission-event` | `{order_id, vendor_order_id, old_state, new_state, robot_id}`, plus `blocks` / `errors` from the fleet snapshot and `robot_x`, `robot_y`, `robot_station`, `robot_battery` when the robot is known. |
| `bin-update` | `{node_id, action, bin_id, actor, detail}`. There is no `payload-update`. |
| `node-update` | `{node_id, action}`. |
| `robot-update` | An array, one object per robot: `vehicle_id`, `state`, position (`x`, `y`, `angle`), battery and the flags the Robots page draws, plus the order line the robot is on. |
| `cell-heartbeat` | `{station, process_id, style_id, recorded_at, ts}` per projected production tick. |
| `sourcing-update` | `{changed}`, sent when a sourceability verdict changed, not on every recompute. |
| `system-status` | `{fleet}`, `{messaging}` or `{database}`, each `connected` or `disconnected`. |
| `cms-transaction` | The array of CMS transaction rows just recorded. |
| `fire-alarm` | `{is_fire, actor}` (`www/handlers_firealarm.go`). |
| `debug-log` | `{time, subsystem, message}` (`debuglog.Entry`), one per debug-log entry. |

```
event: order-update
data: {"type": "status_changed", "order_id": 42, "new_status": "delivered"}

event: order-update
data: {"type": "dispatched", "order_id": 42, "vendor_order_id": "sg-42-abc123"}

event: node-update
data: {"node_id": 5, "action": "updated"}
```

## Error Responses

A JSON route that refuses answers with an `error` field:

```json
{
  "error": "description of what went wrong"
}
```

The config save adds `ok: false` and an `errors` map keyed by field
(`configRefusal`). A `login` route reached without a session answers `401`
`{"error": "unauthorized"}` under `/api`, and redirects to `/login` everywhere
else (`(*Handlers).requireAuth`).

HTTP status codes:
- `400` — Bad request (missing/invalid parameters)
- `401` — Unauthorized (an `/api` route behind `requireAuth`, no session)
- `404` — Resource not found
- `409` — Refused against the current state (a bin label that already exists, a station uid already enrolled)
- `500` — Internal server error
