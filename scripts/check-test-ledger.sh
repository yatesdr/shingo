#!/usr/bin/env bash
# tier: dev
# check-test-ledger.sh — a test may leave only with a row in the ledger.
#
# A func Test... present at the base and gone at the tip (the working tree) must
# have a row in docs/testing/population-ledger.md naming it and its package
# (import path). A test moved to another file of its package has not gone; one
# renamed or moved to another package has. The missing rows are printed as
# skeletons to fill in. The rules and their pins live in shared/scripts/testledger.
#
# BASE: `git merge-base HEAD origin/main`, or the ref in TEST_LEDGER_BASE. With
# no origin/main (a shallow clone, a fresh worktree) it fails and says so rather
# than comparing against nothing.
#
# Exit 0 = every vanished test has a row, exit 1 = one has none or there is no
# base to compare against.

set -uo pipefail

cd "$(dirname "$0")/../shared"

if go run ./scripts/testledger -root ..; then
  echo "ok   test ledger (every test gone since the base has a row)"
  exit 0
fi
exit 1
