#!/usr/bin/env bash
# Fail when a tracked text file carries a merge-conflict marker line.
#
# WHY THIS EXISTS: a stray diff3 marker line rode 71 commits of one rebase in
# CHANGELOG.md before anyone saw it; Go source would have failed to build, but
# Markdown, YAML, SQL and scripts carry one silently.
#
# A marker is a line that starts with seven <, >, or | followed by a space or
# the end of the line, or a line that is exactly seven =. Binary files are
# skipped (git grep -I).
set -u
cd "$(dirname "$0")/.."

hits="$(git grep -I -n -E '^(<<<<<<<|>>>>>>>|\|\|\|\|\|\|\|)( |$)|^=======$' -- . 2>/dev/null)"
if [ -n "$hits" ]; then
  echo "FAIL: merge-conflict marker lines in tracked files:"
  printf '%s\n' "$hits" | head -20
  exit 1
fi
echo "ok   check-conflict-markers ($(git ls-files | wc -l | tr -d ' ') tracked files, no marker line)"
