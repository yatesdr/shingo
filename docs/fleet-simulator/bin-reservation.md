# Bin Reservation & Claiming

## Overview

Bin claiming protects bins from being dispatched by multiple orders simultaneously. The system uses atomic SQL updates (`claimed_by IS NULL` guard) to reserve bins during planning. The staging sweep (`ReleaseExpiredStagedBins`) flips bins from `staged` to `available` after TTL expiry. The orphan claim sweep (`ReleaseOrphanedClaims`) catches leaked claims from non-atomic terminal transitions. Bugs in this area cause double-dispatches (two robots sent to the same bin), phantom robots (dispatched with no bin), and permanently stuck inventory (claims never released).

## Test files

- `engine/engine_claim_test.go` — claim hand-off and store/move guards (TC-13, 23a, 25)
- `engine/engine_quality_test.go` — quality-hold dispatch (TC-21)
- `engine/engine_linechangeover_test.go` — changeover with in-flight move (TC-23d)
- `engine/engine_terminal_test.go` — cancel and fleet-failure return-claim transfer (TC-23b, 30, 36)
- `engine/engine_reconciliation_test.go` — orphaned bin claim sweep (TC-80)
- `engine/engine_concurrent_test.go` — concurrent retrieve + staging expiry vs active claim (TC-28, 37)

Run this domain's tests:

```bash
cd shingo-core
go test -v -run "TestClaimBin|TestDispatch_QualityHoldBin|TestMoveBin|TestCancel_ClaimTransfers|TestLineChangeover|TestStoreOrder_ClaimsStagedBin|TestConcurrentRetrieve|TestFailedOrder_TransfersReturnClaim|TestRetrieveClaimFailure|TestStagingExpiry|TestOrphanedBinClaim" ./engine/ -timeout 60s
```

The TC-nn case ledger that used to sit below this line was a test report — what
was run on a date and what it found — not reference, so it now lives outside the
repo with the other reports. The tests themselves are the live record; the files
above are where to read them.
