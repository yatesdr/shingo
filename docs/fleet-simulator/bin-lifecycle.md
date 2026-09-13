# Bin Lifecycle & Manifest

## Overview

The BinManifestService centralizes all bin manifest mutations — clear, sync UOP, set for production, confirm — into atomic operations. ClaimForDispatch routes through the remainingUOP protocol extension to determine the right atomic operation: nil→plain claim, zero→ClearAndClaim (full depletion), positive→SyncUOPAndClaim (partial consumption). This replaces scattered `db.SetBinManifest`/`db.ClearBinManifest` calls with service-level operations that close TOCTOU race windows.

Core tests use PostgreSQL 16 via testcontainers (Docker required). Edge tests use SQLite in a temp directory — no Docker required.

## Test files

- `service/bin_manifest_test.go` — BinManifestService unit tests (TC-62a-h, 12 tests)
- `dispatch/bin_lifecycle_test.go` — full/partial depletion integration (TC-63, 64, 4 tests)
- `dispatch/planning_test.go` — extractRemainingUOP unit tests (TC-65, 7 tests)

Run this domain's tests:

```bash
cd shingo-core
go test -v -run "TestBinManifestService|TestFullDepletion|TestPartialConsumption|TestExtractRemainingUOP|TestConcurrentRetrieveEmpty_GhostBin" ./service/ ./dispatch/ -timeout 60s
```

The TC-nn case ledger that used to sit below this line was a test report — what
was run on a date and what it found — not reference, so it now lives outside the
repo with the other reports. The tests themselves are the live record; the files
above are where to read them.
