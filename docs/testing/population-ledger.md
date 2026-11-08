# Test population ledger

One row per removed, folded or rewritten test. Codes: STRENGTHEN, STALE, TIMING, COST, DEAD, DUP, FOLD
(definitions: the gate-speed brief §3.3). `scripts/check-test-ledger.sh` fails the gate when a `func Test`
present at the merge base is absent at the tip without a row here.

## Rows

### STRENGTHEN — `TestDoorWalk_EveryWriteToAStationViewIsAnnounced`
- test: `TestDoorWalk_EveryWriteToAStationViewIsAnnounced` · pkg `shingoedge/cmd/shingoedge` · run `-run '^TestDoorWalk_EveryWriteToAStationViewIsAnnounced$'` · `shingo-edge/cmd/shingoedge/door_walk_test.go:128` @ `22c81558` · tags `none`
- code: STRENGTHEN
- assertion map: every old assertion kept, unchanged (status `door_walk_test.go:233-235` → `:270-272`; moved vs fixture `:248-250` → `:285-287`; judge `:360-380` → same function). Added: the read after each door quotes the ETag read before it, as `operator.js:180` does; a view the door did not move must answer 304 with that ETag and no body (`door_walk_test.go:326-330`), one it moved 200 with a new ETag (`:331-335`). Before, the walk compared the ETags of two plain GETs and never sent `If-None-Match`, so a populated board's 304 through the real router was asserted only by `TestIdleCost_TwentyTwoHomeBoard`.
- mutation: `writeConditional` answers every conditional GET with the body:
  ```diff
  --- a/shingo-edge/www/handlers_operator_stations.go
  +++ b/shingo-edge/www/handlers_operator_stations.go
  @@ -138,10 +138,6 @@ func writeConditional(w http.ResponseWriter, r *http.Request, body []byte) {
   	w.Header().Set("Cache-Control", "no-cache")
  -	if r.Header.Get("If-None-Match") == etag {
  -		w.WriteHeader(http.StatusNotModified)
  -		return
  -	}
   	w.Header().Set("Content-Type", "application/json")
  ```
  · old test at base: `go test -count=1 -tags lockcheck -run '^TestDoorWalk_EveryWriteToAStationViewIsAnnounced$' ./cmd/shingoedge/` → `--- PASS: TestDoorWalk_EveryWriteToAStationViewIsAnnounced (61.31s)` (not in its scope) · rewritten red at tip: same command → `door_walk_test.go:204: view 1 answered 200 and a full body to If-None-Match of its own ETag "W/\"466649ab5a41db53\""` (219 such lines), `--- FAIL: TestDoorWalk_EveryWriteToAStationViewIsAnnounced (11.89s)`
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: none
- verified at: `f4ea3b2c`

### STRENGTHEN — `TestRunningCost_ABoardRebuildsOnlyForItsOrders`
- test: `TestRunningCost_ABoardRebuildsOnlyForItsOrders` · pkg `shingoedge/cmd/shingoedge` · run `-run '^TestRunningCost_ABoardRebuildsOnlyForItsOrders$'` · `shingo-edge/cmd/shingoedge/running_cost_test.go:44` @ `22c81558` · tags `none`
- code: STRENGTHEN
- assertion map: every old assertion kept, unchanged (rebuilt iff the board draws the order `running_cost_test.go:134-136` → `:142-144`; first view 200 `:194-196` → `:204-206`). Added: the conditional view GET whose statements price a rebuild must answer 304 with at least one statement (`running_cost_test.go:97-103`); the header said it measured "one 304 view GET" and nothing checked the status. This is `TestIdleCost_TwentyTwoHomeBoard`'s 304 check (`idle_cost_test.go:87-89`), on the same 22-home board, router and counting store.
- mutation: the `writeConditional` diff above · old test at base: `go test -count=1 -tags lockcheck -run '^TestRunningCost_ABoardRebuildsOnlyForItsOrders$' ./cmd/shingoedge/` → `--- PASS: TestRunningCost_ABoardRebuildsOnlyForItsOrders (8.79s)` (not in its scope) · rewritten red at tip: same command → `running_cost_test.go:102: the press board's conditional view answered 200 with 18 statements: the cost measures no 304`, `--- FAIL: TestRunningCost_ABoardRebuildsOnlyForItsOrders (0.59s)`
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: none
- verified at: `f4ea3b2c`

### COST — `TestIdleCost_TwentyTwoHomeBoard`
- test: `TestIdleCost_TwentyTwoHomeBoard` · pkg `shingoedge/cmd/shingoedge` · run `-run '^TestIdleCost_TwentyTwoHomeBoard$'` · `shingo-edge/cmd/shingoedge/idle_cost_test.go:34` @ `22c81558` · tags `none`
- code: COST
- assertion map: first view answers 200 (`idle_cost_test.go:58-61`) → `running_cost_test.go:204-206` (same board) and `door_walk_test.go:331-337`; the idle poll answers 304 with statements > 0 (`:87-89`) → `running_cost_test.go:97-103` (same 22-home board) and `door_walk_test.go:326-330` (press and loader boards, after every door that moves nothing); Touch returns no error (`:93-95`) → `door_walk_test.go:682-705` (`touchedByStream`: the station is online and seen once its stream connects; the router's toucher discards Touch's error, so its effect is what can be asserted); the station stream sends a connected frame (`:146`) → `door_walk_test.go:669-672`. The per-hour statement and byte figures were logged, never asserted: dropped, numbers to the evidence file. `twentyTwoHomeBoard` moved verbatim to `running_cost_test.go:252`, its one remaining caller.
- mutation: the `writeConditional` diff above · old test red at base: `go test -count=1 -tags lockcheck -run '^TestIdleCost_TwentyTwoHomeBoard$' ./cmd/shingoedge/` → `idle_cost_test.go:88: the idle poll answered 200 with 19 statements: the measurement measured nothing` · survivors red at tip: the two STRENGTHEN rows above
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: none
- verified at: `f4ea3b2c`

### DUP — `TestFeedsPin_HeartbeaterCountsActiveOrders`
- test: `TestFeedsPin_HeartbeaterCountsActiveOrders` · pkg `shingoedge/cmd/shingoedge` · run `-run '^TestFeedsPin_HeartbeaterCountsActiveOrders$'` · `shingo-edge/cmd/shingoedge/main_feedspin_test.go:160` @ `22c81558` · tags `none`
- code: DUP
- assertion map: main.go's `messaging.NewHeartbeater` call has 5 arguments (`main_feedspin_test.go:179`) → the compiler: the constructor takes 5 (`messaging/heartbeat.go:83`) and its seven test callers pass 5 (`messaging/heartbeat_feedspin_test.go:51,70,111`, `messaging/heartbeat_test.go:18,43,64,92`); the last argument reads no `db.CountActiveOrders` (`:180-185`) → the compiler (the fifth parameter is `ordersTopic string`) and, for the per-beat count it stands for, `messaging/heartbeat_feedspin_test.go:79-85` (the body carries no `active_orders`); no call found (`:192-194`) dropped because `hb` is used at `main.go:295-343` and passed to `buildInboundRouter`, so a missing construction does not compile, and a renamed constructor is not a regression.
- mutation: X5's order count put back (constructor, call site, wire field):
  ```diff
  --- a/protocol/payloads.go
  +++ b/protocol/payloads.go
   type EdgeHeartbeat struct {
   	StationID string `json:"station_id"`
  +	Orders    int    `json:"active_orders"`
  --- a/shingo-edge/messaging/heartbeat.go
  +++ b/shingo-edge/messaging/heartbeat.go
  +// ActiveOrderCountFunc returns the number of active (non-terminal) orders.
  +type ActiveOrderCountFunc func() int
  +
   type Heartbeater struct {
  +	orderCountFn ActiveOrderCountFunc
  -func NewHeartbeater(client *Client, stationID, version, instance string, ordersTopic string) *Heartbeater {
  +func NewHeartbeater(client *Client, stationID, version, instance string, ordersTopic string, orderCountFn ActiveOrderCountFunc) *Heartbeater {
  +		orderCountFn: orderCountFn,
   	b := &protocol.EdgeHeartbeat{StationID: h.stationID}
  +	if h.orderCountFn != nil {
  +		b.Orders = h.orderCountFn()
  +	}
  --- a/shingo-edge/cmd/shingoedge/main.go
  +++ b/shingo-edge/cmd/shingoedge/main.go
  -	hb := messaging.NewHeartbeater(msgClient, stationID, Version, instanceID, cfg.Messaging.OrdersTopic)
  +	hb := messaging.NewHeartbeater(msgClient, stationID, Version, instanceID, cfg.Messaging.OrdersTopic, func() int {
  +		return db.CountActiveOrders()
  +	})
  ```
  · old test red at base: `go test -count=1 -tags lockcheck -run '^TestFeedsPin_HeartbeaterCountsActiveOrders$' ./cmd/shingoedge/` → `main_feedspin_test.go:189: NewHeartbeater argument count = 6, want 5 (after X5: 5)` and `main_feedspin_test.go:189: last argument reads db.CountActiveOrders = true, want false (after X5: false)` · survivor red at tip, arity arm: `go test -count=1 -tags lockcheck -run '^TestFeedsPin_HeartbeatCountsActiveOrdersEveryBeat$' ./messaging/` → `messaging\heartbeat_feedspin_test.go:51:62: not enough arguments in call to NewHeartbeater` (have 5, want 6); with those callers given `nil` for the new parameter → `heartbeat_feedspin_test.go:89: active_orders on the heartbeat body = true, want false (after X5 (was 1 order count per body): false)` and `--- FAIL: TestFeedsPin_HeartbeatBodyBytes`. Last-argument arm at arity 5 (the closure in place of `cfg.Messaging.OrdersTopic`): `go build ./cmd/shingoedge/` → `cmd\shingoedge\main.go:294:76: cannot use func() int {…} (value of type func() int) as string value in argument to messaging.NewHeartbeater`
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: none
- verified at: `5d73ed29`

### STALE — `TestFeedsPin_SubjectClosureSteps`
- test: `TestFeedsPin_SubjectClosureSteps` · pkg `shingoedge/cmd/shingoedge` · run `-run '^TestFeedsPin_(SubjectClosureSteps|AckDoesNotReconcile|PublishOnRegister)$'` · `shingo-edge/cmd/shingoedge/main_feedspin_test.go:68` @ `22c81558` · tags `none` (the file header, shared with `TestFeedsPin_AckDoesNotReconcile` and `TestFeedsPin_PublishOnRegister`)
- code: STALE
- assertion map: no assertion change. The header (`main_feedspin_test.go:12-15`) said the closures sit inside a function that needs a live Kafka client, "so there is no seam to drive them". They are built by `buildInboundRouter` (`main.go:1112`, from "feat(www,edge): the boards are told, not polled, rebuild only what changed, and say what they know and how old it is (floor-visible)"), which needs no Kafka client, and the door walk dispatches every inbound key through it. The new header says so, says why the call lists stay source-shape pins (the walk asserts the board effect, not the calls), and says where the deleted heartbeater pin's assertions live.
- mutation: no assertion change; the header diff is the change
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: none
- verified at: `5d73ed29`
### TIMING — `TestReleaseU1_TheAskAddsNoWaitToTheRelease`
- test: `TestReleaseU1_TheAskAddsNoWaitToTheRelease` · pkg `shingoedge/engine` · run `-run '^TestReleaseU1_TheAskAddsNoWaitToTheRelease$'` · `shingo-edge/engine/unloader_ask_pending_test.go:79` @ `22c81558` · tags `none`
- code: TIMING
- assertion map:
  - `unloader_ask_pending_test.go:92-95` (median of 5 releases with Core silent, minus the median of 5 with no U1, under AskWait/2) → `unloader_ask_pending_test.go:87-101`: one release with Core silent and the ask's wait on a stopped `clock.Manual`; it must return. Any positive wait on the ask never runs out there, so the bound is now "no wait at all", where it was "under 1 s".
  - `:39-41` (each release's outcome string) → `:32-37` `wantReleased`, the same string, at `:93`.
  - added witness `:102-111`: the U1's `loader.retrieve` ask is on the outbox, Core answered none, the window shows it pending (the old form passed with no U1 asked at all).
  - `:90-91` `t.Logf` of the three medians: dropped because it asserted nothing; the Core-answering arm and the no-U1 baseline existed only to feed it and the subtraction. Base medians (solo, N=3): no U1 25.6/26.1/25.9 ms; Core silent 18.2/18.2/22.0 ms; Core answering 22.5/25.3/34.1 ms.
- mutation: `shingo-edge/engine/operator_demand_unloader.go`, `askUnloaderFulls`:
  ```diff
  -	a, err := e.AskLate(protocol.AskLoaderRetrieve, req, 0, func(a *protocol.EdgeAnswer) {
  +	a, err := e.AskLate(protocol.AskLoaderRetrieve, req, AskWait*3/4, func(a *protocol.EdgeAnswer) {
  ```
  · old test red at base: `go test -count=1 -tags lockcheck -run '^TestReleaseU1_TheAskAddsNoWaitToTheRelease$' ./engine/` → `unloader_ask_pending_test.go:93: release with Core not answering took 1.5184054s, 1.4942143s over the baseline 24.1911ms, want under 1s more: the U1's ask must not be waited for`
  · survivor red at tip: same command → `unloader_ask_pending_test.go:100: the release did not return within 30s with Core not answering its U1: it waits on the ask`
  · with `AskWait/4` in place of `AskWait*3/4`: old test GREEN at base (`median ... Core not answering 519.4563ms`, `--- PASS`), survivor red at tip (the same `:100` line). With `e.askUnloaderFulls(loader, window, served)` removed: survivor red at tip (`:103: no ask on the outbox`).
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: none (`git grep -n 'TheAskAddsNoWaitToTheRelease\|timedRelease'` over the five modules: the test's own file only)
- verified at: `cb19739f` with this commit's test diff applied

### STRENGTHEN — `TestRegression_RemovalOrderClearsBinPointer`
- test: `TestRegression_RemovalOrderClearsBinPointer` · pkg `shingoedge/engine` · run `-run '^TestRegression_RemovalOrderClearsBinPointer$'` · `shingo-edge/engine/uop_regression_test.go:24` @ `22c81558` · tags `none`
- code: STRENGTHEN
- assertion map:
  - `uop_regression_test.go:55-60` (the test nulled `active_bin_id` itself, "simulate pickup clear", then asserted it nil) → `uop_regression_test.go:49-53`: `HandleBinPickedUp` at the line's slot for the bound bin, then `ActiveBinID == nil`. The old comment's reason for not driving it ("needs inventoryDelta wired, which test contexts don't") was false: `testEngine` wires `fakeDeltaSink{db}` (`produce_swap_test.go:130`).
  - the same `:58-60` read after the completion → `:55-59`: still nil after `emitOrderCompleted` (a removal binds nothing).
  - fixture `seedReconcilerNode` (named for the reconciler deleted in "refactor: flip bin UOP authority to Edge, delete reconciler"; its only user) → `seedConsumeNode`, the same graph; `test_node_seed_test.go` deleted.
- mutation: `shingo-edge/engine/handler_bin_picked_up.go:230`, the leave at pickup:
  ```diff
  -	if bound {
  +	if false && bound {
  ```
  · old test GREEN at base: `go test -count=1 -tags lockcheck -run '^TestRegression_RemovalOrderClearsBinPointer$' ./engine/` → `ok  shingoedge/engine` (it set the pointer it asserted)
  · survivor red at tip: same command → `uop_regression_test.go:52: after the pickup ActiveBinID = 99, want nil (the bound bin left the slot)`
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: `shingo-edge/engine/uop_regression_test.go` T3's header cited the phantom `TestRegression_RemovalOrderClearsBinAndZeroesUOP`; it now cites this test, and the phantom's allow-list entry is dropped from `shingo-edge/engine/release_census_test.go` `phantomCitations`
- verified at: `004c6f8e` with this commit's test diff applied

### DUP — `TestRegression_11_DeliveryOrderStillResetsLineUOP`
- test: `TestRegression_11_DeliveryOrderStillResetsLineUOP` · pkg `shingoedge/engine` · run `-run '^TestRegression_11_DeliveryOrderStillResetsLineUOP$'` · `shingo-edge/engine/uop_regression_test.go:69` @ `22c81558` · tags `none`
- code: DUP
- assertion map: `uop_regression_test.go:106-110` (`RemainingUOPCached == 200` after a delivery of bin 202 to the line, Core's count 200 on the landing) → `uop_regression_test.go:104-107` @ tip in `TestRegression_DeliveryResetsToCapacityWithBin`: the same assertion, verbatim, on the same fixture shape (`seedConsumeNode` cap 200, runtime 50, complex order with steps to the node, confirmed, bin set, `emitOrderCompletedWithBinUOP(..., 200)`), which also asserts the bind (`:108-111`). The header's "positive counterpart" reason moves to the survivor's header; its `binArrivingAt`/`resolveReplenishUOP` lines (both deleted in "refactor(uop): bind runtime cache to release-click + delivered, not confirm") go with it.
- mutation: `shingo-edge/engine/handler_bin_landed.go:50`, the landing's count:
  ```diff
  -		BinID: l.BinID, Epoch: l.Epoch, Count: l.Count, PlacedSeq: &seq,
  +		BinID: l.BinID, Epoch: l.Epoch, Count: 0, PlacedSeq: &seq,
  ```
  · removed test red at base: `go test -count=1 -tags lockcheck -run '^TestRegression_11_DeliveryOrderStillResetsLineUOP$' ./engine/` → `uop_regression_test.go:108: RemainingUOP = 0, want 200 (the count Core stamped on the delivery envelope)`
  · survivor red at tip: `go test -count=1 -tags lockcheck -run '^TestRegression_DeliveryResetsToCapacityWithBin$' ./engine/` → `uop_regression_test.go:105: RemainingUOP = 0, want 200 (the count Core stamped on the delivery envelope)`
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: none (`git grep -n 'TestRegression_11_DeliveryOrderStillResetsLineUOP'` over the five modules: its own file only)
- verified at: `004c6f8e` with this commit's test diff applied

### STALE — `TestRegression_DeliveryResetsToCapacityWithBin`
- test: `TestRegression_DeliveryResetsToCapacityWithBin` · pkg `shingoedge/engine` · run `-run '^TestRegression_DeliveryResetsToCapacityWithBin$'` · `shingo-edge/engine/uop_regression_test.go:122` @ `22c81558` · tags `none`
- code: STALE
- assertion map: no assertion change. Header: "delivery completion ... seats the count Core stamped on the envelope" → the landing seats it (`emitOrderCompletedWithBinUOP` sends landing, delivered, completion); "Companion to TestRegression_RemovalOrderClearsBinAndZeroesUOP" (no such test) → `TestRegression_RemovalOrderClearsBinPointer`, plus the removed DUP's reason (a removal filter that swallowed deliveries).
- mutation: the DUP row's `handler_bin_landed.go:50` `Count: 0` · survivor red at tip → `uop_regression_test.go:105: RemainingUOP = 0, want 200 (the count Core stamped on the delivery envelope)`
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: the phantom citation above (`release_census_test.go` `phantomCitations` entry dropped)
- verified at: `004c6f8e` with this commit's test diff applied

### DUP — `TestWaitKindStation_MatchesCore`
- test: `TestWaitKindStation_MatchesCore` · pkg `shingoedge/orders` · run `-run '^TestWaitKindStation_MatchesCore$'` · `shingo-edge/orders/waitkind_test.go:11` @ `22c81558` · tags `none`
- code: DUP
- assertion map: `orders/waitkind_test.go:13-15` (`orders.WaitKindStation == "station"`) → `protocol/waitkind_test.go:11-14` `TestWaitKindStation_TheWireValue` (`protocol.WaitKindStation == "station"`). Its premise ("Edge cannot import Core, so the literal here is the contract") is gone: `orders.WaitKindStation = protocol.WaitKindStation` (`orders/types.go:99`), as is Core's (`dispatch/complex.go:21` → `dispatch/plan/legs.go:125` → `protocol/waitkind.go:11`). Nothing in protocol pinned the value before this row (`TestWaitKindsPartition` reads the constant). The file held only this test and is deleted.
- mutation: `protocol/waitkind.go:11`:
  ```diff
  -const WaitKindStation = "station"
  +const WaitKindStation = "stations"
  ```
  · removed test red at base: `go test -count=1 -tags lockcheck -run '^TestWaitKindStation_MatchesCore$' ./orders/` (shingo-edge) → `waitkind_test.go:14: orders.WaitKindStation = "stations", want "station" (dispatch.WaitKindStation)`
  · survivor red at tip: `go test -count=1 -run '^TestWaitKindStation_TheWireValue$' .` (protocol) → `waitkind_test.go:13: WaitKindStation = "stations", want "station": the value crosses the wire between an Edge and a Core of other builds`
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: `shingo-edge/engine/material_orders.go:50-52` (cited `TestWaitKindStation_MatchesCore`; now `TestWaitKindStation_TheWireValue`, and "mirrors Core's dispatch.WaitKindStation" corrected to the protocol alias); `shingo-edge/orders/types.go:95-98` (same correction)
- verified at: `b47a96c7` with this commit's diff applied

### DUP — `TestWaitKindStation_MatchesCore`
- test: `TestWaitKindStation_MatchesCore` · pkg `shingoedge/engine` · run `-run '^TestWaitKindStation_MatchesCore$'` · `shingo-edge/engine/material_orders_invariant_test.go:327` @ `22c81558` · tags `none`
- code: DUP
- assertion map: `engine/material_orders_invariant_test.go:329-332` (`waitKindStation == "station"`; `waitKindStation = orders.WaitKindStation`, `material_orders.go:53` at the tip) → `protocol/waitkind_test.go:11-14` `TestWaitKindStation_TheWireValue`. Its premise ("Edge cannot import Core, so the value is duplicated") is false at the tip: one definition, `protocol/waitkind.go:11`, and `TestWaitOwnershipIsDeclaredOnce` forbids a second spelling in either module.
- mutation: the row above's `protocol/waitkind.go:11` `"stations"` · removed test red at base: `go test -count=1 -tags lockcheck -run '^TestWaitKindStation_MatchesCore$' ./engine/` (shingo-edge) → `material_orders_invariant_test.go:330: waitKindStation = "stations", want "station" — this must equal dispatch.WaitKindStation, which is what Core's release fence and its population partition read` · survivor red at tip: as above → `waitkind_test.go:13: WaitKindStation = "stations", want "station": ...`
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: as the row above (`git grep -n 'TestWaitKindStation_MatchesCore'` over the five modules is empty at the tip)
- verified at: `b47a96c7` with this commit's diff applied

### STRENGTHEN — `TestHandlePayloadCatalog_StoresCATID`
- test: `TestHandlePayloadCatalog_StoresCATID` · pkg `shingoedge/engine` · run `-run '^TestHandlePayloadCatalog_StoresCATID$'` · `shingo-edge/engine/catid_pin_test.go:61` @ `22c81558` · tags `none`
- code: STRENGTHEN
- assertion map: `catid_pin_test.go:66-70` (a row written by the test's own `putCatalog` reads back its CATID; `HandlePayloadCatalog` never called, despite the name and doc) → `catid_pin_test.go:69-80`: `Engine.HandlePayloadCatalog` with two payloads, one replacing an older CATID on an existing row and one new; each row's CATID is Core's afterwards. The store's own read-back stays pinned by `store/payload_catalog_sync_test.go` `TestSyncPayloadCatalog_OneTxConditionalUpsertAndPrune`.
- mutation: `shingo-edge/engine/engine.go:821`, `HandlePayloadCatalog`'s row build:
  ```diff
   			UOPCapacity: b.UOPCapacity,
  -			CATID:       b.CATID,
   			Manifest:    b.Manifest,
  ```
  · old test GREEN at base, and so is every engine test matching `-run 'CATID|Catid|Catalog'` (28 pass): `go test -count=1 -tags lockcheck -run 'CATID|Catid|Catalog' ./engine/` → `ok  shingoedge/engine`
  · survivor red at tip: `go test -count=1 -tags lockcheck -run '^TestHandlePayloadCatalog_StoresCATID$' ./engine/` → `catid_pin_test.go:78: catalog PL-7 CATID = "" after the sync, want "40012345" (Core's)`
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: none (`git grep -n 'TestHandlePayloadCatalog_StoresCATID'` over the five modules: its own file only)
- verified at: `d36d1a07` with this commit's test diff applied

### STALE — `TestReleaseOrderWithLineside_FallthroughLogShape_IncludesOrderAndDisposition`
- test: `TestReleaseOrderWithLineside_FallthroughLogShape_IncludesOrderAndDisposition` · pkg `shingoedge/engine` · run `-run '^TestReleaseOrderWithLineside_FallthroughLogShape_IncludesOrderAndDisposition$'` · `shingo-edge/engine/operator_release_fallthrough_test.go:212` @ `22c81558` · tags `none`
- code: STALE
- assertion map: no assertion change. The file header (`:3-11`) said `ReleaseOrderWithLineside` "has four code paths" that skip the manifest sync; at the tip it is a wrapper over `runAct` (`release_doors.go:80-86`) and the skips are in `Engine.applyLeg` (`release_commit.go:92,122`), `Engine.releaseOrderWithFullLineside` (`:254`) and `release.PlanLeg` (`release/plan.go:135`). The header now names those. The test's doc (`:207-211`) named `operator_release.go`, which does not exist; it now names `Engine.applyLeg`.
- mutation: `shingo-edge/engine/release_commit.go:92`:
  ```diff
  -		e.logRelease("order=%d disposition=%q — skipping manifest sync: no_process_node",
  +		e.logRelease("order=%d mode=%q — skipping manifest sync: no_process_node",
  ```
  · survivor red at tip: `go test -count=1 -tags lockcheck -run '^TestReleaseOrderWithLineside_FallthroughLogShape_IncludesOrderAndDisposition$' ./engine/` → `operator_release_fallthrough_test.go:243: log line missing disposition= field: "order=1 mode=\"capture_lineside\" — skipping manifest sync: no_process_node"`
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: none
- verified at: `e217c1ff` with this commit's test diff applied

### STALE — `TestReleaseOrderWithLineside_NoActiveClaim_LogsSkip`
- test: `TestReleaseOrderWithLineside_NoActiveClaim_LogsSkip` · pkg `shingoedge/engine` · run `-run '^TestReleaseOrderWithLineside_NoActiveClaim_LogsSkip$'` · `shingo-edge/engine/operator_release_fallthrough_test.go:155` @ `22c81558` · tags `none`
- code: STALE
- assertion map: no assertion change. `:197` called the `toClaim is nil` line the "existing log line shape (pre-cleanup)"; the string is in no `engine/*.go` at the tip (a grep scoped to `engine/` reads the test as dead), it is written by `release.PlanLeg` (`release/plan.go:135`), which the comment now says.
- mutation: `shingo-edge/release/plan.go:135`, `toClaim is nil (runtime` → `toClaim is missing (runtime` · survivor red at tip: `go test -count=1 -tags lockcheck -run '^TestReleaseOrderWithLineside_NoActiveClaim_LogsSkip$' ./engine/` → `operator_release_fallthrough_test.go:206: toClaim==nil log line not found.`
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: none
- verified at: `e217c1ff` with this commit's test diff applied

### FOLD — `TestFindCMSBoundary_StoreErrorPropagates`
- test: `TestFindCMSBoundary_StoreErrorPropagates` · pkg `shingocore/engine` · run `-run '^TestFindCMSBoundary_StoreErrorPropagates$'` · `shingo-core/engine/cms_transactions_test.go:135` @ `22c81558` · tags `docker`
- code: FOLD
- assertion map: the test called `material.FindCMSBoundary` (the `*Engine` method it once covered was deleted in "feat(cms): a bin crossing a storeroom boundary reaches the ledger"; the engine package holds no boundary walk) against real Postgres for node 99999. Split by what each half pins:
  - `cms_transactions_test.go:141-144` (`err != nil` for a missing node) → `shingo-core/store/material_store_docker_test.go:37-40,52-54`, case `FindCMSBoundary_StoreErrorPropagates` of `TestMaterialStore_AMissingRowIsAnError`: the real `*store.DB`, through `material.Store`, answers `GetNode` of a missing id with an error and no node. That is the assumption `material`'s fake encodes (`material/fake_store_test.go` `GetNode`), and it was checked nowhere else at the `*store.DB` layer (the mutation below leaves `store/nodes` `TestNodeCRUD` and `TestNodeGetByName` green).
  - `:145-147` (nil node and empty code alongside the error) → `material/material_test.go:191-202` `TestFindCMSBoundary_StoreErrorPropagates` (same name, fake store, untagged): the walk propagates the store's error with a nil node and an empty code.
  - The table's other cases (`GetBin`, `GetPayloadByCode` with `sql.ErrNoRows`) are the fake's other missing-row answers, same fixture (one test DB), tag (`docker`) and shape. The scar comment moves verbatim above the case.
- mutation: `shingo-core/store/nodes.go`, `(*DB).GetNode`:
  ```diff
  -func (db *DB) GetNode(id int64) (*nodes.Node, error) { return nodes.Get(db.DB, id) }
  +func (db *DB) GetNode(id int64) (*nodes.Node, error) {
  +	n, err := nodes.Get(db.DB, id)
  +	if err == sql.ErrNoRows {
  +		return &nodes.Node{ID: id}, nil
  +	}
  +	return n, err
  +}
  ```
  · removed test red at base: `go test -count=1 -tags docker,lockcheck -run '^TestFindCMSBoundary_StoreErrorPropagates$' ./engine/` (shingo-core) → `cms_transactions_test.go:142: expected error for missing node, got nil (node <nil>)`
  · survivor red at tip: `go test -count=1 -tags docker,lockcheck -run '^TestMaterialStore_AMissingRowIsAnError$' ./store/` → `material_store_docker_test.go:53: a missing row: found=true err=<nil>, want no value and an error` (subtest `FindCMSBoundary_StoreErrorPropagates`)
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: none needed. `git grep -n 'FindCMSBoundary_StoreErrorPropagates'` over the five modules at the tip: `shingo-core/material/material_test.go:186,191` (the material test of the same name, which stays) and the moved scar comment, which now resolves to that test.
- verified at: `37cf21bb` with this commit's diff applied

### STALE — `TestGateChoreo_RetrieveOwnDigLegIsNotParked`
- test: `TestGateChoreo_RetrieveOwnDigLegIsNotParked` · pkg `shingocore/dispatch` · run `-run '^TestGateChoreo_RetrieveOwnDigLegIsNotParked$'` · `shingo-core/dispatch/lane_gate_retrieve_docker_test.go:254` @ `22c81558` · tags `docker`
- code: STALE
- assertion map: no assertion change. The MUTATION note (`:249-253`) named `laneOwnerFor`, which is no longer on this path: admission's foreign-dig arm asks `ownsDig`, which resolves through `digAskerFor`. The note now names `digAskerFor` and says the `laneOwnerFor` hop is not on the path.
- mutation: the note's own mutation at base, `lane_gate.go:922` `return *o.ParentOrderID, true` → `return orderID, true`: `ok shingocore/dispatch 6.122s` (green — the note was false) · re-derived, `lane_gate.go:760` `owner = *o.ParentOrderID` → `_ = owner`: `lane_gate_retrieve_docker_test.go:290: append calls = 0, want 1 — the leg is parked at the gate by its own parent's dig (leg 2, parent/dig owner 1, gate-staged=true)` (digAskerFor identical at base and tip)
- coverage Δ: lead (Phase 1 baseline → certification diff)
- citations rewritten: none
- verified at: `8cd5c7ca` + this commit
