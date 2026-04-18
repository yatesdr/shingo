# Test Catalog

> This file is the single source of truth for test coverage in shingo-core.
> When adding or removing a test function, update this catalog.
>
> **How to read this file:**
> - Each section is a test file. Each row is a test function.
> - TC-numbered rows are behavioral regression tests that track design-doc scenarios.
> - Rows marked `-` are unit/structural tests (store CRUD, messaging, handlers).
>   These pin layer contracts and catch regressions during refactoring but do not
>   cross subsystem boundaries the way TC-numbered tests do.
> - The **TC Numbering Gaps**, **Tests Missing TC Comments**, and **Behavioral Coverage Gaps**
>   sections at the bottom track what is missing.
>
> **Convention:** A `// TC-N:` comment above the function, or `TestTC##_*` in the function name.
>
> **Docker gating:** Test files that need a live Postgres carry `//go:build docker`
> on the first line. Run those with `-tags=docker` (or `make test-all`). The
> fake-backed suites in `material/`, `fulfillment/`, and `dispatch/binresolver/`
> are tag-free and run on every push without Docker.

```sh
go test -v ./<package> -run <TestFunctionName>                # unit + fake-backed
go test -v -tags=docker ./<package> -run <TestFunctionName>   # Postgres-backed
```

---

## Fleet Simulator (`dispatch/fleet_simulator_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| TC-1 | `TestSimulator_ComplexOrderBinTasks` | Complex order blocks must include JackLoad/JackUnload bin tasks |
| TC-2 | `TestSimulator_StagedComplexOrder` | Staged complex order - pre-wait blocks sent initially, post-wait blocks held |
| TC-3 | `TestSimulator_SimpleRetrieveOrder` | Simple retrieve - single pickup/dropoff dispatched to simulator |
| TC-4 | `TestSimulator_StateMapping` | Simulator state mapping matches RDS adapter mapping |
| TC-5 | `TestSimulator_FleetFailure_NoVendorOrderID` | Fleet creation failure causes order to fail with no vendor order ID |

## Group Resolver (`dispatch/group_resolver_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| TC-40a | `TestTC40a_FIFOBuriedOlderThanAccessible` | FIFO mode - buried bin older than accessible triggers reshuffle |
| TC-40a | `TestTC40a_FIFOAccessibleOlderThanBuried` | FIFO mode - accessible bin older than buried, no reshuffle needed |
| TC-40b | `TestTC40b_COSTIgnoresBuriedWhenAccessible` | COST mode - oldest accessible returned, older buried bin ignored |
| TC-40b | `TestTC40b_COSTFallsToBuriedWhenNoAccessible` | COST mode - falls back to buried when no accessible bins |
| TC-41 | `TestTC41_EmptyStarvation_BuriedEmptiesUnreachable` | Empty cart starvation - FindEmptyCompatibleBin is lane-unaware |
| - | `TestGroupResolveRetrieve_AccessibleFIFO` | Retrieve resolves accessible bins in FIFO order |
| - | `TestGroupResolveRetrieve_BuriedFails` | Retrieve fails when only buried bins available |
| - | `TestGroupResolveStore_BackToFront` | Store places bins back-to-front in a lane |
| - | `TestGroupResolveStore_Consolidation` | Store consolidates bins into existing lane positions |
| - | `TestGroupResolveStore_FullLane` | Store fails cleanly when target lane is full |
| - | `TestGroupResolveRetrieve_LockedLaneSkipped` | Retrieve skips locked lanes and finds bins in unlocked lanes |
| - | `TestNodeGroupResolveRetrieve_DirectChildren` | Node group resolve retrieve from direct children |
| - | `TestNodeGroupResolveRetrieve_Mixed` | Node group resolve retrieve with mixed accessibility |
| - | `TestNodeGroupResolveStore_DirectChildren` | Node group resolve store to direct children |
| - | `TestGroupResolveStore_BinTypeRestriction` | Store respects bin-type restrictions on lane slots |
## Dispatch End-to-End (`dispatch/end_to_end_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| TC-41 | `TestTC41_RetrieveEmpty_BuriedEmptyTriggersReshuffle` | retrieve_empty for a buried empty triggers reshuffle instead of returning buried bin |
| TC-71 | `TestDispatcher_MoveOrder_SameNode` | Move order where source and destination are the same node must fail |
| - | `TestDispatcher_RetrieveOrder_FullLifecycle` | Retrieve order full lifecycle through dispatcher |
| - | `TestDispatcher_MoveOrder_FullLifecycle` | Move order full lifecycle through dispatcher |
| - | `TestDispatcher_StoreOrder_FullLifecycle` | Store order full lifecycle through dispatcher |
| - | `TestDispatcher_CancelOrder` | Cancel an in-flight order via dispatcher |
| - | `TestDispatcher_RedirectOrder` | Redirect an in-flight order via dispatcher |
| - | `TestDispatcher_SyntheticNodeResolution` | Synthetic node (NGRP) resolves to concrete child nodes |
| - | `TestDispatcher_MultiOrderToSyntheticNGRP` | Multiple orders targeting same synthetic node resolve correctly |
| - | `TestDispatcher_RetrieveEmptyToSyntheticNGRP` | Retrieve empty dispatches to synthetic NGRP node |
| - | `TestDispatcher_DotNotationBypassesResolver` | Dot-notation source bypasses node group resolver |
| - | `TestDispatcher_FleetFailure` | Fleet adapter failure during dispatch handled gracefully |
| - | `TestDispatcher_PriorityHandling` | Order priority passed through to fleet adapter |
| - | `TestHandleRetrieve_BinTracking` | Retrieve handler tracks bin movement correctly |
| - | `TestHandleOrderIngest` | Order ingest validates and persists incoming orders |
| - | `TestDispatcher_MoveOrder_NGRPSource` | Move order with NGRP source resolves and picks accessible bin |
| - | `TestDispatcher_MoveOrder_NGRPSource_NoBin` | Move order with NGRP source when no bins available fails cleanly |
| - | `TestDispatcher_MoveOrder_NGRPSource_BuriedBin` | Move order with NGRP source when bin is buried triggers reshuffle |

## Bin Lifecycle (`dispatch/bin_lifecycle_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestFullDepletion_ClearsManifest` | Fully consumed bin has its manifest cleared automatically |
| - | `TestPartialConsumption_SyncsUOP` | Partially consumed bin syncs remaining UOP count |
| - | `TestConcurrentRetrieveEmpty_BothClaimed_NoOverlap` | Concurrent retrieve-empty requests both claim bins with no overlap |
| - | `TestComplexOrder_RemainingUOP_ProcessNodeOnly` | Complex order with remaining UOP processes at node only |

## Complex Order Dispatch (`dispatch/complex_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestResolvePerBinDestinations_SinglePickupDropoff` | Single pickup/dropoff resolved to one bin with correct src/dst |
| - | `TestResolvePerBinDestinations_SwapPattern` | Swap pattern resolves two bins with crossed src/dst |
| - | `TestResolvePerBinDestinations_ReStaging` | Re-staging pattern resolves bin back to staging node |
| - | `TestResolvePerBinDestinations_EmptyDropoff` | Empty dropoff (retrieve_empty) resolves correctly |
| - | `TestResolvePerBinDestinations_GhostPickup` | Ghost pickup (no bin at node) resolves gracefully |
| - | `TestResolvePerBinDestinations_MultiplePickupsSameNode` | Multiple pickups at same node resolved to separate bins |
## Dispatcher Core (`dispatch/dispatcher_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestHandleOrderReceipt` | HandleOrderReceipt creates and persists a new order |
| - | `TestHandleOrderReceipt_DuplicateCompletedOrderIgnored` | Duplicate receipt for already-completed order is ignored |
| - | `TestHandleOrderReceipt_RejectsWrongStation` | Order receipt rejected when station ID does not match |
| - | `TestHandleOrderRequest_Retrieve_NoSource` | Retrieve request with no available source bins fails cleanly |
| - | `TestHandleOrderRequest_Retrieve_InvalidDeliveryNode` | Retrieve request with invalid delivery node rejected |
| - | `TestHandleOrderRequest_Move_MissingPickup` | Move request with missing pickup location rejected |
| - | `TestHandleOrderRequest_Move_NoPayloadAtPickup` | Move request with no payload at pickup location rejected |
| - | `TestHandleOrderRequest_UnknownType` | Unknown order type rejected with error |
| - | `TestHandleOrderRequest_UsesRegisteredPlanner` | Dispatcher delegates to registered planner for order planning |
| - | `TestHandleOrderRequest_UnknownStyle` | Unknown fulfillment style rejected with error |
| - | `TestHandleOrderCancel` | Order cancel sets status and unclaims bins |
| - | `TestHandleOrderCancel_UnclaimsPayloads` | Cancelled order releases bin claims |
| - | `TestHandleOrderCancel_RejectsWrongStation` | Cancel rejected when station ID does not match |
| - | `TestHandleOrderCancel_AllowsCoreRole` | Cancel allowed when caller has core role |
| - | `TestHandleOrderCancel_DuplicateCancelledOrderIgnored` | Cancel of already-cancelled order is idempotent |
| - | `TestHandleOrderRedirect_RejectsWrongStation` | Redirect rejected when station ID does not match |
| - | `TestFIFOPayloadSourceSelection` | Payload source selection uses FIFO ordering |
| - | `TestStatusConstants` | Order status constants match expected values |
| - | `TestOrderTypeConstants` | Order type constants match expected values |
| - | `TestRegression_HandleOrderReceipt_ReturnsOnError` | HandleOrderReceipt returns error on database failure |

## Planning (`dispatch/planning_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestExtractRemainingUOP_NilEnvelope` | ExtractRemainingUOP returns zero for nil envelope |
| - | `TestExtractRemainingUOP_EmptyPayload` | ExtractRemainingUOP returns zero for empty payload |
| - | `TestExtractRemainingUOP_NoField` | ExtractRemainingUOP returns zero when field missing |
| - | `TestExtractRemainingUOP_Zero` | ExtractRemainingUOP returns zero when field is zero |
| - | `TestExtractRemainingUOP_Positive` | ExtractRemainingUOP returns correct positive value |
| - | `TestExtractRemainingUOP_MalformedJSON` | ExtractRemainingUOP handles malformed JSON gracefully |

## Reshuffle (`dispatch/reshuffle_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestPlanReshuffle_SingleBlocker` | Reshuffle plan for single blocking bin |
| - | `TestPlanReshuffle_MultipleBlockers` | Reshuffle plan for multiple blocking bins |
| - | `TestPlanReshuffle_NoShuffleSlots` | Reshuffle fails when no available shuffle slots |
| - | `TestLaneLock_PreventsConcurrent` | Lane lock prevents concurrent reshuffle operations |
| - | `TestCompoundOrderCreation` | Compound order created correctly from reshuffle plan |
| - | `TestHandleChildOrderFailure` | Child order failure triggers rollback of reshuffle |
| - | `TestHandleChildOrderFailure_InFlightSibling` | Child order failure with in-flight sibling orphaned |
## Engine - Test Helpers (`engine/engine_testhelpers_test.go`)

Shared test scaffolding for the engine package. No test functions — all
exported names are setup helpers consumed by `engine_test.go` and
`engine_regression_test.go`. Carries `//go:build docker`.

| ID | Function | Description |
|----|----------|-------------|
| - | `testDB` | Opens a fresh per-test Postgres database via `internal/testdb.Open` |
| - | `setupTestData` | Creates standard storage/line/payload fixtures via `testdb.SetupStandardData` |
| - | `createTestBinAtNode` | Thin wrapper around `testdb.CreateBinAtNode` |
| - | `testEnvelope` | Builds a baseline protocol envelope via `testdb.Envelope` |
| - | `newTestEngine` | Constructs a real Engine wired to the test DB and simulator, with `t.Cleanup` Stop |

## Engine - Core Lifecycle (`engine/engine_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| TC-15 | `TestSimulator_FullLifecycle` | Full lifecycle - order submitted, dispatched, delivered, confirmed |
| TC-2 | `TestSimulator_StagedComplexOrderRelease` | Staged complex order release |
| TC-ClaimBin | `TestClaimBin_SilentOverwrite` | Silent claim overwrite - second order claiming same bin |
| TC-21 | `TestTC21_QualityHoldBinNotDispatched` | Only available bin is in quality hold - order fails cleanly |
| TC-23a | `TestTC23a_MoveClaimedStagedBin` | Operator tries to move a claimed bin via a second store order |
| TC-23b | `TestTC23b_CancelThenMoveBin` | Cancel in-flight store order - return order claims bin |
| TC-23c | `TestTC23c_ChangeoverWithMissingBin` | Changeover with one bin already gone |
| TC-23d | `TestTC23d_ChangeoverWhileMoveInFlight` | Changeover while move-to-quality-hold is still in flight |
| TC-24 | `TestTC24_ComplexOrderBinPoaching` | Complex order bin poaching |
| TC-24b | `TestTC24b_StaleBinLocationAfterComplexOrder` | Stale bin location after complex order completes |
| TC-24c | `TestTC24c_PhantomInventoryRetrieve` | Phantom inventory - retrieve dispatched to empty node |
| TC-25 | `TestTC25_StoreOrderClaimsStagedBinAtCoreNode` | Store order correctly claims staged bin at core node |
| TC-28 | `TestTC28_ConcurrentRetrieveSamePart` | Two lines request the same part at the same time |
| TC-30 | `TestTC30_FailedOrderReturnClaimTransfer` | Failed order creates a return - does the return inherit the reservation? |
| TC-36 | `TestTC36_RetrieveClaimFailure_QueueNotFail` | Retrieve claim failure - queue instead of fail |
| TC-38 | `TestTC38_CancelDeliveredOrder_NoReturnOrder` | Cancel delivered order must not create return order / receipt on cancelled order |
| TC-39 | `TestTC39_TerminateOrder_RejectsTerminalStatuses` | TerminateOrder rejects terminal statuses |
| TC-80 | `TestTC80_OrphanedBinClaim_ReconciliationDetectsAndSweepFixes` | Orphaned bin claim after terminal order - reconciliation detects and sweep fixes |

## Engine - Regression (`engine/engine_regression_test.go`)

Regression suite extracted from `engine_test.go` in Stage 9 so the
happy-path behavior file is not dominated by bug-fix scaffolding.
Carries `//go:build docker`.

| ID | Function | Description |
|----|----------|-------------|
| - | `TestMaybeCreateReturnOrder_SourceNode` | Return order created with correct source node after failure |
| - | `TestRegression_BinMovesOnDelivered` | Regression - bin moves on delivered order completion |
| - | `TestRegression_CancelEmptyEdgeUUID` | Regression - cancel on empty edge with no vendor UUID |
| - | `TestRegression_MultiBinMovesOnDelivered` | Regression - multiple bins moved on delivered order completion |
| - | `TestRegression_CompletionIdempotentAfterDelivery` | Regression - completion handler is idempotent after delivery |

## Engine - Concurrency (`engine/engine_concurrent_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| TC-9 | `TestTC09_ComplexOrderZeroSteps` | Complex order with zero steps |
| TC-10 | `TestTC10_NonexistentDeliveryNode` | Order references nonexistent delivery node |
| TC-12 | `TestTC12_ZeroQuantity` | Order requests zero quantity |
| TC-37 | `TestTC37_StagingExpiryVsActiveClaim` | Staging sweep flips bin to available while still claimed |
| - | `TestConcurrent_ClaimRaceDeterministic` | Concurrent bin claim race produces deterministic winner |
| - | `TestConcurrent_DispatchStress` | Stress test - many concurrent dispatches complete without deadlock |
| - | `TestRedirect_MidTransit` | Redirect order mid-transit updates destination |
| - | `TestFulfillmentScanner_QueueToDispatch` | Fulfillment scanner promotes queued orders to dispatched |
## Engine - Complex Orders (`engine/engine_complex_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| TC-42 | `TestComplexOrder_CancelMidTransit` | Complex order cancelled while robot in transit |
| TC-47 | `TestComplexOrder_EmptyPostWaitRelease` | Empty post-wait release |
| TC-48 | `TestComplexOrder_RedirectStaleStepsJSON` | Complex order redirect does not update StepsJSON |
| TC-49 | `TestComplexOrder_GhostRobotNoBin` | Ghost robot - claimComplexBins finds no bin |
| TC-50 | `TestComplexOrder_ConcurrentSameNodeDoubleClaimRace` | Concurrent complex orders targeting same node - double claim race |
| TC-55 | `TestComplexOrder_SequentialBackfill` | Sequential Backfill (Order B) - simplest, no wait |
| TC-56 | `TestComplexOrder_SequentialRemoval` | Sequential Removal (Order A) - wait/release lifecycle |
| TC-57 | `TestComplexOrder_TwoRobotSwap_Resupply` | Two-Robot Swap Resupply (Order A) |
| TC-58 | `TestComplexOrder_TwoRobotSwap_Removal` | Two-Robot Swap Removal (Order B) |
| TC-59 | `TestComplexOrder_StagingAndDeliver` | Staging + Deliver Separation |
| TC-60 | `TestComplexOrder_SingleRobotSwap` | Single-Robot 9-Step Swap |
| TC-DW | `TestComplexOrder_DoubleWait` | Double-wait complex order - Phase 3 evacuate flow prerequisite |
| - | `TestComplexOrder_FleetFailureMidTransit` | Complex order fleet failure during mid-transit |

## Engine - Compound Orders (`engine/engine_compound_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| TC-44 | `TestBuriedBin_ReshuffleViaEngine` | Compound order basic lifecycle (reshuffle via engine) |
| TC-45 | `TestCompound_ChildFailureMidReshuffle_BlockerStranding` | Compound order child failure handling - blocker stranding |
| TC-46 | `TestCompound_CancelParentWhileChildInFlight` | Compound order cancellation while child in flight |
| TC-51 | `TestCompound_AdvanceSkipsFailedChild_PrematureCompletion` | AdvanceCompoundOrder skips failed children - premature completion |
| TC-52 | `TestLaneLock_Contention_SecondReshuffleBlocked` | Lane lock contention - second reshuffle blocked |
| TC-53 | `TestCompound_RestockChild_BinStatusAvailable` | ApplyBinArrival status mapping for compound restock children |
| TC-54 | `TestCompound_StagingTTLExpiryDuringReshuffle` | Staging TTL expiry during compound order execution |
| - | `TestCompound_TwoRobotSwap_FullLifecycle` | Full two-robot swap compound order lifecycle |

## Engine - Order Completion (`engine/wiring_completion_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| TC-CO-1 | `TestOrderCompleted_BinAlreadyAtDest` | Normal receipt - bin already at destination (idempotent safety net) |
| TC-CO-2 | `TestOrderCompleted_NoBinID` | handleOrderCompleted with missing BinID - early return, no crash |
| TC-CO-3 | `TestOrderCompleted_MissingNodes` | handleOrderCompleted with missing source/delivery nodes - early return |
| TC-CO-4 | `TestOrderCompleted_NonExistentOrder` | handleOrderCompleted for non-existent order - log and return |
| TC-CO-5 | `TestOrderCompleted_SafetyNetArrival` | handleOrderCompleted safety net - bin NOT at dest yet |
| TC-CO-6 | `TestOrderCompleted_RetrieveEmptyOverride` | handleOrderCompleted with retrieve_empty payload - staged=false override |
| TC-CO-7 | `TestOrderCompleted_ComplexWaitOverride` | handleOrderCompleted with complex order + WaitIndex > 0 - staged=false override |

## Engine - Staging (`engine/wiring_staging_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| TC-RS-1 | `TestResolveNodeStaging_LinesideNode` | Normal lineside node (no parent) - staged=true |
| TC-RS-2 | `TestResolveNodeStaging_StorageSlotUnderLane` | Storage slot under a LANE parent - staged=false |
| TC-RS-3 | `TestResolveNodeStaging_NonLaneParent` | Node with non-LANE parent - staged=true (treated as lineside) |
| TC-RS-4 | `TestResolveNodeStaging_NoParent` | Node with no parent ID - staged=true (lineside default) |

## Engine - Vendor Status (`engine/wiring_vendor_status_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| TC-VS-1 | `TestVendorStatus_RunningUpdatesStatus` | RUNNING state updates order to in_transit and assigns robot ID |
| TC-VS-2 | `TestVendorStatus_IdempotentStatus` | Idempotent status - driving same state twice does not error |
| TC-VS-3 | `TestVendorStatus_FinishedDelivers` | FINISHED terminal state - order delivered, bin moved to dest |
| TC-VS-4 | `TestVendorStatus_FailedTerminal` | FAILED terminal state - order failed, EventOrderFailed emitted |
| TC-VS-5 | `TestVendorStatus_StoppedCancels` | STOPPED terminal state - order cancelled, EventOrderCancelled emitted |
| TC-VS-6 | `TestVendorStatus_NonExistentOrder` | Non-existent order - handleVendorStatusChange logs and returns gracefully |

## Engine - Vendor Robot Assignment (`engine/wiring_vendor_robot_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| TC-90 | `TestVendorStatus_RobotID_FirstAssignment` | First robot assignment persists robot ID and sends waybill |
| TC-91 | `TestVendorStatus_RobotID_CaseD_NoClobber` | Case D regression - subsequent event with empty RobotID does NOT clobber |
| TC-92 | `TestVendorStatus_RobotID_Reassignment` | Robot reassignment - event with different non-empty RobotID updates |
| TC-93 | `TestVendorStatus_RobotID_IdempotentNoChange` | Idempotent no-write - same status + same robot = no state change |
| TC-94 | `TestVendorStatus_RobotID_OptionC_SingleWrite` | Option C dedup - first robot assignment + status change = single UpdateOrderVendor |
| TC-95 | `TestVendorStatus_RobotID_NarrowWrite_SameStatusNewRobot` | Idempotent path uses narrow UpdateOrderRobotID when robot changes without status change |
## Web Handlers - Bin Actions (`www/handlers_bins_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| TC-70 | `TestExecuteBinAction_Move_SameNode` | Moving a bin to its current node is physically impossible and must be rejected |
| - | `TestExecuteBinAction_Activate` | Activate bin action handler |
| - | `TestExecuteBinAction_Flag` | Flag bin action handler |
| - | `TestExecuteBinAction_Maintenance` | Maintenance bin action handler |
| - | `TestExecuteBinAction_Retire` | Retire bin action handler |
| - | `TestExecuteBinAction_QualityHold` | Quality hold bin action handler |
| - | `TestExecuteBinAction_QualityHold_NoReason` | Quality hold rejected when no reason provided |
| - | `TestExecuteBinAction_Lock` | Lock bin action handler |
| - | `TestExecuteBinAction_Lock_NoExplicitActor` | Lock bin defaults actor when not specified |
| - | `TestExecuteBinAction_Unlock` | Unlock bin action handler |
| - | `TestExecuteBinAction_Release` | Release bin action handler |
| - | `TestExecuteBinAction_LoadPayload` | Load payload into bin |
| - | `TestExecuteBinAction_LoadPayload_MissingCode` | Load payload rejected when code missing |
| - | `TestExecuteBinAction_LoadPayload_UnknownCode` | Load payload rejected when code unknown |
| - | `TestExecuteBinAction_Clear` | Clear bin contents |
| - | `TestExecuteBinAction_ConfirmManifest` | Confirm bin manifest |
| - | `TestExecuteBinAction_ConfirmManifest_NoManifest` | Confirm manifest rejected when no manifest exists |
| - | `TestExecuteBinAction_UnconfirmManifest` | Unconfirm bin manifest |
| - | `TestExecuteBinAction_Move` | Move bin to destination node |
| - | `TestExecuteBinAction_Move_MissingNodeID` | Move rejected when destination node ID missing |
| - | `TestExecuteBinAction_Move_UnknownNode` | Move rejected when destination node unknown |
| - | `TestExecuteBinAction_RecordCount` | Record count discrepancy on bin |
| - | `TestExecuteBinAction_RecordCount_NoDiscrepancy` | Record count with no discrepancy is no-op |
| - | `TestExecuteBinAction_AddNote` | Add note to bin |
| - | `TestExecuteBinAction_AddNote_DefaultNoteType` | Add note with default note type |
| - | `TestExecuteBinAction_AddNote_MissingMessage` | Add note rejected when message missing |
| - | `TestExecuteBinAction_Update` | Update bin metadata |
| - | `TestExecuteBinAction_Update_PartialFields` | Update bin with only partial fields set |
| - | `TestExecuteBinAction_UnknownAction` | Unknown bin action rejected |
## Store - Audit (`store/audit_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestAuditLog` | Audit log CRUD and query |

## Store - Bins (`store/bins_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestClaimBin` | ClaimBin sets order_id and reserves bin |
| - | `TestUnclaimOrderBins` | UnclaimOrderBins releases all bins for an order |
| - | `TestFindEmptyCompatibleBin` | FindEmptyCompatibleBin returns first unclaimed bin at node |

## Store - Corrections (`store/corrections_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestCorrectionCRUD` | Correction record CRUD lifecycle |

## Store - Inbox (`store/inbox_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestRecordInboundMessage` | Inbound message recorded with dedup key |

## Store - Nodes (`store/nodes_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestNodeCRUD` | Node CRUD lifecycle |
| - | `TestLaneQueries` | Lane-specific queries (children, parent, siblings) |

## Store - Orders (`store/orders_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestOrderCRUD` | Order CRUD lifecycle |
| - | `TestListOrders` | ListOrders with status filtering and pagination |
| - | `TestListDispatchedVendorOrderIDs` | ListDispatchedVendorOrderIDs returns all active vendor IDs |
| - | `TestOrderCompoundFields` | Order compound fields (parent_id, child_index, step_index) persisted |

## Store - Outbox (`store/outbox_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestOutboxCRUD` | Outbox message CRUD lifecycle |
| - | `TestOutboxDeadLetterReplay` | Dead-lettered outbox message can be replayed |

## Store - Payloads (`store/payloads_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestBinTypeCRUD` | Bin type CRUD lifecycle |
| - | `TestBinCRUD` | Bin CRUD lifecycle |
| - | `TestPayloadCRUD` | Payload CRUD lifecycle |
| - | `TestPayloadBinTypeJunction` | Payload-to-bin-type junction table operations |
| - | `TestPayloadTemplateCRUD` | Payload template CRUD lifecycle |
| - | `TestBinManifestLifecycle` | Bin manifest lifecycle (create, update, confirm, clear) |
| - | `TestPayloadManifestCRUD` | Payload manifest CRUD |
| - | `TestNodePayloadAssignment` | Node-payload assignment and lookup |
| - | `TestConfirmBinManifest_ProducedAt` | ConfirmBinManifest sets produced_at timestamp |

## Store - Reconciliation (`store/reconciliation_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestListOrderCompletionAnomalies` | ListOrderCompletionAnomalies detects orphaned claims and stuck orders |
| - | `TestGetReconciliationSummary` | GetReconciliationSummary returns aggregate counts |

## Store - Recovery (`store/recovery_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestRepairConfirmedOrderCompletion` | RepairConfirmedOrderCompletion fixes inconsistent terminal state |
| - | `TestReleaseTerminalBinClaimRejectsActiveOrder` | ReleaseTerminalBinClaim refuses when order still active |
| - | `TestReleaseTerminalBinClaimAllowsCancelledOrder` | ReleaseTerminalBinClaim succeeds for cancelled/failed orders |
## Service - Bin Manifest (`service/bin_manifest_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestBinManifestService_ClearForReuse` | ClearForReuse resets manifest and claim for bin reuse |
| - | `TestBinManifestService_ClearForReuse_MakesVisibleToFindEmpty` | Cleared bin becomes visible to FindEmptyCompatibleBin |
| - | `TestBinManifestService_SyncUOP_PreservesManifest` | SyncUOP preserves existing manifest entries |
| - | `TestBinManifestService_ClearAndClaim_Atomic` | ClearAndClaim atomically clears and claims a bin |
| - | `TestBinManifestService_ClearAndClaim_FailsIfAlreadyClaimed` | ClearAndClaim fails when bin already claimed |
| - | `TestBinManifestService_SyncUOPAndClaim` | SyncUOPAndClaim combines UOP sync with claim in one operation |
| - | `TestBinManifestService_ClaimForDispatch_NilIsPlainClaim` | ClaimForDispatch with nil UOP behaves as plain claim |
| - | `TestBinManifestService_ClaimForDispatch_ZeroClearsManifest` | ClaimForDispatch with zero UOP clears manifest |
| - | `TestBinManifestService_ClaimForDispatch_PositiveSyncsUOP` | ClaimForDispatch with positive UOP syncs remaining units |
| - | `TestBinManifestService_SetForProduction` | SetForProduction marks bin manifest for production node |
| - | `TestBinManifestService_Confirm` | Confirm finalizes bin manifest |
| - | `TestBinManifestService_ClearAndClaim_FailsIfLocked` | ClearAndClaim fails when bin is locked |
| - | `TestBinManifestService_ClaimForDispatch_ConcurrentRace` | ClaimForDispatch under concurrent race only one wins |

## Messaging - Client (`messaging/client_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestNewClient` | Client constructor initializes with correct defaults |
| - | `TestClient_Connect_NoBrokers` | Connect fails gracefully when no brokers configured |
| - | `TestClient_IsConnected` | IsConnected reflects connection state |
| - | `TestClient_Close` | Close shuts down client cleanly |
| - | `TestClient_CloseIdempotent` | Close is idempotent - second call is no-op |
| - | `TestClient_Subscribe_NotConnected` | Subscribe rejected when client not connected |
| - | `TestClient_Publish_NotConnected` | Publish rejected when client not connected |
| - | `TestClient_PublishEnvelope` | PublishEnvelope sends encoded envelope to topic |
| - | `TestClient_Reconfigure` | Reconfigure updates broker list without restart |
| - | `TestClient_HandlerRegistration` | Handler registration maps topic to callback |
| - | `TestClient_EnvelopeEncoding` | Envelope encoding round-trips without data loss |
| - | `TestClient_DataEnvelope` | DataEnvelope wraps payload with metadata |
| - | `TestClient_StopChanClosed` | Stop channel signals graceful shutdown |
| - | `TestClient_ConcurrentAccess` | Concurrent publish/subscribe does not race |
| - | `TestClient_DebugLog` | Debug logging outputs when enabled |
| - | `TestClient_BackoffCalculation` | Backoff calculation produces increasing intervals |

## Messaging - Core Data Service (`messaging/core_data_service_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestNodeListResponse_IncludesNodeGroups` | NodeList response includes node group (NGRP) entries |
| - | `TestNodeListResponse_GlobalPath_IncludesNodeGroups` | NodeList on global path includes node groups |

## Messaging - Core Handler (`messaging/core_handler_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestCoreHandlerDeduplicatesRedirectByEnvelopeID` | Core handler deduplicates redirect messages by envelope ID |
| - | `TestCoreHandlerDeduplicatesOrderRequestByEnvelopeID` | Core handler deduplicates order requests by envelope ID |
| - | `TestCoreHandlerDeduplicationPersistsAcrossHandlerRestart` | Deduplication state persists across handler restarts |
| - | `TestCoreHandlerDeduplicatesReceiptAcrossHandlerRestart` | Receipt deduplication persists across handler restarts |
## RDS Client (`rds/client_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestCreateJoinOrder` | CreateJoinOrder sends request and parses response |
| - | `TestCreateJoinOrder_Error` | CreateJoinOrder handles HTTP error response |
| - | `TestGetOrderDetails` | GetOrderDetails retrieves order from RDS |
| - | `TestGetOrderDetails_NotFound` | GetOrderDetails returns error for non-existent order |
| - | `TestTerminateOrder` | TerminateOrder sends cancel to RDS |
| - | `TestListOrders` | ListOrders retrieves paginated order list |
| - | `TestSetPriority` | SetPriority updates order priority in RDS |
| - | `TestPing` | Ping checks RDS connectivity |
| - | `TestGetRobotsStatus` | GetRobotsStatus retrieves robot status list |
| - | `TestHTTPError` | HTTP error response parsed correctly |
| - | `TestCheckResponse` | CheckResponse handles various HTTP status codes |
| - | `TestOrderStateIsTerminal` | OrderStateIsTerminal identifies terminal states |
| - | `TestPollerTrackUntrack` | Poller track/untrack manages order polling set |
| - | `TestPollerDetectsStateTransition` | Poller detects state transitions between polling cycles |
| - | `TestPollerRemovesTerminal` | Poller removes terminal orders from tracking set |

## Fleet - SeerRDS Adapter (`fleet/seerrds/adapter_test.go`)

| ID | Function | Description |
|----|----------|-------------|
| - | `TestReleaseOrder_PinsVehicle` | ReleaseOrder pins vehicle to station after order completes |
| - | `TestReleaseOrder_NoVehicleFallback` | ReleaseOrder handles missing vehicle ID gracefully |

---
## TC Numbering Gaps

TC numbers are assigned sequentially as regression tests are added. Gaps in the
1-95 range represent tests that were either deleted, moved to `shingo-edge`, or
were TC numbers allocated in design docs but not yet implemented in `shingo-core`.

| Gap | Status | Notes |
|-----|--------|-------|
| TC-6 through TC-8 | Reserved in design docs | Not yet implemented in `shingo-core` - may exist in `shingo-edge` |
| TC-11 | Reserved in design docs | Not yet implemented in `shingo-core` - may exist in `shingo-edge` |
| TC-13 | Reserved in design docs | Not yet implemented in `shingo-core` - may exist in `shingo-edge` |
| TC-14 | Design doc: return-order SourceNode | Covered by `TestMaybeCreateReturnOrder_SourceNode` but not TC-numbered in code |
| TC-14b | Design doc: child failure orphaned siblings | Covered by `TestHandleChildOrderFailure_InFlightSibling` but not TC-numbered |
| TC-16 through TC-20 | Reserved in design docs | Not yet implemented in `shingo-core` - may exist in `shingo-edge` |
| TC-22 | Reserved in design docs | Not yet implemented in `shingo-core` - may exist in `shingo-edge` |
| TC-26, TC-27 | Reserved in design docs | Not yet implemented in `shingo-core` - may exist in `shingo-edge` |
| TC-29 | Reserved in design docs | Not yet implemented in `shingo-core` - may exist in `shingo-edge` |
| TC-31 through TC-35 | Reserved in design docs | Not yet implemented in `shingo-core` - may exist in `shingo-edge` |
| TC-43 | Design doc: complex order round-trip | Implicitly covered by `engine_complex_test.go` suite but not TC-numbered |
| TC-62 through TC-67 | Reserved in design docs | Not yet implemented in `shingo-core` - may exist in `shingo-edge` |
| TC-68 | Design doc: post-delivery cancel guard | Not yet implemented - behavioral gap (see Coverage Gaps) |
| TC-69 | Design doc: node list sync | Covered by `TestNodeListResponse_IncludesNodeGroups` but not TC-numbered |
| TC-71a through TC-71e | Design doc: NGRP source variants | Partially covered by `TestDispatcher_MoveOrder_NGRPSource*` but not TC-numbered |
| TC-73, TC-74 | Reserved in design docs | Not yet implemented in `shingo-core` - may exist in `shingo-edge` |
| TC-81 through TC-89 | Reserved | Not yet allocated - next available TC number is TC-96 |
## Tests Missing TC Comments

The convention is a `// TC-N:` comment above the test function. These tests have
TC numbers in their function names or in the catalog but lack the comment header:

| File | Function | Expected TC |
|------|----------|-------------|
| `engine/engine_test.go` | `TestSimulator_FullLifecycle` | TC-15 |
| `engine/engine_test.go` | `TestSimulator_StagedComplexOrderRelease` | TC-2 |
| `engine/engine_test.go` | `TestClaimBin_SilentOverwrite` | TC-ClaimBin |
| `engine/engine_test.go` | `TestTC21_*` through `TestTC80_*` | TC-21 through TC-80 |
| `engine/engine_concurrent_test.go` | `TestTC09_*`, `TestTC10_*`, `TestTC12_*`, `TestTC37_*` | TC-9, TC-10, TC-12, TC-37 |
| `engine/engine_complex_test.go` | All `TestComplexOrder_*` tests | TC-42 through TC-60, TC-DW |
| `engine/engine_compound_test.go` | All `TestCompound_*` and `TestBuriedBin_*` tests | TC-44 through TC-54 |
| `dispatch/fleet_simulator_test.go` | `TestSimulator_ComplexOrderBinTasks` | TC-1 |
| `dispatch/fleet_simulator_test.go` | `TestSimulator_StagedComplexOrder` | TC-2 |
| `dispatch/fleet_simulator_test.go` | `TestSimulator_StateMapping` | TC-4 |
| `dispatch/fleet_simulator_test.go` | `TestSimulator_FleetFailure_NoVendorOrderID` | TC-5 |
| `dispatch/group_resolver_test.go` | `TestTC40a_FIFOBuriedOlderThanAccessible` | TC-40a |
| `dispatch/group_resolver_test.go` | `TestTC40b_COSTIgnoresBuriedWhenAccessible` | TC-40b |
| `dispatch/group_resolver_test.go` | `TestTC41_EmptyStarvation_BuriedEmptiesUnreachable` | TC-41 |
| `engine/wiring_vendor_robot_test.go` | `TestVendorStatus_RobotID_OptionC_SingleWrite` | TC-94 |

## Behavioral Coverage Gaps

Based on the fleet simulator design docs and codebase analysis, these flows
should have tests but currently do not:

- **Dual-bin-at-same-node scenario** - The recently fixed bug where two bins
  at the same node could cause a claim collision has no dedicated regression
  test. The fix is implicitly exercised by ``TestTC28_ConcurrentRetrieveSamePart``
  but no test explicitly sets up two bins at the same physical node.

- **Full delivery path: receipt to bin movement to claim release chain** -
  No single test traces the complete chain from order receipt through vendor
  status updates, order completion, bin movement confirmation, and claim
  release. Individual lifecycle tests cover fragments.

- **Post-delivery cancel guard (TC-68)** - Design docs describe a guard
  against cancelling an order that has already been delivered/completed.
  ``TC-38`` covers the no return order case but not the broader cancel guard.

- **handleChildOrderFailure leaving in-flight siblings orphaned (TC-14b)** -
  ``TestHandleChildOrderFailure_InFlightSibling`` exists but is not
  TC-numbered and may not cover the full orphan detection + recovery path.

- **Produce-node complex order completion** - Design docs describe produce-node
  behavior where a complex order post-wait block completes and the bin
  transitions from staged to production. No test covers the staged=false
  override at the produce node specifically.

- **HTTP-level tests for `/api/bins/request-transport`** - The transport
  endpoint has backend validation (same-node rejection) but no Go test.
  The ``www`` package currently only tests ``executeBinAction`` directly,
  not HTTP handlers. Adding httptest coverage would require new test
  infrastructure.

- **TC-71 covers dispatch-level same-node rejection** but there is no
  corresponding test for an edge station submitting a move order where
  both ``source_node`` and ``delivery_node`` resolve to the same concrete
  node through NGRP resolution.

## TC Numbering Backlog

The following unnumbered tests are behavioral enough to warrant TC assignment.
Prioritize tests with Regression, Concurrent, or Race in the name:

| File | Function | Rationale |
|------|----------|-----------|
| `engine/engine_regression_test.go` | `TestMaybeCreateReturnOrder_SourceNode` | Regression fix with no TC - return order SourceNode correctness |
| `engine/engine_regression_test.go` | `TestRegression_BinMovesOnDelivered` | Regression - bin moves on delivered order completion |
| `engine/engine_regression_test.go` | `TestRegression_CancelEmptyEdgeUUID` | Regression - cancel on empty edge with no vendor UUID |
| `engine/engine_regression_test.go` | `TestRegression_MultiBinMovesOnDelivered` | Regression - multiple bins moved on delivered order completion |
| `engine/engine_regression_test.go` | `TestRegression_CompletionIdempotentAfterDelivery` | Regression - completion handler idempotency |
| `dispatch/reshuffle_test.go` | `TestHandleChildOrderFailure_InFlightSibling` | Design-doc scenario TC-14b - orphaned sibling detection |
| `service/bin_manifest_test.go` | `TestBinManifestService_ClaimForDispatch_ConcurrentRace` | Real race condition in claim path |
| `engine/engine_concurrent_test.go` | `TestConcurrent_ClaimRaceDeterministic` | Concurrent claim race behavioral scenario |
| `engine/engine_concurrent_test.go` | `TestConcurrent_DispatchStress` | Stress test - deadlock detection under concurrency |
| `engine/engine_concurrent_test.go` | `TestRedirect_MidTransit` | Mid-transit redirect behavioral scenario |
| `engine/engine_concurrent_test.go` | `TestFulfillmentScanner_QueueToDispatch` | Queue-to-dispatch promotion behavioral scenario |
| `engine/engine_complex_test.go` | `TestComplexOrder_FleetFailureMidTransit` | Fleet failure during complex order transit |
| `engine/engine_compound_test.go` | `TestCompound_TwoRobotSwap_FullLifecycle` | Full two-robot swap lifecycle |

## Statistics

Counts are approximate — the test suite changes faster than this catalog does.
Rebuild with `grep -r '^func Test' shingo-core/` when a precise number matters.

| Metric | Count |
|--------|-------|
| Total test files | 37 (35 + engine_testhelpers_test.go + engine_regression_test.go split from engine_test.go) |
| Total test functions (excl. helpers) | ~262 |
| Docker-gated files (``//go:build docker``) | 39 across core and `shingo-edge/store/outbox_test.go` |
| Tag-free files (fake-backed) | `material/`, `fulfillment/`, `dispatch/binresolver/` |
| Functions with TC comments (``// TC-N:``) | 31 |
| Functions with TC in name (``TestTC##_*``) | 29 |
| Functions in catalog with TC ID | 70 |
| Functions in catalog without TC ID | 192 |
| TC numbering gaps (1-95 range) | 30 |
| Behavioral coverage gaps | 7 |
