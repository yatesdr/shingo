# Test population ledger

One row per removed, folded or rewritten test. Codes: STRENGTHEN, STALE, TIMING, COST, DEAD, DUP, FOLD
(definitions: the gate-speed brief §3.3). `scripts/check-test-ledger.sh` fails the gate when a `func Test`
present at the merge base is absent at the tip without a row here.

## Coverage

Every row's coverage Δ comes from one measurement of the whole branch, not one per row: a profile per tag
set (untagged, `docker,lockcheck`, and `sim` over the sim-only tests) with
`-coverpkg=shingo/protocol/...,shingo/shared/...,shingocore/...,shingoedge/...,shingo/integration/...`, taken
before the first row and again at the tip, compared block by block over the files the branch did not change
(a changed file is compared by its covered statements). The base was profiled twice to see what moves between
two runs of one tree (untagged 1 block, docker 29, sim 3: goroutine and teardown races); a block lost only
within that noise is not counted.

Result: docker 0, sim 0, untagged 12 blocks (14 statements), none from a row below:

- 5 were covered by the edge `www` tests only through rows other tests had left in the database they all
  shared, which a database per test removes: `apiGetHourlyCounts`' sum over every process, a containment
  list loop and a process-service loop. (`TestApiGetHourlyCounts_NoProcessReturnsEmpty` asserts the status
  alone; nothing pins the sum.)
- 7 were reached only because the door walk ran for a minute: `Uptime() > 30`, three ticker cases and the
  PLC SSE backoff past 32 s. The walk now runs for ten seconds.

No assertion read any of the twelve.

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
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `f4ea3b2c`

### STRENGTHEN — `TestRunningCost_ABoardRebuildsOnlyForItsOrders`
- test: `TestRunningCost_ABoardRebuildsOnlyForItsOrders` · pkg `shingoedge/cmd/shingoedge` · run `-run '^TestRunningCost_ABoardRebuildsOnlyForItsOrders$'` · `shingo-edge/cmd/shingoedge/running_cost_test.go:44` @ `22c81558` · tags `none`
- code: STRENGTHEN
- assertion map: every old assertion kept, unchanged (rebuilt iff the board draws the order `running_cost_test.go:134-136` → `:142-144`; first view 200 `:194-196` → `:204-206`). Added: the conditional view GET whose statements price a rebuild must answer 304 with at least one statement (`running_cost_test.go:97-103`); the header said it measured "one 304 view GET" and nothing checked the status. This is `TestIdleCost_TwentyTwoHomeBoard`'s 304 check (`idle_cost_test.go:87-89`), on the same 22-home board, router and counting store.
- mutation: the `writeConditional` diff above · old test at base: `go test -count=1 -tags lockcheck -run '^TestRunningCost_ABoardRebuildsOnlyForItsOrders$' ./cmd/shingoedge/` → `--- PASS: TestRunningCost_ABoardRebuildsOnlyForItsOrders (8.79s)` (not in its scope) · rewritten red at tip: same command → `running_cost_test.go:102: the press board's conditional view answered 200 with 18 statements: the cost measures no 304`, `--- FAIL: TestRunningCost_ABoardRebuildsOnlyForItsOrders (0.59s)`
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `f4ea3b2c`

### COST — `TestIdleCost_TwentyTwoHomeBoard`
- test: `TestIdleCost_TwentyTwoHomeBoard` · pkg `shingoedge/cmd/shingoedge` · run `-run '^TestIdleCost_TwentyTwoHomeBoard$'` · `shingo-edge/cmd/shingoedge/idle_cost_test.go:34` @ `22c81558` · tags `none`
- code: COST
- assertion map: first view answers 200 (`idle_cost_test.go:58-61`) → `running_cost_test.go:204-206` (same board) and `door_walk_test.go:331-337`; the idle poll answers 304 with statements > 0 (`:87-89`) → `running_cost_test.go:97-103` (same 22-home board) and `door_walk_test.go:326-330` (press and loader boards, after every door that moves nothing); Touch returns no error (`:93-95`) → `door_walk_test.go:682-705` (`touchedByStream`: the station is online and seen once its stream connects; the router's toucher discards Touch's error, so its effect is what can be asserted); the station stream sends a connected frame (`:146`) → `door_walk_test.go:669-672`. The per-hour statement and byte figures were logged, never asserted: dropped, numbers to the evidence file. `twentyTwoHomeBoard` moved verbatim to `running_cost_test.go:252`, its one remaining caller.
- mutation: the `writeConditional` diff above · old test red at base: `go test -count=1 -tags lockcheck -run '^TestIdleCost_TwentyTwoHomeBoard$' ./cmd/shingoedge/` → `idle_cost_test.go:88: the idle poll answered 200 with 19 statements: the measurement measured nothing` · survivors red at tip: the two STRENGTHEN rows above
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
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
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `5d73ed29`

### STALE — `TestFeedsPin_SubjectClosureSteps`
- test: `TestFeedsPin_SubjectClosureSteps` · pkg `shingoedge/cmd/shingoedge` · run `-run '^TestFeedsPin_(SubjectClosureSteps|AckDoesNotReconcile|PublishOnRegister)$'` · `shingo-edge/cmd/shingoedge/main_feedspin_test.go:68` @ `22c81558` · tags `none` (the file header, shared with `TestFeedsPin_AckDoesNotReconcile` and `TestFeedsPin_PublishOnRegister`)
- code: STALE
- assertion map: no assertion change. The header (`main_feedspin_test.go:12-15`) said the closures sit inside a function that needs a live Kafka client, "so there is no seam to drive them". They are built by `buildInboundRouter` (`main.go:1112`, from "feat(www,edge): the boards are told, not polled, rebuild only what changed, and say what they know and how old it is (floor-visible)"), which needs no Kafka client, and the door walk dispatches every inbound key through it. The new header says so, says why the call lists stay source-shape pins (the walk asserts the board effect, not the calls), and says where the deleted heartbeater pin's assertions live.
- mutation: no assertion change; the header diff is the change
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
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
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
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
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
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
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none (`git grep -n 'TestRegression_11_DeliveryOrderStillResetsLineUOP'` over the five modules: its own file only)
- verified at: `004c6f8e` with this commit's test diff applied

### STALE — `TestRegression_DeliveryResetsToCapacityWithBin`
- test: `TestRegression_DeliveryResetsToCapacityWithBin` · pkg `shingoedge/engine` · run `-run '^TestRegression_DeliveryResetsToCapacityWithBin$'` · `shingo-edge/engine/uop_regression_test.go:122` @ `22c81558` · tags `none`
- code: STALE
- assertion map: no assertion change. Header: "delivery completion ... seats the count Core stamped on the envelope" → the landing seats it (`emitOrderCompletedWithBinUOP` sends landing, delivered, completion); "Companion to TestRegression_RemovalOrderClearsBinAndZeroesUOP" (no such test) → `TestRegression_RemovalOrderClearsBinPointer`, plus the removed DUP's reason (a removal filter that swallowed deliveries).
- mutation: the DUP row's `handler_bin_landed.go:50` `Count: 0` · survivor red at tip → `uop_regression_test.go:105: RemainingUOP = 0, want 200 (the count Core stamped on the delivery envelope)`
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
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
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: `shingo-edge/engine/material_orders.go:50-52` (cited `TestWaitKindStation_MatchesCore`; now `TestWaitKindStation_TheWireValue`, and "mirrors Core's dispatch.WaitKindStation" corrected to the protocol alias); `shingo-edge/orders/types.go:95-98` (same correction)
- verified at: `b47a96c7` with this commit's diff applied

### DUP — `TestWaitKindStation_MatchesCore`
- test: `TestWaitKindStation_MatchesCore` · pkg `shingoedge/engine` · run `-run '^TestWaitKindStation_MatchesCore$'` · `shingo-edge/engine/material_orders_invariant_test.go:327` @ `22c81558` · tags `none`
- code: DUP
- assertion map: `engine/material_orders_invariant_test.go:329-332` (`waitKindStation == "station"`; `waitKindStation = orders.WaitKindStation`, `material_orders.go:53` at the tip) → `protocol/waitkind_test.go:11-14` `TestWaitKindStation_TheWireValue`. Its premise ("Edge cannot import Core, so the value is duplicated") is false at the tip: one definition, `protocol/waitkind.go:11`, and `TestWaitOwnershipIsDeclaredOnce` forbids a second spelling in either module.
- mutation: the row above's `protocol/waitkind.go:11` `"stations"` · removed test red at base: `go test -count=1 -tags lockcheck -run '^TestWaitKindStation_MatchesCore$' ./engine/` (shingo-edge) → `material_orders_invariant_test.go:330: waitKindStation = "stations", want "station" — this must equal dispatch.WaitKindStation, which is what Core's release fence and its population partition read` · survivor red at tip: as above → `waitkind_test.go:13: WaitKindStation = "stations", want "station": ...`
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
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
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
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
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `e217c1ff` with this commit's test diff applied

### STALE — `TestReleaseOrderWithLineside_NoActiveClaim_LogsSkip`
- test: `TestReleaseOrderWithLineside_NoActiveClaim_LogsSkip` · pkg `shingoedge/engine` · run `-run '^TestReleaseOrderWithLineside_NoActiveClaim_LogsSkip$'` · `shingo-edge/engine/operator_release_fallthrough_test.go:155` @ `22c81558` · tags `none`
- code: STALE
- assertion map: no assertion change. `:197` called the `toClaim is nil` line the "existing log line shape (pre-cleanup)"; the string is in no `engine/*.go` at the tip (a grep scoped to `engine/` reads the test as dead), it is written by `release.PlanLeg` (`release/plan.go:135`), which the comment now says.
- mutation: `shingo-edge/release/plan.go:135`, `toClaim is nil (runtime` → `toClaim is missing (runtime` · survivor red at tip: `go test -count=1 -tags lockcheck -run '^TestReleaseOrderWithLineside_NoActiveClaim_LogsSkip$' ./engine/` → `operator_release_fallthrough_test.go:206: toClaim==nil log line not found.`
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
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
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none needed. `git grep -n 'FindCMSBoundary_StoreErrorPropagates'` over the five modules at the tip: `shingo-core/material/material_test.go:186,191` (the material test of the same name, which stays) and the moved scar comment, which now resolves to that test.
- verified at: `37cf21bb` with this commit's diff applied

### STALE — `TestGateChoreo_RetrieveOwnDigLegIsNotParked`
- test: `TestGateChoreo_RetrieveOwnDigLegIsNotParked` · pkg `shingocore/dispatch` · run `-run '^TestGateChoreo_RetrieveOwnDigLegIsNotParked$'` · `shingo-core/dispatch/lane_gate_retrieve_docker_test.go:254` @ `22c81558` · tags `docker`
- code: STALE
- assertion map: no assertion change. The MUTATION note (`:249-253`) named `laneOwnerFor`, which is no longer on this path: admission's foreign-dig arm asks `ownsDig`, which resolves through `digAskerFor`. The note now names `digAskerFor` and says the `laneOwnerFor` hop is not on the path.
- mutation: the note's own mutation at base, `lane_gate.go:922` `return *o.ParentOrderID, true` → `return orderID, true`: `ok shingocore/dispatch 6.122s` (green — the note was false) · re-derived, `lane_gate.go:760` `owner = *o.ParentOrderID` → `_ = owner`: `lane_gate_retrieve_docker_test.go:290: append calls = 0, want 1 — the leg is parked at the gate by its own parent's dig (leg 2, parent/dig owner 1, gate-staged=true)` (digAskerFor identical at base and tip)
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `8cd5c7ca` + this commit

### FOLD — `TestBinMove_LosingTheBinIsNotAServerError`
- test: `TestBinMove_LosingTheBinIsNotAServerError` · pkg `shingocore/www` · run `-run '^TestBinMove_LosingTheBinIsNotAServerError$'` · `shingo-core/www/handlers_bin_race_test.go:53` @ `22c81558` · tags `docker`
- code: FOLD
- assertion map: same fixture (testHandlersWithSim, SetupStandardData, a bin at storage, an in-transit holder whose hard claim `claimForRobot` writes) and the same request. status != 500 (Fatal, `:70`) → survivor's status == 409 Fatal (`handlers_bin_move_reserved_test.go:125`); status == 409 (`:73`) → same line; message non-empty (`:81`) → survivor's message names `#holder` (`:128`); message names the bin (`:84`) → **added** to the survivor (`:131`); no core-operator row left pending (`:93-97`) → survivor's zero core-operator rows (`:138`, stronger). The scar comment moved verbatim above the survivor; `raceTheBin` had no other caller and went with the file.
- mutation: `engine/bin_move.go:328` RefusedClaimed's sentence drops the label (`"bin %s is already claimed by order #%d", label, r.Holder` → `"the bin is already claimed by order #%d", r.Holder`) · old test red at base: `handlers_bin_race_test.go:85: message "the bin is already claimed by order #1" does not name the bin` · survivor red at tip: `handlers_bin_move_reserved_test.go:131: message "the bin is already claimed by order #1" does not name the bin BIN-CLAIMED` · the survivor as it stood at base stays green under it (`ok shingocore/www 3.884s`): the label was pinned only by the folded test
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none in the five modules (the only mentions were the test's own); the survivor's header names it as replaced
- verified at: `e93355f1` + this commit

### DUP — `TestCitedTestsExist`
- test: `TestCitedTestsExist` · pkg `shingoedge/engine` · run `-run '^TestCitedTestsExist$'` · `shingo-edge/engine/release_census_test.go:343` @ `22c81558` · tags `none`
- code: DUP
- assertion map: every `Test…` named in a shingo-edge comment is a `func Test…` in the repo, `phantomCitations` excepted (`release_census_test.go:392-395`) → `shared/scripts/testcite` `Scan`, run by `scripts/check-test-citations.sh` over all five modules with no allow-list (`shared/scripts/testcite/main_test.go` `TestScan_Fixture`); the 49 `phantomCitations` rows were rewritten to the live test or to a gravestone, so the allow-list is gone with the test. A phantom whose test now exists must leave the list (`:396-401`) → dropped because there is no list. Narrower than the old reading in one place, on purpose: a name the words touching it mark as history ("it was TestX", "TestX was deleted") is a gravestone, not a pin, and is exempt (`TestCitations_Gravestones`); the old test allow-listed such lines by hand.
- mutation: `shingo-edge/domain/flow.go`, a comment citing a test that does not exist:
  ```diff
  +// Pinned by TestFlowMutation_NoSuchTest.
   package domain
  ```
  · old test red at base: `cd shingo-edge && go test -count=1 -run '^TestCitedTestsExist$' ./engine` (a `git archive 22c81558` extract) → `release_census_test.go:394: a comment cites a test that does not exist: TestFlowMutation_NoSuchTest @ domain/flow.go` · survivor red at tip: `bash scripts/check-test-citations.sh` → `shingo-edge/domain/flow.go:1: TestFlowMutation_NoSuchTest is cited but no such test exists`
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none (the name is cited nowhere)
- verified at: `ba1931d9` (working tree)

### DUP — `TestFeedsPin_HeartbeatAndAckTTL`
- test: `TestFeedsPin_HeartbeatAndAckTTL` · pkg `shingo/protocol` · run `-run '^TestFeedsPin_HeartbeatAndAckTTL$'` · `protocol/expiry_feedspin_test.go:18` @ `22c81558` · tags `none`
- code: DUP (the TTL-table arm only; the test stays, rewritten)
- assertion map: `DataTTLFor(subject) == 5m` for heartbeat, ack, register, registered (`expiry_feedspin_test.go:34-36`) → `protocol/protocol_test.go:304-307` (`TestDataTTLForSubjects`, the same four subjects at 5m, asserted `:320-321`); stamped lifetime `ExpiresAt - Timestamp == 5m` (`:42-44`) → kept, `expiry_feedspin_test.go:31-33`; a copy 2 minutes late is dispatched (`:45-47`) → kept, `:34-36`; the `ttlAfter`, `dispatchedLateAft` and `label` columns → dropped because every row's after equals its want (the TTL change landed) and only the failure messages read them. Not a whole-test DUP: the mutation `return clock.Now().UTC().After(hdr.ExpiresAt.Add(-4 * time.Minute))` in `IsExpiredHeader` reds this test alone in protocol (`expiry_feedspin_test.go:46: edge.heartbeat 2m0s late dispatched = false, want true (after F1: true)` at base, `:35` at tip); `TestDataTTLForSubjects` stays green under it.
- mutation:
  ```diff
   func DataTTLFor(subject string) time.Duration {
  +	if subject == SubjectEdgeHeartbeat {
  +		return 90 * time.Second
  +	}
  ```
  · old test red at base: `cd protocol && go test -count=1 .` → `expiry_feedspin_test.go:36: DataTTLFor(edge.heartbeat) = 1m30s, want 5m0s (after F1: 5m0s)` · survivor red at tip: same command → `protocol_test.go:321: DataTTLFor("edge.heartbeat") = 1m30s, want 5m0s` (the rewritten test also goes red: `expiry_feedspin_test.go:32: edge.heartbeat stamped lifetime = 1m30s, want 5m0s`)
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none (header rewritten; the name is cited nowhere else)
- verified at: `d912452e` (working tree)

### DUP — `TestFormatDuration_Ladder`
- test: `TestFormatDuration_Ladder` · pkg `shingo/protocol` · run `-run '^TestFormatDuration_Ladder$'` · `protocol/fault_sentence_test.go:180` @ `22c81558` · tags `none`
- code: DUP
- assertion map: its ten rows (`fault_sentence_test.go:185-194`, asserted `:197-199`) → `protocol/duration_test.go` `TestFormatDuration_TheLadder`: -5s → the -3s floor `:48-49`; 0 → `:24`; 18s → `:27`; 59s → `:28` (59.999s, same band and truncation); 1m → `:29`; 3m12s and 4m07s → `:30` (247s, the zero-padded seconds) and `:31`; 45m → `:33-34` (23m, 59m, same band); 1h30m → `:35-36` (1h 00m, 2h 05m); 25h → `:38-39` (1d 00h, 3d 04h); all asserted `:42-43`.
- mutation: a 13-mutation sweep over `FormatDuration`'s band edges and format strings: every mutation that reds the removed test reds the survivor (9 of 13); the other 4 (`<` → `<=` at 10m, 1h, 24h, and `%02dm` → `%dm`) red only the survivor. Quoted arm:
  ```diff
  -		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
  +		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes()))
  ```
  · old test red at base: `cd protocol && go test -count=1 -run '^(TestFormatDuration_Ladder|TestFormatDuration_TheLadder)$' .` → `fault_sentence_test.go:198: FormatDuration(1h30m0s) = "1h 90m", want "1h 30m"` · survivor red at tip: `go test -count=1 -run '^TestFormatDuration_TheLadder$' .` → `duration_test.go:43: FormatDuration(1h0m0s) = "1h 60m", want "1h 00m"`
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none (the name is cited nowhere)
- verified at: `d912452e` (working tree)

### STALE — `TestIngestor_HeaderDebugLineVsStationFilter_FeedsPin`
- test: `TestIngestor_HeaderDebugLineVsStationFilter_FeedsPin` · pkg `shingo/protocol` · run `-run '^TestIngestor_HeaderDebugLineVsStationFilter_FeedsPin$'` · `protocol/ingestor_debug_feedspin_test.go:18` @ `22c81558` · tags `none`
- code: STALE
- assertion map: debug-line prefixes (`:68`) → the same comparison, `:71`, message trimmed; dispatched (`:71`) → the same, `:74`. The header said the header line is written before the station filter; at the tip `Ingestor.HandleRaw` writes it after.
- mutation: no assertion change; header diff (the `after`/`label` columns had no reader but the messages)
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_HandleEdgeHeartbeat`
- test: `TestFeedsPin_HandleEdgeHeartbeat` · pkg `shingocore/messaging` · run `-run '^TestFeedsPin_HandleEdgeHeartbeat$'` · `shingo-core/messaging/core_data_service_feedspin_test.go:110` @ `22c81558` · tags `docker`
- code: STALE
- assertion map: every comparison kept unchanged (trace, register request, the no-ack branch, ack keys, station_id, timezone, server_ts, registry row); only the `afterTrace`/`afterAckKeys`/`label` fields, identical to the `want*` fields in every row, and the message text that printed them are gone. The doc's "today nothing at all is sent" on the error path is false: the handler acks.
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_HandleNodeListRequest_Scope`
- test: `TestFeedsPin_HandleNodeListRequest_Scope` · pkg `shingocore/messaging` · run `-run '^TestFeedsPin_HandleNodeListRequest_Scope$'` · `shingo-core/messaging/core_data_service_feedspin_test.go:261` @ `22c81558` · tags `docker`
- code: STALE
- assertion map: replies count, event subject, wantExact and wantHas kept unchanged; only the `afterReplies`/`label` fields (identical in every row) and their message text are gone.
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_HandleNodeListRequest_GeometryAndDigest`
- test: `TestFeedsPin_HandleNodeListRequest_GeometryAndDigest` · pkg `shingocore/messaging` · run `-run '^TestFeedsPin_HandleNodeListRequest_GeometryAndDigest$'` · `shingo-core/messaging/core_data_service_feedspin_test.go:344` @ `22c81558` · tags `docker`
- code: STALE
- assertion map: geometry, scene_revision, digest equals `NodesDigest`, digest stable across replies: kept unchanged. The doc's "no digest today" and `protocol.Digest` are replaced by the `NodesDigest(Nodes, Loaders, PayloadBinTypes, Family)` it asserts.
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_HandleCatalogPayloadsRequest`
- test: `TestFeedsPin_HandleCatalogPayloadsRequest` · pkg `shingocore/messaging` · run `-run '^TestFeedsPin_HandleCatalogPayloadsRequest$'` · `shingo-core/messaging/core_data_service_feedspin_test.go:412` @ `22c81558` · tags `docker`
- code: STALE
- assertion map: trace, key set {digest, payloads}, rows and code order kept unchanged; `afterKeys`, identical to `wantKeys`, is gone. The doc's "payloads only" key set is corrected.
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_ListOpenSupplyRefusalsCallers`
- test: `TestFeedsPin_ListOpenSupplyRefusalsCallers` · pkg `shingocore/messaging` · run `-run '^TestFeedsPin_ListOpenSupplyRefusalsCallers$'` · `shingo-core/messaging/supply_refusal_callers_feedspin_test.go:22` @ `22c81558` · tags `none`
- code: STALE
- assertion map: callers == `["../messaging/feeds.go"]` (`:51`) → the same comparison (`:55`), message trimmed; the header now names `buildRefusalsFeed` as the one non-test caller.
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_HandleSupplyRefusal`
- test: `TestFeedsPin_HandleSupplyRefusal` · pkg `shingocore/messaging` · run `-run '^TestFeedsPin_HandleSupplyRefusal$'` · `shingo-core/messaging/supply_refusal_plant_claims_feedspin_test.go:30` @ `22c81558` · tags `docker`
- code: STALE
- assertion map: trace, payload identity, open rows, stored station/refused_by kept unchanged; the `after`/`label` columns and four "(after: same)" message suffixes are gone.
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_HandlePlantClaims`
- test: `TestFeedsPin_HandlePlantClaims` · pkg `shingocore/messaging` · run `-run '^TestFeedsPin_HandlePlantClaims$'` · `shingo-core/messaging/supply_refusal_plant_claims_feedspin_test.go:131` @ `22c81558` · tags `docker`
- code: STALE
- assertion map: P1 mirror, P2 mirror and wantRows kept unchanged; the `after`/`label` columns and the "F5" message suffixes are gone.
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### DUP — `TestFeedsPin_HandlePlantClaims_ReportRecord`
- test: `TestFeedsPin_HandlePlantClaims_ReportRecord` · pkg `shingocore/messaging` · run `-run '^TestFeedsPin_HandlePlantClaims_ReportRecord$'` · `shingo-core/messaging/supply_refusal_plant_claims_feedspin_test.go:242` @ `22c81558` · tags `docker`
- code: DUP
- assertion map (one condition, `:258`): the row exists, once (the Scan at `:256-257`) → `shingo-core/messaging/plant_claims_reports_test.go:58` (`TestHandlePlantClaims_ReportRow` step 0, P1 row `edge.test/d1`; `reportRow` returns "" with no row; more than one cannot happen, `process_id` is the primary key); station == edge.test → `:58`; digest == report.Digest → `:58`; `received_at IS NOT NULL` → dropped because the column is `NOT NULL` and `RecordPlantClaimsReport` writes it on insert and update, so a row that exists has it.
- mutation:
  ```diff
  -	prev, err := s.db.RecordPlantClaimsReport(report.ProcessID, station, report.Digest, clock.Now().UTC())
  +	prev, err := s.db.RecordPlantClaimsReport(report.ProcessID, station, "", clock.Now().UTC())
  ```
  (`shingo-core/messaging/plant_claims_reports.go`) · old test red at base: `cd shingo-core && go test -tags docker,lockcheck -count=1 -run '^TestFeedsPin_HandlePlantClaims_ReportRecord$' ./messaging/` → `supply_refusal_plant_claims_feedspin_test.go:259: report row = (1 rows, station "edge.test", digest "", stamped true), want 1 row from edge.test with digest "0123456789abcdef", stamped (F5)` · survivor red at tip: `-run '^TestHandlePlantClaims_ReportRow$' ./messaging/` → `plant_claims_reports_test.go:59: step 0: P1 row = "edge.test/", want "edge.test/d1"`
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none (the name is cited nowhere)
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_EdgesPageRow`
- test: `TestFeedsPin_EdgesPageRow` · pkg `shingocore/www` · run `-run '^TestFeedsPin_EdgesPageRow$'` · `shingo-core/www/handlers_edges_render_feedspin_test.go:21` @ `22c81558` · tags `docker`
- code: STALE
- assertion map: cells == want (`:52`) → the same (`:49`); no edge-unclaimed (`:55`) → the same (`:52`); `after := want` had no reader but the message.
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_NodeListReplyApply`
- test: `TestFeedsPin_NodeListReplyApply` · pkg `shingoedge/engine` · run `-run '^TestFeedsPin_NodeListReplyApply$'` · `shingo-edge/engine/node_list_apply_feedspin_test.go:81` @ `22c81558` · tags `none`
- code: STALE
- assertion map: unchanged, comments only: `:146`→`:154`, `:164`→`:172`, `:169`→`:177`, `:172`→`:180`, `:175`→`:183`
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_EmptyLoaderSet`
- test: `TestFeedsPin_EmptyLoaderSet` · pkg `shingoedge/engine` · run `-run '^TestFeedsPin_EmptyLoaderSet$'` · `shingo-edge/engine/node_list_apply_feedspin_test.go:185` @ `22c81558` · tags `none`
- code: STALE
- assertion map: unchanged, comments only: `:212`→`:220`
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_CatalogApplyUnchangedWritesNothing`
- test: `TestFeedsPin_CatalogApplyUnchangedWritesNothing` · pkg `shingoedge/engine` · run `-run '^TestFeedsPin_CatalogApplyUnchangedWritesNothing$'` · `shingo-edge/engine/node_list_apply_feedspin_test.go:220` @ `22c81558` · tags `none`
- code: STALE
- assertion map: unchanged, comments only: `:240`→`:248`
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestHandleSupplyRefusalState_Arms_FeedsPin`
- test: `TestHandleSupplyRefusalState_Arms_FeedsPin` · pkg `shingoedge/engine` · run `-run '^TestHandleSupplyRefusalState_Arms_FeedsPin$'` · `shingo-edge/engine/supply_refusal_feedspin_test.go:67` @ `22c81558` · tags `none`
- code: STALE
- assertion map: unchanged, comments only: `:148`→`:157`, `:154`→`:163`. The header's promised GetContainment non-200 pin went with GetContainment; the header now says so.
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestSupplyRefusal_Emit_FeedsPin`
- test: `TestSupplyRefusal_Emit_FeedsPin` · pkg `shingoedge/engine` · run `-run '^TestSupplyRefusal_Emit_FeedsPin$'` · `shingo-edge/engine/supply_refusal_feedspin_test.go:168` @ `22c81558` · tags `none`
- code: STALE
- assertion map: unchanged, comments only: `:206`→`:215`
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestClearOnLoad_FeedsPin`
- test: `TestClearOnLoad_FeedsPin` · pkg `shingoedge/engine` · run `-run '^TestClearOnLoad_FeedsPin$'` · `shingo-edge/engine/supply_refusal_feedspin_test.go:220` @ `22c81558` · tags `none`
- code: STALE
- assertion map: unchanged, comments only: `:270`→`:279`, `:277`→`:286`
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestSupplyRefusal_LostAckNeverReachesLoaderEdge_FeedsPin`
- test: `TestSupplyRefusal_LostAckNeverReachesLoaderEdge_FeedsPin` · pkg `shingoedge/engine` · run `-run '^TestSupplyRefusal_LostAckNeverReachesLoaderEdge_FeedsPin$'` · `shingo-edge/engine/supply_refusal_feedspin_test.go:296` @ `22c81558` · tags `none`
- code: STALE (the doc now says the lost ack heals; the name, which says the opposite, is kept)
- assertion map: unchanged, comments only: `:314`→`:326`, `:337`→`:349`, `:342`→`:354`, `:359`→`:371`
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_HeartbeatBodyBytes`
- test: `TestFeedsPin_HeartbeatBodyBytes` · pkg `shingoedge/messaging` · run `-run '^TestFeedsPin_HeartbeatBodyBytes$'` · `shingo-edge/messaging/heartbeat_feedspin_test.go:19` @ `22c81558` · tags `none`
- code: STALE
- assertion map: `:55`→`:54`, `:58`→`:57` (comparison kept; `after`/`label` columns, equal to want in every row, and their message args dropped); want literal `:31`→`:34`
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_HeartbeatCountsActiveOrdersEveryBeat`
- test: `TestFeedsPin_HeartbeatCountsActiveOrdersEveryBeat` · pkg `shingoedge/messaging` · run `-run '^TestFeedsPin_HeartbeatCountsActiveOrdersEveryBeat$'` · `shingo-edge/messaging/heartbeat_feedspin_test.go:68` @ `22c81558` · tags `none`
- code: STALE
- assertion map: `:82`→`:80`, `:89`→`:87` (comparison kept; `after`/`label` dropped); the active_orders case another lane cites as a survivor moved `:79-85`→`:77-83`
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_NodeAndCatalogRequestBodies`
- test: `TestFeedsPin_NodeAndCatalogRequestBodies` · pkg `shingoedge/messaging` · run `-run '^TestFeedsPin_NodeAndCatalogRequestBodies$'` · `shingo-edge/messaging/heartbeat_feedspin_test.go:97` @ `22c81558` · tags `none`
- code: STALE
- assertion map: `:115`→`:113`, `:118`→`:116` (comparison kept; `after`/`label` dropped). The doc now says the catalog request body is not pinned here, which has been true since the pin was written.
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_HeartbeatLoopReasksEverySecondTick`
- test: `TestFeedsPin_HeartbeatLoopReasksEverySecondTick` · pkg `shingoedge/messaging` · run `-run '^TestFeedsPin_HeartbeatLoopReasksEverySecondTick$'` · `shingo-edge/messaging/heartbeat_feedspin_test.go:130` @ `22c81558` · tags `none`
- code: STALE
- assertion map: `:134`→`:134`, `:160`→`:154` (comparison kept; `after`/`label` dropped)
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none (`messaging/heartbeat_test.go:103` cites it by its unchanged name)
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_PlantClaimsReportBytes`
- test: `TestFeedsPin_PlantClaimsReportBytes` · pkg `shingoedge/messaging` · run `-run '^TestFeedsPin_PlantClaimsReportBytes$'` · `shingo-edge/messaging/plant_claims_feedspin_test.go:35` @ `22c81558` · tags `none`
- code: STALE
- assertion map: `:59`→`:61`, `:64`→`:66`, `:68`→`:70`, `:72`→`:74`, `:75`→`:77` (comparison kept; `label` dropped); the digest comment now names `protocol.ClaimsDigest`
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_PlantClaimsHourlyLoop`
- test: `TestFeedsPin_PlantClaimsHourlyLoop` · pkg `shingoedge/messaging` · run `-run '^TestFeedsPin_PlantClaimsHourlyLoop$'` · `shingo-edge/messaging/plant_claims_feedspin_test.go:88` @ `22c81558` · tags `none`
- code: STALE
- assertion map: `:92`→`:95`, `:107`→`:108` (comparison kept; `after`/`label` dropped)
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestFeedsPin_SourcingSnapshotRewrites`
- test: `TestFeedsPin_SourcingSnapshotRewrites` · pkg `shingoedge/store` · run `-run '^TestFeedsPin_SourcingSnapshotRewrites$'` · `shingo-edge/store/sourcing_state_feedspin_test.go:26` @ `22c81558` · tags `none`
- code: STALE
- assertion map: `:83`→`:82`, `:87`→`:86`, `:90`→`:89`, `:94`→`:93`, `:97`→`:96`, `:101`→`:100`, `:104`→`:103` (comparison kept; `after*`/`label` collapsed into `want*`, "was N" kept as row comments)
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestContainmentJS_FeedsPin`
- test: `TestContainmentJS_FeedsPin` · pkg `shingoedge/www` · run `-run '^TestContainmentJS_FeedsPin$'` · `shingo-edge/www/containment_js_feedspin_test.go:15` @ `22c81558` · tags `none`
- code: STALE
- assertion map: `:23`→`:24` (unchanged; doc only)
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestStatus_Keys_FeedsPin`
- test: `TestStatus_Keys_FeedsPin` · pkg `shingoedge/www` · run `-run '^TestStatus_Keys_FeedsPin$'` · `shingo-edge/www/handler_status_feedspin_test.go:19` @ `22c81558` · tags `none`
- code: STALE
- assertion map: `:55`→`:51`, `:62`→`:56` (comparison kept; the spent prediction prose dropped from the messages)
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestContainmentState_FeedsPin`
- test: `TestContainmentState_FeedsPin` · pkg `shingoedge/www` · run `-run '^TestContainmentState_FeedsPin$'` · `shingo-edge/www/handlers_containment_feedspin_test.go:214` @ `22c81558` · tags `none`
- code: STALE
- assertion map: `:219`→`:228`, `:223`→`:232`, `:260`→`:270` (all 11 rows' got/want kept; `base`/`label` dropped)
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestContainmentPage_FeedsPin`
- test: `TestContainmentPage_FeedsPin` · pkg `shingoedge/www` · run `-run '^TestContainmentPage_FeedsPin$'` · `shingo-edge/www/handlers_containment_feedspin_test.go:268` @ `22c81558` · tags `none`
- code: STALE
- assertion map: `:274`→`:284`, `:295`→`:304`, `:300`→`:309` (needles/want kept; `base`/`label` dropped)
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestContainmentState_CoreDown_FeedsPin`
- test: `TestContainmentState_CoreDown_FeedsPin` · pkg `shingoedge/www` · run `-run '^TestContainmentState_CoreDown_FeedsPin$'` · `shingo-edge/www/handlers_containment_feedspin_test.go:310` @ `22c81558` · tags `none`
- code: STALE (not a DUP: the empty-containment JSON check `:318` and its section check `:322` have no survivor)
- assertion map: `:319`→`:331`, `:323`→`:335`, `:330`→`:342`, `:345`→`:356`, `:348`→`:359`, `:351`→`:362` (conditions kept; base prose dropped from messages)
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestProductionRefreshTriggers_FeedsPin`
- test: `TestProductionRefreshTriggers_FeedsPin` · pkg `shingoedge/www` · run `-run '^TestProductionRefreshTriggers_FeedsPin$'` · `shingo-edge/www/production_refresh_feedspin_test.go:37` @ `22c81558` · tags `none`
- code: STALE (not a DUP of `TestProductionRefresh_OneStream`: the trigger list with throttles, the non-sse triggers, the handler names and the per-page rows have no survivor there)
- assertion map: `:43`→`:44`, `:48`→`:49`, `:70`→`:68`, `:104`→`:99` (comparison kept; `after`/`label` dropped)
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STALE — `TestSSE_DebugLogReachesEveryClient_FeedsPin`
- test: `TestSSE_DebugLogReachesEveryClient_FeedsPin` · pkg `shingoedge/www` · run `-run '^TestSSE_DebugLogReachesEveryClient_FeedsPin$'` · `shingo-edge/www/sse_debuglog_feedspin_test.go:70` @ `22c81558` · tags `none`
- code: STALE (the name, which says the opposite of what it asserts, is kept)
- assertion map: `:85`→`:86`, `:104`→`:103`, `:111`→`:110` (comparison kept; `after`/`label` dropped)
- mutation: no assertion change; header diff
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `d912452e` (working tree)

### STRENGTHEN — `TestScenario_TwoRobotChangeoverRelease_BothLegsOnTheWire`
- test: `TestScenario_TwoRobotChangeoverRelease_BothLegsOnTheWire` · pkg `shingo/integration/scenarios` · run `-run '^TestScenario_TwoRobotChangeoverRelease_BothLegsOnTheWire$'` · `integration/scenarios/two_robot_release_test.go:63` @ `22c81558` · tags `docker`
- code: STRENGTHEN
- assertion map: every old assertion kept, unchanged (step 1 counts `two_robot_release_test.go:169-172` → `:172-175`; one envelope per leg `:183-185` → `:186-188`; the supply's carries nothing `:188-191` → `:191-194`; the evac's disposition `:194-204` → `:197-207`; no OrderRelease after the pickup `:236-239` → `:250-253`). Added: the evac reads not departed before `HandleRaw` (`:236-238`) and departed after it (`:244-247`): the BinPickedUp reached `HandleBinPickedUpMessage` for the evac it names (`stampDepartureIfLeftCell`). Before, step 3's absence held as well for an envelope never decoded, filtered out or routed nowhere. Its reader, `pendingReleases`, is the one step 1 shows finding both legs' envelopes. Not caught here and caught by the multi-node row below: a supply released again at the pickup (this supply is already released at the click, and a second release sends nothing). The header's `SetBinPickedUpHandler` (`:58-60`, a name nothing declares) now names the subject route the harness and `cmd/shingoedge` `buildInboundRouter` share.
- mutation: the pickup reaches no handler:
  ```diff
  --- a/shingo-edge/engine/handler_bin_picked_up.go
  +++ b/shingo-edge/engine/handler_bin_picked_up.go
   func (e *Engine) HandleBinPickedUpMessage(bp protocol.BinPickedUp) {
  +	if bp.OrderUUID != "" {
  +		return
  +	}
  ```
  · old test GREEN at base: `go test -count=1 -tags docker,lockcheck -run '^(TestScenario_TwoRobotChangeoverRelease_BothLegsOnTheWire|TestScenario_MultiNodeChangeover_ReleasesStayOnTheirNode)$' ./scenarios/` (in `integration`) → `ok  shingo/integration/scenarios 5.276s` · survivor red at tip: same command → `two_robot_release_test.go:245: step 2: after its BinPickedUp the evac reads departed=false (err <nil>), want departed: the pickup never reached the handler`
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `02d36d96` with this commit's diff applied

### STRENGTHEN — `TestScenario_MultiNodeChangeover_ReleasesStayOnTheirNode`
- test: `TestScenario_MultiNodeChangeover_ReleasesStayOnTheirNode` · pkg `shingo/integration/scenarios` · run `-run '^TestScenario_MultiNodeChangeover_ReleasesStayOnTheirNode$'` · `integration/scenarios/changeover_release_multi_node_test.go:43` @ `22c81558` · tags `docker`
- code: STRENGTHEN
- assertion map: every old assertion kept, unchanged (two evac releases `changeover_release_multi_node_test.go:166-175` → `:172-181`; no OrderRelease after evac A's pickup `:196-199` → `:218-221`; none after evac B's `:217-220` → `:240-243`). Added: `departed` (`:189-197`) checks no evac departed before the pickups (`:214`), only evac A after A's (`:216`), both after B's (`:238`): each envelope reached the handler for its own evac and not the other. And after the pickups the operator's release of supply A sends one OrderRelease, supply A's (`:249-258`): the supplies the pickups left alone were releasable, so the two absences are the pickup's answer and not legs that could not have gone.
- mutation: (1) the pickup reaches no handler (the diff in the row above) · old test GREEN at base: the command above → `ok` · survivor red at tip: → `changeover_release_multi_node_test.go:216: after evac A pickup: evac 2 reads departed=false, want true`. (2) the supply goes at the evac's pickup again (the chain "feat(edge): a release is an intent the act plans and a worker keeps (S5)" deleted):
  ```diff
  --- a/shingo-edge/engine/handler_bin_picked_up.go
  +++ b/shingo-edge/engine/handler_bin_picked_up.go
   	if task, terr := e.db.GetChangeoverNodeTaskByEvacOrderID(order.ID); terr == nil && task != nil {
  +		if task.NextMaterialOrderID != nil {
  +			_ = e.ReleaseOrderWithLineside(*task.NextMaterialOrderID, ReleaseDisposition{CalledBy: "pickup"})
  +		}
  ```
  · old test red at base: → `changeover_release_multi_node_test.go:197: after evac A pickup: got 1 envelopes, want 0` · survivor red at tip: → `changeover_release_multi_node_test.go:219: after evac A pickup: got 1 envelopes, want 0` (the two-robot test is green under it at base and tip, as its row says)
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none
- verified at: `02d36d96` with this commit's diff applied

### DEAD — `TestKeepStagedChangeover_EveryShapePin`
- test: `TestKeepStagedChangeover_EveryShapePin` · pkg `shingoedge/engine` · run `-run '^TestKeepStagedChangeover_EveryShapePin$'` · `shingo-edge/engine/keep_staged_changeover_pin_test.go:374` @ `22c81558` · tags `none`
- code: DEAD (the `today` column of `keepStagedChangeoverPin`; the test and its 101 subtests stay). The header `:38-43` was STALE with it and is rewritten in the same commit.
- assertion map: the one comparison, each subtest's observed row against its pin row (`keep_staged_changeover_pin_test.go:397-398`, `got != row.after`) → `:400-401` (`got != want`), the same 101 strings, unchanged. `pinRow.today` (`:425`, written by all 101 rows `:428-730`) dropped because nothing read it: the assertion arm moved to `row.after` in "fix(edge): the keeper is a keep-staged spot's only refill owner, through a changeover too" and its message in "fix(edge): a single-robot changeover collects the carrier it parked, never a stray" (both 2026-10-16); 89 of the 101 `today` strings differ from `after`. The header said "The test asserts today until that change lands, and after from then on"; both changes landed, so it now says what each row is and where the pre-change answers live (the file's history). The ALN_011 extra case's comment ("today's leg lifts it anyway", `:413`) described the `today` answer (`legs=S`); it now describes the row the test asserts (`legs=D+R+M`, the spare sent back).
- mutation: the read-by-nothing proof, then one the comparison catches. (1) every `today` string at base replaced with `"scrambled today"` (101 rows): `go test -count=1 -tags lockcheck -run '^TestKeepStagedChangeover_EveryShapePin$' ./engine/` → `ok  shingoedge/engine 7.260s`. (2) a changeover start sends no spare back:
  ```diff
  --- a/shingo-edge/engine/keep_staged_changeover.go
  +++ b/shingo-edge/engine/keep_staged_changeover.go
  @@ func (e *Engine) applyChangeoverSpots(
  -			e.returnSpare(n, ch.spot, c.InboundSource, read.payload, read.binID, origin)
  +			_, _ = read, origin
  ```
  · old test red at base: same command → `keep_staged_changeover_pin_test.go:398: got legs=M ret=0 ref=0 exp=2 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n / want legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n` (43 subtests FAIL) · survivor red at tip: same command → `keep_staged_changeover_pin_test.go:401:` the same got/want pair (43 subtests FAIL)
- coverage Δ: 0 in each tag set (untagged, docker,lockcheck, sim) beyond the noise floor; see Coverage above
- citations rewritten: none (`git grep -n 'pinRow\|keepStagedChangeoverPin'` over the five modules: this file only)
- verified at: `8dc164f9` with this commit's diff applied

### DUP — `TestScenario_SequentialLine_CyclesBackToBack`
- test: `TestScenario_SequentialLine_CyclesBackToBack` · pkg `shingo/integration/scenarios` · run `-run '^TestScenario_SequentialLine_CyclesBackToBack$'` · `integration/scenarios/sequential_line_test.go:134` @ `818e9d89` · tags `docker`
- code: DUP
- assertion map: every assertion is `TestScenario_SequentialLine_SecondRequestWaitsForTheSwap`'s (`integration/scenarios/sequential_line_test.go:152`), which builds the same `newKsrCell(sequential, plain)` per button and runs the same `seqRemoval`, `seqAtTheLine`, `seqLand` and `seqConfirm` for both of its two cycles, plus two refusals asserted to make nothing. removed on the changeover-catid-first branch before this ledger existed, carried by its rebase onto `818e9d89`; the row records the commit that removed it: "test(sim): the sequential line's back-to-back cycles are its refusal scenario's".
- mutation: none run for this row; the surviving test runs the removed one's helpers on the same cell, line for line
- coverage Δ: not measured for this row (no assertion or call left the package)
- citations rewritten: none (`git grep` finds the name only here)
- verified at: the commit that adds this row

### STALE — `TestSeedDemo_KanbanIsSeededAtItsLevel`
- test: `TestSeedDemo_KanbanIsSeededAtItsLevel` · pkg `shingocore/cmd/seeddev` · run `-run '^TestSeedDemo_KanbanIsSeededAtItsLevel$'` · `shingo-core/cmd/seeddev/seed_demo_mg2_test.go:45` @ `818e9d89` · tags `docker`
- code: STALE
- assertion map: renamed `TestSeedDemo_KanbanIsSeededAtItsPools` (`shingo-core/cmd/seeddev/seed_demo_mg2_test.go:45`) when the seed moved to `plants/master.yaml`: the press-empties kanban is born at its pools (6 STANDARD resident, its want), so the old "at its level" assertion names a seed shape that no longer exists; the new pin reads the pools (red on the unsized seed: 6 of 8, 4 of 6). removed on the changeover-catid-first branch before this ledger existed, carried by its rebase onto `818e9d89`; the row records the commit that removed it: "test(sim): the planner golden is re-cut on the master".
- mutation: none run for this row; the rename's red-before is in that commit
- coverage Δ: not measured for this row (the same seed path runs under the new name)
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestAutoCutover_UnmappedValue_StillCutsOver`
- test: `TestAutoCutover_UnmappedValue_StillCutsOver` · pkg `shingoedge/engine` · run `-run '^TestAutoCutover_UnmappedValue_StillCutsOver$'` · `shingo-edge/engine/plc_catid_monitor_test.go:355` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: it pinned a defect as intended behaviour (both CATID sets empty cut the process over); the opposite is `TestPredicate_BothSetsEmptyIsInert` (`shingo-edge/engine/catid_predicate_test.go:79`), and the automatic cutover it exercised was later removed outright (a notice and a one-tap instead). removed on the changeover-catid-first branch before this ledger existed, carried by its rebase onto `818e9d89`; the row records the commit that removed it: "fix(edge): the auto-cutover decides on one predicate, and -1 names no part".
- mutation: none run for this row; the replacing pin's red-before is in that commit
- coverage Δ: not measured for this row
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestChangeover_AutoCompleteFiresOnTerminalTransition`
- test: `TestChangeover_AutoCompleteFiresOnTerminalTransition` · pkg `shingoedge/engine` · run `-run '^TestChangeover_AutoCompleteFiresOnTerminalTransition$'` · `shingo-edge/engine/changeover_gate_test.go:105` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: it pinned `tryCompleteProcessChangeover` completing a changeover from an order's terminal transition; one door now completes a changeover (`completeCutover`) and a terminal transition pokes the cutover monitor instead, so the behaviour it asserted is the defect that commit's red-before list names ("a terminal transition completed a changeover"). The door's latch is `TestDoor_TerminalWriteLatchedForCompleteAndCancel` (`shingo-edge/engine/changeover_door_test.go:285`). removed on the changeover-catid-first branch before this ledger existed, carried by its rebase onto `818e9d89`; the row records the commit that removed it: "feat(edge): one door completes a changeover, and mark as running completes it".
- mutation: none run for this row
- coverage Δ: not measured for this row (`tryCompleteProcessChangeover` is gone)
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestTryComplete_AutoPathRunsTheConfirmPrePass`
- test: `TestTryComplete_AutoPathRunsTheConfirmPrePass` · pkg `shingoedge/engine` · run `-run '^TestTryComplete_AutoPathRunsTheConfirmPrePass$'` · `shingo-edge/engine/changeover_gate_2prime_test.go:142` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: renamed `TestRestore_FlippedRowRunsTheConfirmPrePass` (`shingo-edge/engine/changeover_gate_2prime_test.go:143`): the automatic path it named (`tryCompleteProcessChangeover`) is gone, and the same pre-pass assertion (a delivered, unconfirmed placing leg does not hang the completion) now runs through the one door, from the restore of a flipped row. removed on the changeover-catid-first branch before this ledger existed, carried by its rebase onto `818e9d89`; the row records the commit that removed it: "feat(edge): one door completes a changeover, and mark as running completes it".
- mutation: none run for this row
- coverage Δ: not measured for this row
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestReportingPoints_ListEnabledAndLookups`
- test: `TestReportingPoints_ListEnabledAndLookups` · pkg `shingoedge/store` · run `-run '^TestReportingPoints_ListEnabledAndLookups$'` · `shingo-edge/store/store_test.go:536` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: renamed `TestReportingPoints_ListPolledAndLookups` (`shingo-edge/store/store_test.go:541`) when the poll moved from every enabled point to `ListPolledReportingPoints` (the enabled point on its style's process's counter tag, one per process: a second enabled point drew every cart twice); the lookups' assertions are kept under the new list. removed on the changeover-catid-first branch before this ledger existed, carried by its rebase onto `818e9d89`; the row records the commit that removed it: "fix(edge): the poll counts one point per process, and names a point whose PLC WarLink lacks".
- mutation: none run for this row; that commit's red-before pins are `TestPoll_OnePointPerProcess` and `TestPoll_UnknownPLCRaisesAReadError`
- coverage Δ: not measured for this row
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestScenario_H_R4_ALateLiftMovesTheTicksAfterTheLiftToTheNextBin`
- test: `TestScenario_H_R4_ALateLiftMovesTheTicksAfterTheLiftToTheNextBin` · pkg `shingo/integration/scenarios` · run `-run '^TestScenario_H_R4_ALateLiftMovesTheTicksAfterTheLiftToTheNextBin$'` · `integration/scenarios/carrier_handoff_pending_test.go:70` @ `818e9d89` · tags `docker`
- code: STALE
- assertion map: renamed `TestScenario_H_R4_ALateLiftsTicksAreLoggedTheNextBinLandsFresh` (`integration/scenarios/carrier_handoff_pending_test.go`): Core's old bin N - k, the after_lift refusal and no stale drop (kept); after the pickup nothing is held and the m ticks are logged (held m -> held 0, logged m); the next bin lands at M and takes its own tick in both books (M - m - 1 -> M - 1). Flipped by decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing).
- mutation: the source of this commit before the walk's consume arm became chargeOrLog (the characterisation commit's tree): the renamed test fails there on the flipped assertions, as `TestPinRuling55_SeatHoldsAndTheLedgersLandings` records case by case
- coverage Δ: not measured for this row (the same paths run under the new name)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestCounterDelta_UnboundTickDrainsPileAndHoldsOnlyTheRemainder`
- test: `TestCounterDelta_UnboundTickDrainsPileAndHoldsOnlyTheRemainder` · pkg `shingoedge/engine` · run `-run '^TestCounterDelta_UnboundTickDrainsPileAndHoldsOnlyTheRemainder$'` · `shingo-edge/engine/lineside_bucket_pins_test.go:157` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: renamed `TestCounterDelta_UnboundTickDrainsPileAndLogsOnlyTheRemainder` (`shingo-edge/engine/lineside_bucket_pins_test.go`): the pile drains 2 and no bin delta is sent (kept); the remainder of 3 is logged, not held (pending 3 -> pending 0, logged 3). Flipped by decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing).
- mutation: the source of this commit before the walk's consume arm became chargeOrLog (the characterisation commit's tree): the renamed test fails there on the flipped assertions, as `TestPinRuling55_SeatHoldsAndTheLedgersLandings` records case by case
- coverage Δ: not measured for this row (the same paths run under the new name)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestEvalStrandedGrowth_BindClearsMidWindow`
- test: `TestEvalStrandedGrowth_BindClearsMidWindow` · pkg `shingoedge/engine` · run `-run '^TestEvalStrandedGrowth_BindClearsMidWindow$'` · `shingo-edge/engine/uop_stranded_monitor_test.go:78` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: it pinned the parked-ticks alarm's growth window (evalStrandedGrowth): a bind clears it; decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing) deleted the consume seat's hold, so there is nothing for the alarm to watch: a stroke with no cart of its part is a "no cart" row in the ledger, shown on the strip (`TestPinNoCart_AStrokeAtAnEmptySeatIsLoggedTheNextCartLandsFull`, `shingo-edge/engine/stroke_charged_or_logged_test.go`), and the monitor's produce sweep keeps its pins (`TestProduceHoldSweep_*`).
- mutation: none run for this row (the code it pinned is deleted)
- coverage Δ: not measured for this row (its statements are gone)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestEvalStrandedGrowth_BoundNeverAlarms`
- test: `TestEvalStrandedGrowth_BoundNeverAlarms` · pkg `shingoedge/engine` · run `-run '^TestEvalStrandedGrowth_BoundNeverAlarms$'` · `shingo-edge/engine/uop_stranded_monitor_test.go:64` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: it pinned the parked-ticks alarm's growth window (evalStrandedGrowth): a bound seat never alarms; decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing) deleted the consume seat's hold, so there is nothing for the alarm to watch: a stroke with no cart of its part is a "no cart" row in the ledger, shown on the strip (`TestPinNoCart_AStrokeAtAnEmptySeatIsLoggedTheNextCartLandsFull`, `shingo-edge/engine/stroke_charged_or_logged_test.go`), and the monitor's produce sweep keeps its pins (`TestProduceHoldSweep_*`).
- mutation: none run for this row (the code it pinned is deleted)
- coverage Δ: not measured for this row (its statements are gone)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestEvalStrandedGrowth_IdleLineNeverAlarms`
- test: `TestEvalStrandedGrowth_IdleLineNeverAlarms` · pkg `shingoedge/engine` · run `-run '^TestEvalStrandedGrowth_IdleLineNeverAlarms$'` · `shingo-edge/engine/uop_stranded_monitor_test.go:49` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: it pinned the parked-ticks alarm's growth window (evalStrandedGrowth): a flat hold never alarms; decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing) deleted the consume seat's hold, so there is nothing for the alarm to watch: a stroke with no cart of its part is a "no cart" row in the ledger, shown on the strip (`TestPinNoCart_AStrokeAtAnEmptySeatIsLoggedTheNextCartLandsFull`, `shingo-edge/engine/stroke_charged_or_logged_test.go`), and the monitor's produce sweep keeps its pins (`TestProduceHoldSweep_*`).
- mutation: none run for this row (the code it pinned is deleted)
- coverage Δ: not measured for this row (its statements are gone)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestEvalStrandedGrowth_StallReArmsOnRenewedGrowth`
- test: `TestEvalStrandedGrowth_StallReArmsOnRenewedGrowth` · pkg `shingoedge/engine` · run `-run '^TestEvalStrandedGrowth_StallReArmsOnRenewedGrowth$'` · `shingo-edge/engine/uop_stranded_monitor_test.go:93` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: it pinned the parked-ticks alarm's growth window (evalStrandedGrowth): a stall re-arms; decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing) deleted the consume seat's hold, so there is nothing for the alarm to watch: a stroke with no cart of its part is a "no cart" row in the ledger, shown on the strip (`TestPinNoCart_AStrokeAtAnEmptySeatIsLoggedTheNextCartLandsFull`, `shingo-edge/engine/stroke_charged_or_logged_test.go`), and the monitor's produce sweep keeps its pins (`TestProduceHoldSweep_*`).
- mutation: none run for this row (the code it pinned is deleted)
- coverage Δ: not measured for this row (its statements are gone)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestEvalStrandedGrowth_SustainedGrowthFiresOnce`
- test: `TestEvalStrandedGrowth_SustainedGrowthFiresOnce` · pkg `shingoedge/engine` · run `-run '^TestEvalStrandedGrowth_SustainedGrowthFiresOnce$'` · `shingo-edge/engine/uop_stranded_monitor_test.go:31` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: it pinned the parked-ticks alarm's growth window over a consume seat's hold (evalStrandedGrowth); decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing) deleted the consume seat's hold, so there is nothing for the alarm to watch: a stroke with no cart of its part is a "no cart" row in the ledger, shown on the strip (`TestPinNoCart_AStrokeAtAnEmptySeatIsLoggedTheNextCartLandsFull`, `shingo-edge/engine/stroke_charged_or_logged_test.go`), and the monitor's produce sweep keeps its pins (`TestProduceHoldSweep_*`).
- mutation: none run for this row (the code it pinned is deleted)
- coverage Δ: not measured for this row (its statements are gone)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestPinR3_ReleasedEmptyIsNotChargedThenTheNextBinOfThePartTakesTheHold`
- test: `TestPinR3_ReleasedEmptyIsNotChargedThenTheNextBinOfThePartTakesTheHold` · pkg `shingoedge/engine` · run `-run '^TestPinR3_ReleasedEmptyIsNotChargedThenTheNextBinOfThePartTakesTheHold$'` · `shingo-edge/engine/carrier_handoff_pins_test.go:206` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: renamed `TestPinR3_ReleasedEmptyIsNotChargedItsStrokesAreLogged` (`shingo-edge/engine/carrier_handoff_pins_test.go`): every old assertion kept but two, flipped by decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing): past the pile the 2 strokes are logged at the position, not held (`held` 2 -> held 0, logged 2), and the next bin of the part lands at N and takes its own stroke (N - 3 -> N - 1). The released bin is still not charged (R3).
- mutation: the source of this commit before the walk's consume arm became chargeOrLog (the characterisation commit's tree): the renamed test fails there on the flipped assertions, as `TestPinRuling55_SeatHoldsAndTheLedgersLandings` records case by case
- coverage Δ: not measured for this row (the same paths run under the new name)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestRuntimeBinding_PLCTicksHoldWhenNoBinThenReplayOntoNextBin`
- test: `TestRuntimeBinding_PLCTicksHoldWhenNoBinThenReplayOntoNextBin` · pkg `shingoedge/engine` · run `-run '^TestRuntimeBinding_PLCTicksHoldWhenNoBinThenReplayOntoNextBin$'` · `shingo-edge/engine/runtime_uop_binding_test.go:225` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: renamed `TestRuntimeBinding_PLCTicksWithNoBinAreLoggedTheNextBinStartsFresh` (`shingo-edge/engine/runtime_uop_binding_test.go`): the gap's 3 ticks are logged in the ledger, not held (pending 3 -> pending 0, logged 3); the cache stays 0 while unbound (kept); the first tick after the delivered bind is the new bin's alone (1000 - 4 -> 1000 - 1). Flipped by decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing).
- mutation: the source of this commit before the walk's consume arm became chargeOrLog (the characterisation commit's tree): the renamed test fails there on the flipped assertions, as `TestPinRuling55_SeatHoldsAndTheLedgersLandings` records case by case
- coverage Δ: not measured for this row (the same paths run under the new name)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestStagedUnbound_TicksParkInPendingNoBinEverBound`
- test: `TestStagedUnbound_TicksParkInPendingNoBinEverBound` · pkg `shingoedge/engine` · run `-run '^TestStagedUnbound_TicksParkInPendingNoBinEverBound$'` · `shingo-edge/engine/wiring_staged_unbound_test.go:35` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: renamed `TestStagedUnbound_TicksWithNoBinEverBoundAreLogged` (`shingo-edge/engine/wiring_staged_unbound_test.go`): the unbound slot never binds (kept); its 46 ticks are logged in the ledger, not parked in pending (pending 46 -> 0, logged 46); the cache stays 0 (kept). Flipped by decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing).
- mutation: the source of this commit before the walk's consume arm became chargeOrLog (the characterisation commit's tree): the renamed test fails there on the flipped assertions, as `TestPinRuling55_SeatHoldsAndTheLedgersLandings` records case by case
- coverage Δ: not measured for this row (the same paths run under the new name)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestStrandedCarrier_LabelFromCore`
- test: `TestStrandedCarrier_LabelFromCore` · pkg `shingoedge/engine` · run `-run '^TestStrandedCarrier_LabelFromCore$'` · `shingo-edge/engine/uop_stranded_monitor_test.go:366` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: it pinned the parked-ticks alarm's carrier label (resolveCarrier); decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing) deleted the consume seat's hold, so there is nothing for the alarm to watch: a stroke with no cart of its part is a "no cart" row in the ledger, shown on the strip (`TestPinNoCart_AStrokeAtAnEmptySeatIsLoggedTheNextCartLandsFull`, `shingo-edge/engine/stroke_charged_or_logged_test.go`), and the monitor's produce sweep keeps its pins (`TestProduceHoldSweep_*`).
- mutation: none run for this row (the code it pinned is deleted)
- coverage Δ: not measured for this row (its statements are gone)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestStrandedDetail_AsksOnlyOnceTheWaitIsLong`
- test: `TestStrandedDetail_AsksOnlyOnceTheWaitIsLong` · pkg `shingoedge/engine` · run `-run '^TestStrandedDetail_AsksOnlyOnceTheWaitIsLong$'` · `shingo-edge/engine/uop_stranded_monitor_test.go:192` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: it pinned the parked-ticks chip's two sentences (formatStrandedDetail, uop_accumulating_cta_after); decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing) deleted the consume seat's hold, so there is nothing for the alarm to watch: a stroke with no cart of its part is a "no cart" row in the ledger, shown on the strip (`TestPinNoCart_AStrokeAtAnEmptySeatIsLoggedTheNextCartLandsFull`, `shingo-edge/engine/stroke_charged_or_logged_test.go`), and the monitor's produce sweep keeps its pins (`TestProduceHoldSweep_*`).
- mutation: none run for this row (the code it pinned is deleted)
- coverage Δ: not measured for this row (its statements are gone)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestStrandedMonitor_DecisionLadder`
- test: `TestStrandedMonitor_DecisionLadder` · pkg `shingoedge/engine` · run `-run '^TestStrandedMonitor_DecisionLadder$'` · `shingo-edge/engine/node_walkers_decision_test.go:341` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: it pinned which consume seats the stranded monitor's consume arm watched; decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing) deleted the consume seat's hold, so there is nothing for the alarm to watch: a stroke with no cart of its part is a "no cart" row in the ledger, shown on the strip (`TestPinNoCart_AStrokeAtAnEmptySeatIsLoggedTheNextCartLandsFull`, `shingo-edge/engine/stroke_charged_or_logged_test.go`), and the monitor's produce sweep keeps its pins (`TestProduceHoldSweep_*`).
- mutation: none run for this row (the code it pinned is deleted)
- coverage Δ: not measured for this row (its statements are gone)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestStrandedMonitor_Evaluate_FiresNamedAlarm`
- test: `TestStrandedMonitor_Evaluate_FiresNamedAlarm` · pkg `shingoedge/engine` · run `-run '^TestStrandedMonitor_Evaluate_FiresNamedAlarm$'` · `shingo-edge/engine/uop_stranded_monitor_test.go:120` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: it pinned the stranded monitor's consume scan naming a growing seat hold (evaluate, EventUOPStranded); decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing) deleted the consume seat's hold, so there is nothing for the alarm to watch: a stroke with no cart of its part is a "no cart" row in the ledger, shown on the strip (`TestPinNoCart_AStrokeAtAnEmptySeatIsLoggedTheNextCartLandsFull`, `shingo-edge/engine/stroke_charged_or_logged_test.go`), and the monitor's produce sweep keeps its pins (`TestProduceHoldSweep_*`).
- mutation: none run for this row (the code it pinned is deleted)
- coverage Δ: not measured for this row (its statements are gone)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestBuildView_StrandedResolverPopulatesChip`
- test: `TestBuildView_StrandedResolverPopulatesChip` · pkg `shingoedge/service` · run `-run '^TestBuildView_StrandedResolverPopulatesChip$'` · `shingo-edge/service/station_stranded_test.go:15` @ `818e9d89` · tags `none`
- code: STALE
- assertion map: it pinned the station view's parked-ticks chip (StationNodeView.StrandedAlarm, SetStrandedResolver); decision 55 (a stroke charges the cart of its part standing at the position, or is logged in the ledger; a consume seat holds nothing) deleted the consume seat's hold, so there is nothing for the alarm to watch: a stroke with no cart of its part is a "no cart" row in the ledger, shown on the strip (`TestPinNoCart_AStrokeAtAnEmptySeatIsLoggedTheNextCartLandsFull`, `shingo-edge/engine/stroke_charged_or_logged_test.go`), and the monitor's produce sweep keeps its pins (`TestProduceHoldSweep_*`).
- mutation: none run for this row (the code it pinned is deleted)
- coverage Δ: not measured for this row (its statements are gone)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestFlip_RecordsTheCarrierItLeaves`
- test: `TestFlip_RecordsTheCarrierItLeaves` · pkg `shingoedge/engine` · run `-run '^TestFlip_RecordsTheCarrierItLeaves$'` · `shingo-edge/engine/lineside_flip_stamp_test.go:143` @ `b3afc355` · tags `none`
- code: STALE
- assertion map: renamed `TestFlip_ChangesNoSeat` (`shingo-edge/engine/lineside_flip_seats_test.go`), the same five doors and seats: every seat now reads after the flip what it read before. Flipped by decision 57 (a cart's part is recorded at its bind, known or assumed; supersedes 38 rule 1 and 40): the unknown consume seats of the departing style are no longer stamped with its part (source flip), and the seat that landed unnamed since the changeover started keeps its bind's assumed record rather than an exemption from the stamp. The known, empty, produce, loader and incoming-only seats are unchanged, as before.
- mutation: the renamed test at `b3afc355`: `lineside_flip_seats_test.go:218: FS-UNSAID after completeCutover gated reads {part:PART-L known:true source:flip}, want {part: known:false source:} as before the flip`, at every flipping door
- coverage Δ: not measured for this row (the stamp's statements are gone)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestLanding_AnUnnamedCartTakesItsOrdersPart`
- test: `TestLanding_AnUnnamedCartTakesItsOrdersPart` · pkg `shingoedge/engine` · run `-run '^TestLanding_AnUnnamedCartTakesItsOrdersPart$'` · `shingo-edge/engine/handler_bin_landed_test.go:18` @ `b3afc355` · tags `none`
- code: STALE
- assertion map: renamed `TestLanding_RecordsWhatTheDoorwayKnows` (same file): kept, the order's part from the delivery and Core's part from the landing where it differs from the order's. Flipped by decision 57: a landing whose Core part is the order's is recorded from the delivery (was landing); a landing neither names (no order, an empty brought, delivered elsewhere) records the claim's part assumed, not known (was unknown from the landing).
- mutation: the renamed test at `b3afc355`: `handler_bin_landed_test.go:64: the seat reads "" known false from "landing", want "PART-OLD" known false from "assumed"` (three rows) and `... "PART-X" known true from "landing", want "PART-X" known true from "delivery"`
- coverage Δ: not measured for this row (the same paths run under the new name)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestPinChangeover_ACartThatCannotSayLogsTheIncomingStrokes`
- test: `TestPinChangeover_ACartThatCannotSayLogsTheIncomingStrokes` · pkg `shingoedge/engine` · run `-run '^TestPinChangeover_ACartThatCannotSayLogsTheIncomingStrokes$'` · `shingo-edge/engine/tick_other_part_test.go:203` @ `b3afc355` · tags `none`
- code: STALE
- assertion map: renamed `TestPinChangeover_ACartAssumedAtItsBindKeepsItsPart` (same file), the cart now recorded as its bind records it (RUN's part, assumed): kept, bound before the start an ALT stroke is logged and a MIRROR-R cart takes its own strokes, and RUN's stroke draws the cart. Flipped by decision 57 (supersedes 52's walk-time reading and 40): bound since the start, and with no changeover open, an ALT stroke is logged (was charged, stamped MIRROR-R).
- mutation: the renamed test at `b3afc355`: `tick_other_part_test.go:248: after an ALT stroke: cart 29, hold 0, logged 0; want cart 30, hold 0, logged 1` (both flipped rows)
- coverage Δ: not measured for this row (decision 52's statements are gone)
- citations rewritten: none
- verified at: the commit that adds this row
### STALE — `TestCarrierBound_ByAdjustmentIsUnknown`
- test: `TestCarrierBound_ByAdjustmentIsUnknown` · pkg `shingoedge/engine` · run `-run '^TestCarrierBound_ByAdjustmentIsUnknown$'` · `shingo-edge/engine/carrier_left_test.go:123` @ `b3afc355` · tags `none`
- code: STALE
- assertion map: renamed `TestCarrierBound_ByAdjustmentIsTheClaimsPartAssumed` (same file): every old assertion kept but the identity, flipped by decision 57: a bind whose envelope names no part records the claim's part assumed, not known (code "" -> PART-CL, known false kept, source assumed). The previous occupant's known identity still never survives.
- mutation: the renamed test at `b3afc355`: `carrier_left_test.go:140: bind recorded code="" known=false from "landing", want the claim's PART-CL assumed` and `:164: correction bind recorded code="" known=false from "delivery"`
- coverage Δ: not measured for this row (the same paths run under the new name)
- citations rewritten: none
- verified at: the commit that adds this row
### DEAD — `TestEngineDbMethodsFrozen`
- test: `TestEngineDbMethodsFrozen` · pkg `shingocore/engine` · run `-run '^TestEngineDbMethodsFrozen$'` · `shingo-core/engine/engine_db_methods_freeze_test.go:30` @ `b9210bce` · tags `none`
- code: DEAD (the ratchet reached its floor; its own header says "When the count hits zero, this test and engine_db_methods.go both get deleted", and the count was 0)
- assertion map: the one assertion, the count of `func (e *Engine)` declarations in `engine_db_methods.go` equals `frozenPassthroughCount` (0), goes with the file it counted, which held only its header comment. Nothing survives it and nothing needs to: the file it read is gone, and the boundary it stood for (a www handler reaches the store through a service, not an Engine passthrough) is the `ServiceAccess` / `EngineOrchestration` split, whose widths `TestServiceAccessWidth` and `TestEngineOrchestrationWidth` pin.
- mutation: the read-by-nothing proof, then one it catches, both at base with the two files restored. (1) a passthrough added outside the file, `func (e *Engine) MutationPassthrough() *store.DB { return e.db }` appended to `engine/engine_accessors.go`: `go test -count=1 -run '^TestEngineDbMethodsFrozen$' ./engine/` → `ok  shingocore/engine 1.811s` (not in its scope; `engine_accessors.go` already declares `func (e *Engine) DB() *store.DB`). (2) the same method appended to `engine/engine_db_methods.go`: same command → `engine_db_methods_freeze_test.go:40: passthrough count grew: 1 > 0 (add to a service interface instead)`, `FAIL shingocore/engine`.
- coverage Δ: 0 (the test reads a source file; it covers no statement of the package)
- citations rewritten: `scripts/check-test-error-discard-ratchet.sh` (its three rules no longer cite the deleted test as the convention's home); `engine/recovery.go` (drops "see engine_db_methods.go"); `shingo-core/docs/engine_db_methods_residual.md` deleted with it
- verified at: the commit that adds this row

### FOLD — `TestWireProtocolDocSubjectsMatchCode`
- test: `TestWireProtocolDocSubjectsMatchCode` · pkg `shingo/protocol` · run `-run '^TestWireProtocolDocSubjectsMatchCode$'` · `protocol/docs_drift_test.go:31` @ `b9210bce` · tags `none`
- code: FOLD (the doc's Known Subjects table is now emitted from `protocol.AllSubjects()` by `shared/scripts/docgen`, so the table and the code cannot disagree, and `TestGeneratedBlocksAreCurrent` fails a stale block)
- assertion map: (1) every `AllSubjects()` value is a row and every row is a subject → the generated block is `AllSubjects()`, one row each, and the generator refuses a value no Subject constant holds; (2) `order.skipped` is never a subject row → a row can only come from `AllSubjects()`, which holds no message type
- mutation: at the tip, `"mutation.subject"` added to `CoreInboundSubjects()`: `go test -count=1 -run '^TestGeneratedBlocksAreCurrent$' ./scripts/docgen/` (shared) → `docs/wire-protocol.md:369: protocol/types.go AllSubjects: no Subject constant holds "mutation.subject"`, `FAIL`; restored
- coverage Δ: 0 (a doc-reading test; it covered no protocol statement beyond `AllSubjects`, which `TestGeneratedBlocksAreCurrent` calls)
- citations rewritten: `protocol/docs_drift_test.go` header; `docs/ui-style-guide/24-drift-detection.md` (generated inventory)
- verified at: the commit that adds this row

### FOLD — `TestWireProtocolDocNoExpiryRowsMatchCode`
- test: `TestWireProtocolDocNoExpiryRowsMatchCode` · pkg `shingo/protocol` · run `-run '^TestWireProtocolDocNoExpiryRowsMatchCode$'` · `protocol/docs_drift_test.go:74` @ `b9210bce` · tags `none`
- code: FOLD (the Subject-Specific TTL table is now emitted from `protocol.DataTTLFor` by `shared/scripts/docgen`; `TestGeneratedBlocksAreCurrent` fails a stale block)
- assertion map: the `**none**` rows equal the subjects whose `DataTTLFor` is `NoExpiry`, both ways → the generated TTL block renders every subject's `DataTTLFor`, `NoExpiry` included, so a change to the set changes the block
- mutation: at the tip, `SubjectEdgeAnswer` given `30 * time.Second` in `protocol/expiry.go`: `go test -count=1 -run '^TestGeneratedBlocksAreCurrent$' ./scripts/docgen/` (shared) → `docs/wire-protocol.md:425: the block generated from "protocol/expiry.go DataTTLFor" is stale; run make docs-gen`, `FAIL`; restored
- coverage Δ: 0 (a doc-reading test)
- citations rewritten: `protocol/docs_drift_test.go` header; `docs/ui-style-guide/24-drift-detection.md` (generated inventory)
- verified at: the commit that adds this row

### FOLD — `TestWireProtocolDocNoExpiryProseCountsTheTable`
- test: `TestWireProtocolDocNoExpiryProseCountsTheTable` · pkg `shingo/protocol` · run `-run '^TestWireProtocolDocNoExpiryProseCountsTheTable$'` · `protocol/docs_drift_test.go:109` @ `b9210bce` · tags `none`
- code: FOLD (into `TestWireProtocolDocNoExpiryProseCountsTheSet`, which counts the prose against `protocol/expiry.go`'s NoExpiry set instead of against the table, now that the table is generated)
- assertion map: the "N subjects carry NO expiry at all" sentence names the count → the same sentence, the same count word, counted from `DataTTLFor(s) == NoExpiry` over `AllSubjects()`
- mutation: at the tip, `SubjectEdgeAnswer` given `30 * time.Second` in `protocol/expiry.go`: `go test -count=1 -run '^TestWireProtocolDocNoExpiryProseCountsTheSet$' .` (protocol) → `prose says "**Nine subjects carry NO expiry at all** …" but 8 subjects are NoExpiry in protocol/expiry.go`, `FAIL`; restored
- coverage Δ: 0 (a doc-reading test)
- citations rewritten: none outside `protocol/docs_drift_test.go`
- verified at: the commit that adds this row

### DEAD — `TestStationCard_ShowsLeadBesideRatio`
- test: `TestStationCard_ShowsLeadBesideRatio` · pkg `shingoedge/www` · run `-run '^TestStationCard_ShowsLeadBesideRatio$'` · `shingo-edge/www/station_card_test.go:34` @ `f0291082` · tags `none`
- code: DEAD (it ran the checks of `station-card.test.js` whose names hold "the lead", and the file at base has none, so the runner ran zero checks and passed; its header describes a "0 ahead" line `station-card.js` at base does not draw)
- assertion map: none to move. The runner now fails a filter that matches no check (`station-card.test.js`, `ran === 0`), so a wrapper cannot pass on nothing again. What the card measures is asserted by `TestStationCard_PreTicksOnlyCountableUnboundPositionsWhenItIsTheOnlyStation` (the whole file: "the status: one line, singular and plural, amber only when stopped") and `TestStationCard_ListsEachCounterWithItsStatus`.
- mutation: the read-by-nothing proof at base, then one the survivors catch at the tip. (1) base `station-card.js`'s `measures` returns `{ ratio: 'BROKEN', stopped: 'BROKEN' }`: `node station-card.test.js "the lead"` → `station card: all passed`, exit 0 (the whole file: `FAIL the measures: …`, `FAIL the counting block: a station tag discovered …`). (2) at the tip, `status` says `'last click ' + n + ' strokes ago'` (the old plural): `node station-card.test.js` → `FAIL the status: one line, singular and plural, amber only when stopped`, `FAIL the station list: one row per counter, …`
- coverage Δ: 0 (a node-run JS test; it covers no Go statement)
- citations rewritten: none (`git grep -n TestStationCard_ShowsLeadBesideRatio` over the five modules: this file only)
- verified at: the commit that adds this row

### STALE — `TestCountingBlock_EachLineLabelledInThreeGroups`
- test: `TestCountingBlock_EachLineLabelledInThreeGroups` · pkg `shingoedge/www` · run `-run '^TestCountingBlock_EachLineLabelledInThreeGroups$'` · `shingo-edge/www/station_card_test.go:42` @ `f0291082` · tags `none`
- code: STALE (the Counting block's lines each carried a provenance chip, discovered, confirmed or measured, and the test asserted the chips; the block is now the Production counter section's MES slots and station counters rows, in words, with no chip, per `docs/ui-style-guide/33-settings-pages.md`)
- assertion map: the slots group then the stations group, in order → `TestCountingRows_TheTitleAndTheMESSlots` ("the counting rows: MES slots counted first, …": the slots row, then `rowOf(html, 'stations')`); each slot's line (`CATID`, `count`, `rising` / `not rising`, `not read at the last pass`) → the same check's slots row text, the counted slot first, unused slots folded (`Prod_Counter_04–05 unused`), unread slots folded; a leading slot with no record ("strokes since T are not in the record") → "the counting rows: unread slots fold, a slot leading with no record says so"; a station card's tag read or not, its positions, its clicks per part and its stop → `TestStationCard_ListsEachCounterWithItsStatus` (the row's tags, `counts …`, the one-line status, amber only when stopped). Dropped: the chip on every line (its label against its text, `a line shows the label it carries`), by design; the tip asserts no chip (`html.indexOf('pd-tag') < 0`).
- mutation: a slot's line loses its count. At base, `slotsGroup` without `' · count '`: `node station-card.test.js "the counting block"` → `FAIL the counting block: two groups, the MES slots discovered and the slot counted on confirmed`, `FAIL the counting block: a slot the PLC counts on, …`. At the tip, `slotsField`'s `reading` without `' · count '`: `node station-card.test.js "the counting rows"` → `FAIL the counting rows: MES slots counted first, in-use slots their own lines, unused ones folded`, `FAIL the counting rows: unread slots fold, a slot leading with no record says so`
- coverage Δ: 0 (a node-run JS test; it covers no Go statement)
- citations rewritten: none (`git grep -n TestCountingBlock_EachLineLabelledInThreeGroups` over the five modules: this file only)
- verified at: the commit that adds this row

### STALE — `TestMarkedEvacNodes_IsNotPressShaped`
- test: `TestMarkedEvacNodes_IsNotPressShaped` · pkg `shingoedge/domain` · run `-run '^TestMarkedEvacNodes_IsNotPressShaped$'` · `shingo-edge/domain/changeover_evac_nodes_test.go:59` @ `8e076d27` · tags `none`
- code: STALE (marks are on every line mode now; the premise that a non-press claim could mark its paired name changed: a sequential claim's paired name is its partner's line, which the partner marks)
- assertion map: a claim marking one of the several nodes it names gets that node back → `TestMarkedEvacNodes_EveryLineModeMarksItsOwnPositions` (each line mode marks its own positions; a sequential claim's mark of its partner's line is dropped, and refused at save by `TestValidateNodeClaim_EveryLineModeMarksItsPositions`)
- mutation: none run for this row
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none (`git grep -n TestMarkedEvacNodes_IsNotPressShaped` over the five modules: this ledger only)
- verified at: the commit that adds this row

### STALE — `TestApplyProducePlan_PrimeAutoConfirmIsMerged`
- test: `TestApplyProducePlan_PrimeAutoConfirmIsMerged` · pkg `shingoedge/engine` · run `-run '^TestApplyProducePlan_PrimeAutoConfirmIsMerged$'` · `shingo-edge/engine/produce_prime_test.go:492` @ `8e076d27` · tags `none`
- code: STALE (a prime is confirmed by its placement now, whatever the claim and the plant say: "feat(edge): one maker for a carrier to a position other than the line")
- assertion map: the matrix of claim × plant auto-confirm (false/false false, the rest true) → `TestApplyProducePlan_APrimeIsConfirmedByItsPlacement`, the same four rows, each now true
- mutation: the old test against the new maker, before it was rewritten: `go test -count=1 -run '^TestApplyProducePlan_PrimeAutoConfirmIsMerged$' ./engine/` → `produce_prime_test.go:525: claim=false cfg=false: prime AutoConfirm = true, want false (claim || config)`
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: `shingo-edge/engine/produce_prime_test.go` (the comment that pointed at it now points at its successor)
- verified at: the commit that adds this row

### DEAD — `TestBuildEvacuateChangeoverSteps_SingleRobot`
- test: `TestBuildEvacuateChangeoverSteps_SingleRobot` · pkg `shingoedge/engine` · run `-run '^TestBuildEvacuateChangeoverSteps_SingleRobot$'` · `shingo-edge/engine/step_builders_test.go:88` @ `8e076d27` · tags `none`
- code: DEAD (the evacuate builder and the `evacuate_on_changeover` switch it served are deleted; marks are the one tool change)
- assertion map: the mode's evacuate shape (where the Tooling done wait sat, which leg fetched) → the one tool change every mode now takes, `TestToolChange_TheTable` (mode × layout × flip × part × carrier × marks: no set-down on a marked position before its leg's `tooling_done` wait, no leg moving a carrier between two positions of a marked cell, every position of a part-marked cell planned, no order ending on a wait, the wait written by the decorator alone) and the swap golden's `toolChange` rows. The per-mode evacuate shape itself is retired with the switch
- mutation: none to run: the function the test called, `BuildEvacuateChangeoverSteps`, is deleted in "refactor(edge): one tool change; the evacuate builders and the old switch are gone", so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestBuildEvacuateChangeoverSteps_TwoRobot`
- test: `TestBuildEvacuateChangeoverSteps_TwoRobot` · pkg `shingoedge/engine` · run `-run '^TestBuildEvacuateChangeoverSteps_TwoRobot$'` · `shingo-edge/engine/step_builders_test.go:206` @ `8e076d27` · tags `none`
- code: DEAD (the evacuate builder and the `evacuate_on_changeover` switch it served are deleted; marks are the one tool change)
- assertion map: the mode's evacuate shape (where the Tooling done wait sat, which leg fetched) → the one tool change every mode now takes, `TestToolChange_TheTable` (mode × layout × flip × part × carrier × marks: no set-down on a marked position before its leg's `tooling_done` wait, no leg moving a carrier between two positions of a marked cell, every position of a part-marked cell planned, no order ending on a wait, the wait written by the decorator alone) and the swap golden's `toolChange` rows. The per-mode evacuate shape itself is retired with the switch
- mutation: none to run: the function the test called, `BuildEvacuateChangeoverSteps`, is deleted in "refactor(edge): one tool change; the evacuate builders and the old switch are gone", so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestBuildEvacuateChangeoverSteps_PressIndex_2Pos`
- test: `TestBuildEvacuateChangeoverSteps_PressIndex_2Pos` · pkg `shingoedge/engine` · run `-run '^TestBuildEvacuateChangeoverSteps_PressIndex_2Pos$'` · `shingo-edge/engine/step_builders_test.go:356` @ `8e076d27` · tags `none`
- code: DEAD (the evacuate builder and the `evacuate_on_changeover` switch it served are deleted; marks are the one tool change)
- assertion map: the mode's evacuate shape (where the Tooling done wait sat, which leg fetched) → the one tool change every mode now takes, `TestToolChange_TheTable` (mode × layout × flip × part × carrier × marks: no set-down on a marked position before its leg's `tooling_done` wait, no leg moving a carrier between two positions of a marked cell, every position of a part-marked cell planned, no order ending on a wait, the wait written by the decorator alone) and the swap golden's `toolChange` rows. The per-mode evacuate shape itself is retired with the switch
- mutation: none to run: the function the test called, `BuildEvacuateChangeoverSteps`, is deleted in "refactor(edge): one tool change; the evacuate builders and the old switch are gone", so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestBuildEvacuateChangeoverSteps_Sequential`
- test: `TestBuildEvacuateChangeoverSteps_Sequential` · pkg `shingoedge/engine` · run `-run '^TestBuildEvacuateChangeoverSteps_Sequential$'` · `shingo-edge/engine/step_builders_test.go:599` @ `8e076d27` · tags `none`
- code: DEAD (the evacuate builder and the `evacuate_on_changeover` switch it served are deleted; marks are the one tool change)
- assertion map: the mode's evacuate shape (where the Tooling done wait sat, which leg fetched) → the one tool change every mode now takes, `TestToolChange_TheTable` (mode × layout × flip × part × carrier × marks: no set-down on a marked position before its leg's `tooling_done` wait, no leg moving a carrier between two positions of a marked cell, every position of a part-marked cell planned, no order ending on a wait, the wait written by the decorator alone) and the swap golden's `toolChange` rows. The per-mode evacuate shape itself is retired with the switch
- mutation: none to run: the function the test called, `BuildEvacuateChangeoverSteps`, is deleted in "refactor(edge): one tool change; the evacuate builders and the old switch are gone", so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestBuildEvacuateChangeoverSteps_SequentialUnpaired_EmptyDispatch`
- test: `TestBuildEvacuateChangeoverSteps_SequentialUnpaired_EmptyDispatch` · pkg `shingoedge/engine` · run `-run '^TestBuildEvacuateChangeoverSteps_SequentialUnpaired_EmptyDispatch$'` · `shingo-edge/engine/step_builders_test.go:663` @ `8e076d27` · tags `none`
- code: DEAD (the evacuate builder and the `evacuate_on_changeover` switch it served are deleted; marks are the one tool change)
- assertion map: the mode's evacuate shape (where the Tooling done wait sat, which leg fetched) → the one tool change every mode now takes, `TestToolChange_TheTable` (mode × layout × flip × part × carrier × marks: no set-down on a marked position before its leg's `tooling_done` wait, no leg moving a carrier between two positions of a marked cell, every position of a part-marked cell planned, no order ending on a wait, the wait written by the decorator alone) and the swap golden's `toolChange` rows. The per-mode evacuate shape itself is retired with the switch
- mutation: none to run: the function the test called, `BuildEvacuateChangeoverSteps`, is deleted in "refactor(edge): one tool change; the evacuate builders and the old switch are gone", so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestBuildSequentialPerPositionEvacuate_StillCarriesAcrossABareWait`
- test: `TestBuildSequentialPerPositionEvacuate_StillCarriesAcrossABareWait` · pkg `shingoedge/engine` · run `-run '^TestBuildSequentialPerPositionEvacuate_StillCarriesAcrossABareWait$'` · `shingo-edge/engine/changeover_position_builder_test.go:160` @ `8e076d27` · tags `none`
- code: DEAD (`buildSequentialPerPositionEvacuate` is deleted with the switch; a sequential position's tool change is the one leg, `toolingPositionAction`)
- assertion map: the sequential position's leg carrying its carrier across a bare wait → the tool change's leg on a sequential cell in `TestToolChange_TheTable`, and `TestSequentialChangeoverEvacuate_OnePositionGetsOneCarrier` (the leg brings its own carrier back and gets no backfill)
- mutation: none to run: the builder is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestDiffStyleClaims_EvacuateFlagIgnoredOnPayloadChange`
- test: `TestDiffStyleClaims_EvacuateFlagIgnoredOnPayloadChange` · pkg `shingoedge/engine` · run `-run '^TestDiffStyleClaims_EvacuateFlagIgnoredOnPayloadChange$'` · `shingo-edge/engine/changeover_diff_test.go:344` @ `8e076d27` · tags `none`
- code: DEAD (the `evacuate_on_changeover` field is deleted, and Edge v47 drops its column)
- assertion map: a part change with the old switch on stays a Swap → nothing: the switch is gone; a part change at a marked position is `TestDiffStyleClaims_MarkedPositionsEvacuateAcrossAPayloadChange`
- mutation: none to run: the field is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestPlanNodeAction_Drop_WithEvacuateMarker`
- test: `TestPlanNodeAction_Drop_WithEvacuateMarker` · pkg `shingoedge/engine` · run `-run '^TestPlanNodeAction_Drop_WithEvacuateMarker$'` · `shingo-edge/engine/changeover_planner_test.go:116` @ `8e076d27` · tags `none`
- code: DEAD (the Drop read the old switch for its cutover wait; it reads the marks now)
- assertion map: a Drop whose cutover waits for the bin to leave → `TestPlanNodeAction_Drop_AtAMarkedNode` (the same wait, keyed on the node's mark)
- mutation: none to run: the field the test set is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestPlanNodeAction_Evacuate_Sequential`
- test: `TestPlanNodeAction_Evacuate_Sequential` · pkg `shingoedge/engine` · run `-run '^TestPlanNodeAction_Evacuate_Sequential$'` · `shingo-edge/engine/changeover_planner_test.go:394` @ `8e076d27` · tags `none`
- code: DEAD (the sequential evacuate builder and its `evacuate_sequential` tag are deleted; the planner plans a sequential position's ordinary swap and the decorator gives a marked one the tool change's leg)
- assertion map: one order for the position, its log tag → the tool change's leg on a sequential cell in `TestToolChange_TheTable`; the applier's log of every shape it creates, `TestLogChangeoverAction_EveryPlannedShapeSaysWhatItCreated`
- mutation: none to run: the builder it planned through is deleted
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestTopsAndBottomsPull_AReplayThatNoLongerResolvesFails`
- test: `TestTopsAndBottomsPull_AReplayThatNoLongerResolvesFails` · pkg `shingocore/engine` · run `-run '^TestTopsAndBottomsPull_AReplayThatNoLongerResolvesFails$'` · `shingo-core/engine/tops_and_bottoms_pull_docker_test.go:136` @ `8e076d27` · tags `docker`
- code: STALE (the find no longer looks a pull's press up from its outside: every Edge names the press, and intake is the one check, "refactor(core): the find looks no press up for a pull of tops & bottoms")
- assertion map: a waiting pull whose press stops carrying the part is failed at the replay with intake's sentence → `TestTopsAndBottomsPull_AReplayAsksForNoPress`: the same plant and the same edit, and the replay now takes the tops & bottoms that lands (the pull names its press; the Edge's START cancels SEND BACK's orders at a style change). Intake's refusal of a pull to no fed press's outside stays pinned by `TestTopsAndBottomsPull_IntakeRefuses`.
- mutation: the old test against the new find, before it was rewritten: `go test -count=1 -tags docker -run '^TestTopsAndBottomsPull_AReplayThatNoLongerResolvesFails$' ./engine/` → `tops_and_bottoms_pull_docker_test.go:150: the replay left the pull dispatched with "", want failed with "TBN-OUT is not the outside position of a press fed with TBN-PART"`
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestFanOutCrossMode_PressIndex2PosToPressIndex3Pos_PlansTheNewPositionAsASwap`
- test: `TestFanOutCrossMode_PressIndex2PosToPressIndex3Pos_PlansTheNewPositionAsASwap` · pkg `shingoedge/engine` · run `-run '^TestFanOutCrossMode_PressIndex2PosToPressIndex3Pos_PlansTheNewPositionAsASwap$'` · `shingo-edge/engine/changeover_diff_test.go:866` @ `0ef21ca1` · tags `none`
- code: STALE (a position only the incoming style uses is planned from its seat: an Add unless a bin is bound there, "fix(edge): one pass plans every press position, and a new one is a delivery")
- assertion map: BACK, named only by the 3-position incoming claim, gets one diff → `TestFanOutCrossMode_PressIndex2PosToPressIndex3Pos_PlansTheNewPositionFromItsSeat` (still one diff, still nothing at MIDDLE); BACK is the Swap with the incoming claim as its from-claim → the same test with a bin bound on BACK (`SeatBin`); with no seat read BACK is now the Add of the incoming claim, by design.
- mutation: the old assertions against the new pass, before the test was rewritten: `go test -count=1 -run '^TestZZOldBody$' ./engine/` → `BACK situation = "add", want SituationSwap`, `BACK FromClaim = <nil>, want the incoming claim's (the from-style didn't have this position)`
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestValidateNodeClaim_EvacDestinationIsFreeForm`
- test: `TestValidateNodeClaim_EvacDestinationIsFreeForm` · pkg `shingoedge/domain` · run `-run '^TestValidateNodeClaim_EvacDestinationIsFreeForm$'` · `shingo-edge/domain/claim_validation_test.go:220` @ `0ef21ca1` · tags `none`
- code: DEAD (the tool change's own clearance destination, `changeover_evac_destination`, is deleted and Edge v47 drops its column; a marked position clears to the outgoing claim's ordinary destination)
- assertion map: a free-form clearance destination (node or group, blank) is never refused → nothing: the field is gone
- mutation: none to run: the field the test set is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestResidentEvacDest_IgnoresTheToolingDestination`
- test: `TestResidentEvacDest_IgnoresTheToolingDestination` · pkg `shingoedge/engine` · run `-run '^TestResidentEvacDest_IgnoresTheToolingDestination$'` · `shingo-edge/engine/swap_evac_dest_fourth_door_test.go:96` @ `0ef21ca1` · tags `none`
- code: DEAD (the tool change's own clearance destination, `changeover_evac_destination`, is deleted and Edge v47 drops its column; a marked position clears to the outgoing claim's ordinary destination)
- assertion map: the resident carrier's aim ignores the clearance override → nothing: there is no override to ignore
- mutation: none to run: the field the test set is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestToolingChangeover_BlankDestinationMeansNormalRouting`
- test: `TestToolingChangeover_BlankDestinationMeansNormalRouting` · pkg `shingoedge/engine` · run `-run '^TestToolingChangeover_BlankDestinationMeansNormalRouting$'` · `shingo-edge/engine/changeover_position_builder_test.go:99` @ `0ef21ca1` · tags `none`
- code: DEAD (the tool change's own clearance destination, `changeover_evac_destination`, is deleted and Edge v47 drops its column; a marked position clears to the outgoing claim's ordinary destination)
- assertion map: with no override, nothing is redirected and the expanded leg is built against OutboundDestination → `TestToolingPositionAction_Shape` (the leg drops at the outgoing claim's outbound, MARKET) and `TestToolingBeatsRelabel`'s check that a marked position clears where an unmarked one does
- mutation: none to run: the decorator's override map and the press's evacDest field are deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestToolingClearanceOverrideRedirectsOnlyMarkedPositions`
- test: `TestToolingClearanceOverrideRedirectsOnlyMarkedPositions` · pkg `shingoedge/engine` · run `-run '^TestToolingClearanceOverrideRedirectsOnlyMarkedPositions$'` · `shingo-edge/engine/changeover_tooling_test.go:235` @ `0ef21ca1` · tags `none`
- code: DEAD (the tool change's own clearance destination, `changeover_evac_destination`, is deleted and Edge v47 drops its column; a marked position clears to the outgoing claim's ordinary destination)
- assertion map: a marked position goes to the override and an unmarked one does not → nothing: the override is gone; a marked position's clearance is the ordinary one (`TestToolingPositionAction_Shape`)
- mutation: none to run: the field the test set is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestToolingWithoutOverridePlansNoOutboundEdit`
- test: `TestToolingWithoutOverridePlansNoOutboundEdit` · pkg `shingoedge/engine` · run `-run '^TestToolingWithoutOverridePlansNoOutboundEdit$'` · `shingo-edge/engine/changeover_tooling_test.go:266` @ `0ef21ca1` · tags `none`
- code: DEAD (the tool change's own clearance destination, `changeover_evac_destination`, is deleted and Edge v47 drops its column; a marked position clears to the outgoing claim's ordinary destination)
- assertion map: with no override no outbound edit is queued; the staging hold is planned → nothing for the first (there is no edit to queue); the hold, `TestToolingAcrossDisjointNodes` and `TestToolChange_TheTable`
- mutation: none to run: the decorator's override map is deleted, so the test no longer compiles

### DEAD — `TestWithResidentEvacDest`
- test: `TestWithResidentEvacDest` · pkg `shingoedge/engine` · run `-run '^TestWithResidentEvacDest$'` · `shingo-edge/engine/swap_evac_dest_test.go:16` @ `0ef21ca1` · tags `none`
- code: DEAD (the Edge's resident aim is deleted, "refactor(edge): the Edge aims every leg at its claim's outbound"; Core sends a carrier aimed at another part's outbound to its own claim's, at the lift)
- assertion map: the copy that overrode one field of a claim → nothing: no claim is copied with another destination
- mutation: none to run: the function it called is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestResidentEvacDest_ForeignResident_RoutesToResidentHome`
- test: `TestResidentEvacDest_ForeignResident_RoutesToResidentHome` · pkg `shingoedge/engine` · run `-run '^TestResidentEvacDest_ForeignResident_RoutesToResidentHome$'` · `shingo-edge/engine/swap_evac_dest_test.go:119` @ `0ef21ca1` · tags `none`
- code: DEAD (the Edge's resident aim is deleted, "refactor(edge): the Edge aims every leg at its claim's outbound"; Core sends a carrier aimed at another part's outbound to its own claim's, at the lift)
- assertion map: a foreign carrier on the cell goes to its own home → `TestByContents_ACarrierOfAnotherPartGoesToItsOwnHome` (shingo-core dispatch: a swap leg at its release and the Material page's move send a full of another part to its own outbound; a drop no claim names stands); the swap's evac leg is aimed at the requested claim's outbound → `TestRequest_AForeignCarrierIsAimedAtTheRequestedClaimsOutbound`
- mutation: none to run: the function it called is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestResidentEvacDest_SameStyleResident_NoOverride`
- test: `TestResidentEvacDest_SameStyleResident_NoOverride` · pkg `shingoedge/engine` · run `-run '^TestResidentEvacDest_SameStyleResident_NoOverride$'` · `shingo-edge/engine/swap_evac_dest_test.go:167` @ `0ef21ca1` · tags `none`
- code: DEAD (the Edge's resident aim is deleted, "refactor(edge): the Edge aims every leg at its claim's outbound"; Core sends a carrier aimed at another part's outbound to its own claim's, at the lift)
- assertion map: the requested style's carrier keeps the claim's outbound → `TestReleaseNodeWithClaim_SameStyleResidentIsUnchanged`; Core leaves a carrier of the drop's own part alone, `TestByContents_ACarrierOfAnotherPartGoesToItsOwnHome` (shingo-core dispatch: a swap leg at its release and the Material page's move send a full of another part to its own outbound; a drop no claim names stands)
- mutation: none to run: the function it called is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestResidentEvacDest_UnknownsFailOpen`
- test: `TestResidentEvacDest_UnknownsFailOpen` · pkg `shingoedge/engine` · run `-run '^TestResidentEvacDest_UnknownsFailOpen$'` · `shingo-edge/engine/swap_evac_dest_test.go:194` @ `0ef21ca1` · tags `none`
- code: DEAD (the Edge's resident aim is deleted, "refactor(edge): the Edge aims every leg at its claim's outbound"; Core sends a carrier aimed at another part's outbound to its own claim's, at the lift)
- assertion map: an unread carrier keeps the claim's outbound → every leg keeps it now; Core asks only of a carrier whose part it holds (otherPartHome)
- mutation: none to run: the function it called is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestReleaseNodeWithClaim_RoutesTheResidentCarrierHome`
- test: `TestReleaseNodeWithClaim_RoutesTheResidentCarrierHome` · pkg `shingoedge/engine` · run `-run '^TestReleaseNodeWithClaim_RoutesTheResidentCarrierHome$'` · `shingo-edge/engine/swap_evac_dest_fourth_door_test.go:20` @ `0ef21ca1` · tags `none`
- code: STALE (the Material page's release is aimed at the claim's outbound and carries the carrier's own part; Core sends it home)
- assertion map: the release of a foreign carrier goes to its own home → `TestReleaseNodeWithClaim_AimsAtTheClaimsOutbound` (the move is to the claim's outbound, carrying the carrier's part) and `TestByContents_ACarrierOfAnotherPartGoesToItsOwnHome` (shingo-core dispatch: a swap leg at its release and the Material page's move send a full of another part to its own outbound; a drop no claim names stands)
- mutation: the old assertion against the new code, by reading: the same fixture now answers the claim's outbound (the new test), not the resident's home; not run, the old body no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestResidentEvacDest_PrefersTheCarriersOwnPayload`
- test: `TestResidentEvacDest_PrefersTheCarriersOwnPayload` · pkg `shingoedge/engine` · run `-run '^TestResidentEvacDest_PrefersTheCarriersOwnPayload$'` · `shingo-edge/engine/lineside_payload_test.go:22` @ `0ef21ca1` · tags `none`
- code: DEAD (the Edge's resident aim is deleted, "refactor(edge): the Edge aims every leg at its claim's outbound"; Core sends a carrier aimed at another part's outbound to its own claim's, at the lift)
- assertion map: the carrier's own part, from Core, decides its home → `TestByContents_ACarrierOfAnotherPartGoesToItsOwnHome` (shingo-core dispatch: a swap leg at its release and the Material page's move send a full of another part to its own outbound; a drop no claim names stands), which reads the bin itself
- mutation: none to run: the function it called is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestResidentEvacDest_MatchingPayloadIsNoOverride`
- test: `TestResidentEvacDest_MatchingPayloadIsNoOverride` · pkg `shingoedge/engine` · run `-run '^TestResidentEvacDest_MatchingPayloadIsNoOverride$'` · `shingo-edge/engine/lineside_payload_test.go:55` @ `0ef21ca1` · tags `none`
- code: DEAD (the Edge's resident aim is deleted, "refactor(edge): the Edge aims every leg at its claim's outbound"; Core sends a carrier aimed at another part's outbound to its own claim's, at the lift)
- assertion map: a carrier of the requested part keeps the claim's outbound → `TestReleaseNodeWithClaim_SameStyleResidentIsUnchanged`
- mutation: none to run: the function it called is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestResidentEvacDest_NoRecordedPayloadFallsThrough`
- test: `TestResidentEvacDest_NoRecordedPayloadFallsThrough` · pkg `shingoedge/engine` · run `-run '^TestResidentEvacDest_NoRecordedPayloadFallsThrough$'` · `shingo-edge/engine/lineside_payload_test.go:76` @ `0ef21ca1` · tags `none`
- code: DEAD (the Edge's resident aim is deleted, "refactor(edge): the Edge aims every leg at its claim's outbound"; Core sends a carrier aimed at another part's outbound to its own claim's, at the lift)
- assertion map: nothing recorded keeps the claim's outbound → every leg keeps it now
- mutation: none to run: the function it called is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestResidentEvacDest_AmbiguousPayloadDeclines`
- test: `TestResidentEvacDest_AmbiguousPayloadDeclines` · pkg `shingoedge/engine` · run `-run '^TestResidentEvacDest_AmbiguousPayloadDeclines$'` · `shingo-edge/engine/lineside_payload_test.go:94` @ `0ef21ca1` · tags `none`
- code: DEAD (the Edge's resident aim is deleted, "refactor(edge): the Edge aims every leg at its claim's outbound"; Core sends a carrier aimed at another part's outbound to its own claim's, at the lift)
- assertion map: two claims for the part at the node: no override → Core's otherPartHome keeps the drop and warns when the claims for the part disagree
- mutation: none to run: the function it called is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestChangeoverPlan_AimsTheOldCarrierAtItsOwnHome`
- test: `TestChangeoverPlan_AimsTheOldCarrierAtItsOwnHome` · pkg `shingoedge/engine` · run `-run '^TestChangeoverPlan_AimsTheOldCarrierAtItsOwnHome$'` · `shingo-edge/engine/changeover_already_done_test.go:115` @ `0ef21ca1` · tags `none`
- code: STALE (a changeover's Swap and Drop are aimed at the outgoing claim's outbound; Core sends a carrier of a third part to its own claim's)
- assertion map: the old carrier of a third style goes to its own home → `TestChangeoverPlan_AimsTheOldCarrierAtTheOutgoingClaimsOutbound` (aimed at the outgoing claim's outbound) and `TestByContents_ACarrierOfAnotherPartGoesToItsOwnHome` (shingo-core dispatch: a swap leg at its release and the Material page's move send a full of another part to its own outbound; a drop no claim names stands)
- mutation: the old assertion against the new code, by reading: the same fixture now answers the claim's outbound (the new test), not the resident's home; not run, the old body no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestClaimForLinesidePayload_IgnoresAClaimOfADeletedStyle`
- test: `TestClaimForLinesidePayload_IgnoresAClaimOfADeletedStyle` · pkg `shingoedge/store` · run `-run '^TestClaimForLinesidePayload_IgnoresAClaimOfADeletedStyle$'` · `shingo-edge/store/deleted_process_claims_test.go:57` @ `0ef21ca1` · tags `none`
- code: DEAD (ClaimForLinesidePayload is deleted with the resident aim, its one reader)
- assertion map: a deleted style's claim answers no lookup → Core's mirror is published from live claims of live styles (messaging/plant_claims_publisher.go reportFor); a deleted process's claims are retired, `TestDeleteProcess_RetiresItsClaims`
- mutation: none to run: the function it called is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestTestOrdersNodePicker_OffersOnlyValidNodes`
- test: `TestTestOrdersNodePicker_OffersOnlyValidNodes` · pkg `shingocore/www` · run `-run '^TestTestOrdersNodePicker_OffersOnlyValidNodes$'` · `shingo-core/www/handlers_test_orders_nodes_test.go:23` @ `4d13d68d` · tags `none`
- code: DEAD (buildNodePickerOptions is deleted: the Test orders page draws its nodes through the shared node adapter, shared/node-picker.js, as the Orders page does)
- assertion map: a disabled node is not offered → shown greyed "disabled" and not taken, `shared/testdata/components/node-picker.test.js` "no blanket rule…" and `core-pickers/test-orders-page.txt` (OLD-SLOT, GRP-OFF); every enabled node once, a group stays selectable and reads "group", a lane reads "lane", a slot reads neither and sits under its parent at any depth → node-picker.test.js "the tree…" and "cannot happen: a container whose children are lanes…", `test-orders-page.txt`'s rows; the two pages draw one list → `TestCorePickerPins` (`corePickerCensusBugs`, "the twins")
- mutation: none to run: the function it called is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestTemplates_NoCacheBustQuery`
- test: `TestTemplates_NoCacheBustQuery` · pkg `shingocore/www` · run `-run '^TestTemplates_NoCacheBustQuery$'` · `shingo-core/www/static_asset_query_guard_test.go:24` @ `7b695346` · tags `none`
- code: STALE (templates name static files through the asset handler's `assetURL`, and a hashed file name is the one cache-bust form; "test(www): every page's import map covers what it reaches, in both browsers, inside a budget", c71e6b49)
- assertion map: no template stamps a `?v=`/query on a /static URL → `TestTemplates_OnlyTheHandlersHashForm` (same file: a query is still refused, and the only versioned form allowed is the handler's own hash)
- mutation: the old assertion against the new code, by reading: a template with `?v=1` still fails the new test; not run, the old body was replaced in place
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestTemplates_NoCacheBustQuery`
- test: `TestTemplates_NoCacheBustQuery` · pkg `shingoedge/www` · run `-run '^TestTemplates_NoCacheBustQuery$'` · `shingo-edge/www/static_asset_query_guard_test.go:24` @ `7b695346` · tags `none`
- code: STALE (as Core's: the asset handler's hashed file name is the one versioned form, c71e6b49)
- assertion map: no template stamps a query on a /static URL → `TestTemplates_OnlyTheHandlersHashForm` (same file)
- mutation: the old assertion against the new code, by reading: a template with `?v=1` still fails the new test; not run, the old body was replaced in place
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestReplenishmentPage_AMintedStyleReadsNotSet`
- test: `TestReplenishmentPage_AMintedStyleReadsNotSet` · pkg `shingoedge/www` · run `-run '^TestReplenishmentPage_AMintedStyleReadsNotSet$'` · `shingo-edge/www/handlers_flow_mint_test.go:238` @ `7b695346` · tags `none`
- code: STALE (reorder_point_source is deleted, and with it the "not set" badge; "refactor(edge): delete reorder_point_source; Edge v48 drops it from both claims tables", 617da174)
- assertion map: a minted style's claims read "not set" → `TestReplenishmentPage_AMintedStyleReadsOff` (same file: its rows read reorder point 0, Auto reorder off, and offer Clear)
- mutation: the old assertion against the new code, by reading: no badge exists to read; the rows' 0/off is what the badge's "not set" stood for
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestBetweenBins_PastThePackDropsThePileAndLogs`
- test: `TestBetweenBins_PastThePackDropsThePileAndLogs` · pkg `shingoedge/engine` · run `-run '^TestBetweenBins_PastThePackDropsThePileAndLogs$'` · `shingo-edge/engine/produce_between_bins_test.go:76` @ `b0e65e2e` · tags `none`
- code: STALE (a pile past one pack is thrown away whole and the seat stops until a cart binds, SPR ALN_011 D2/D3; "fix(edge): past one pack, throw the whole pile away and stop the seat")
- assertion map: stroke 133 drops the pile of 132 as no_carrier, clears the mark, the next bind's tick is +1 with a no_bin row of 5 → `TestBetweenBins_PastThePackThrowsThePileAwayAndStops` (same file: one hold_cap row of 132, the mark cleared, the seat stopped; the strokes after it join the stretch, a carrier coming included; the next bind's tick is +1 with a no_bin row of 6)
- mutation: the old assertion against the new code: `produce_between_bins_test.go:89: drop 0 = {... Qty:132 Reason:hold_cap ...}, want {... Qty:132 Reason:no_carrier ...}`
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestStrictPack_HoldCapDropsTheExcessOverOnePack`
- test: `TestStrictPack_HoldCapDropsTheExcessOverOnePack` · pkg `shingoedge/engine` · run `-run '^TestStrictPack_HoldCapDropsTheExcessOverOnePack$'` · `shingo-edge/engine/wiring_counter_delta_holdreplay_test.go:479` @ `b0e65e2e` · tags `none`
- code: STALE (as above: the bound strict seat at its pack throws the pile away whole, D2)
- assertion map: a full bin with 131 held takes 5: the hold is capped at 132 with one hold_cap row of 4 → `TestStrictPack_PastOnePackThrowsThePileAwayAndStops` (same file: the hold goes to 0 with one hold_cap row of 131, the seat stops, and the 5 and the 3 after them join the no-cart stretch as 8)
- mutation: the old assertion against the new code: `wiring_counter_delta_holdreplay_test.go:483: seat = count 132, hold 0 of ""; want count 132, hold 132 of "WIDGET-A"` and `:492: drop = {... Qty:131 Reason:hold_cap ...}, want hold_cap of 4 WIDGET-A`
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestUpdateCellReorder_AClaimNamingALane`
- test: `TestUpdateCellReorder_AClaimNamingALane` · pkg `shingoedge/engine` · run `-run '^TestUpdateCellReorder_AClaimNamingALane$'` · `shingo-edge/engine/claim_leg_writers_test.go:117` @ `aea6321d` · tags `none`
- code: DEAD (`engine.UpdateCellReorder` went with the Replenishment page it backed: the reorder-idle build, E5)
- assertion map: a reorder edit of a claim naming a lane on any of five legs is refused → the same refusal at the one writer left, a flow save: `TestSaveFlow_ALaneOnEachLeg` (`claim_leg_writers_test.go`, unchanged) walks the composer's save over a lane on each leg.
- mutation: none needed: the code under test is deleted, and the test no longer compiles against the tree.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestUpdateCellReorder_LeavesUnmentionedColumnsAlone`
- test: `TestUpdateCellReorder_LeavesUnmentionedColumnsAlone` · pkg `shingoedge/engine` · run `-run '^TestUpdateCellReorder_LeavesUnmentionedColumnsAlone$'` · `shingo-edge/engine/claim_partial_update_test.go:88` @ `aea6321d` · tags `none`
- code: DEAD (`engine.UpdateCellReorder` went with the Replenishment page: E5)
- assertion map: a reorder-point edit through the page's door leaves every optional column as it was → the contract with no door: `TestPartialClaimUpdate_LeavesUnmentionedColumnsAlone` and `TestClaimUpdate_ExplicitEmptyStillClears` (same file, unchanged). The file header now names the page as the history and the contract as what stays pinned.
- mutation: none needed: the code under test is deleted.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestSaveFlow_ARestatedReorderPointLeavesThePreviewCurrent`
- test: `TestSaveFlow_ARestatedReorderPointLeavesThePreviewCurrent` · pkg `shingoedge/engine` · run `-run '^TestSaveFlow_ARestatedReorderPointLeavesThePreviewCurrent$'` · `shingo-edge/engine/flow_restated_reorder_test.go:16` @ `aea6321d` · tags `none`
- code: DEAD (its subject was the Replenishment page's Apply on an unchanged row; the page and its write are gone, E5, and the provenance stamp it once moved left in v48)
- assertion map: none carried: there is no second writer of a claim's reorder point outside the flow save the preview belongs to.
- mutation: none needed: the code under test is deleted.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestReplenishmentPage_AMintedStyleReadsOff`
- test: `TestReplenishmentPage_AMintedStyleReadsOff` · pkg `shingoedge/www` · run `-run '^TestReplenishmentPage_AMintedStyleReadsOff$'` · `shingo-edge/www/handlers_flow_mint_test.go:238` @ `aea6321d` · tags `none`
- code: DEAD (it read `/replenishment`, which is gone: E5)
- assertion map: a minted style's claims render on the page as point 0, auto-reorder off, with Clear → not re-pinned on Processes: the reorder cell draws a claim from its own point and flag (`processes-desktop.js` `reorderCell`), and no test reads that cell for a minted style.
- mutation: none needed: the page under test is deleted.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestInputFromClaimUngated_SpeaksNoGatedColumn`
- test: `TestInputFromClaimUngated_SpeaksNoGatedColumn` · pkg `shingoedge/domain` · run `-run '^TestInputFromClaimUngated_SpeaksNoGatedColumn$'` · `shingo-edge/domain/flow_ungated_test.go:25` @ `aea6321d` · tags `none`
- code: DEAD (`domain.InputFromClaimUngated` lost its only production caller, the Replenishment page's edit, and goes with it: E5)
- assertion map: none carried: no partial writer of a whole claim remains. The store test that re-saved a stale row through it (`TestUpsertClaim_StoresNoPairOnAModeThatNamesNone`) re-saves it through `InputFromClaim`, the whole echo, and asserts the same thing.
- mutation: none needed: the function under test is deleted.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestInputFromClaimUngated_SpeaksEveryUnconditionalColumn`
- test: `TestInputFromClaimUngated_SpeaksEveryUnconditionalColumn` · pkg `shingoedge/domain` · run `-run '^TestInputFromClaimUngated_SpeaksEveryUnconditionalColumn$'` · `shingo-edge/domain/flow_ungated_test.go:59` @ `aea6321d` · tags `none`
- code: DEAD (as the row above)
- assertion map: none carried (as above).
- mutation: none needed: the function under test is deleted.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestAcquiringStatusesRaiseAnAnomalyButAreNeverSwept`
- test: `TestAcquiringStatusesRaiseAnAnomalyButAreNeverSwept` · pkg `shingo/protocol` · run `-run '^TestAcquiringStatusesRaiseAnAnomalyButAreNeverSwept$'` · `protocol/status_test.go:470` @ `aea6321d` · tags `none`
- code: STALE (the never-swept half (`IsStuckSweepCandidate` false for both acquiring statuses, and the sweep set non-empty) had nothing left to answer: the predicate and the sweep are gone (reorder-idle build, C1))
- assertion map: the anomaly half, unchanged → `TestAcquiringStatusesRaiseAnAnomaly` (same file): every acquiring status is a runtime-stuck candidate.
- mutation: none needed: the code under test is deleted or the assertion is unchanged under a new name.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestFleetRefusal_ADemotedOrderIsOutOfTheDestructiveSweep`
- test: `TestFleetRefusal_ADemotedOrderIsOutOfTheDestructiveSweep` · pkg `shingocore/dispatch` · run `-run '^TestFleetRefusal_ADemotedOrderIsOutOfTheDestructiveSweep$'` · `shingo-core/dispatch/fleet_demote_sweep_docker_test.go:48` @ `aea6321d` · tags `docker`
- code: STALE (the sweep half asked whether a demoted order could age into `AbandonStuckOrders`; the sweep is gone (C1))
- assertion map: the watched half, unchanged → `TestFleetRefusal_ADemotedOrderIsWatched` (same file): a demoted order rests in a runtime-stuck candidate status.
- mutation: none needed: the code under test is deleted or the assertion is unchanged under a new name.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestApiOrders_CreationRequiresAuth_LifecycleDoesNot`
- test: `TestApiOrders_CreationRequiresAuth_LifecycleDoesNot` · pkg `shingoedge/www` · run `-run '^TestApiOrders_CreationRequiresAuth_LifecycleDoesNot$'` · `shingo-edge/www/handlers_api_orders_test.go:71` @ `aea6321d` · tags `none`
- code: STALE (it built its own copy of the routes, so a change to `router.go` could never fail it; and Submit and Abort moved behind login (the Orders page is their only caller; Abort writes a person's pause))
- assertion map: creation needs a login, unchanged; release stays public, unchanged; submit and abort now answer 303 signed out → `TestApiOrders_CreationSubmitAbortNeedLogin_ReleaseDoesNot` (same file), on the real `NewRouter`.
- mutation: moving submit or abort back to the public group turns the new test red.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: `shingo-edge/www/router.go`'s comment names the new test
- verified at: the commit that adds this row

### STALE — `TestStagedParent_OperatorStagedRow_StaysWithTheAbandonSweep`
- test: `TestStagedParent_OperatorStagedRow_StaysWithTheAbandonSweep` · pkg `shingocore/dispatch` · run `-run '^TestStagedParent_OperatorStagedRow_StaysWithTheAbandonSweep$'` · `shingo-core/dispatch/compound_staged_docker_test.go:232` @ `aea6321d` · tags `docker`
- code: STALE (the name and text said an operator-staged row was the abandon sweep's; the sweep is gone (C1) and the row is a person's (it waits on RELEASE))
- assertion map: unchanged assertion (`SweepStalledChapters` leaves an operator-staged parent alone) → `TestStagedParent_OperatorStagedRow_StaysWithAPerson` (same file), renamed with its comment and failure text.
- mutation: none needed: the code under test is deleted or the assertion is unchanged under a new name.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestAbandonStuckOperatorGatedStaging`
- test: `TestAbandonStuckOperatorGatedStaging` · pkg `shingocore/engine` · run `-run '^TestAbandonStuckOperatorGatedStaging$'` · `shingo-core/engine/reconciliation_service_test.go:399` @ `aea6321d` · tags `docker`
- code: DEAD (`AbandonStuckOrders` and its 4 h class are removed (C1, owner ruling 2026-11-14: Core keeps no automatic cancel of a waiting order))
- assertion map: none carried: the Edge's idle rule replaces the bound (`shingo-edge/engine/idle_cancel_test.go`).
- mutation: none needed: the code under test is deleted or the assertion is unchanged under a new name.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestAbandonStuckOrders`
- test: `TestAbandonStuckOrders` · pkg `shingocore/engine` · run `-run '^TestAbandonStuckOrders$'` · `shingo-core/engine/reconciliation_service_test.go:335` @ `aea6321d` · tags `docker`
- code: DEAD (`AbandonStuckOrders` is removed (C1))
- assertion map: none carried: nothing in Core cancels on a timer; the Edge's idle rule sends back parked robots at an idle line (`TestIdleCancel_TheHourSendsBackAParkedPairAndSwitchesOff`).
- mutation: none needed: the code under test is deleted or the assertion is unchanged under a new name.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestAbandonSweep_ExemptsVacateWait`
- test: `TestAbandonSweep_ExemptsVacateWait` · pkg `shingocore/engine` · run `-run '^TestAbandonSweep_ExemptsVacateWait$'` · `shingo-core/engine/reconciliation_service_test.go:618` @ `aea6321d` · tags `docker`
- code: DEAD (an exemption of the removed sweep (C1))
- assertion map: none carried: with no sweep there is nothing to exempt a vacate hold from; its own bounds (the lift, the lifter's end, the hard release) are pinned in `dispatch`.
- mutation: none needed: the code under test is deleted or the assertion is unchanged under a new name.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestGateStagedNotSwept`
- test: `TestGateStagedNotSwept` · pkg `shingocore/engine` · run `-run '^TestGateStagedNotSwept$'` · `shingo-core/engine/reconciliation_service_test.go:540` @ `aea6321d` · tags `docker`
- code: DEAD (an exemption of the removed sweep (C1))
- assertion map: none carried: nothing cancels a gate-staged order on a timer now.
- mutation: none needed: the code under test is deleted or the assertion is unchanged under a new name.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestPreDispatchNotSwept`
- test: `TestPreDispatchNotSwept` · pkg `shingocore/engine` · run `-run '^TestPreDispatchNotSwept$'` · `shingo-core/engine/reconciliation_service_test.go:470` @ `aea6321d` · tags `docker`
- code: DEAD (an exemption of the removed sweep (C1))
- assertion map: none carried: nothing cancels a pre-dispatch order on a timer now; the Edge's idle rule leaves one alone (`TestIdleCancel_WhatItLeavesAlone`, "no robot has reached it").
- mutation: none needed: the code under test is deleted or the assertion is unchanged under a new name.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestStuckContext`
- test: `TestStuckContext` · pkg `shingocore/engine` · run `-run '^TestStuckContext$'` · `shingo-core/engine/cancel_return_test.go:58` @ `aea6321d` · tags `none`
- code: DEAD (`stuckContext`, the parenthesis the removed sweep added to its reason, goes with it (C1))
- assertion map: none carried.
- mutation: none needed: the code under test is deleted or the assertion is unchanged under a new name.

### DEAD — `TestSceneGraph_AdjacencyIsUndirected`
- test: `TestSceneGraph_AdjacencyIsUndirected` · pkg `shingoedge/engine` · run `-run '^TestSceneGraph_AdjacencyIsUndirected$'` · `shingo-edge/engine/scene_graph_test.go:49` @ `aea6321d` · tags `none`
- code: DEAD (`engine.SceneAdjacency` is deleted: no caller in any of the five modules, owner follow-up F3; the key-route picker it was kept for walks the directed shared route model, shared/scene-net.js sceneRoute)
- assertion map: an undirected neighbour list, duplicates, self-segments and half-named segments dropped → nothing: no code asks for an undirected adjacency now. The directed route model the picker walks is pinned by `TestSceneNetRoutesAsTheDesktopDoes` (shared/testdata/js/scene-net.test.js)
- mutation: none to run: the function it called is deleted, so the test no longer compiles
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestFlowSave_GatedOffStationBodyIsRefused`
- test: `TestFlowSave_GatedOffStationBodyIsRefused` · pkg `shingoedge/www` · run `-run '^TestFlowSave_GatedOffStationBodyIsRefused$'` · `shingo-edge/www/handlers_flow_mint_test.go:97` @ `aea6321d` · tags `none`
- code: DEAD (the station's flow save: no station sends one. The operator station's composer previews and mints and never posts `flow/save`; the route is behind login since "fix(www): saving a style's flow needs a signed-in person", and the body builder it posted, composer-model.js's `flowSaveBody`, had no caller and is removed with it)
- assertion map: none carried for the save: an unauthenticated save is the login bounce (`TestRouter_FlowSaveIsBehindLogin`, `shingo-edge/www/router_flow_save_gate_test.go:20`). HMI setup's gate on the station's one write stays pinned from the station's own body (`TestFlowMint_SetupGatesTheStationsMint`, `shingo-edge/www/handlers_flow_mint_test.go:95`).
- mutation: none needed: the code under test is deleted or the assertion is unchanged under a new name.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestSaveFlow_RunningPositionMoveRefusesBothDoorsByName`
- test: `TestSaveFlow_RunningPositionMoveRefusesBothDoorsByName` · pkg `shingoedge/www` · run `-run '^TestSaveFlow_RunningPositionMoveRefusesBothDoorsByName$'` · `shingo-edge/www/handlers_flow_running_move_test.go:41` @ `aea6321d` · tags `none`
- code: STALE
- assertion map: renamed `TestSaveFlow_RunningPositionMoveRefusesByName` (`shingo-edge/www/handlers_flow_running_move_test.go:38`). The "HMI door" it also walked, a save with no session, no longer reaches the handler (flow save is behind login: `TestRouter_FlowSaveIsBehindLogin`). The desktop door's assertions are kept unchanged: 409 and the sentence naming the position, the bin and the move (`:122-127`), not stale (`:128-130`), and nothing moved after the refusal (`:134-146`). The gate-opening it did for the HMI door is gone: HMI setup does not gate a desktop save.
- mutation: none needed: the code under test is deleted or the assertion is unchanged under a new name.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### STALE — `TestFlow_APressWhoseStylesDisagreeOnTheRefillIsWarnedNotRefused`
- test: `TestFlow_APressWhoseStylesDisagreeOnTheRefillIsWarnedNotRefused` · pkg `shingoedge/engine` · run `-run '^TestFlow_APressWhoseStylesDisagreeOnTheRefillIsWarnedNotRefused$'` · `shingo-edge/engine/refill_by_disagreement_test.go:21` @ `aea6321d` · tags `none`
- code: STALE (the behaviour it pinned is ruled wrong: which robot brings a press its next carrier may differ by style, owner 2026-11-14, "thats the whole point of styles. it can change by part". The warning it asserted, `domain.RefillByDisagreementFinding`, is deleted with its preview and save calls)
- assertion map: rewritten as `TestFlow_TwoStylesMayDifferOnTheNextCarrier` (`shingo-edge/engine/refill_by_per_style_test.go:19`) on the same fixture: the preview of a press whose two styles differ carries no next-carrier finding, the save goes through, and the saved claim keeps this style's Robot 2. Red before the deletion (the warning was present), green after.
- mutation: the new test was seen red at the base, with the deleted code in place.
- coverage Δ: not measured for this branch (no before/after profile was taken)
- citations rewritten: none outside this ledger
- verified at: the commit that adds this row

### DEAD — `TestRobotMotionSaysWhenARobotMoves`
- test: `TestRobotMotionSaysWhenARobotMoves` · pkg `shingo/shared` · run `-run '^TestRobotMotionSaysWhenARobotMoves$'` · `shared/scene_geom_test.go:148` @ `02439175` · tags `none`
- code: DEAD
- assertion map: the test ran `shared/testdata/js/robot-motion.test.js` over `movedFrom` and `movingAt`, which "feat(core,edge): both robot maps read Core's word on which robots move" removes from every module: whether a robot moves is judged on Core's robot poll since "feat(core): Core's robot poll says which robots are moving", and `TestRobotMotion_OneDefinitionForBothSurfaces` (`shared/scene_geom_test.go:153`, the check at `:170`) fails a page that names either. Each case moves to the Go rule, `TestRobotMotion_AMovePastTheJitterWithinTheLinger`:
  - `robot-motion.test.js:32-39` (the jitter: 19 cm and 21 cm at 10 m rows, both axes, the 5 cm floor at 1 m rows, no scene, standing still) → `shingo-core/engine/robot_motion_test.go:37-44`, the same cases by name (the no-scene case split in two).
  - `:24-26` (moving at the move, within the linger, not once it has run out) → `:67-75`, on the poll's clock (moving at once, still 4 s on, not at 5 s).
  - `:27-28` (never seen moving, 0 and absent) → `:64-65` (a first sight is no move) and `:77-78` (a robot back after a poll that did not list it).
- mutation: the jitter share 2% → 3%, at the base in `shared/robot-motion.js` `movedFrom` (`scale * 0.02` → `scale * 0.03`), at the tip in `shingo-core/engine/robot_motion.go` (`robotMoveShare = 0.02` → `0.03`) · old red at base: `go test -count=1 -run '^TestRobotMotionSaysWhenARobotMoves$' .` in `shared` → `FAIL 10 m rows: 21 cm is a move`, `FAIL the move is a distance, both axes counted`, `--- FAIL: TestRobotMotionSaysWhenARobotMoves (0.08s)` · survivor red at tip: `go test -count=1 -run '^TestRobotMotion_AMovePastTheJitterWithinTheLinger$' ./engine/` in `shingo-core` → `robot_motion_test.go:47: 10 m rows: 21 cm is a move: moving = false, want true`, `--- FAIL: TestRobotMotion_AMovePastTheJitterWithinTheLinger (0.00s)`
- coverage Δ: not measured (no before/after profile); the test ran Node over a JS file and covered no Go statement of its own, and `runSceneJS` stays, read by the other scene tests
- citations rewritten: none (nothing outside its own file named it)
- verified at: the commit that adds this row

### STALE — `TestDemoteAfterFleetRefusal_RepeatedRefusalTellsOnce`
- test: `TestDemoteAfterFleetRefusal_RepeatedRefusalTellsOnce` · pkg `shingocore/dispatch` · run `-run '^TestDemoteAfterFleetRefusal_RepeatedRefusalTellsOnce$'` · `shingo-core/dispatch/dispatcher_test.go:1285` @ `b7d82846` · tags `docker`
- code: STALE (rewritten in place as `TestDemoteAfterFleetRefusal_EachRefusalTellsTheDispatchAndThePark`, then renamed `TestDemoteAfterFleetRefusal_OnlyTheFirstRefusalTellsThePark`, `shingo-core/dispatch/dispatcher_test.go:1313`, when `dispatched` became the fleet's yes: the station hears `queued v2` once over the three refusals, the base's one tell again)
- assertion map: the fixture, the three refusals and the per-pass status check (`queued` under `fleet_unavailable`, `:1314-1317` → `:1345-1348`) are kept unchanged. The count of wait-changed tells (`:1320-1323`, one over three refusals) becomes the list of the order's station tells with their versions (`:1352-1355`, `dispatched v2, queued v3` per pass). "feat(core): the station is told by the write that changed its order" tells the station from every status write: the hand's `dispatched` was told nobody before, and the park's `queued` was told once for the whole window. The caller's `told` snapshot, the only input the old assertion read, went with `NotifyWaitChanged`.
- mutation: the park tells nothing. At the base in `shingo-core/dispatch/dispatcher.go` `DemoteAfterFleetRefusal` (`d.NotifyWaitChanged(order)` → `_ = order`), at the tip in `shingo-core/store/order_tell.go` `queueTellTx` (`if told.Status == protocol.StatusQueued { return nil }` before the tell is built) · old red at base: `go test -count=1 -tags docker,lockcheck -run '^TestDemoteAfterFleetRefusal_RepeatedRefusalTellsOnce$' ./dispatch/` → `dispatcher_test.go:1321: wait-changed tells = 0 over 3 refusals, want 1` · survivor red at tip: `go test -count=1 -tags docker,lockcheck -run '^TestDemoteAfterFleetRefusal_OnlyTheFirstRefusalTellsThePark$' ./dispatch/` → `dispatcher_test.go:1349: station tells over 3 refusals = [], want [queued v2]`
- coverage Δ: not measured (no before/after profile); the rewritten test drives the same `DispatchDirect` and `DemoteAfterFleetRefusal` calls, and the tell it reads is written by `queueTellTx`
- citations rewritten: none (nothing outside its own file named it)
- verified at: the commit that adds this row

### DEAD — `TestReplySender_SendUpdate`
- test: `TestReplySender_SendUpdate` · pkg `shingocore/dispatch` · run `-run '^TestReplySender_SendUpdate$'` · `shingo-core/dispatch/replies_test.go:125` @ `b7d82846` · tags `docker`
- code: DEAD
- assertion map: `ReplySender.SendUpdate` is gone: "feat(core): the station is told by the write that changed its order" retires the per-call-site status sends, and every `order.update` is queued by the status write itself (`shingo-core/store/order_tell.go` `queueTellTx`). Each assertion moves to the tell's pins in `shingo-core/engine/status_tell_pin_test.go`:
  - one outbox row of type `order.update` (`replies_test.go:141-146`) → `requireTells` per writer (`status_tell_pin_test.go:112-121`), which reads every row the step adds, its type and its station;
  - the order's uuid and status (`:152-157`) → the same rows, read back from the payload (`tellsAfter`, `:57-87`);
  - the detail (`:158-160`) → the confirm's tell carrying its history sentence (`:288-291`).
- mutation: the update carries no detail. At the base in `shingo-core/dispatch/reply_sender.go` `SendUpdate` (`Detail: detail` → `Detail: ""`), at the tip in `shingo-core/store/order_tell.go` `queueTellTx` (the default to the write's detail removed) · old red at base: `go test -count=1 -tags docker,lockcheck -run '^TestReplySender_SendUpdate$' ./dispatch/` → `replies_test.go:159: detail = "", want "awaiting inventory"` · survivor red at tip: `go test -count=1 -tags docker,lockcheck -run '^TestStatusTellPin_RowsPerTypedWriter$/^ConfirmReceipt$' ./engine/` → `status_tell_pin_test.go:290: confirmed update = [...Detail: ...], want detail "receipt: confirmed, count: 1" and robot AMB-01`
- coverage Δ: not measured (no before/after profile); `SendUpdate` has no caller at the tip, and `queueTellTx` is reached by every status writer's pin
- citations rewritten: none (nothing outside its own file named it)
- verified at: the commit that adds this row

### STALE — `TestHandover_CreateFailureLeavesTheOrderClaimedAndDoesNotRollBack`
- test: `TestHandover_CreateFailureLeavesTheOrderClaimedAndDoesNotRollBack` · pkg `shingocore/dispatch` · run `-run '^TestHandover_CreateFailureLeavesTheOrderClaimedAndDoesNotRollBack$'` · `shingo-core/dispatch/fleet_handover_test.go:121` @ `b7d82846` · tags `docker`
- code: STALE (rewritten in place as `TestHandover_CreateFailureClosesTheAskAndLeavesTheOrderWhereItRests`, `shingo-core/dispatch/fleet_handover_test.go:121`)
- assertion map: the fixture, the refusing fleet and the call are kept. "status = dispatched, the claim stands" becomes "status = queued as it rested, the ask closed (in the row and in hand), the error AskClosed"; "vendor_order_id empty" is kept. `dispatched` became the fleet's yes: the hand opens an ask and writes no status, and a refusal closes the ask and demotes the hand in one transaction (refuseAsk), so there is no `dispatched` to leave and no claim left standing.
- mutation: the refusal does not close the ask. At the tip in `shingo-core/dispatch/fleet_handover.go` `handoverToFleet` (`if !d.refuseAsk(order, vendorOrderID) {` → `if true {`) · old red at base: not run (the base wrote `dispatched` in the hand; the assertion it pinned is the behaviour this change removes) · survivor red at tip: `go test -count=1 -tags docker,lockcheck -run '^TestHandover_CreateFailureClosesTheAskAndLeavesTheOrderWhereItRests$' ./dispatch/` → `fleet_handover_test.go:145: err = mock: not connected (*errors.errorString), want AskClosed` and `fleet_handover_test.go:151: status queued, ask "sg-1-…" (in hand "sg-1-…"); want queued, as it rested, with the ask closed`
- coverage Δ: not measured (no before/after profile); the rewritten test drives the same `dispatchToFleetCore` call
- citations rewritten: none (nothing outside its own file named it)
- verified at: the commit that adds this row

### DEAD — `TestDeckClassifierMatchesEngine`
- test: `TestDeckClassifierMatchesEngine` · pkg `shingocore/fleet/simulator` · run `-run '^TestDeckClassifierMatchesEngine$'` · `shingo-core/fleet/simulator/deck_classifier_parity_test.go:17` @ `82c487ff` · tags `sim`
- code: DEAD
- assertion map: the test held the simulator's copy of the engine's binTask classifier (`deckLoadsOn` / `deckEmptiesOn` against `engine.IsPickupBlock` / `engine.IsDropoffBlock`) to the original over 23 names. Both copies are gone: the deck reads the block's `fleet.OrderBlock.Action`, the label Core sent the block with, and no name is classified on either side, so there is no second copy to drift. What the deck does with the action is held by `TestDeck_LoadsOnPickupAndEmptiesOnFinalUnload` (`shingo-core/fleet/simulator/driver_deck_test.go:90`).
- mutation: the deck loading on a wait instead of a pickup (`shingo-core/fleet/simulator/deck_sim.go`, `case protocol.ActionPickup` → `case protocol.ActionWait` in `blockCompleted`) · survivor red: `go test -tags sim -count=1 -run '^TestDeck_LoadsOnPickupAndEmptiesOnFinalUnload$' ./fleet/simulator/` in `shingo-core` → `driver_deck_test.go:100: after the pickup AMR-01 should be busy, loaded, at A; got {ID:AMR-01 Busy:true At:A Loaded:false Fault:false}`
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: `shingo-core/fleet/simulator/deck_sim.go` (the comment that named the parity test is deleted with the copy)
- verified at: the commit that adds this row

### FOLD — `TestIsPickupBlock`
- test: `TestIsPickupBlock` · pkg `shingocore/engine` · run `-run '^TestIsPickupBlock$'` · `shingo-core/engine/wiring_block_completed_test.go:13` @ `82c487ff` · tags `none`
- code: FOLD
- assertion map: `IsPickupBlock` and `IsDropoffBlock` are folded into `legacyBlockAction`, the name rule kept for the orders sent before `orders.block_actions` and the leg rows written before legs carried their action. Its 21 cases are rows of `TestLegacyBlockAction` (`shingo-core/engine/wiring_block_completed_test.go:20`), each name with the action it reads as: the 10 pickups as `pickup`, the 8 non-pickups as the action the dual table gave them (`dropoff` for the unload, drop and release names, `wait` for Wait, `""` for Script and Navigate), the 3 substring cases as before.
- mutation: the substring precedence swapped, so "load" is tested before "unload" (`shingo-core/engine/wiring_block_completed.go`, `legacyBlockAction`) · survivor red: `go test -count=1 -run '^TestLegacyBlockAction$' ./engine/` in `shingo-core` → `legacyBlockAction("unloadCustomA") = "pickup", want "dropoff" — unload substring beats load`
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: `shingo-core/cmd/soakstat/bin_residence_overlap_docker_test.go` (`mustBlock`'s comment), `shingo-core/engine/stranded_transit.go` (`pickupAt`'s comment)
- verified at: the commit that adds this row

### FOLD — `TestIsDropoffBlock`
- test: `TestIsDropoffBlock` · pkg `shingocore/engine` · run `-run '^TestIsDropoffBlock$'` · `shingo-core/engine/wiring_block_completed_test.go:57` @ `82c487ff` · tags `none`
- code: FOLD
- assertion map: folded with `TestIsPickupBlock` into `TestLegacyBlockAction` (`shingo-core/engine/wiring_block_completed_test.go:20`): its 19 cases are rows there, each name with its one action, so the disjointness it checked (no name read as both) holds by construction: a name has one action.
- mutation: as `TestIsPickupBlock`'s row · survivor red: `legacyBlockAction("customUnloadSpecial") = "pickup", want "dropoff" — substring match on 'unload'`
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: as `TestIsPickupBlock`'s row
- verified at: the commit that adds this row

### STALE — `TestBuildMovement_DerivesFromUOPTimesPartsPerCycle`
- test: `TestBuildMovement_DerivesFromUOPTimesPartsPerCycle` · pkg `shingocore/material` · run `-run '^TestBuildMovement_DerivesFromUOPTimesPartsPerCycle$'` · `shingo-core/material/material_test.go:317` @ `68105255` · tags `none`
- code: STALE
- assertion map: the test pinned a CMS posting of a one-line template's bin as `uop_remaining × parts_per_cycle` (8 left at a ratio of 2 posted 16). The owner ruled (2026-11-18) that a bin of one part counts pieces and the line's ratio is an assembly's consume setting no reader multiplies, so the rule it pinned is retired, not lost. Its fixture moves whole to `TestBuildMovement_OnePartPostsTheBinsCount` (`shingo-core/material/material_test.go:321`), asserting the bin's count (−8/+8); a kit keeps the product and its pin, `TestBuildMovement_MultiPartTemplateProducesTwoRowsPerPart`, is unchanged.
- mutation: a one-line template given its ratio again (`shingo-core/material/material.go` `postablePerUnit`, `len(template) == 1` → never) · survivor red at tip: `go test -count=1 -run '^TestBuildMovement_OnePartPostsTheBinsCount$' ./material/` in `shingo-core` → `material_test.go:340: deltas = -16, 16; want -8, +8 (the bin's 8 pieces, not 8 x the ratio 2)`
- coverage Δ: not measured (no before/after profile was taken); the replacement runs the same fixture through the same function
- citations rewritten: `docs/uop-architecture.md` names the replacement; the replacement's own comment names the retired test
- verified at: the commit that adds this row

### STALE — `TestBaselineDDL_DeclaresEveryTable`
- test: `TestBaselineDDL_DeclaresEveryTable` · pkg `shingocore/store/schema` · run `-run '^TestBaselineDDL_DeclaresEveryTable$'` · `shingo-core/store/schema/ddl_totality_test.go:91` @ `e06b29b0` · tags `none`
- code: STALE (rewritten as `TestBaseline_CreatesEveryShippedTable`, same file, now package `schema_test`). It asked which half of the replaying schema each shipped table lived in, the baseline DDL constant or a migration named in `migrationOnlyTables`, and that the constant declared no table the snapshot had lost. The squash at v183 deletes the constant and leaves one half: the generated baseline creates every table.
- assertion map: every snapshot table is in the baseline DDL or `migrationOnlyTables` → every snapshot table is created by `store/schema/baseline.sql` (`ddl_totality_test.go:80-84`); the allowlist names no table the baseline declares, and none the snapshot lost → the allowlist is gone with its half; the baseline DDL declares no table the snapshot lost → the baseline creates no table the snapshot does not ship (`:85-90`). Added: the baseline carries no partition (`:92-95`) and no session setting (`:96-99`), and records schema_migrations v1..`store.RetiredChain` (`:101-111`). The allowlist's table notes are carried as `COMMENT ON TABLE` in `store/schema/comments.sql`.
- mutation: the committed baseline's `CREATE TABLE public.dashboards (` renamed `dashboards_x`, and `SET statement_timeout = 0;` put above its first statement · rewritten red: `go test -count=1 -run 'TestBaseline_CreatesEveryShippedTable' ./store/schema/` → `the snapshot ships table "dashboards" and the baseline does not create it`, `the baseline creates table "dashboards_x" and the snapshot does not ship it`, `the baseline changes a session setting ... SET statement_timeout = 0;`, `--- FAIL` (`build\lane-schema\c4-totality-drift-red.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: `docs/ground-rules.md` (the one-home rule, now the generated baseline's)
- verified at: the commit that adds this row

### STALE — `TestBaselineDDL_DoesNotIndexMigrationAddedColumns`
- test: `TestBaselineDDL_DoesNotIndexMigrationAddedColumns` · pkg `shingocore/store` · run `-run '^TestBaselineDDL_DoesNotIndexMigrationAddedColumns$'` · `shingo-core/store/schema_index_drift_test.go:74` @ `e06b29b0` · tags `none`
- code: STALE (rewritten as `TestBaseline_CreatesEveryShippedIndex`, same file). It read the upgrade order: a baseline `CREATE INDEX` over a column a migration adds, not in `migrateAddBaselineColumns`, failed every boot of an existing database. Since the squash at v183 the baseline meets only a database with no tables, so that hazard has no path; what can still drift is the baseline against the snapshot.
- assertion map: no baseline index names a migration-added column outside the pre-baseline list → every index the snapshot ships is created by the baseline with the same statement, and the baseline creates no other (`schema_index_drift_test.go:57-83`). `TestSchemaDriftParsers_MatchTheShapesInUse` keeps its name and now pins the one parser against the snapshot's count and three real statement shapes (a partial, a unique partial, an `ON ONLY` parent index).
- mutation: the committed baseline's `idx_bins_status` built over `label` instead of `status` · rewritten red: `go test -count=1 -run 'TestBaseline_CreatesEveryShippedIndex' ./store/` → `the baseline creates idx_bins_status differently: baseline: ... (label); snapshot: ... (status)`, `--- FAIL` (`build\lane-schema\c4-totality-drift-red.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: `docs/ui-style-guide/24-drift-detection.md` (generated block, `make docs-gen`)
- verified at: the commit that adds this row

### DEAD — `TestV72_RenamesTheOperatorStationEverywhere`
- test: `TestV72_RenamesTheOperatorStationEverywhere` · pkg `shingocore/store` · run `-run '^TestV72_RenamesTheOperatorStationEverywhere$'` · `shingo-core/store/migration_v72_station_test.go:60` @ `e06b29b0` · tags `docker`
- code: DEAD (v72, the rewrite of the old operator station across four tables, goes with the chain in the squash)
- assertion map: the old station's rows moved in orders, mission_telemetry, cell_config and dashboards, the array rewritten in place → nothing: a rewrite of rows from before v72, and nothing outside tests and `migrations.go` writes the old value (grep of `core-spot`: none)
- mutation: none to run: the test re-runs v72 through the self-heal, and the squash deletes v72 and the self-heal
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV72_IsSafeToRunTwice`
- test: `TestV72_IsSafeToRunTwice` · pkg `shingocore/store` · run `-run '^TestV72_IsSafeToRunTwice$'` · `shingo-core/store/migration_v72_station_test.go:106` @ `e06b29b0` · tags `docker`
- code: DEAD (v72's idempotence under the per-boot self-heal; the squash removes both)
- assertion map: three re-runs leave no old rows → nothing
- mutation: none to run: as `TestV72_RenamesTheOperatorStationEverywhere`'s row
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV81_EpisodesBecomeProduceOrConsume`
- test: `TestV81_EpisodesBecomeProduceOrConsume` · pkg `shingocore/store` · run `-run '^TestV81_EpisodesBecomeProduceOrConsume$'` · `shingo-core/store/migration_v81_episode_role_test.go:75` @ `e06b29b0` · tags `docker`
- code: DEAD (v81, the rewrite of cell episodes from supply/evacuate to consume/produce, goes with the chain)
- assertion map: key and direction rewritten together, other kinds untouched → nothing: a rewrite of rows from before v81
- mutation: none to run: the test re-runs v81 through the self-heal, which the squash removes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV81_IsSafeToRunTwice`
- test: `TestV81_IsSafeToRunTwice` · pkg `shingocore/store` · run `-run '^TestV81_IsSafeToRunTwice$'` · `shingo-core/store/migration_v81_episode_role_test.go:143` @ `e06b29b0` · tags `docker`
- code: DEAD (v81's idempotence under the self-heal)
- assertion map: three re-runs leave key and direction right → nothing
- mutation: none to run: as `TestV81_EpisodesBecomeProduceOrConsume`'s row
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV93_BackfillDerivesCrossingsDropsAndBoundaries`
- test: `TestV93_BackfillDerivesCrossingsDropsAndBoundaries` · pkg `shingocore/store` · run `-run '^TestV93_BackfillDerivesCrossingsDropsAndBoundaries$'` · `shingo-core/store/migration_v93_exception_test.go:49` @ `e06b29b0` · tags `docker`
- code: DEAD (v93's one-time backfill of bin_uop_exception from bin_uop_ledger goes with the chain)
- assertion map: crossings, drops and boundaries derived from history → nothing: the backfill's rows stay in every plant database and the live writers of exception rows are pinned in `uop` and `store/audit`
- mutation: none to run: the test deletes v93's `schema_migrations` row and re-opens; the squash deletes v93 and refuses a database with a hole in 1..183
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV93_IdempotentOnReapply`
- test: `TestV93_IdempotentOnReapply` · pkg `shingocore/store` · run `-run '^TestV93_IdempotentOnReapply$'` · `shingo-core/store/migration_v93_exception_test.go:139` @ `e06b29b0` · tags `docker`
- code: DEAD (v93's truncate-then-backfill on a re-run)
- assertion map: two applies write one row → nothing
- mutation: none to run: as `TestV93_BackfillDerivesCrossingsDropsAndBoundaries`' row
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV93BackfillOpsMatchEpochBumpOps`
- test: `TestV93BackfillOpsMatchEpochBumpOps` · pkg `shingocore/store` · run `-run '^TestV93BackfillOpsMatchEpochBumpOps$'` · `shingo-core/store/migration_v93_exception_test.go:189` @ `e06b29b0` · tags `docker`
- code: DEAD (it pins `store.BumpOpsForBackfill`, v93's copy of the bump-op set, which goes with `migrations.go`)
- assertion map: the backfill's set equals `audit.EpochBumpOps` adjusted for the ops born or retired since → nothing: the audit set itself stays pinned to the bump call sites by `TestEpochBumpOpsCoversEveryBumpSite` (`shingo-core/store/audit/epoch_bump_ops_test.go:33`)
- mutation: none to run: the set it compares is deleted
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestV93_OpenNegativeBinsReadsTheExceptionTable`
- test: `TestV93_OpenNegativeBinsReadsTheExceptionTable` · pkg `shingocore/store` · run `-run '^TestV93_OpenNegativeBinsReadsTheExceptionTable$'` · `shingo-core/store/migration_v93_exception_test.go:223` @ `e06b29b0` · tags `docker`
- code: STALE (it ran no migration; moved to the reader's package and renamed)
- assertion map: every assertion kept → `TestOpenNegativeBins_DatesTheBinByItsExceptionRow` (`shingo-core/store/bins/ledger_integrity_test.go:166`): the bin is listed after its raw row is purged, and `NegativeSince` is the exception row's `occurred_at`. The raw row is seeded with `testdb.SeedAudit` instead of a local copy of it
- mutation: `bins.OpenNegativeBins` reads another exception kind (`e.kind = 'negative_crossingX'`) · survivor red at `27f5fb2e`: `ledger_integrity_test.go:194: NegativeSince = <nil>, want the exception row's occurred_at ...` (`data-model-attack-2026-11-19/build/lane-relocate/02-red-bins-exception-kind.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV94_BackfillAggregatesAtTheReviewedGrain`
- test: `TestV94_BackfillAggregatesAtTheReviewedGrain` · pkg `shingocore/store` · run `-run '^TestV94_BackfillAggregatesAtTheReviewedGrain$'` · `shingo-core/store/migration_v94_daily_test.go:46` @ `e06b29b0` · tags `docker`
- code: DEAD (v94's backfill of bin_uop_delta_daily goes with the chain; the grain it pinned is the daily job's, and is pinned on the job)
- assertion map: the five rows of the two-day history, every column, actor in the grain → `TestRollupBinUOPDeltaDay_AggregatesAtTheReviewedGrain` (`shingo-core/store/audit/bin_uop_purge_test.go:140`): the same history rolled up by `audit.RollupBinUOPDeltaDay` day by day, with the rows each day writes counted and every row compared whole (the old test compared three rows whole, two in part, and the actor on bin 1 only)
- mutation: on `audit.RollupBinUOPDeltaDay` at `27f5fb2e`, survivor red each time: `min(w.after_uop)` → `min(w.before_uop)` (rows 0-4, `lane-relocate/03-red-audit-min-of-before.log`); the epoch window scoped to the day (rows 3-4 in epoch 0, `lane-relocate/04-red-audit-day-scoped-window.log`); reason taken out of the grain (`day-2 rollup wrote 1 rows, want 2`, `lane-relocate/05-red-audit-reason-out-of-grain.log`), all under `data-model-attack-2026-11-19/build/`
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestV94_RollupJobMatchesBackfill`
- test: `TestV94_RollupJobMatchesBackfill` · pkg `shingocore/store` · run `-run '^TestV94_RollupJobMatchesBackfill$'` · `shingo-core/store/migration_v94_daily_test.go:155` @ `e06b29b0` · tags `docker`
- code: STALE (its body never ran the backfill it was named for; moved to the job's package and renamed)
- assertion map: every assertion kept → `TestRollupBinUOPDeltaDay_TheEpochCarriesAcrossDays` (`shingo-core/store/audit/bin_uop_purge_test.go:190`): 2 rows then 1, day 2 in epoch 1, the epoch-1 row's six values
- mutation: the epoch window scoped to the day · survivor red at `27f5fb2e`: `bin_uop_purge_test.go:223: day-2 delta landed in epoch 0, want 1` (`data-model-attack-2026-11-19/build/lane-relocate/04-red-audit-day-scoped-window.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none (the comment on `pgBumpOpsArraySQL` in `store/audit/bin_uop_daily.go` still describes a lockstep test against the backfill; production comments are outside this change)
- verified at: the commit that adds this row

### STALE — `TestV94_RollupJobIsIdempotentPerDay`
- test: `TestV94_RollupJobIsIdempotentPerDay` · pkg `shingocore/store` · run `-run '^TestV94_RollupJobIsIdempotentPerDay$'` · `shingo-core/store/migration_v94_daily_test.go:212` @ `e06b29b0` · tags `docker`
- code: STALE (it ran no migration; moved to the job's package and renamed)
- assertion map: every assertion kept → `TestRollupBinUOPDeltaDay_IsIdempotentPerDay` (`shingo-core/store/audit/bin_uop_purge_test.go:242`)
- mutation: the upsert accumulates (`ticks = bin_uop_delta_daily.ticks + EXCLUDED.ticks`) · survivor red at `27f5fb2e`: `after two rollups: rows=1 ticks=4 consumed=25, want 1/2/25` (`data-model-attack-2026-11-19/build/lane-relocate/06-red-audit-upsert-accumulates.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV99_PreservesEveryStoredValue`
- test: `TestV99_PreservesEveryStoredValue` · pkg `shingocore/store` · run `-run '^TestV99_PreservesEveryStoredValue$'` · `shingo-core/store/migration_v99_parts_per_cycle_test.go:76` @ `e06b29b0` · tags `docker`
- code: DEAD (v99's rename of payload_manifest.quantity goes with the chain)
- assertion map: six stored ratios unchanged across the rename → nothing: a property of the rename
- mutation: none to run: the test rewinds the column and re-runs v99, which the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestV99_DefaultBecomesOne`
- test: `TestV99_DefaultBecomesOne` · pkg `shingocore/store` · run `-run '^TestV99_DefaultBecomesOne$'` · `shingo-core/store/migration_v99_parts_per_cycle_test.go:113` @ `e06b29b0` · tags `docker`
- code: STALE (its assertion is the column's DEFAULT, reached by rewinding v99; the survivor inserts on the migrated schema)
- assertion map: a line inserted without a ratio reads 1 → `TestManifestLine_NoStatedRatioIsOnePerCycle` (`shingo-core/store/payloads/payloads_test.go:347`), read back through `payloads.ListManifest` instead of raw SQL. The one writer (`insertManifestLine`) names the column, so no live writer relies on the default today
- mutation: the column's DEFAULT set to 0 inside the survivor, as a baseline that lost it would read · red at `27f5fb2e`: `payloads_test.go:366: parts_per_cycle = 0, want 1` (`data-model-attack-2026-11-19/build/lane-relocate/21-red-payloads-default-zero.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV99_IsIdempotent`
- test: `TestV99_IsIdempotent` · pkg `shingocore/store` · run `-run '^TestV99_IsIdempotent$'` · `shingo-core/store/migration_v99_parts_per_cycle_test.go:142` @ `e06b29b0` · tags `docker`
- code: DEAD (v99's rename guard under the self-heal)
- assertion map: two applies keep the value → nothing
- mutation: none to run: as `TestV99_PreservesEveryStoredValue`'s row
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV100_ATaggedNodeStillWalksAsABoundary`
- test: `TestV100_ATaggedNodeStillWalksAsABoundary` · pkg `shingocore/store` · run `-run '^TestV100_ATaggedNodeStillWalksAsABoundary$'` · `shingo-core/store/migration_v100_cms_storeroom_test.go:30` @ `e06b29b0` · tags `docker`
- code: DEAD (v100's delete of the retired key, checked from the positive side, goes with the chain)
- assertion map: v100 deletes no more than its key → nothing; the walk from a slot to its tagged synthetic root and that root's storeroom code → `TestFindCMSBoundary_LogsAtSyntheticRoot` (`shingo-core/engine/cms_transactions_test.go:92`), the same tree and the same three assertions, on the base already
- mutation: none to run: the test deletes v100's `schema_migrations` row and re-opens; the survivor named is unchanged
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestNodePropertyKeyAbsent_DoesNotReportAFailedQueryAsSuccess`
- test: `TestNodePropertyKeyAbsent_DoesNotReportAFailedQueryAsSuccess` · pkg `shingocore/store` · run `-run '^TestNodePropertyKeyAbsent_DoesNotReportAFailedQueryAsSuccess$'` · `shingo-core/store/migration_v100_cms_storeroom_test.go:87` @ `e06b29b0` · tags `docker`
- code: DEAD (its subject, `schema.NodePropertyKeyAbsent`, is called only by the v100 and v169 verifies, which go with the chain)
- assertion map: true for an absent key, false for a present one, false on a failed query → nothing in this row; if the squash keeps the function, the pin belongs beside it in `store/schema`'s tests
- mutation: none to run: its only callers are deleted
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV100_RemovesTheRetiredBoundaryProperty`
- test: `TestV100_RemovesTheRetiredBoundaryProperty` · pkg `shingocore/store` · run `-run '^TestV100_RemovesTheRetiredBoundaryProperty$'` · `shingo-core/store/migration_v100_cms_storeroom_test.go:124` @ `e06b29b0` · tags `docker`
- code: DEAD (v100's keyed delete)
- assertion map: the retired key gone and `cms_storeroom` kept → nothing
- mutation: none to run: as `TestV100_ATaggedNodeStillWalksAsABoundary`'s row
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### STRENGTHEN — `TestV102_HopkinsvilleDayOneCardIsNotRed`
- test: `TestV102_HopkinsvilleDayOneCardIsNotRed` · pkg `shingocore/store` · run `-run '^TestV102_HopkinsvilleDayOneCardIsNotRed$'` · `shingo-core/store/migration_v102_posting_backfill_test.go:41` @ `e06b29b0` · tags `docker`
- code: STRENGTHEN (it ran no migration; moved to the card's package and renamed)
- assertion map: both assertions kept → `TestCMSPostingService_HistoryThatPredatesTheFeedIsHealthy` (`shingo-core/service/cms_transaction_service_test.go:136`). Added: with the same history unqueued (NULL) the card reads unhealthy with the never-queued finding, so the absence of that wording cannot pass by the wording changing
- mutation: `cms.PostingHealth` counts `posting_id = 0` as unposted · survivor red at `27f5fb2e`: `cms_transaction_service_test.go:174: the card reports the plant's history as unqueued work: "12 transactions has been recorded but never queued for posting ..."` (`data-model-attack-2026-11-19/build/lane-relocate/11-red-cms-health-counts-zero.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV102_BackfillsLegacyRowsWithZero`
- test: `TestV102_BackfillsLegacyRowsWithZero` · pkg `shingocore/store` · run `-run '^TestV102_BackfillsLegacyRowsWithZero$'` · `shingo-core/store/migration_v102_posting_backfill_test.go:108` @ `e06b29b0` · tags `docker`
- code: DEAD (v102's backfill of NULL to 0 goes with the chain; the 0 rows it wrote stay at the plants, and their reading is pinned)
- assertion map: posting_id 0 after v102 → nothing; `PostingHealth` leaves a 0 row out of unposted → `TestPostingHealth_ARowThatPredatesTheFeedIsNotUnposted` (`shingo-core/store/cms/postings_test.go:537`), with a fresh row beside it so the count is 1, not 0, and the sender's queue (`ListUnpostedOlderThan`) leaving the 0 row out too
- mutation: on `store/cms` at `27f5fb2e`, survivor red each time: the health count reads `posting_id IS NULL OR posting_id = 0` (`postings_test.go:555: unposted = 2, want 1`, `lane-relocate/11-red-cms-health-counts-zero.log`); the queue reads the same (`postings_test.go:566: the sender's queue = [1 2], want only the fresh row 2`, `lane-relocate/12-red-cms-queue-lists-zero.log`), both under `data-model-attack-2026-11-19/build/`
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DUP — `TestV102_BackfillDoesNotClaimNewWork`
- test: `TestV102_BackfillDoesNotClaimNewWork` · pkg `shingocore/store` · run `-run '^TestV102_BackfillDoesNotClaimNewWork$'` · `shingo-core/store/migration_v102_posting_backfill_test.go:168` @ `e06b29b0` · tags `docker`
- code: DUP (of `TestPostingHealth_CountsUnpostedTransactions`; it ran no migration)
- assertion map: a fresh transaction counts as unposted → `TestPostingHealth_CountsUnpostedTransactions` (`shingo-core/store/cms/postings_test.go:498`, two fresh rows count 2); a fresh row arrives NULL → `TestTransactions_StoreroomAndRobotRoundTrip` (`:332`)
- mutation: the health count made to count nothing (`... WHERE posting_id IS NULL AND false`) · both red at `27f5fb2e`: old `migration_v102_posting_backfill_test.go:185: unposted_transaction_count = 0, want 1`; survivor `postings_test.go:513: unposted = 0, want 2` (`data-model-attack-2026-11-19/build/lane-relocate/28-dup-v102-new-work-both-arms.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV108_MovesTheCATIDToThePartAndLeavesTheGuardAlone`
- test: `TestV108_MovesTheCATIDToThePartAndLeavesTheGuardAlone` · pkg `shingocore/store` · run `-run '^TestV108_MovesTheCATIDToThePartAndLeavesTheGuardAlone$'` · `shingo-core/store/migration_v108_manifest_identity_test.go:53` @ `e06b29b0` · tags `docker`
- code: DEAD (it drives `store.RunManifestIdentityCorrection`, v108's body, which goes with `migrations.go`)
- assertion map: the line re-pointed at the payload code, its cat id moved to the part → nothing; the guard's derived value for a line linked to a part is the part's cat id → `TestPayloadCATIDs` (`shingo-core/store/payloads/catids_test.go:25`)
- mutation: none to run: the correction it calls is deleted
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV108_LeavesAKitAloneAndStillDerivesItsCATIDs`
- test: `TestV108_LeavesAKitAloneAndStillDerivesItsCATIDs` · pkg `shingocore/store` · run `-run '^TestV108_LeavesAKitAloneAndStillDerivesItsCATIDs$'` · `shingo-core/store/migration_v108_manifest_identity_test.go:96` @ `e06b29b0` · tags `docker`
- code: DEAD (v108's correction leaving a kit's lines)
- assertion map: the kit's lines not re-pointed → nothing; a line with no part behind it still derives its own value → `TestPayloadCATIDs_ReadsAnUnrepointedLine` (`shingo-core/store/payloads/catids_test.go:83`)
- mutation: none to run: the correction it calls is deleted
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV108_DoesNotRefuseItsOwnSeedDeletion`
- test: `TestV108_DoesNotRefuseItsOwnSeedDeletion` · pkg `shingocore/store` · run `-run '^TestV108_DoesNotRefuseItsOwnSeedDeletion$'` · `shingo-core/store/migration_v108_manifest_identity_test.go:138` @ `e06b29b0` · tags `docker`
- code: DEAD (v108's verify exempting its own seed deletion)
- assertion map: the correction runs over the seed row and a neighbour → nothing: the correction and its verify are deleted
- mutation: none to run: the correction it calls is deleted
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV108_RefusesWhenTheGuardWouldChange`
- test: `TestV108_RefusesWhenTheGuardWouldChange` · pkg `shingocore/store` · run `-run '^TestV108_RefusesWhenTheGuardWouldChange$'` · `shingo-core/store/migration_v108_manifest_identity_test.go:185` @ `e06b29b0` · tags `docker`
- code: DEAD (v108's verify refusing a changed derived set)
- assertion map: the transaction refused and left as it was → nothing
- mutation: none to run: the correction it calls is deleted
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV119_BackfillPromotesReasonFromMetadata`
- test: `TestV119_BackfillPromotesReasonFromMetadata` · pkg `shingocore/store` · run `-run '^TestV119_BackfillPromotesReasonFromMetadata$'` · `shingo-core/store/migration_v119_reason_test.go:37` @ `e06b29b0` · tags `docker`
- code: DEAD (v119's 7-day backfill of bin_uop_ledger.reason goes with the chain)
- assertion map: which rows carry the reason after the backfill → nothing
- mutation: none to run: the test deletes v119's `schema_migrations` row and re-opens; the squash deletes v119 and refuses a hole in 1..183
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV128_TickKeyOnCellPartEvents`
- test: `TestV128_TickKeyOnCellPartEvents` · pkg `shingocore/store` · run `-run '^TestV128_TickKeyOnCellPartEvents$'` · `shingo-core/store/migration_v128_test.go:34` @ `e06b29b0` · tags `docker`
- code: DEAD (the chain's end-state shape: the index present, the side table and a column gone; it asserts no row)
- assertion map: the unique index's runtime effect, a redelivered tick is one row → `TestProductionTick_SameTickTwiceIsOneRow` (`shingo-core/messaging/production_tick_docker_test.go:147`); the shape → the committed snapshot (`store/schema/schema.snapshot.sql`, compared by `TestSchemaSnapshotIsCurrent`)
- mutation: none to run: it reads the catalog, and the runtime survivor named is on the base, unchanged
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV128_ToleratesADuplicateTick`
- test: `TestV128_ToleratesADuplicateTick` · pkg `shingocore/store` · run `-run '^TestV128_ToleratesADuplicateTick$'` · `shingo-core/store/migration_v128_test.go:53` @ `e06b29b0` · tags `docker`
- code: DEAD (v128 collapsing duplicates before it builds the index)
- assertion map: a duplicate group collapsed to one row and the index built → nothing
- mutation: none to run: the test drops the index and re-runs v128, which the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV131_PileMirrorShape`
- test: `TestV131_PileMirrorShape` · pkg `shingocore/store` · run `-run '^TestV131_PileMirrorShape$'` · `shingo-core/store/migration_v131_test.go:38` @ `e06b29b0` · tags `docker`
- code: DEAD (the chain's end-state shape of lineside_buckets and the drain ledger; its runtime half moved)
- assertion map: the state CHECK refusing `inactive` → `TestLinesideBuckets_ThePileIsKeyedByItsSeatAndOneOfTwoStates` (`shingo-core/store/inventory/buckets_test.go:152`), now naming the constraint, with `active` and `stranded` landing at the seat; the key (core_node_name, payload_code, state) → the same test, a second row per seat and state refused by the key's name, and as the level upsert's conflict target `TestBucketLevel_OneNodeOneRowAcrossStations` and `TestBucketLevel_ActiveAndStrandedRowsAreIndependent` (`shingo-core/uop/bucket_level_test.go:156`, `:134`); the dropped columns → the committed snapshot
- mutation: both constraints dropped inside the survivor · red at `27f5fb2e`: `buckets_test.go:164: state inactive: <nil>, want the state CHECK's refusal`, `buckets_test.go:172: a second active row at one seat: <nil>, want the seat key's refusal` (`data-model-attack-2026-11-19/build/lane-relocate/23-red-buckets-constraints-dropped.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV131_TruncatesAndReshapesAPlantDatabase`
- test: `TestV131_TruncatesAndReshapesAPlantDatabase` · pkg `shingocore/store` · run `-run '^TestV131_TruncatesAndReshapesAPlantDatabase$'` · `shingo-core/store/migration_v131_test.go:61` @ `e06b29b0` · tags `docker`
- code: DEAD (v131 over the pre-v131 shape)
- assertion map: truncation, reshaping, dedup rows and episode closes → nothing
- mutation: none to run: the test rewinds the tables and re-runs v131, which the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestLoaderMigration_FedDirectlyBackfillMatchesBlankInbound`
- test: `TestLoaderMigration_FedDirectlyBackfillMatchesBlankInbound` · pkg `shingocore/store` · run `-run '^TestLoaderMigration_FedDirectlyBackfillMatchesBlankInbound$'` · `shingo-core/store/migration_v136_fed_directly_test.go:19` @ `e06b29b0` · tags `docker`
- code: DEAD (v136's backfill of fed_directly)
- assertion map: fed_directly equals a blank inbound for loaders written before the column → nothing
- mutation: none to run: the test drops the column and re-runs v136, which the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV138_QualityHoldStatusBecomesFlagged`
- test: `TestV138_QualityHoldStatusBecomesFlagged` · pkg `shingocore/store` · run `-run '^TestV138_QualityHoldStatusBecomesFlagged$'` · `shingo-core/store/migration_v138_quality_hold_status_test.go:16` @ `e06b29b0` · tags `docker`
- code: DEAD (v138's rewrite of the retired status)
- assertion map: the old status flagged, other statuses and the containment marker untouched → nothing
- mutation: none to run: the test deletes v138's `schema_migrations` row and re-opens
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV142_VendorTimesReadAsSeconds`
- test: `TestV142_VendorTimesReadAsSeconds` · pkg `shingocore/store` · run `-run '^TestV142_VendorTimesReadAsSeconds$'` · `shingo-core/store/migration_v142_vendor_seconds_test.go:18` @ `e06b29b0` · tags `docker`
- code: DEAD (v142's correction of stamps stored in the wrong unit)
- assertion map: the wrong-unit row corrected, the others untouched, twice → nothing
- mutation: none to run: the test deletes v142's `schema_migrations` row and re-opens
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV144_ProcessStylesStationID`
- test: `TestV144_ProcessStylesStationID` · pkg `shingocore/store` · run `-run '^TestV144_ProcessStylesStationID$'` · `shingo-core/store/migration_v144_test.go:17` @ `e06b29b0` · tags `docker`
- code: DEAD (a row written before v144 reads NULL)
- assertion map: the pre-v144 row reads NULL → nothing: the live writer names the column (`store/plantclaims/plantclaims.go:156`)
- mutation: none to run: the test drops the column and re-runs v144, which the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV145_OrdersStationUnaware`
- test: `TestV145_OrdersStationUnaware` · pkg `shingocore/store` · run `-run '^TestV145_OrdersStationUnaware$'` · `shingo-core/store/migration_v145_test.go:15` @ `e06b29b0` · tags `docker`
- code: DEAD (an order written before v145 reads false)
- assertion map: the pre-v145 order reads station_unaware false → nothing: the one orders INSERT names the column (`store/orders/orders.go:258`) and `TestWriter_RoundTripsEveryFieldItWrites` (`shingo-core/store/orders/writer_roundtrip_test.go:54`) round-trips it; the default serves only rows from before the column
- mutation: none to run: the test drops the column and re-runs v145, which the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV146_OrdersProjectsTo`
- test: `TestV146_OrdersProjectsTo` · pkg `shingocore/store` · run `-run '^TestV146_OrdersProjectsTo$'` · `shingo-core/store/migration_v146_test.go:16` @ `e06b29b0` · tags `docker`
- code: DEAD (an order written before v146 reads blank)
- assertion map: the pre-v146 order projects to nobody → nothing: the INSERT names the column, round-tripped by `TestWriter_RoundTripsEveryFieldItWrites` (`shingo-core/store/orders/writer_roundtrip_test.go:54`)
- mutation: none to run: the test drops the column and re-runs v146, which the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV147_OrdersPlan`
- test: `TestV147_OrdersPlan` · pkg `shingocore/store` · run `-run '^TestV147_OrdersPlan$'` · `shingo-core/store/migration_v147_test.go:15` @ `e06b29b0` · tags `docker`
- code: DEAD (an order written before v147 reads no plan)
- assertion map: the pre-v147 order carries no plan → nothing: the INSERT names the column, round-tripped by `TestWriter_RoundTripsEveryFieldItWrites` (`shingo-core/store/orders/writer_roundtrip_test.go:54`)
- mutation: none to run: the test drops the column and re-runs v147, which the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV148_SourcingBecomesQueuedWithACause`
- test: `TestV148_SourcingBecomesQueuedWithACause` · pkg `shingocore/store` · run `-run '^TestV148_SourcingBecomesQueuedWithACause$'` · `shingo-core/store/migration_v148_test.go:16` @ `e06b29b0` · tags `docker`
- code: DEAD (v148's rewrite of sourcing orders)
- assertion map: queued with a cause, history untouched, twice → nothing
- mutation: none to run: the test deletes v148's `schema_migrations` row and re-opens
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestMigrate_LinesideReportCarriesNoCarrierColumns`
- test: `TestMigrate_LinesideReportCarriesNoCarrierColumns` · pkg `shingocore/store` · run `-run '^TestMigrate_LinesideReportCarriesNoCarrierColumns$'` · `shingo-core/store/migration_v149_test.go:18` @ `e06b29b0` · tags `docker`
- code: DEAD (the chain's end-state shape of edge_lineside_reports, guarding a re-add on boot that the squash's removal of the per-boot replay takes away)
- assertion map: the seat and reported_at present → `TestUpsertEdgeLinesideReport_LatestWins` (`shingo-core/store/edge_lineside_reports_docker_test.go:60`), which reads all four back through the upsert; the six absent columns → the committed snapshot
- mutation: none to run: it reads the catalog, and the survivor named is on the base, unchanged
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestV173_AnOrdersCartsBecomeRowsPerLift`
- test: `TestV173_AnOrdersCartsBecomeRowsPerLift` · pkg `shingocore/store` · run `-run '^TestV173_AnOrdersCartsBecomeRowsPerLift$'` · `shingo-core/store/migration_v173_test.go:49` @ `e06b29b0` · tags `docker`
- code: DEAD (v173 and v174's conversion of orders.bin_id to rows per lift, its census and deferral, go with `order_carts_migration.go`)
- assertion map: the rows per case, the census lines, v174 deferred then completed → nothing; the writers' cart rows are pinned by the order-carts pins (`shingo-core/store/order_carts_pins_docker_test.go`, `shingo-core/engine/order_carts_pins_docker_test.go`)
- mutation: none to run: the test ages the tables and re-runs v173, which the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestLoaderMigration_V176_EveryLoaderKeepsTheClear`
- test: `TestLoaderMigration_V176_EveryLoaderKeepsTheClear` · pkg `shingocore/store` · run `-run '^TestLoaderMigration_V176_EveryLoaderKeepsTheClear$'` · `shingo-core/store/migration_v176_no_clear_partials_test.go:20` @ `e06b29b0` · tags `docker`
- code: DEAD (v176's column over loaders written before it)
- assertion map: every older loader reads no_clear_partials false → nothing
- mutation: none to run: the test drops the column and re-runs v176, which the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestMigrate_V177_DepthOnlyInLanes`
- test: `TestMigrate_V177_DepthOnlyInLanes` · pkg `shingocore/store` · run `-run '^TestMigrate_V177_DepthOnlyInLanes$'` · `shingo-core/store/migration_v177_depth_only_in_lanes_test.go:21` @ `e06b29b0` · tags `docker`
- code: DEAD (v177's clearing of depths a removed lane left behind)
- assertion map: the depth per case, twice → nothing; the rule the node writes hold is pinned by the node writers' tests
- mutation: none to run: the test deletes v177's `schema_migrations` row and re-opens
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestLoaderMigration_V178_EveryLoaderPicksNoEdge`
- test: `TestLoaderMigration_V178_EveryLoaderPicksNoEdge` · pkg `shingocore/store` · run `-run '^TestLoaderMigration_V178_EveryLoaderPicksNoEdge$'` · `shingo-core/store/migration_v178_edge_station_uid_test.go:20` @ `e06b29b0` · tags `docker`
- code: DEAD (v178's column over loaders written before it)
- assertion map: every older loader reads a blank edge_station_uid → nothing
- mutation: none to run: the test drops the column and re-runs v178, which the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestBlockRecordMigration_V179_OldOrdersStayNullAndTheSeedLiftsAtItsLoad`
- test: `TestBlockRecordMigration_V179_OldOrdersStayNullAndTheSeedLiftsAtItsLoad` · pkg `shingocore/store` · run `-run '^TestBlockRecordMigration_V179_OldOrdersStayNullAndTheSeedLiftsAtItsLoad$'` · `shingo-core/store/migration_v179_block_record_test.go:23` @ `e06b29b0` · tags `docker`
- code: DEAD (v179 over orders and sequences written before it; its runtime half moved)
- assertion map: an older order reads NULL, and the name rule's fill of the other sequences → nothing; an order born now reads `{}`, empty and not NULL → `TestWriter_BornByTheColumnDefaults` (`shingo-core/store/orders/writer_roundtrip_test.go:283`); the seed lifts at 2 and the CHECK refuses 4 and -1 → `TestLoadSequences_TheSeedLiftsAtItsLoadAndTheCheckHoldsTheIndex` (`shingo-core/store/payloads/load_sequences_test.go:98`), read through `GetLoadSequence`, the refusals now naming the constraint and a legal 3 landing
- mutation: inside the survivors at `27f5fb2e`, red each time: block_actions' DEFAULT dropped (`writer_roundtrip_test.go:301: an order born now reads BlockActions map[], want empty and not nil`, `lane-relocate/24-red-orders-defaults-changed.log`); the CHECK dropped and the seed moved to 1 (`load_sequences_test.go:114`, `:120` twice, `lane-relocate/22-red-loadseq-check-dropped-seed-moved.log`), both under `data-model-attack-2026-11-19/build/`
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestBlockRecord_TheCreateReplacesAndTheAppendMerges`
- test: `TestBlockRecord_TheCreateReplacesAndTheAppendMerges` · pkg `shingocore/store` · run `-run '^TestBlockRecord_TheCreateReplacesAndTheAppendMerges$'` · `shingo-core/store/migration_v179_block_record_test.go:87` @ `e06b29b0` · tags `docker`
- code: STALE (it ran no migration; moved to the fleet's-yes writers' file and renamed)
- assertion map: every assertion kept → `TestBlockRecord_TheYesReplacesAndTheAppendMerges` (`shingo-core/store/handover_id_write_pins_docker_test.go:73`)
- mutation: at `27f5fb2e`, survivor red each time: `ClaimAppend` replaces instead of merging (`handover_id_write_pins_docker_test.go:108: after the append (owed true) the record is map[V-live-b3:dropoff V-live-b4:], want the create's two and the segment's two`, `lane-relocate/15-red-claimappend-replaces.log`); `FleetYesTx` records nothing (`:98: after the create the record is map[], want the two blocks it sent`, `lane-relocate/16-red-fleetyes-records-nothing.log`), both under `data-model-attack-2026-11-19/build/`
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestCarrierKeysMigration_V180_EveryFamilyStartsBlank`
- test: `TestCarrierKeysMigration_V180_EveryFamilyStartsBlank` · pkg `shingocore/store` · run `-run '^TestCarrierKeysMigration_V180_EveryFamilyStartsBlank$'` · `shingo-core/store/migration_v180_carrier_keys_test.go:32` @ `e06b29b0` · tags `docker`
- code: DEAD (v180's columns over families that existed)
- assertion map: an older family reads both keys blank → nothing; a family made now reads blank keys → `TestCarrierFamilyKeys_GoWithTheFamily` (`shingo-core/store/bins/bins_test.go:886`), the type made again under the same code
- mutation: none to run: the test drops the columns and re-runs v180, which the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestCarrierFamilyKeys_SetReadsBackOnEveryTypeOfTheFamily`
- test: `TestCarrierFamilyKeys_SetReadsBackOnEveryTypeOfTheFamily` · pkg `shingocore/store` · run `-run '^TestCarrierFamilyKeys_SetReadsBackOnEveryTypeOfTheFamily$'` · `shingo-core/store/migration_v180_carrier_keys_test.go:61` @ `e06b29b0` · tags `docker`
- code: STALE (it ran no migration; moved under the same name to `store/bins`, the package that owns the functions)
- assertion map: every assertion kept → `TestCarrierFamilyKeys_SetReadsBackOnEveryTypeOfTheFamily` (`shingo-core/store/bins/bins_test.go:858`), calling `bins.CreateType`, `ListFamilies` and `SetFamilyKeys`, which the store's wrappers (`store/bin_types.go`) call one to one
- mutation: `bins.SetFamilyKeys` misses a gone family (`n == 0` → `n < 0`) · survivor red at `27f5fb2e`: `bins_test.go:877: setting a family that does not exist: <nil>, want ErrNoSuchFamily` (`data-model-attack-2026-11-19/build/lane-relocate/13-red-bins-setkeys-no-such-family.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestCarrierFamilyKeys_GoWithTheFamily`
- test: `TestCarrierFamilyKeys_GoWithTheFamily` · pkg `shingocore/store` · run `-run '^TestCarrierFamilyKeys_GoWithTheFamily$'` · `shingo-core/store/migration_v180_carrier_keys_test.go:88` @ `e06b29b0` · tags `docker`
- code: STALE (it ran no migration; moved under the same name to `store/bins`)
- assertion map: every assertion kept → `TestCarrierFamilyKeys_GoWithTheFamily` (`shingo-core/store/bins/bins_test.go:886`)
- mutation: `bins.DeleteType` and `bins.SetTypeFamily` keep the emptied family (`AND false`) · survivor red at `27f5fb2e`: `bins_test.go:905`, `:909`, `:918` (`data-model-attack-2026-11-19/build/lane-relocate/14-red-bins-family-outlives-its-types.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestOrderMigration_V181_EveryOrderStartsAtOne`
- test: `TestOrderMigration_V181_EveryOrderStartsAtOne` · pkg `shingocore/store` · run `-run '^TestOrderMigration_V181_EveryOrderStartsAtOne$'` · `shingo-core/store/migration_v181_order_version_test.go:19` @ `e06b29b0` · tags `docker`
- code: DEAD (v181's column over orders written before it; its runtime half moved)
- assertion map: older orders read version 1 → nothing; an order born now reads 1, the column's DEFAULT, since Create does not name it → `TestWriter_BornByTheColumnDefaults` (`shingo-core/store/orders/writer_roundtrip_test.go:283`)
- mutation: version's DEFAULT set to 2 inside the survivor · red at `27f5fb2e`: `writer_roundtrip_test.go:295: an order born now reads version 2, want 1` (`data-model-attack-2026-11-19/build/lane-relocate/24-red-orders-defaults-changed.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestOrderMigration_V182_NoOrderIsAsking`
- test: `TestOrderMigration_V182_NoOrderIsAsking` · pkg `shingocore/store` · run `-run '^TestOrderMigration_V182_NoOrderIsAsking$'` · `shingo-core/store/migration_v182_order_ask_test.go:20` @ `e06b29b0` · tags `docker`
- code: DEAD (v182's column over orders written before it; its runtime half moved)
- assertion map: older orders read a blank ask → nothing; an order born now reads blank, and reads back a written ask → `TestWriter_BornByTheColumnDefaults` (`shingo-core/store/orders/writer_roundtrip_test.go:283`)
- mutation: the ask's DEFAULT set to `sg-born` inside the survivor · red at `27f5fb2e`: `writer_roundtrip_test.go:298: an order born now reads ask "sg-born", want blank` (`data-model-attack-2026-11-19/build/lane-relocate/24-red-orders-defaults-changed.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestOrderMigration_V183_TheRuleRefusesANewBreakAndKeepsTheOldRows`
- test: `TestOrderMigration_V183_TheRuleRefusesANewBreakAndKeepsTheOldRows` · pkg `shingocore/store` · run `-run '^TestOrderMigration_V183_TheRuleRefusesANewBreakAndKeepsTheOldRows$'` · `shingo-core/store/migration_v183_dispatched_names_its_job_test.go:23` @ `e06b29b0` · tags `docker`
- code: DEAD (v183 adding the rule NOT VALID over rows already breaking it; its runtime half moved)
- assertion map: the old row kept and the rule present after v183 → nothing (such rows stay at the plants; `TestListTrackedVendorOrderIDs` holds one by dropping the rule); a new vendorless `dispatched` write refused by the rule's name, and one naming its job landing → `TestOrders_ADispatchedRowNamesItsJob` (`shingo-core/store/orders/orders_test.go:271`), which also reads the landed row back
- mutation: the rule dropped inside the survivor · red at `27f5fb2e`: ``orders_test.go:284: a vendorless `dispatched` write = <nil>, want the rule's refusal`` (`data-model-attack-2026-11-19/build/lane-relocate/25-red-orders-rule-dropped.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestMigrate_PendingRestocksRetired`
- test: `TestMigrate_PendingRestocksRetired` · pkg `shingocore/store` · run `-run '^TestMigrate_PendingRestocksRetired$'` · `shingo-core/store/migrations_retire_test.go:211` @ `e06b29b0` · tags `docker`
- code: DEAD (v70's drop, read as the chain's end state)
- assertion map: pending_restocks absent → the committed snapshot
- mutation: none to run: it reads the catalog
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestMigrate_CorrectionsRetired`
- test: `TestMigrate_CorrectionsRetired` · pkg `shingocore/store` · run `-run '^TestMigrate_CorrectionsRetired$'` · `shingo-core/store/migrations_retire_test.go:227` @ `e06b29b0` · tags `docker`
- code: DEAD (v104's drop surviving a second pass of the replaying baseline, which the squash removes)
- assertion map: corrections absent, and absent after a re-migrate → the committed snapshot; the second pass it guarded goes with the per-boot replay
- mutation: none to run: it calls `MigrateForTest` for the replay the squash removes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestMigrate_RetiredTableNotResurrected`
- test: `TestMigrate_RetiredTableNotResurrected` · pkg `shingocore/store` · run `-run '^TestMigrate_RetiredTableNotResurrected$'` · `shingo-core/store/migrations_retire_test.go:245` @ `e06b29b0` · tags `docker`
- code: DEAD (the self-heal dropping a stray retired table)
- assertion map: a stray pending_restocks dropped by a migration pass → nothing: the pass is gone
- mutation: none to run: it calls `MigrateForTest` for the self-heal the squash removes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### STRENGTHEN — `TestRetireReshuffleRestore_CancelsStray`
- test: `TestRetireReshuffleRestore_CancelsStray` · pkg `shingocore/store` · run `-run '^TestRetireReshuffleRestore_CancelsStray$'` · `shingo-core/store/migrations_retire_test.go:261` @ `e06b29b0` · tags `docker`
- code: STRENGTHEN (it ran no migration; moved to the terminal chokepoint's file and renamed)
- assertion map: every assertion kept → `TestRetireReshuffleRestoreOrders_CancelsAStrayAndItsChild` (`shingo-core/store/orders_terminalize_test.go:183`). Added: the child read back as cancelled (the old test read the parent only). The stations told: `TestStatusTellPin_RowsPerTypedWriter/RetireReshuffleRestoreOrders` (`shingo-core/engine/status_tell_pin_test.go:571`)
- mutation: the sweep skips every child (`if true || protocol.IsTerminal(c.Status)`) · survivor red at `27f5fb2e`: `orders_terminalize_test.go:198: cancelled = 1, want 2 (parent + child)` (`data-model-attack-2026-11-19/build/lane-relocate/17-red-retire-skips-children.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### STRENGTHEN — `TestRetireReshuffleRestore_NoOpClean`
- test: `TestRetireReshuffleRestore_NoOpClean` · pkg `shingocore/store` · run `-run '^TestRetireReshuffleRestore_NoOpClean$'` · `shingo-core/store/migrations_retire_test.go:295` @ `e06b29b0` · tags `docker`
- code: STRENGTHEN (it ran no migration; moved and renamed)
- assertion map: the count kept → `TestRetireReshuffleRestoreOrders_LeavesACleanDatabaseAlone` (`shingo-core/store/orders_terminalize_test.go:215`). Added: the live order beside it read back at its status
- mutation: the sweep selects every other order type (`order_type<>$1`) · survivor red at `27f5fb2e`: `orders_terminalize_test.go:222: cancelled = 1 on a clean DB, want 0` (`data-model-attack-2026-11-19/build/lane-relocate/18-red-retire-wrong-type.log`)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestMigrate_NoMigrationOscillatesOnEveryBoot`
- test: `TestMigrate_NoMigrationOscillatesOnEveryBoot` · pkg `shingocore/store` · run `-run '^TestMigrate_NoMigrationOscillatesOnEveryBoot$'` · `shingo-core/store/migrations_retire_test.go:332` @ `e06b29b0` · tags `docker`
- code: DEAD (the self-heal's verify set over a migrated database; the squash removes the per-boot self-heal)
- assertion map: no migration fails its post-condition on a second pass → nothing: the post-tip migrations' Verify and the boot's drift report replace it
- mutation: none to run: `MigrationsFailingTheirPostCondition` walks the chain the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestMigrate_StageTwoProduces`
- test: `TestMigrate_StageTwoProduces` · pkg `shingocore/store` · run `-run '^TestMigrate_StageTwoProduces$'` · `shingo-core/store/migrations_stage_two_test.go:17` @ `e06b29b0` · tags `docker`
- code: DEAD (v168's UPDATE and its self-heal on the next boot)
- assertion map: a stage 2 the store wrote consume made produce, partials and push off, by a re-migrate → nothing; the live door makes stage 2 produce → `TestCreateTwoStage_LinksStagesWithNoPlacement` (`shingo-core/service/loader_two_stage_docker_test.go:52`), and a produce loader refuses auto push at the door → `TestCreateLoader_TakesUnloaderSettingsAtCreate` (`:170`). A stage 2 written consume is no longer put right at boot; every binary from v168 on writes it produce
- mutation: none to run: it calls `MigrateForTest` for the self-heal the squash removes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestMigrate_Kanbans_ThePivot`
- test: `TestMigrate_Kanbans_ThePivot` · pkg `shingocore/store` · run `-run '^TestMigrate_Kanbans_ThePivot$'` · `shingo-core/store/kanban_migration_docker_test.go:70` @ `e06b29b0` · tags `docker`
- code: DEAD (v169's pivot of node properties into kanbans and its census go with `kanban_migration.go`)
- assertion map: the census lines, each property's column, a second pass changing nothing → nothing
- mutation: none to run: the test ages the schema and re-runs v169, which the squash deletes
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestMigrate_V171_TheEpisodeKindIsKanban`
- test: `TestMigrate_V171_TheEpisodeKindIsKanban` · pkg `shingocore/store` · run `-run '^TestMigrate_V171_TheEpisodeKindIsKanban$'` · `shingo-core/store/kanban_migration_docker_test.go:244` @ `e06b29b0` · tags `docker`
- code: DEAD (v171's rewrite of the episode kind)
- assertion map: old-word rows read kanban, others untouched, twice → nothing
- mutation: none to run: the test deletes v171's `schema_migrations` row and re-migrates
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestMigrate_V172_TheFamilies`
- test: `TestMigrate_V172_TheFamilies` · pkg `shingocore/store` · run `-run '^TestMigrate_V172_TheFamilies$'` · `shingo-core/store/kanban_migration_docker_test.go:281` @ `e06b29b0` · tags `docker`
- code: DEAD (v172's grouping of the types that predated families, and its census, go with `kanban_family_migration.go`; its runtime half moved)
- assertion map: the footprint families, the marker's, the code collision and the episode keys of the older types → nothing; a type inserted after with none stated takes a family of one coded by its code (the `bin_types_family_of_one` trigger) → `TestCreateType_ATypeWithNoFamilyTakesAFamilyOfOne` (`shingo-core/store/bins/bins_test.go:796`), which also pins the trigger's other branches: a bare marker takes its carrier's family, a code that names a family makes its own
- mutation: inside the survivor at `27f5fb2e`, red each time: the trigger dropped (`bins_test.go:819: insert type FAM-PLAIN: ERROR: null value in column "family_id" ...`, `lane-relocate/26-red-bintypes-trigger-dropped.log`); the collision branch removed (`bins_test.go:848: a type coded as an existing family is of family 3 (FAM-TAKEN) ...`, `lane-relocate/27-red-bintypes-collision-branch-removed.log`), both under `data-model-attack-2026-11-19/build/`
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestMigrate_V172_FamiliesAndThePool`
- test: `TestMigrate_V172_FamiliesAndThePool` · pkg `shingocore/store` · run `-run '^TestMigrate_V172_FamiliesAndThePool$'` · `shingo-core/store/kanban_family_migration_docker_test.go:34` @ `e06b29b0` · tags `docker`
- code: DEAD (v172 over the pre-family shape, and its census; its runtime half moved)
- assertion map: the families of the older types, the open episode's key, the census's near-footprint line → nothing; a type inserted with none stated takes a family of one, in the insert → `TestCreateType_ATypeWithNoFamilyTakesAFamilyOfOne` (`shingo-core/store/bins/bins_test.go:796`)
- mutation: as `TestMigrate_V172_TheFamilies`' row
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestRecordCount_NoCursorMeansNoFence`
- test: `TestRecordCount_NoCursorMeansNoFence` · pkg `shingocore/service` · run `-run '^TestRecordCount_NoCursorMeansNoFence$'` · `shingo-core/service/record_count_fence_test.go:86` @ `d2686a1d` · tags `docker`
- code: STALE
- assertion map: renamed `TestRecordCount_NoCursorFencesAtTheStart` (`shingo-core/service/record_count_fence_test.go`): one adjustment (kept); with no cursor in the generation the addressed station's copy is fenced at net 0, seq 0 for that station, and the count's `FenceFor` it gives the same (nil/nil/"" -> 0/0/the station). Flipped by the info-flow build's C6 ruling: every count Core states carries a fence per addressed station, at that station's own cursor, not "exactly one cursor, else none".
- mutation: `cursorFences` put back on the base's rule (fence only when exactly one station has a cursor, naming it; else none) · renamed test red: `record_count_fence_test.go:110: fence = {<nil>, <nil>, ""}, want {0, 0, "stn-test"}`, `--- FAIL: TestRecordCount_NoCursorFencesAtTheStart`
- coverage Δ: not measured for this row (the same paths run under the new name)
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestRecordCount_TwoCountingStationsMeansNoFence`
- test: `TestRecordCount_TwoCountingStationsMeansNoFence` · pkg `shingocore/service` · run `-run '^TestRecordCount_TwoCountingStationsMeansNoFence$'` · `shingo-core/service/record_count_fence_test.go:118` @ `d2686a1d` · tags `docker`
- code: STALE
- assertion map: renamed `TestRecordCount_TwoCountingStationsEachIsFencedAtItsOwn` (`shingo-core/service/record_count_fence_test.go`): one adjustment, to the station claiming the seat (kept); it is fenced at that station's own cursor, never the other counting station's (no fence -> -3/1/the claiming station), and `FenceFor` the other station gives that station's own cursor. Flipped by C6, as the row above.
- mutation: as the row above · renamed test red: `record_count_fence_test.go:154: fence = {<nil>, <nil>, ""}, want {-3, 1, "stn-test"}`, `--- FAIL: TestRecordCount_TwoCountingStationsEachIsFencedAtItsOwn`
- coverage Δ: not measured for this row (the same paths run under the new name)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestApiConfig_UpdateStationID`
- test: `TestApiConfig_UpdateStationID` · pkg `shingoedge/www` · run `-run '^TestApiConfig_UpdateStationID$'` · `shingo-edge/www/handlers_api_config_test.go:736` @ `d2686a1d` · tags `none`
- code: DEAD (E1's legacy `station_id` carry goes with the Station UID field: the Configuration page no longer sets the uid, so the door writes neither id)
- assertion map: a Station section carrying only the legacy `station_id` is saved → nothing (the key is not read); a section carrying a `station_uid` other than the running one is refused, the running one or none accepted → `TestPinEdgeConfig_StationID` (`shingo-edge/www/handlers_config_pins_test.go:367`)
- mutation: in the survivor, `applyStation`'s refusal never fires (`*s.StationUID == "never-posted"`): `--- FAIL: TestPinEdgeConfig_StationID/a_different_uid_is_refused`, `--- FAIL: TestPinEdgeConfig_StationID/a_blank_uid_is_refused`
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: `handlers_config_pins_test.go`'s Station UID block, which named it, is rewritten
- verified at: the commit that adds this row

### DEAD — `TestConfigPage_LegacyStationIDCarried`
- test: `TestConfigPage_LegacyStationIDCarried` · pkg `shingoedge/www` · run `-run '^TestConfigPage_LegacyStationIDCarried$'` · `shingo-edge/www/config_page_render_test.go:153` @ `d2686a1d` · tags `none`
- code: DEAD (the page's `data-legacy-station-id` carry goes with the Station UID field)
- assertion map: a legacy-only Edge's id in the page data, an Edge with a uid carrying none → nothing; the uid is rendered as text and as no field the page could post → `TestConfigPage_StationUIDIsText` (`shingo-edge/www/config_page_render_test.go:153`)
- mutation: not run (the carry it pinned is deleted, not weakened)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestPinEdgeConfig_LegacyOnlyStationSaves`
- test: `TestPinEdgeConfig_LegacyOnlyStationSaves` · pkg `shingoedge/www` · run `-run '^TestPinEdgeConfig_LegacyOnlyStationSaves$'` · `shingo-edge/www/handlers_config_pins_test.go:1100` @ `d2686a1d` · tags `none`
- code: DEAD (E1's legacy carry, as `TestApiConfig_UpdateStationID`'s row)
- assertion map: a legacy-only Edge's timezone save with its `station_id` carried → nothing; a Station section with no `station_uid` saves its timezone and auto-confirm and leaves the uid → `TestPinEdgeConfig_StationID` `no_uid_is_accepted` (`shingo-edge/www/handlers_config_pins_test.go:367`)
- mutation: not run (the carry it pinned is deleted, not weakened)
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestCoreSyncService_StartupReconcileCallsAllHooks`
- test: `TestCoreSyncService_StartupReconcileCallsAllHooks` · pkg `shingoedge/engine` · run `-run '^TestCoreSyncService_StartupReconcileCallsAllHooks$'` · `shingo-edge/engine/engine_db_test.go:388` @ `d2686a1d` · tags `none`
- code: STALE (the behaviour it pinned was changed by ruling: StartupReconcile asks for no node list or catalog)
- assertion map: node-sync and catalog-sync hooks called once each → called 0 times each; the send-function error from the order status request → unchanged. Same fixture, renamed for what it now holds: `TestCoreSyncService_StartupReconcileAsksOnlyForOrders` (`shingo-edge/engine/engine_db_test.go`)
- mutation: the renamed test fails against the base's StartupReconcile (1/1 asks, want 0/0)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestSubProcessIDsRoundTrip`
- test: `TestSubProcessIDsRoundTrip` · pkg `shingocore/store/heartbeat` · run `-run '^TestSubProcessIDsRoundTrip$'` · `shingo-core/store/heartbeat/cellconfig_test.go:12` @ `d2686a1d` · tags `none`
- code: DEAD (the JSONB encoding of cell_config.sub_process_ids; v189 drops the table and the encoder with it)
- assertion map: nil/empty/one/many round-trip through `marshalIDs`/`unmarshalIDs` → nothing: no cell is stored, so no sub-process list is encoded
- mutation: none to run: the functions it calls are deleted with the table
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestUnmarshalIDsLenientInputs`
- test: `TestUnmarshalIDsLenientInputs` · pkg `shingocore/store/heartbeat` · run `-run '^TestUnmarshalIDsLenientInputs$'` · `shingo-core/store/heartbeat/cellconfig_test.go:44` @ `d2686a1d` · tags `none`
- code: DEAD (as `TestSubProcessIDsRoundTrip`: the decoder of a dropped column)
- assertion map: "", "null", "[]" decode to an empty slice → nothing
- mutation: none to run: `unmarshalIDs` is deleted with the table
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### DEAD — `TestCoverage_CellConfig`
- test: `TestCoverage_CellConfig` · pkg `shingocore/store/heartbeat` · run `-run '^TestCoverage_CellConfig$'` · `shingo-core/store/heartbeat/cellconfig_docker_test.go:17` @ `d2686a1d` · tags `docker`
- code: DEAD (cell_config's CRUD; v189 drops the table and its functions)
- assertion map: list/upsert/get/update/delete of a cell_config row → nothing; the process-picker half (`DistinctProcesses`: one row per ticking process, most ticks first, its style) → `TestDistinctProcesses_RanksAStationsTickingProcesses`, which also asserts the second row and that another station's process is not listed
- mutation: none to run for the CRUD half; the picker half's assertions are carried whole
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestOnCoreAck_AQuietAckReadsOnePlusTwoPerProcess`
- test: `TestOnCoreAck_AQuietAckReadsOnePlusTwoPerProcess` · pkg `shingoedge/engine` · run `-run '^TestOnCoreAck_AQuietAckReadsOnePlusTwoPerProcess$'` · `shingo-edge/engine/feeds_claims_test.go:99` @ `ebdabedf` · tags `none`
- code: STALE (pin 9: the behaviour it pinned was changed by ruling D1, the owned map's cached digests)
- assertion map: a quiet ack reads exactly 1 + 2P statements, 0 writes, sends no report (P = 1, 3, 6) → a quiet heartbeat's quote and a quiet ack run 0 statements, build no family and send nothing, from a Core that speaks the owned map and from an older one, at the same P and fixture: `TestOnCoreAck_AQuietAckRunsNoStatementAndNoHash` (`shingo-edge/engine/owned_test.go`); 0 hashes by that and `TestOwned_EveryOwnedDigestIsInABuildOrASend`
- mutation: the new pin fails with the claims family rebuilt on every ack (4 statements and a build per family at P=1)
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: `PREDICTIONS.md` row 9 names it (a working doc, outside the repo)
- verified at: the commit that adds this row

### STALE — `TestClaimSendDue_GuardAndFlag`
- test: `TestClaimSendDue_GuardAndFlag` · pkg `shingoedge/engine` · run `-run '^TestClaimSendDue_GuardAndFlag$'` · `shingo-edge/engine/feeds_claims_test.go:53` @ `ebdabedf` · tags `none`
- code: STALE (`ClaimSendDue` and `ClaimsSettled` are gone; the claims keys go through `sendDue` and the family's `settleOwned`)
- assertion map: the same steps, digests and flag texts, through `sendDue(claimsGuardKey(p), …)` and `settleOwned` with P2 pending → `TestOwned_ClaimsGuardAndFlag` (`shingo-edge/engine/owned_test.go`)
- mutation: unchanged assertions; a guard that does not hold a repeat fails step 1 as before
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### FOLD — `TestReconcileClaims_RepublishesWhatDiffers`
- test: `TestReconcileClaims_RepublishesWhatDiffers` · pkg `shingoedge/messaging` · run `-run '^TestReconcileClaims_RepublishesWhatDiffers$'` · `shingo-edge/messaging/plant_claims_reconcile_test.go:91` @ `ebdabedf` · tags `none`
- code: FOLD (`ReconcileClaims` is gone: the compare is the engine's owned map, the read and send the publisher's `ClaimsMembers` and `SendClaims`)
- assertion map: same fixture (SAME, STALE, MISSING, BARE; Core holding SAME, STALE, BARE, GONE) and the same published set `BARE:0 GONE:0 MISSING:1 STALE:1`, from the owned map and from an older Core's Claims → `TestAnswerOwned_ClaimsRepublishesWhatDiffers` (`shingo-edge/engine/owned_test.go`); the empty report for a bare or gone name → `TestSendClaims_TheReportOrTheEmptyOne` (`shingo-edge/messaging/plant_claims_reconcile_test.go`)
- mutation: not run against the fold
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### FOLD — `TestReconcileClaims_GuardHoldsARepeat`
- test: `TestReconcileClaims_GuardHoldsARepeat` · pkg `shingoedge/messaging` · run `-run '^TestReconcileClaims_GuardHoldsARepeat$'` · `shingo-edge/messaging/plant_claims_reconcile_test.go:120` @ `ebdabedf` · tags `none`
- code: FOLD (`ReconcileClaims` is gone; the guard is the engine's real one, not a fake)
- assertion map: the same differing answer twice publishes each process once → the second half of `TestAnswerOwned_ClaimsRepublishesWhatDiffers` (`shingo-edge/engine/owned_test.go`)
- mutation: not run against the fold
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### FOLD — `TestReconcileClaims_FailedReadPublishesNothing`
- test: `TestReconcileClaims_FailedReadPublishesNothing` · pkg `shingoedge/messaging` · run `-run '^TestReconcileClaims_FailedReadPublishesNothing$'` · `shingo-edge/messaging/plant_claims_reconcile_test.go:137` @ `ebdabedf` · tags `none`
- code: FOLD (`ReconcileClaims` is gone)
- assertion map: a process list that cannot be read publishes nothing, not even the empty reports, and settles nothing → `TestAnswerOwned_AFailedReadPublishesNothing` (`shingo-edge/engine/owned_test.go`, the family never read and read before) and `TestClaimsMembers_AFailedListReadKnowsNothing` (`shingo-edge/messaging/plant_claims_reconcile_test.go`)
- mutation: not run against the fold
- coverage Δ: not measured (no before/after profile was taken)
- citations rewritten: none
- verified at: the commit that adds this row

### STRENGTHEN — `TestFeedScopes_CoverAllFeeds`
- test: `TestFeedScopes_CoverAllFeeds` · pkg `shingocore/messaging` · run `-run '^TestFeedScopes_CoverAllFeeds$'` · `shingo-core/messaging/feeds_test.go:320` @ `d2686a1d` · tags `none`
- code: STRENGTHEN
- assertion map: renamed `TestCoreFeeds_EveryFeedHasARow` (`shingo-core/messaging/feeds_test.go`) when Core's three lists of a feed's facts (`feedScopes`, the build switch, the send switch) became one row per feed (`coreFeeds`): every feed key has a row and none names a key protocol does not list (kept, now as one row per key in `protocol.AllFeeds`' order); every ask subject is in `CoreInboundSubjects` (kept). The lifetime-or-noMemo check is gone with `noMemo` (a zero ttl is never memoised; the lifetimes are pinned by `TestFeedMemo_ScopeAndTTLCharacterised`). Added: every row has a build, a carrier and a send.
- mutation: the sourcing row without its send · rewritten red: `go test -count=1 -tags lockcheck -run 'TestCoreFeeds_' ./messaging/` → `feeds_test.go:337: sourcing: build set true, send set false, carrier "sourcing.state"; want all three`, `--- FAIL: TestCoreFeeds_EveryFeedHasARow`
- coverage Δ: not measured for this row (the same table is read under the new name)
- citations rewritten: none
- verified at: the commit that adds this row

### STALE — `TestRevision_IsAFunctionOfTheRowsOnly`
- test: `TestRevision_IsAFunctionOfTheRowsOnly` · pkg `shingocore/store/scene` · run `-run '^TestRevision_IsAFunctionOfTheRowsOnly$'` · `shingo-core/store/scene/revision_test.go:13` @ `d2686a1d` · tags `none`
- code: STALE
- assertion map: renamed `TestRevision_IsAFunctionOfTheContentOnly` (`shingo-core/store/scene/revision_test.go`) when the scene revision became a hash of what the rows say instead of each row's (id, synced_at): read order does not matter, a hex SHA-256, an unchanged scene keeps its revision, a row replaced with the count kept is a new scene, a point and an edge do not collide (kept, over content); a re-synced point (synced_at bumped, nothing else) is now the same scene, not a new one (flipped: the build's C4 ruling, one geometry send per Edge at upgrade). Added: a point moved a millimetre, handles at (0,0) against none, and the same text cut into different fields are each a new scene.
- mutation: the revision over (id, synced_at), as at `d2686a1d` · rewritten red: `go test -count=1 -run 'TestRevision_' ./store/scene/` → `revision_test.go:41: the same scene synced again has a new revision …`, `revision_test.go:48: a moved point left the revision unchanged`, `revision_test.go:60: handles at (0,0) hash like no handles`, `revision_test.go:72: two points that cut the same text into different fields hash alike`, `--- FAIL: TestRevision_IsAFunctionOfTheContentOnly`
- coverage Δ: not measured for this row (the same function runs under the new name)
- citations rewritten: `TestNodeFeed_TheSameSceneReSyncedIsNotSentAgain`'s header
- verified at: the commit that adds this row

### STRENGTHEN — `TestPayloadsChanged_DropsTheMemos`
- test: `TestPayloadsChanged_DropsTheMemos` · pkg `shingocore/messaging` · run `-run '^TestPayloadsChanged_DropsTheMemos$'` · `shingo-core/messaging/node_feed_test.go:351` @ `d2686a1d` · tags `docker`
- code: STRENGTHEN
- assertion map: renamed `TestFeedsChanged_PushesWhatTheWriteChanged` (`shingo-core/messaging/node_feed_test.go`) when the payload service's memo-drop hook (`PayloadsChanged`, gone) became the change doors' body (`CoreDataService.FeedsChanged`): a payload write through the service, wired as the composition root wires it, leaves no catalog memo and no station's node-list memo (kept); an Edge holding nothing is sent the catalog with the new part and a node list with its carrier row on its next heartbeat (kept, for the Edge Core never sent a feed). The heartbeat that sent an Edge holding the old digest the new catalog is now the push: each Edge Core has sent a feed to is pushed one catalog with the new part and one node list with its carrier row, and a heartbeat quoting what it was pushed is sent nothing; the Edge Core never sent anything is pushed nothing.
- mutation: `FeedsChanged` arms no push · rewritten red: `SHINGO_TEST_REQUIRE_DOCKER=1 go test -tags=docker,lockcheck -count=1 -p 1 -run 'TestFeedsChanged_' ./messaging/` → `node_feed_test.go:637: edge.test was pushed <nil>; want one catalog with the new part and one node list with its carrier row`, `--- FAIL: TestFeedsChanged_PushesWhatTheWriteChanged`
- coverage Δ: not measured for this row (the drop runs under the new name; the push is new)
- citations rewritten: `PayloadsChanged`'s doc, moved to `feed_doors.go`'s header
- verified at: the commit that adds this row

### STRENGTHEN — `TestPayloadService_PayloadsChangedHook`
- test: `TestPayloadService_PayloadsChangedHook` · pkg `shingocore/service` · run `-run '^TestPayloadService_PayloadsChangedHook$'` · `shingo-core/service/payload_service_test.go:247` @ `d2686a1d` · tags `docker`
- code: STRENGTHEN
- assertion map: renamed `TestPayloadService_EveryPayloadWriteGoesThroughTheDoor` (`shingo-core/service/payload_service_test.go`) when the hook became the change doors (`service.FeedDoors`): every payload write calls it once, after it succeeded, and a refused write calls nothing (kept). Added: each call is the plant-wide door (no stations).
- mutation: none run for this row; the assertions are the old test's on the renamed hook, and the door's red-before is `TestFeedsChanged_PushesWhatTheWriteChanged`'s row
- coverage Δ: not measured for this row (the same writes run under the new name)
- citations rewritten: none
- verified at: the commit that adds this row
