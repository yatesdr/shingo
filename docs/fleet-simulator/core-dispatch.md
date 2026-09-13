# Core Dispatch

## Overview

Core dispatch translates order requests into fleet transport orders. It handles state mapping from fleet to Shingo statuses (CREATED→dispatched, RUNNING→in_transit, WAITING→staged, FINISHED→delivered), staged order release timing, bad input rejection, and the full dispatch-to-receipt lifecycle. This is the foundation — every other system builds on top of dispatch working correctly.

## Test files

- `dispatch/fleet_simulator_test.go` — outbound fleet instruction verification (TC-1, 3, 4, 5)
- `engine/engine_simulator_test.go` — engine-level lifecycle and staging tests (TC-2, 15)
- `engine/engine_terminal_test.go` — post-delivery cancel guard (TC-68)
- `engine/engine_concurrent_test.go` — malformed input handling (TC-9, 10, 12)
- `messaging/core_data_service_test.go` — node list sync (TC-69)

Run this domain's tests:

```bash
cd shingo-core
go test -v -run "TestSimulator|TestComplexOrder|TestCancelDeliveredOrder|TestTerminateOrder|TestNodeListResponse|TestMaybeCreateReturnOrder" ./engine/ ./dispatch/ ./messaging/ -timeout 120s
```

The TC-nn case ledger that used to sit below this line was a test report — what
was run on a date and what it found — not reference, so it now lives outside the
repo with the other reports. The tests themselves are the live record; the files
above are where to read them.
