#!/usr/bin/env bash
# check-comment-references.sh — what a comment CITES must still be there.
#
# WHY THIS IS THE HALF THAT LASTS. This codebase reasons in its comments, and it
# has been burned every time one of them went quietly false:
#
#   - a six-mode census in swap_leg_role.go written with a file:line per row.
#     Every one rotted — by +43 in one file, +88 in another, and by MINUS 58 and
#     71 in a third. A correction that called the drift "uniformly +43" was
#     itself wrong, because nobody could check it mechanically.
#   - capacity.go's header saying NGRP dropoffs "are intentionally not gated by
#     this helper", in a file whose own function routes them straight to
#     checkNGRPCapacity. Two design rounds read past it.
#   - a "mutual deadlock" claim on Face 2's narrowing that named no test for its
#     whole life.
#
# The mechanical half of that — does the thing a comment points at still EXIST —
# is checkable on the commit that breaks it, which is where a false comment is
# cheap instead of expensive.
#
# WHAT IT CHECKS, deliberately narrow so a failure always means something:
#
#   1. A file:line CITATION must name a file that exists, at a line inside it.
#      "See foo.go:900" against an 800-line file is unambiguously rotten.
#   2. A declared "PIN: TestFoo" marker must resolve to a real test. This is what
#      makes "a scar without a pin is a hypothesis" enforceable rather than
#      aspirational: delete the pin and the scar that leans on it fails the
#      build, instead of quietly becoming a claim with nothing behind it.
#
# WHAT IT DOES NOT CHECK, on purpose:
#
#   - whether a still-valid line number is the RIGHT line. Not catchable; cite a
#     SYMBOL for anything you expect to survive (AGENTS.md).
#   - bare mentions of a file or a test in prose. Tried first, not viable: the
#     tree names ~30 tests in comments and most of those are correct precisely
#     BECAUSE the test is gone ("pre-refactor this was TestChangeover_State...",
#     "TestBinManifestService_SyncUOP_PreservesManifest deleted in D8"). Failing
#     on those asks the codebase to stop recording its own history. Globs
#     (TestClearBin_FiresEmptyOut_*) and TYPES that begin with Test
#     (domain.TestCommand) make it worse. The same held for bare ".go" mentions:
#     twenty hits, nearly all prose about a filename-suffix rule or a vendored
#     library file.
#   - arbitrary identifiers. Too noisy to be read, and a check nobody reads is a
#     check nobody keeps.
#
# Exit 0 = clean, exit 1 = a comment points at something that is not there.

set -uo pipefail

cd "$(dirname "$0")/.."

MODULES="protocol shared shingo-core shingo-edge integration"

# Files a citation may name that are not in this tree. Each needs a reason — an
# entry without one is indistinguishable from silencing a failure.
declare -A ALLOWED_MISSING_FILE=(
  ["migrateloaders.go"]="the deleted command; comments cite it BECAUSE it is gone"
  ["sqlite_linux_amd64.go"]="modernc.org/sqlite, a dependency; the citation is into vendored source, not this tree"
)

FAIL=0

# --- 1. file:line citations -----------------------------------------------
cites=$(grep -RhoE --include='*.go' '^[[:space:]]*//.*' $MODULES 2>/dev/null |
  grep -oE '[a-zA-Z0-9_/-]+[.]go:[0-9]+' | sort -u)

bad_cites=""
while IFS= read -r cite; do
  [ -z "$cite" ] && continue
  f="${cite%%:*}"
  ln="${cite##*:}"
  base="${f##*/}"
  if [ -n "${ALLOWED_MISSING_FILE[$base]:-}" ]; then
    continue
  fi
  # RESOLVE ON A PATH-COMPONENT BOUNDARY, three ways, in this order. All three
  # shapes occur in the tree and the first cut of this check got every one of
  # them wrong:
  #
  #   exact       "protocol/payloads.go" is a real path from the repo root, and
  #               a "*/..." glob cannot match it (nothing precedes it).
  #   suffix      "edge/engine/changeover.go" is how this codebase abbreviates
  #               shingo-edge; it resolves only as a trailing path fragment.
  #   basename    "capacity.go" on its own. -name, NOT -path "*capacity.go":
  #               the glob form matched a SUFFIX, so a citation to reshuffle.go
  #               resolved to complex_reshuffle.go — a different, shorter file —
  #               and a perfectly good citation was reported as past the end.
  found=""
  [ -f "$f" ] && found="$f"
  if [ -z "$found" ] && [[ "$f" == */* ]]; then
    found=$(find $MODULES -path "*/$f" -print -quit 2>/dev/null)
  fi
  if [ -z "$found" ]; then
    found=$(find $MODULES -name "$base" -print -quit 2>/dev/null)
  fi
  if [ -z "$found" ]; then
    bad_cites+="$cite|no such file"$'\n'
    continue
  fi
  total=$(wc -l < "$found" | tr -d ' ')
  if [ "$ln" -gt "$total" ]; then
    bad_cites+="$cite|$found has only $total lines"$'\n'
  fi
done <<< "$cites"

if [ -n "${bad_cites//[$'\n' ]/}" ]; then
  echo "Comments cite file:line positions that cannot be right:"
  while IFS='|' read -r cite why; do
    [ -z "$cite" ] && continue
    echo "  $cite -- $why"
  done <<< "$bad_cites"
  echo "  Cite a SYMBOL for anything you expect to survive; line numbers drift silently."
  FAIL=1
fi

# --- 2. PIN: markers ------------------------------------------------------
# The name must LOOK like a test -- Test/Fuzz/Benchmark -- which disambiguates
# the marker from ordinary prose ("THE PIN: the slot is held as a RESERVATION...",
# a sentence rather than a citation) and costs nothing, since a Go test cannot be
# named anything else.
all_tests=$(grep -RhoE --include='*_test.go' '^func (Test|Fuzz|Benchmark)[A-Za-z0-9_]*' $MODULES 2>/dev/null |
  sed 's/^func //' | sort -u)
pinned=$(grep -RhoE --include='*.go' 'PIN:[[:space:]]*(Test|Fuzz|Benchmark)[A-Za-z0-9_]*' $MODULES 2>/dev/null |
  sed -E 's/PIN:[[:space:]]*//' | sort -u)

missing_pins=""
while IFS= read -r t; do
  [ -z "$t" ] && continue
  grep -qx "$t" <<< "$all_tests" || missing_pins+="$t"$'\n'
done <<< "$pinned"

if [ -n "${missing_pins//[$'\n' ]/}" ]; then
  echo "Comments declare PIN: markers naming tests that do not exist:"
  while IFS= read -r t; do
    [ -z "$t" ] && continue
    echo "  $t"
    grep -RnE --include='*.go' "PIN:[[:space:]]*$t" $MODULES 2>/dev/null | head -2 | sed 's/^/      /'
  done <<< "$missing_pins"
  echo "  A scar without a pin is a hypothesis. Either the test was renamed and the marker"
  echo "  should follow it, or the pin is gone and the claim is now unguarded -- say so."
  FAIL=1
fi

if [ "$FAIL" -eq 0 ]; then
  echo "ok   comment references (file:line citations and PIN: markers resolve)"
fi
exit $FAIL
