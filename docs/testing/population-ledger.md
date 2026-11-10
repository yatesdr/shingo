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
