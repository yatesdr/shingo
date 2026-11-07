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
#   2. (moved) A declared "PIN: Test..." marker must resolve to a real test.
#      That is what makes "a scar without a pin is a hypothesis" enforceable.
#      check-test-citations.sh owns it now, with subtest paths, beside every
#      other test a comment names; it was checked here first.
#
# WHAT IT DOES NOT CHECK, on purpose:
#
#   - whether a still-valid line number is the RIGHT line. Not catchable; cite a
#     SYMBOL for anything you expect to survive (AGENTS.md).
#   - bare mentions of a test in prose: check-test-citations.sh reads them. A
#     blanket check was tried here first and was not viable, because many such
#     mentions are right precisely BECAUSE the test is gone ("pre-refactor this
#     was TestChangeover_State...", "...PreservesManifest deleted in D8");
#     failing on those asks the codebase to stop recording its own history.
#     That check exempts them by the words touching the name, and reads globs
#     and types spelled Test... as what they are.
#   - bare mentions of a file in prose: twenty hits, nearly all prose about a
#     filename-suffix rule or a vendored library file.
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

# RESOLVE ON A PATH-COMPONENT BOUNDARY, three ways, in this order. All three
# shapes occur in the tree and the first cut of this check got every one of
# them wrong:
#
#   exact       "protocol/payloads.go" is a real path from the repo root, and
#               a "*/..." glob cannot match it (nothing precedes it).
#   suffix      "edge/engine/changeover.go" is how this codebase abbreviates
#               shingo-edge; it resolves only as a trailing path fragment.
#   basename    "capacity.go" on its own. The whole last component, NOT a
#               suffix of it: a suffix match resolved a citation to
#               reshuffle.go to complex_reshuffle.go — a different, shorter
#               file — and a perfectly good citation was reported as past the
#               end.
#
# EVERY MATCH, NOT THE FIRST. A suffix or a basename can name several files,
# and "the first" is whichever a directory walk reaches first: alphabetical on
# NTFS, hash order on ext4. A citation to compound.go passed on Windows
# (dispatch's) and failed in CI (internal/testdb's, a tenth the length). The
# line must fit in at least one of the files the citation could name, the same
# answer on every OS.
#
# ONE WALK OF THE TREE, NOT ONE PER CITATION. Every .go file and its line count
# is listed once, and every citation is resolved against that list in one awk
# pass. It used to run find two or three times per citation and wc once per
# candidate: over a hundred citations, that was 50s of the gate on Windows,
# where a fork is expensive. The exact arm still covers any path from the repo
# root; suffix and basename still search only the modules.
index=$(find . -name '*.go' -not -path './.git/*' -not -path '*/node_modules/*' -exec wc -l {} + 2>/dev/null |
  awk '$2 != "total" { sub(/^\.\//, "", $2); print $1, $2 }')

allowed="${!ALLOWED_MISSING_FILE[*]}"
bad_cites=$(awk -v mods="$MODULES" -v allowed="$allowed" '
  BEGIN {
    nm = split(mods, m, " "); for (i = 1; i <= nm; i++) inmod[m[i]] = 1
    na = split(allowed, a, " "); for (i = 1; i <= na; i++) skip[a[i]] = 1
  }
  NR == FNR {
    lines[$2] = $1; path[++np] = $2
    top = $2; sub(/\/.*/, "", top); modfile[$2] = (top in inmod)
    b = $2; sub(/.*\//, "", b); base[$2] = b
    next
  }
  $0 == "" { next }
  {
    cite = $0; f = cite; sub(/:[0-9]+$/, "", f); ln = cite; sub(/.*:/, "", ln)
    bn = f; sub(/.*\//, "", bn)
    if (bn in skip) next
    n = 0; delete cand
    if (f in lines) cand[++n] = f
    if (n == 0 && index(f, "/") > 0)
      for (i = 1; i <= np; i++) { p = path[i]
        if (modfile[p] && length(p) > length(f) && substr(p, length(p) - length(f)) == "/" f) cand[++n] = p }
    if (n == 0)
      for (i = 1; i <= np; i++) { p = path[i]
        if (modfile[p] && base[p] == bn) cand[++n] = p }
    if (n == 0) { print cite "|no such file"; next }
    longest = ""; total = 0
    for (i = 1; i <= n; i++) if (lines[cand[i]] + 0 > total) { total = lines[cand[i]] + 0; longest = cand[i] }
    if (ln + 0 > total) print cite "|" longest " has only " total " lines"
  }' <(printf '%s\n' "$index") <(printf '%s\n' "$cites"))

if [ -n "${bad_cites//[$'\n' ]/}" ]; then
  echo "Comments cite file:line positions that cannot be right:"
  while IFS='|' read -r cite why; do
    [ -z "$cite" ] && continue
    echo "  $cite -- $why"
  done <<< "$bad_cites"
  echo "  Cite a SYMBOL for anything you expect to survive; line numbers drift silently."
  FAIL=1
fi

if [ "$FAIL" -eq 0 ]; then
  echo "ok   comment references (file:line citations resolve)"
fi
exit $FAIL
