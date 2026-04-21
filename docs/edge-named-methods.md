# Edge named methods — Phase 4 traceability

**Total call sites migrated:** 64
**Unique methods:** 61
**Status:** all migrated in PR 4 (refactor/shingo-architecture-2)

Phase 4 of the shingo architecture refactor eliminated the
`DB() *store.DB` escape hatch on `shingo-edge/www` handlers. Every
`h.engine.DB().X(...)` call site was rewritten to `h.engine.X(...)`, and
the corresponding named methods were added to `*engine.Engine` in
`shingo-edge/engine/engine_db_methods.go`. The `EngineAccess` interface in
`shingo-edge/www/engine_iface.go` declares the same surface so the test
stub in `helpers_test.go` can satisfy it without reaching into
`*store.DB`. `DB() *store.DB` has been removed from `EngineAccess`.

Unlike the Phase 3 core work (which routed handlers through per-domain
`*service.X` types), Phase 4 lands the named methods directly on
`*engine.Engine` as thin passthroughs. Extracting them into per-domain
services (StyleService / ProcessService / OperatorStationService /
ChangeoverService / AdminService / ShiftService / ...) is a deferred
follow-up once the surface stops churning.

## Per-handler migration table

| Handler file                 | Line | Original call                                                           | Named method                                                        | Status   |
| ---------------------------- | ---- | ----------------------------------------------------------------------- | ------------------------------------------------------------------- | -------- |
| handlers_api_orders.go       | 38   | `h.engine.DB().GetProcessNode(*processNodeID)`                          | `h.engine.GetProcessNode(*processNodeID)`                           | migrated |
| handlers_api_orders.go       | 114  | `h.engine.DB().GetProcessNode(*processNodeID)`                          | `h.engine.GetProcessNode(*processNodeID)`                           | migrated |
| handlers_api_orders.go       | 151  | `h.engine.DB().GetProcessNode(*processNodeID)`                          | `h.engine.GetProcessNode(*processNodeID)`                           | migrated |
| handlers_api_orders.go       | 221  | `h.engine.DB().GetProcessNode(*processNodeID)`                          | `h.engine.GetProcessNode(*processNodeID)`                           | migrated |
| handlers_api_orders.go       | 316  | `h.engine.DB().UpdateOrderFinalCount(orderID, req.FinalCount, true)`    | `h.engine.UpdateOrderFinalCount(orderID, req.FinalCount, true)`     | migrated |
| handlers_api_config.go       | 27   | `h.engine.DB().ConfirmAnomaly(snapshotID)`                              | `h.engine.ConfirmAnomaly(snapshotID)`                               | migrated |
| handlers_api_config.go       | 40   | `h.engine.DB().DismissAnomaly(snapshotID)`                              | `h.engine.DismissAnomaly(snapshotID)`                               | migrated |
| handlers_api_config.go       | 184  | `h.engine.DB().ListReportingPoints()`                                   | `h.engine.ListReportingPoints()`                                    | migrated |
| handlers_api_config.go       | 202  | `h.engine.DB().CreateReportingPoint(req.PLCName, req.TagName, ...)`      | `h.engine.CreateReportingPoint(req.PLCName, req.TagName, ...)`      | migrated |
| handlers_api_config.go       | 231  | `h.engine.DB().GetReportingPoint(id)`                                   | `h.engine.GetReportingPoint(id)`                                    | migrated |
| handlers_api_config.go       | 233  | `h.engine.DB().UpdateReportingPoint(id, req.PLCName, ..., req.Enabled)` | `h.engine.UpdateReportingPoint(id, req.PLCName, ..., req.Enabled)`  | migrated |
| handlers_api_config.go       | 253  | `h.engine.DB().GetReportingPoint(id)`                                   | `h.engine.GetReportingPoint(id)`                                    | migrated |
| handlers_api_config.go       | 255  | `h.engine.DB().DeleteReportingPoint(id)`                                | `h.engine.DeleteReportingPoint(id)`                                 | migrated |
| handlers_api_config.go       | 271  | `h.engine.DB().ListStyles()`                                            | `h.engine.ListStyles()`                                             | migrated |
| handlers_api_config.go       | 293  | `h.engine.DB().CreateStyle(req.Name, req.Description, req.ProcessID)`    | `h.engine.CreateStyle(req.Name, req.Description, req.ProcessID)`    | migrated |
| handlers_api_config.go       | 321  | `h.engine.DB().UpdateStyle(id, req.Name, req.Description, ...)`          | `h.engine.UpdateStyle(id, req.Name, req.Description, ...)`          | migrated |
| handlers_api_config.go       | 335  | `h.engine.DB().DeleteStyle(id)`                                         | `h.engine.DeleteStyle(id)`                                          | migrated |
| handlers_api_config.go       | 360  | `h.engine.DB().ListPayloadCatalog()`                                    | `h.engine.ListPayloadCatalog()`                                     | migrated |
| handlers_api_config.go       | 376  | `h.engine.DB().ListProcesses()`                                         | `h.engine.ListProcesses()`                                          | migrated |
| handlers_api_config.go       | 401  | `h.engine.DB().CreateProcess(req.Name, ...)`                            | `h.engine.CreateProcess(req.Name, ...)`                             | migrated |
| handlers_api_config.go       | 428  | `h.engine.DB().UpdateProcess(id, req.Name, ...)`                        | `h.engine.UpdateProcess(id, req.Name, ...)`                         | migrated |
| handlers_api_config.go       | 442  | `h.engine.DB().DeleteProcess(id)`                                       | `h.engine.DeleteProcess(id)`                                        | migrated |
| handlers_api_config.go       | 463  | `h.engine.DB().SetActiveStyle(id, req.StyleID)`                         | `h.engine.SetActiveStyle(id, req.StyleID)`                          | migrated |
| handlers_api_config.go       | 482  | `h.engine.DB().ListStylesByProcess(id)`                                 | `h.engine.ListStylesByProcess(id)`                                  | migrated |
| handlers_api_config.go       | 500  | `h.engine.DB().ListStyleNodeClaims(styleID)`                            | `h.engine.ListStyleNodeClaims(styleID)`                             | migrated |
| handlers_api_config.go       | 522  | `h.engine.DB().UpsertStyleNodeClaim(in)`                                | `h.engine.UpsertStyleNodeClaim(in)`                                 | migrated |
| handlers_api_config.go       | 538  | `h.engine.DB().DeleteStyleNodeClaim(id)`                                | `h.engine.DeleteStyleNodeClaim(id)`                                 | migrated |
| handlers_api_config.go       | 701  | `h.engine.DB().GetAdminUser(username)`                                  | `h.engine.GetAdminUser(username)`                                   | migrated |
| handlers_api_config.go       | 718  | `h.engine.DB().UpdateAdminPassword(username, hash)`                     | `h.engine.UpdateAdminPassword(username, hash)`                      | migrated |
| handlers_production.go       | 12   | `db := h.engine.DB()` (then `db.ListProcesses`, `db.ListStylesByProcess`, `db.ListShifts`, `db.ListHourlyCounts`, `db.HourlyCountTotals`) | `h.engine.ListProcesses / ListStylesByProcess / ListShifts / ListHourlyCounts / HourlyCountTotals` | migrated |
| handlers_production.go       | 117  | `h.engine.DB().ListShifts()`                                            | `h.engine.ListShifts()`                                             | migrated |
| handlers_production.go       | 137  | `db := h.engine.DB()` (then `db.DeleteShift`, `db.UpsertShift`)          | `h.engine.DeleteShift / UpsertShift`                                | migrated |
| handlers_production.go       | 170  | `h.engine.DB().HourlyCountTotals(processID, dateStr)`                    | `h.engine.HourlyCountTotals(processID, dateStr)`                    | migrated |
| handlers_operator_stations.go| 20   | `h.engine.DB().GetOperatorStation(id)`                                  | `h.engine.GetOperatorStation(id)`                                   | migrated |
| handlers_operator_stations.go| 25   | `h.engine.DB().TouchOperatorStation(id, "online")`                       | `h.engine.TouchOperatorStation(id, "online")`                       | migrated |
| handlers_operator_stations.go| 39   | `h.engine.DB().BuildOperatorStationView(id)`                            | `h.engine.BuildOperatorStationView(id)`                             | migrated |
| handlers_operator_stations.go| 47   | `h.engine.DB().TouchOperatorStation(id, "online")`                       | `h.engine.TouchOperatorStation(id, "online")`                       | migrated |
| handlers_operator_stations.go| 52   | `h.engine.DB().ListActiveOrders()`                                      | `h.engine.ListActiveOrders()`                                       | migrated |
| handlers_operator_stations.go| 61   | `h.engine.DB().ListOperatorStations()`                                  | `h.engine.ListOperatorStations()`                                   | migrated |
| handlers_operator_stations.go| 75   | `h.engine.DB().CreateOperatorStation(in)`                               | `h.engine.CreateOperatorStation(in)`                                | migrated |
| handlers_operator_stations.go| 94   | `h.engine.DB().UpdateOperatorStation(id, in)`                           | `h.engine.UpdateOperatorStation(id, in)`                            | migrated |
| handlers_operator_stations.go| 107  | `h.engine.DB().DeleteOperatorStation(id)`                               | `h.engine.DeleteOperatorStation(id)`                                | migrated |
| handlers_operator_stations.go| 131  | `h.engine.DB().MoveOperatorStation(id, req.Direction)`                   | `h.engine.MoveOperatorStation(id, req.Direction)`                   | migrated |
| handlers_operator_stations.go| 139  | `h.engine.DB().ListProcessNodes()`                                      | `h.engine.ListProcessNodes()`                                       | migrated |
| handlers_operator_stations.go| 153  | `h.engine.DB().ListProcessNodesByStation(stationID)`                     | `h.engine.ListProcessNodesByStation(stationID)`                     | migrated |
| handlers_operator_stations.go| 167  | `h.engine.DB().CreateProcessNode(in)`                                   | `h.engine.CreateProcessNode(in)`                                    | migrated |
| handlers_operator_stations.go| 172  | `h.engine.DB().EnsureProcessNodeRuntime(id)`                            | `h.engine.EnsureProcessNodeRuntime(id)`                             | migrated |
| handlers_operator_stations.go| 187  | `h.engine.DB().UpdateProcessNode(id, in)`                               | `h.engine.UpdateProcessNode(id, in)`                                | migrated |
| handlers_operator_stations.go| 200  | `h.engine.DB().DeleteProcessNode(id)`                                   | `h.engine.DeleteProcessNode(id)`                                    | migrated |
| handlers_operator_stations.go| 408  | `h.engine.DB().UpdateProcessNodeRuntimeOrders(id, nil, nil)`             | `h.engine.UpdateProcessNodeRuntimeOrders(id, nil, nil)`             | migrated |
| handlers_operator_stations.go| 607  | `h.engine.DB().GetStationNodeNames(id)`                                 | `h.engine.GetStationNodeNames(id)`                                  | migrated |
| handlers_operator_stations.go| 628  | `h.engine.DB().SetStationNodes(id, req.Nodes)`                          | `h.engine.SetStationNodes(id, req.Nodes)`                           | migrated |
| handlers_admin_pages.go      | 14   | `db := h.engine.DB()` (then `db.ListShifts`)                            | `h.engine.ListShifts`                                               | migrated |
| handlers_admin_pages.go      | 48   | `db := h.engine.DB()` (then `db.ListProcesses`, `db.ListStyles`, `db.ListOperatorStations`, `db.ListStylesByProcess`, `db.ListOperatorStationsByProcess`, `db.ListProcessNodesByProcess`) | `h.engine.ListProcesses / ListStyles / ListOperatorStations / ListStylesByProcess / ListOperatorStationsByProcess / ListProcessNodesByProcess` | migrated |
| handlers_admin_pages.go      | 135  | `db := h.engine.DB()` (then `db.AdminUserExists`, `db.CreateAdminUser`, `db.GetAdminUser`) | `h.engine.AdminUserExists / CreateAdminUser / GetAdminUser`        | migrated |
| handlers_changeover.go       | 32   | `buildChangeoverViewData(db *store.DB, ...)` (helper took `*store.DB` and called `db.ListStylesByProcess`, `db.GetStyle`, `db.GetActiveProcessChangeover`, `db.ListChangeoverStationTasks`, `db.ListChangeoverNodeTasks`, `db.ListProcessNodesByProcess`, `db.GetStyleNodeClaim`, `db.GetOrder`) | helper signature changed to `buildChangeoverViewData(activeProcess *store.Process)`; body now calls the matching `h.engine.X` methods | migrated |
| handlers_changeover.go       | 112  | `db := h.engine.DB()` (then `db.ListProcesses`, `db.ListProcessChangeovers`) | `h.engine.ListProcesses / ListProcessChangeovers`                  | migrated |
| handlers_changeover.go       | 157  | `db := h.engine.DB()` (then `db.ListProcesses`)                         | `h.engine.ListProcesses`                                            | migrated |
| handlers_kanbans.go          | 11   | `db := h.engine.DB()` (then `db.ListProcesses`, `db.ListActiveOrdersByProcess`, `db.ListActiveOrders`) | `h.engine.ListProcesses / ListActiveOrdersByProcess / ListActiveOrders` | migrated |
| handlers_kanbans.go          | 59   | `db := h.engine.DB()` (then `db.ListActiveOrdersByProcess`, `db.ListActiveOrders`) | `h.engine.ListActiveOrdersByProcess / ListActiveOrders`            | migrated |
| handlers_material.go         | 53   | `buildStationViews(db *store.DB, ...)` (helper took `*store.DB` and called `db.ListOperatorStationsByProcess`, `db.BuildOperatorStationView`) | helper signature changed to `buildStationViews(eng EngineAccess, ...)`; body now calls the matching `eng.X` methods | migrated |
| handlers_material.go         | 68   | `db := h.engine.DB()` (then `db.ListProcesses`, `db.GetStyle`)            | `h.engine.ListProcesses / GetStyle`                                 | migrated |
| handlers_material.go         | 107  | `db := h.engine.DB()` (then `db.ListProcesses`)                         | `h.engine.ListProcesses`                                            | migrated |
| handlers_manual_message.go   | 13   | `db := h.engine.DB()` (then `db.ListActiveOrders`)                       | `h.engine.ListActiveOrders`                                         | migrated |
| handlers_manual_order.go     | 9    | `db := h.engine.DB()` (then `db.ListProcessNodes`)                       | `h.engine.ListProcessNodes`                                         | migrated |
| helpers.go                   | 31   | `db := h.engine.DB()` (then `db.ListUnconfirmedAnomalies`, `db.ListReportingPoints`) | `h.engine.ListUnconfirmedAnomalies / ListReportingPoints`          | migrated |

## Unique method inventory

Alphabetical list of the 61 named methods now defined on `*engine.Engine`
(in `shingo-edge/engine/engine_db_methods.go`) and mirrored in the
`EngineAccess` interface (in `shingo-edge/www/engine_iface.go`). Each
method is a one-line delegate to `e.db.<SameName>(...)`.

1. `AdminUserExists() (bool, error)` — reports whether any admin user is provisioned (gates first-login bootstrap).
2. `BuildOperatorStationView(stationID int64) (*store.OperatorStationView, error)` — returns the denormalized view (station + nodes + bin state) for the operator display.
3. `ConfirmAnomaly(id int64) error` — marks a counter-snapshot anomaly as operator-confirmed.
4. `CreateAdminUser(username, passwordHash string) (int64, error)` — inserts the first admin user during login bootstrap.
5. `CreateOperatorStation(in store.OperatorStationInput) (int64, error)` — inserts a new operator station.
6. `CreateProcess(name, description, productionState, counterPLC, counterTag string, counterEnabled bool) (int64, error)` — inserts a new process.
7. `CreateProcessNode(in store.ProcessNodeInput) (int64, error)` — inserts a new process node.
8. `CreateReportingPoint(plcName, tagName string, styleID int64) (int64, error)` — inserts a new reporting-point row.
9. `CreateStyle(name, description string, processID int64) (int64, error)` — inserts a new style tied to a process.
10. `DeleteOperatorStation(id int64) error` — deletes an operator station.
11. `DeleteProcess(id int64) error` — deletes a process.
12. `DeleteProcessNode(id int64) error` — deletes a process node.
13. `DeleteReportingPoint(id int64) error` — deletes a reporting-point row.
14. `DeleteShift(shiftNumber int) error` — removes the shift with the given shift number.
15. `DeleteStyle(id int64) error` — deletes a style.
16. `DeleteStyleNodeClaim(id int64) error` — deletes a style/node claim.
17. `DismissAnomaly(id int64) error` — marks a counter-snapshot anomaly as dismissed.
18. `EnsureProcessNodeRuntime(processNodeID int64) (*store.ProcessNodeRuntimeState, error)` — lazily creates the per-node runtime row.
19. `GetActiveProcessChangeover(processID int64) (*store.ProcessChangeover, error)` — returns the in-flight changeover for a process, if any.
20. `GetAdminUser(username string) (*store.AdminUser, error)` — fetches an admin user by username.
21. `GetOperatorStation(id int64) (*store.OperatorStation, error)` — fetches an operator station row.
22. `GetOrder(id int64) (*store.Order, error)` — fetches a single order row.
23. `GetProcessNode(id int64) (*store.ProcessNode, error)` — fetches a process-node row.
24. `GetReportingPoint(id int64) (*store.ReportingPoint, error)` — fetches a reporting-point row.
25. `GetStationNodeNames(stationID int64) ([]string, error)` — returns the CoreNodeName list assigned to a station.
26. `GetStyle(id int64) (*store.Style, error)` — fetches a style row.
27. `GetStyleNodeClaim(id int64) (*store.StyleNodeClaim, error)` — fetches a style/node claim row.
28. `HourlyCountTotals(processID int64, countDate string) (map[int]int64, error)` — returns hourly counter totals for a process/date summed across styles.
29. `ListActiveOrders() ([]store.Order, error)` — returns all non-terminal orders.
30. `ListActiveOrdersByProcess(processID int64) ([]store.Order, error)` — returns non-terminal orders filtered by process.
31. `ListChangeoverNodeTasks(changeoverID int64) ([]store.ChangeoverNodeTask, error)` — returns per-node tasks for a changeover.
32. `ListChangeoverStationTasks(changeoverID int64) ([]store.ChangeoverStationTask, error)` — returns per-station tasks for a changeover.
33. `ListHourlyCounts(processID, styleID int64, countDate string) ([]store.HourlyCount, error)` — returns hourly count rows for a specific style/date.
34. `ListOperatorStations() ([]store.OperatorStation, error)` — returns all operator stations.
35. `ListOperatorStationsByProcess(processID int64) ([]store.OperatorStation, error)` — returns operator stations for a process.
36. `ListPayloadCatalog() ([]*store.PayloadCatalogEntry, error)` — returns the cached payload catalog entries from core.
37. `ListProcessChangeovers(processID int64) ([]store.ProcessChangeover, error)` — returns the changeover history for a process.
38. `ListProcessNodes() ([]store.ProcessNode, error)` — returns all process nodes.
39. `ListProcessNodesByProcess(processID int64) ([]store.ProcessNode, error)` — returns process nodes filtered by process.
40. `ListProcessNodesByStation(stationID int64) ([]store.ProcessNode, error)` — returns process nodes assigned to a station.
41. `ListProcesses() ([]store.Process, error)` — returns all processes.
42. `ListReportingPoints() ([]store.ReportingPoint, error)` — returns all reporting-point rows.
43. `ListShifts() ([]store.Shift, error)` — returns all configured shifts.
44. `ListStyleNodeClaims(styleID int64) ([]store.StyleNodeClaim, error)` — returns style/node claims for a style.
45. `ListStyles() ([]store.Style, error)` — returns all styles.
46. `ListStylesByProcess(processID int64) ([]store.Style, error)` — returns styles for a process.
47. `ListUnconfirmedAnomalies() ([]store.CounterSnapshot, error)` — returns unconfirmed counter anomalies for the global popover.
48. `MoveOperatorStation(id int64, direction string) error` — reorders an operator station up or down.
49. `SetActiveStyle(processID int64, styleID *int64) error` — sets (or clears) the active style on a process.
50. `SetStationNodes(stationID int64, nodeNames []string) error` — syncs the set of process_nodes attached to a station.
51. `TouchOperatorStation(id int64, healthStatus string) error` — updates the heartbeat/health row for a station.
52. `UpdateAdminPassword(username, passwordHash string) error` — sets a new password hash for an admin user.
53. `UpdateOperatorStation(id int64, in store.OperatorStationInput) error` — updates an operator station.
54. `UpdateOrderFinalCount(id int64, finalCount int64, confirmed bool) error` — records the operator-entered final count on an order.
55. `UpdateProcess(id int64, name, description, productionState, counterPLC, counterTag string, counterEnabled bool) error` — updates a process.
56. `UpdateProcessNode(id int64, in store.ProcessNodeInput) error` — updates a process node.
57. `UpdateProcessNodeRuntimeOrders(processNodeID int64, activeOrderID, stagedOrderID *int64) error` — resets/advances the per-node active and staged order pointers.
58. `UpdateReportingPoint(id int64, plcName, tagName string, styleID int64, enabled bool) error` — updates a reporting-point row.
59. `UpdateStyle(id int64, name, description string, processID int64) error` — updates a style.
60. `UpsertShift(shiftNumber int, name, startTime, endTime string) error` — inserts or replaces a shift by shift number.
61. `UpsertStyleNodeClaim(in store.StyleNodeClaimInput) (int64, error)` — inserts or updates a style/node claim.

## Follow-up

The named methods are thin passthroughs by design. A follow-up PR is
expected to group them into per-domain services
(`StyleService` / `ProcessService` / `OperatorStationService` /
`ChangeoverService` / `AdminService` / `ShiftService` / ...) mirroring
the core-side split completed in Phase 3. At that point the methods
either move to those services (and the engine stops growing) or stay on
the engine as thin façades.

## depguard allow-list status

All 7 edge entries in `.golangci.yml`'s `exclusions.rules` continue to
import `shingoedge/store` for DTO types (e.g. `store.StyleNodeClaimInput`,
`store.Order`, `store.OperatorStationInput`), so none of them come off
the list as part of this PR. The allow-list is unchanged. The list
will shrink when the follow-up domain-service split removes the DTO
dependency — at that point each handler file can be audited and its
allow-list entry removed in the same PR.
