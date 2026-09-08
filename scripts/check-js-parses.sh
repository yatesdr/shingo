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

FAIL=0
CHECKED=0

# Repo-wide and self-maintaining: a new static directory is covered the day it
# is added, with nobody having to remember this file. Test bundles are included
# on purpose — they are JavaScript too, and they cost nothing to parse.
while IFS= read -r f; do
  CHECKED=$((CHECKED + 1))

  cp "$f" "$TMP/candidate.mjs"
  if node --check "$TMP/candidate.mjs" >/dev/null 2>&1; then
    continue
  fi

  # Not valid as a module — it may be an honest classic script.
  cp "$f" "$TMP/candidate.cjs"
  if node --check "$TMP/candidate.cjs" >/dev/null 2>&1; then
    continue
  fi

  # Neither grammar accepts it. Re-run without suppression so the operator
  # gets node's own message, which names the line and the offending token.
  echo "FAIL js-parse — $f does not parse:"
  node --check "$TMP/candidate.mjs" 2>&1 | sed 's/^/    /' || true
  echo
  FAIL=1
done < <(find . -name '*.js' -not -path './.git/*' -not -path '*/node_modules/*' -print)

if [ "$FAIL" -eq 0 ]; then
  echo "ok   js-parse ($CHECKED files)"
else
  echo "A module that does not parse takes its whole import graph down with it."
fi

exit "$FAIL"
