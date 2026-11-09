#!/usr/bin/env bash
# tier: dev
# check-test-citations.sh — a test a Go comment cites must still exist.
#
# A comment that names a test is read as "this is pinned": the scar's PIN
# (docs/ground-rules.md), a "covered by ..." in a DUP argument, a `PIN:` marker. A rename,
# a fold or a deletion leaves the comment claiming coverage nothing provides,
# and no build fails. Every Test... named in a Go comment of the five modules,
# and every PIN: Test/Fuzz/Benchmark marker, must name a test function in one of
# them; TestParent/case must name a subtest the parent's body spells
# literally. A gravestone ("It was TestX", "TestX was deleted") records history
# and is exempt by the words touching the name. The rules and their pins live in
# shared/scripts/testcite; -graves lists what the exemption let through.
#
# Exit 0 = clean, exit 1 = a comment cites a test that is not there.

set -uo pipefail

cd "$(dirname "$0")/../shared"

if go run ./scripts/testcite -root ..; then
  echo "ok   test citations (every test a comment names exists)"
  exit 0
fi
exit 1
