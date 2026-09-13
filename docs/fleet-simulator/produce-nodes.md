# Produce Nodes & Edge Wiring

## Overview

Produce nodes fill empty bins. When the operator finalizes (locks the UOP count), the system manifests the bin at Core via an ingest order, then dispatches swap choreography based on the claim's swap mode: simple (bare ingest), sequential (ingest + complex removal), single_robot (10-step all-in-one swap), or two_robot (two coordinated complex orders). Edge wiring handles UOP tracking — counter deltas increment produce UOP (counting UP) and decrement consume UOP (counting DOWN, floored at zero). Completion events reset UOP based on node role and order type.

Edge tests use SQLite in a temp directory — no Docker required.

## Test files

- `engine/produce_swap_test.go` — swap mode finalization (TC-66a-f, 7 tests)
- `engine/wiring_test.go` — edge event handlers (TC-67a-g, TC-70, 8+ tests)

Run this domain's tests:

```bash
cd shingo-edge
go test -v -run "TestProduce|TestWiring|TestHandlePayloadCatalog" ./engine/ -timeout 60s
```

The TC-nn case ledger that used to sit below this line was a test report — what
was run on a date and what it found — not reference, so it now lives outside the
repo with the other reports. The tests themselves are the live record; the files
above are where to read them.
