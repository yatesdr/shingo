# Concurrency & Stress

## Overview

Concurrency tests verify the system under contention. The primary concern is the TOCTOU (time-of-check-time-of-use) race between `FindSourceBinFIFO` and `ClaimBin` — two orders can both find the same bin before either claims it. The PostFindHook enables deterministic race injection by pausing between find and claim. `simulator.ParallelGroup` provides barrier-synchronized goroutine launches for stress testing. These tests confirm that contention produces correct outcomes: no double-claims, no permanent failures, excess orders queued for retry.

## Test files

- `engine/engine_concurrent_test.go` — all concurrency tests (TC-71a-e)

Run this domain's tests:

```bash
cd shingo-core
go test -v -run "TestConcurrent|TestRedirect|TestFulfillmentScanner" ./engine/ -timeout 120s
```

The TC-nn case ledger that used to sit below this line was a test report — what
was run on a date and what it found — not reference, so it now lives outside the
repo with the other reports. The tests themselves are the live record; the files
above are where to read them.
