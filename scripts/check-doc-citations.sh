#!/usr/bin/env bash
# tier: dev
# check-doc-citations.sh — what a doc cites must still be there.
#
# The three readers that hold Go comments to their citations, pointed at the
# markdown docs (shared/scripts/internal/docindex: docs/**, the READMEs, the
# module docs; CHANGELOG.md and the population ledger are exempt): every test
# a doc names exists and every `go test -run` alternative matches one
# (testcite -docs); every code-shaped identifier resolves (deadident -docs);
# no file:line, and every cited path and relative link is tracked (repolint
# -docs). And every doc carries "Verified against: <sha>" under its title, the
# sha an ancestor of HEAD (docstamp -check, read-only: scripts/docs-verify.sh
# is what restamps). A doc that still names what a commit deleted fails the
# commit that deleted it, not the reader who goes looking months later.
# Gravestones ("TestX was deleted") are exempt as they are in comments.
#
# The four run side by side. `go run`, as the sibling guards do: its cached
# executables are the ones check-test-citations.sh, check-dead-identifiers.sh
# and check-repolint.sh build in the same gate step.
#
# Exit 0 = clean, exit 1 = a finding (or a reader that could not run).

set -uo pipefail

cd "$(dirname "$0")/../shared" || exit 1

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

readers="testcite deadident repolint docstamp"
pids=""
for r in $readers; do
  flag=-docs
  [ "$r" = docstamp ] && flag=-check
  ( go run "./scripts/$r" -root .. "$flag" >"$tmp/$r.out" 2>&1; echo $? >"$tmp/$r.rc" ) &
  pids="$pids $!"
done
# These jobs by pid: a bare wait would also wait for anything else this shell
# has started.
# shellcheck disable=SC2086
wait $pids

failed=""
for r in $readers; do
  if [ "$(cat "$tmp/$r.rc" 2>/dev/null)" != 0 ]; then
    grep -v '^exit status [0-9]*$' "$tmp/$r.out"
    failed="${failed:+$failed, }$r"
  fi
done
if [ -n "$failed" ]; then
  echo "FAIL doc citations ($failed)"
  exit 1
fi
echo "ok   doc citations (tests, -run patterns, identifiers, paths, links and stamps in every in-scope doc resolve)"
