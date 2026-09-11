#!/usr/bin/env bash
# check-test-error-discard-ratchet.sh — a monotonic-decrease ratchet on the
# number of test lookups that throw their error away and read the value anyway.
#
# THE SHAPE:
#
#     order, _ := db.GetOrder(id)
#     if order.Status != StatusStaged { ... }
#
# When GetOrder succeeds this is a saved line. When it fails it is a nil
# pointer dereference on the NEXT line, and that is a different kind of
# failure: the run no longer reports what went wrong. The pgx error — the one
# sentence that said the connection was refused, or that the SASL handshake
# timed out — was discarded at the `_`, and what CI prints instead is a panic
# trace naming a struct field, plus every other goroutine in the process. Under
# a saturated docker suite that is not the rare case; one unhealthy container
# turns thirty of these into thirty panics that all name the wrong thing.
#
# A run that destroys its own evidence costs more than the run it replaced,
# because the next step after reading it is to reproduce it. The fix is one
# line: testutil.Must / testutil.MustNoErr (protocol/testutil/must.go) name the
# failing call and the error on the line that has both.
#
# WHY A COUNT AND NOT A LIST, AND NOT errcheck-ON-TESTS. There are ~1500 of
# these left. Turning errcheck on for tests in .golangci.yml would land ~1500
# findings in one report, which that file's own header rejects for exactly the
# reason it gives about truncated reports: a wall of pre-existing findings is a
# wall people learn to scroll past, and it blocks the unrelated PR that happens
# to touch a test file. A ratchet blocks only the thing worth blocking — a NEW
# one — and it does so in the diff that adds it.
#
# THE THREE RULES (same as engine_db_methods_freeze_test.go, which is where
# this repo's ratchet convention lives):
#
#   count >  FROZEN  → FAIL. A new silent discard was added. Use testutil.Must.
#   count <  FROZEN  → FAIL. Sites were converted and the constant was left
#                      stale. Ratchet it down IN THE SAME COMMIT, or the number
#                      stops describing anything.
#   count == FROZEN  → pass.
#
# The under-count rule is the load-bearing one. docs/ui-style-guide.md's z-layer
# and chip rulings both say it: a ratchet parked under reality is weaker than
# the floor it stands in for, because it goes green while the thing it measures
# drifts. A constant nobody is forced to move is a comment.
#
# WHAT RETIRES THIS SCRIPT: FROZEN reaching 0. At that point every test lookup
# in the repo either checks its error or routes through testutil.Must, the
# monotone property has been paid for, and the guard that replaces this one is
# `errcheck` with `check-blank: true` enabled for tests in .golangci.yml — a
# hard floor rather than a ratchet. Delete this file and that constant together
# with the exemption, the way the chip ratchets were deleted once the family
# could pass the floor.
#
# WHAT IS COUNTED: an errcheck `check-blank` finding in a *_test.go file whose
# source line assigns an error to `_` in a multi-value assignment — `v, _ :=
# f()` or `v, _ = f()`, in any arity. That is the shape that reads the value
# afterwards. NOT counted: `_ = f()` and a bare `defer x.Close()`, which discard
# an error but leave no nil to dereference, so they cannot produce the
# evidence-destroying failure above.
#
# Exit 0 = count matches FROZEN, exit 1 = it does not.

set -euo pipefail

cd "$(dirname "$0")/.."

# ── THE RATCHET CONSTANT ───────────────────────────────────────────────────
# Current number of silent error discards in test files, all five modules,
# with -tags docker. Measured 2026-09-06 against golangci-lint v2.11.4.
#
# It read 1540 when it was written and that number was measured on
# lineside/carrier alone, before the CMS branch's test files existed. On the
# integrated tree the same run counts 1557, so this step has been failing since
# the two branches met — the 17 extra sites were converted, and then the demand
# quota page's tests took another 36 with them when the table was dropped.
# The pickup-count rider converted the 12 GetProcessNodeRuntime/GetOrder
# discards its test files carried (8 pre-existing, 4 its own additions), taking
# the count 1503 → 1489.
#
# 1488 → 1484: the pair-as-one-job batch deleted the Face 1 and spare pins with
# the mechanisms they guarded (swap_mutual_hold_pin_test.go whole, plus four
# tests across bin_lifecycle, window4, swap_press_index_deadlock and
# swap_peer_test), and four discard sites went with them. Removed by DELETION,
# not by conversion — which is a legitimate way down and worth saying, because
# the number does not distinguish them and a reader chasing "what was converted"
# would find nothing.
#
# 1484 → 1480: withholding keep_staged took the order-completion walk out of
# changeover_flow_test.go's keep-staged evacuate case (a withheld node plans no
# orders, so there is nothing to complete), and its four discard sites went with
# it. Deletion again, not conversion.
#
# TO UPDATE: only downward, and only in the same commit that removed the
# sites. Run this script; it prints the real count in the failure message.
FROZEN=1480

if ! command -v golangci-lint >/dev/null 2>&1; then
  echo "FAIL test-error-discard ratchet — golangci-lint not on PATH"
  echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4"
  exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "FAIL test-error-discard ratchet — python3 not on PATH"
  exit 1
fi

ROOT="$(pwd)"
MODULES="protocol shared shingo-core shingo-edge integration"

# GOWORK=off IS AN UNDERCOUNT, NOT A NEUTRAL SETTING, and it does not announce
# itself: with the workspace off, shingo-edge and integration cannot resolve
# their sibling modules, golangci-lint reports one `typechecking error` line on
# stderr and then lints whatever still compiled — and STILL EXITS 1, the same
# code as a normal run. Measured on this tree: shingo-edge counts 742 sites in
# workspace mode and 166 with GOWORK=off. Unset it so go.work is discovered
# from the module directory, and treat that stderr line as fatal below.
unset GOWORK

# errcheck is NOT enabled in .golangci.yml and this does not enable it there —
# the config below is private to this script and lives in a temp file, so the
# repo lint gate is unchanged. `relative-path-mode: gomod` is what makes the
# reported paths module-relative rather than relative to wherever this temp
# file landed, which is what makes the output identical on Windows and Linux.
#
# The config file needs a .yml NAME, not just yaml CONTENT: golangci-lint hands
# it to viper, which picks its parser off the extension and refuses a bare
# mktemp file with `Unsupported Config Type "tmp.7yYzQ4QvzS"`. Hence the temp
# DIRECTORY.
TMPD="$(mktemp -d)"
trap 'rm -rf "$TMPD"' EXIT
CFG="$TMPD/errcheck.yml"
RAW="$TMPD/findings.txt"
ERRLOG="$TMPD/stderr.txt"
cat >"$CFG" <<'YML'
version: "2"
run:
  tests: true
  relative-path-mode: gomod
issues:
  max-same-issues: 0
  max-issues-per-linter: 0
output:
  formats:
    text:
      print-issued-lines: false
      print-linter-name: false
      colors: false
linters:
  default: none
  enable:
    - errcheck
  settings:
    errcheck:
      check-blank: true
      check-type-assertions: false
  exclusions:
    presets: []
YML

: >"$RAW"
for m in $MODULES; do
  # --allow-serial-runners for the same reason gate.sh's step_lint uses it:
  # golangci-lint takes a machine-global lock and a concurrent lane would
  # otherwise fail this run for a reason outside the diff.
  #
  # --build-tags docker or the *_docker_test.go files are not compiled and
  # every site in them is invisible, which would silently lower the count.
  rc=0
  ( cd "$ROOT/$m" && golangci-lint run --allow-serial-runners --build-tags docker \
      --config="$CFG" ./... 2>"$ERRLOG" ) >"$TMPD/out.txt" || rc=$?
  # Exit 1 is "issues found", which is the normal case here and not an error.
  # ANY OTHER non-zero is a run that produced no findings for a reason that has
  # nothing to do with the code — a config it could not read, a package that
  # did not typecheck — and silently counting zero for that module is how a
  # ratchet goes green while measuring nothing. Fail loudly instead.
  if [ "$rc" -ne 0 ] && [ "$rc" -ne 1 ]; then
    echo "FAIL test-error-discard ratchet — golangci-lint exited $rc in $m:"
    sed 's/^/       /' "$ERRLOG" | head -20
    exit 1
  fi
  # The other way to count zero for the wrong reason, and the one that keeps
  # exit code 1 while doing it.
  if grep -q "typechecking error" "$ERRLOG"; then
    echo "FAIL test-error-discard ratchet — $m did not typecheck, so its sites were not seen:"
    sed 's/^/       /' "$ERRLOG" | head -10
    exit 1
  fi
  sed "s|^|$m/|" "$TMPD/out.txt" >>"$RAW"
done

# The findings arrive as an ARGUMENT, not on stdin: stdin is already carrying
# the heredoc that is this program.
count="$(python3 - "$ROOT" "$RAW" <<'PY'
import os
import re
import sys

root = sys.argv[1]
# `<module>/<module-relative path>:<line>:<col>: <message>`
LOC = re.compile(r'^(.+\.go):(\d+):\d+: ')
# The blank sits in a multi-value assignment: `v, _ :=` / `v, _ =` / `a, _, _ =`.
SHAPE = re.compile(r',\s*_\s*:?=')

sites = set()
cache = {}
for raw in open(sys.argv[2], encoding='utf-8', errors='replace'):
    m = LOC.match(raw.strip())
    if not m:
        continue
    rel = m.group(1).replace('\\', '/')
    if not rel.endswith('_test.go'):
        continue
    line = int(m.group(2))
    path = os.path.join(root, rel)
    if path not in cache:
        try:
            with open(path, encoding='utf-8', errors='replace') as fh:
                cache[path] = fh.read().replace('\r\n', '\n').split('\n')
        except OSError:
            cache[path] = []
    src = cache[path]
    if line - 1 >= len(src):
        continue
    if SHAPE.search(src[line - 1]):
        # A file is linted once per package it belongs to; dedupe by site.
        sites.add((rel, line))
print(len(sites))
PY
)"

if [ "$count" -gt "$FROZEN" ]; then
  echo "FAIL test-error-discard ratchet: $count silent error discards in tests, frozen at $FROZEN."
  echo "     This diff added $((count - FROZEN)). The shape is \`v, _ := f()\` followed by a read of v:"
  echo "     when f fails the run reports a nil dereference instead of f's error, and the cause is gone."
  echo "     Use testutil.Must(t, v, err, \"f(...)\") or testutil.MustNoErr — protocol/testutil/must.go."
  echo "     Do NOT raise FROZEN. It only moves down."
  exit 1
fi
if [ "$count" -lt "$FROZEN" ]; then
  echo "FAIL test-error-discard ratchet: $count silent error discards in tests, but FROZEN says $FROZEN."
  echo "     This diff removed $((FROZEN - count)) and left the constant stale. Set FROZEN=$count in"
  echo "     scripts/check-test-error-discard-ratchet.sh, in this same commit — a ratchet parked above"
  echo "     reality passes while the number it reports means nothing."
  exit 1
fi
echo "ok   test-error-discard ratchet ($count, frozen)"
exit 0
