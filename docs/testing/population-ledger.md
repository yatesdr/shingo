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
