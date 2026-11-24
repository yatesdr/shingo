#!/usr/bin/env bash
# tier: alone
# check-test-cost.sh — a new test over 10 s run alone says why.
#
# A top-level test the base does not have (the test ledger's base and notion
# of a test: shared/scripts/internal/testset) that carries no
# `// cost: <reason>` line in its doc comment is run alone, from its package's
# test binary, and fails when it costs more than 10 s twice: once anyhow, then
# again on a quiet host. A host that never goes quiet leaves the test NOT
# JUDGED, printed, never red. shared/scripts/testcost says what "alone" and
# "costs" mean, and the repolint's test-costs rule where a reason stands.
#
# TIER alone: it times tests, so it runs only when the scripts step runs by
# itself (`bash scripts/gate.sh scripts`, which CI's guards job runs), never
# beside a gate's own build and test steps, whose load is what it must not
# time. Docker runs its own Postgres for the docker tests.
#
# BASE: TEST_LEDGER_BASE, else `git merge-base HEAD origin/main`.
#
# Exit 0 = no new test over the line without a reason, 1 = one is, 2 = it
# could not run.

set -uo pipefail

cd "$(dirname "$0")/../shared"

exec go run ./scripts/testcost -root ..
