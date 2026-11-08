#!/usr/bin/env bash
# tier: dev
# check-repolint.sh — the repo-wide text rules, in one Go process.
#
# One `git ls-files` listing, one read of each tracked file and one node process
# for every .js file, in place of a bash guard per rule: comment references
# (file:line), conflict markers, js-parse, plant addresses, swap-mode literals,
# synthetic part numbers, text files, and EXPIRY-by-symbol pins. Whole tree,
# every run. Each rule prints its own `ok   <rule> (...)` line, or
# `FAIL <rule>  file:line  why` per finding. The rules, their reasons and the
# cases each must fire on live in shared/scripts/repolint.
#
# A new repo-wide rule is a rule there, not a new scripts/check-*.sh.
#
# Exit 0 = clean, exit 1 = a finding (a missing node is one), exit 2 = the tree
# could not be read.

set -uo pipefail

cd "$(dirname "$0")/../shared"

exec go run ./scripts/repolint -root ..
