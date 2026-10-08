#!/usr/bin/env bash
# check-js-parses.sh — every .js file in the repo must actually parse.
#
# WHY THIS EXISTS: Go tests never parse JavaScript. The www drift tests
# (inline_onclick, order_status, template_field, ...) all scan for PATTERNS;
# none of them is a parser. So a hard syntax error in a shipped bundle passes
# every gate this repo has and reaches a plant.
#
# It did. Hopkinsville 2026-09-08: a stray `}` left behind by a deletion in
# operator-release.js took every operator HMI dark. The operator station is an
# ES MODULE GRAPH, so the blast radius of one unparseable file is not that
# file — the graph never links, operator.js never runs a single line, and
# every board sits on the "Loading..." placeholder forever. The server stays
# perfectly healthy the whole time, which is what makes it expensive to
# diagnose: services active, assets 200, API 200, no panics.
#
# The check is the cheapest possible: hand each file to node and ask whether
# it parses. It found exactly one bad file out of 69 shipped bundles.
#
# ESM FIRST, THEN COMMONJS. `node --check` picks its goal symbol from the file
# extension, and the two are not the same grammar: ESM is implicitly strict,
# so a legitimately-classic script (octal literal, `with`, duplicate params)
# would be a false positive if we only tried .mjs. Trying .cjs after gives
# classic scripts an honest pass. Nothing is given up — the failure this
# guards against is an unbalanced brace, and that fails BOTH grammars.
#
# A MISSING node IS A HARD FAILURE, NOT A SKIP. "Skip when the tool is
# absent" is precisely how a check reports green on code it never looked at,
# which is the class of bug this file exists to end. The message says node is
# missing rather than naming a file, so it can't be mistaken for a real
# finding. CI runners (ubuntu-latest) ship node preinstalled.
#
# Exit 0 = clean, exit 1 = a file failed to parse (or node is unavailable).

set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v node >/dev/null 2>&1; then
  echo "FAIL js-parse — node is not installed, so the JS was NOT verified."
  echo "  This is deliberately a failure and not a skip: a syntax error in a"
  echo "  shipped bundle blanks every operator HMI, and a gate that quietly"
  echo "  passes on unread code is how that reached a plant."
  echo "  fix: install Node (any version with --check), then re-run."
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# check_one <file> — ESM first, then CommonJS (see the header). Each file gets
# its own scratch directory, because node picks the grammar from the extension
# and the copies of parallel checks must not land on one another. A file that
# parses under neither grammar leaves node's own message, which names the line
# and the token, under $TMP/fail.
check_one() {
  local f="$1" d
  d="$(mktemp -d "$TMP/c.XXXXXX")"
  cp "$f" "$d/candidate.mjs"
  node --check "$d/candidate.mjs" >/dev/null 2>&1 && return 0
  cp "$f" "$d/candidate.cjs"
  node --check "$d/candidate.cjs" >/dev/null 2>&1 && return 0
  { echo "FAIL js-parse — $f does not parse:"
    node --check "$d/candidate.mjs" 2>&1 | sed 's/^/    /' || true
    echo
  } > "$d/fail"
  return 0
}
export -f check_one
export TMP

# Repo-wide and self-maintaining: a new static directory is covered the day it
# is added, with nobody having to remember this file. Test bundles are included
# on purpose — they are JavaScript too, and they cost nothing to parse.
#
# IN PARALLEL, ONE NODE PER CORE. A node start is most of each check's cost, and
# the files are independent, so the checks run side by side; one after another
# they were 24s of the gate on Windows. Failures are collected afterwards and
# printed in file order, so the output reads the same however the checks
# interleaved.
JOBS="$(nproc 2>/dev/null || echo "${NUMBER_OF_PROCESSORS:-4}")"
case "$JOBS" in *[!0-9]*|"") JOBS=4 ;; esac
find . -name '*.js' -not -path './.git/*' -not -path '*/node_modules/*' -print0 | sort -z > "$TMP/files"
CHECKED="$(grep -zc '' "$TMP/files")"
xargs -0 -P "$JOBS" -n 1 bash -c 'check_one "$1"' _ < "$TMP/files"

FAIL=0
while IFS= read -r -d '' f; do
  for d in "$TMP"/c.*; do
    [ -f "$d/fail" ] || continue
    if grep -qF -- "FAIL js-parse — $f does not parse:" "$d/fail"; then
      cat "$d/fail"
      FAIL=1
    fi
  done
done < "$TMP/files"

if [ "$FAIL" -eq 0 ]; then
  echo "ok   js-parse ($CHECKED files)"
else
  echo "A module that does not parse takes its whole import graph down with it."
fi

exit "$FAIL"
