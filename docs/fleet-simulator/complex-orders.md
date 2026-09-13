# Complex & Compound Orders

## Overview

Complex orders handle multi-step robot instructions (pickup, dropoff, wait, swap) through StepsJSON → claimComplexBins → fleet dispatch. Compound reshuffles are multi-child orders for buried bin retrieval in supermarket NGRP lanes — the system detects blocked bins, plans a sequence of unbury/retrieve/restock children, and executes them sequentially with lane locking. The order_bins junction table tracks multi-pickup orders where a single complex order moves multiple bins.

## Test files

- `engine/engine_complex_test.go` — complex order lifecycle and production cycle patterns (TC-42–60)
- `engine/engine_compound_test.go` — compound reshuffle tests (TC-40a–54)
- `dispatch/group_resolver_test.go` — FIFO/COST resolution, buried bin detection
- `dispatch/end_to_end_test.go` — reshuffle end-to-end
- `dispatch/reshuffle_test.go` — reshuffle planning

Run this domain's tests:

```bash
cd shingo-core
go test -v -run "TestComplexOrder|TestCompound|TestBuriedBin|TestLaneLock|TestFIFOSelection|TestCOSTSelection|TestFindEmptyCompatible|TestRetrieveEmpty" ./engine/ ./dispatch/ -timeout 120s
```

The TC-nn case ledger that used to sit below this line was a test report — what
was run on a date and what it found — not reference, so it now lives outside the
repo with the other reports. The tests themselves are the live record; the files
above are where to read them.
