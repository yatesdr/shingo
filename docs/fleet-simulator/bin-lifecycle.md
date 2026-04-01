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

## Index

| TC | Description | Status |
|----|-------------|--------|
| TC-62a | ClearForReuse nulls manifest and makes bin visible to FindEmpty | PASS |
| TC-62b | SyncUOP preserves manifest and payload while updating count | PASS |
| TC-62c | ClearAndClaim atomically clears manifest + claims in one UPDATE | PASS |
| TC-62d | ClearAndClaim rejects already-claimed bin | PASS |
| TC-62e | ClearAndClaim rejects locked bin | PASS |
| TC-62f | SyncUOPAndClaim updates count + claims atomically | PASS |
| TC-62g | SetForProduction sets manifest, payload, UOP | PASS |
| TC-62h | Confirm marks manifest as confirmed | PASS |
| TC-63a | ClaimForDispatch nil → plain claim (no manifest change) | PASS |
| TC-63b | ClaimForDispatch zero → ClearAndClaim (full depletion) | PASS |
| TC-63c | ClaimForDispatch positive → SyncUOPAndClaim (partial) | PASS |
| TC-64a | Full depletion (remainingUOP=0) clears manifest on dispatch | PASS |
| TC-64b | Partial consumption (remainingUOP=42) syncs UOP, preserves manifest | PASS |
| TC-64c | Concurrent retrieve_empty cannot steal bin during clear+claim | PASS |
| TC-64d | Concurrent ClaimForDispatch race — one ClearAndClaim, one SyncUOPAndClaim, exactly one wins | PASS |
| TC-65 | extractRemainingUOP: nil envelope, empty payload, missing field, zero, positive, malformed | PASS |

## Verified scenarios

### BinManifestService — centralized manifest mutations (TC-62)

**Scenario:** All bin manifest mutations (clear, sync UOP, set for production, confirm) now flow through a single `BinManifestService` instead of scattered `db.SetBinManifest` / `db.ClearBinManifest` calls. The service also provides atomic `ClearAndClaim` and `SyncUOPAndClaim` operations that close the TOCTOU race window between clearing a bin's manifest and claiming it for dispatch.

**Expected behavior:** Each operation mutates exactly the fields it should and nothing else. Atomic operations succeed or fail as a unit — no partial state. Claims are rejected if the bin is already claimed or locked.

**Result:** PASS. 12 unit tests in `service/bin_manifest_test.go`.

**Test:** `shingocore/service/bin_manifest_test.go` — `TestBinManifestService_*`

---

### ClaimForDispatch routing — remainingUOP protocol (TC-63, TC-64, TC-65)

**Scenario:** Edge sends `remaining_uop` on move orders to tell Core the bin's consumption state. Three cases: nil (legacy, no sync), zero (fully depleted — clear manifest), positive (partial consumption — sync UOP count). The dispatcher extracts this from the envelope and routes through `ClaimForDispatch`.

**Expected behavior:** nil → plain `ClaimBin` (no manifest change). Zero → `ClearAndClaim` (atomic clear + claim, bin becomes visible to `FindEmptyCompatibleBin`). Positive → `SyncUOPAndClaim` (UOP updated, manifest preserved, bin claimed).

**Result:** PASS. 4 integration tests in `dispatch/bin_lifecycle_test.go`, 7 unit tests for `extractRemainingUOP` in `dispatch/planning_test.go`.

**Bug found and fixed:** `DecrementBinUOP` was dead code — never called by any path. Removed. The new `SyncUOPAndClaim` replaces it with an atomic operation.

**Bug found and fixed:** `tryAutoRequestEmpty` was incorrectly called on produce node ingest completion. This function is bin_loader-only — it calls `RequestEmptyBin` which gates on `role == "bin_loader"`. Removed the call; produce nodes don't auto-request after ingest because simple mode still has the filled bin at the node, and swap modes already have complex orders in flight.

**Test:** `shingocore/dispatch/bin_lifecycle_test.go` — `TestFullDepletion_ClearsManifest`, `TestPartialConsumption_SyncsUOP`, `TestConcurrentRetrieveEmpty_GhostBin`; `shingocore/dispatch/planning_test.go` — `TestExtractRemainingUOP_*`
