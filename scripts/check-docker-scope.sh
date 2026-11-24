#!/usr/bin/env bash
# tier: dev
# inputs: scripts/gate.sh scripts/check-docker-scope.sh
# side: linux
# check-docker-scope.sh — pin `gate.sh scope`'s rule: which changed files can
# reach a Postgres-backed test.
#
# The rows are fed to `gate.sh scope -` in one call, which names every file
# that reaches; each row's verdict is whether its file is named. The NEEDED
# rows are files docker-tagged tests read today (bins_pins_test.go reads
# static/pages/bins-echo.js through filepath.Join; the kanban pins read
# kanban/testdata/same_family.json; the cells golden; the planner golden's
# plants) and files the rule must not know about (a new extension, a script).
# The NOT NEEDED rows are the allow-list's entries. A row that flips is either
# a scope change to argue in gate.sh's table or a hole.
#
# Exit 0 = every row classified as pinned, 1 = a row flipped.

set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

ROWS="
NEEDED|shingo-core/www/static/pages/bins-echo.js
NEEDED|shingo-core/kanban/testdata/same_family.json
NEEDED|shingo-core/www/testdata/cells_api_golden.json
NEEDED|shingo-core/www/templates/layout.html
NEEDED|shingo-core/www/static/style.css
NEEDED|plants/demo.yaml
NEEDED|shared/components.css
NEEDED|shared/fedshape/vectors.json
NEEDED|shingo-core/engine/engine.go
NEEDED|shingo-edge/engine/engine.go
NEEDED|shingo-core/store/schema.sql
NEEDED|shingo-core/engine/engine_test.go
NEEDED|integration/scenarios/x_test.go
NEEDED|go.work
NEEDED|docs/tool/main.go
NEEDED|shingo-core/www/static/README.md
NEEDED|scripts/gate.sh
NEEDED|shingo-edge/engine/testdata/fixture.json
NEEDED|some/new/kind.xyz
NOT NEEDED|AGENTS.md
NOT NEEDED|shingo-core/LOCKS.md
NOT NEEDED|docs/testing/population-ledger.md
NOT NEEDED|shingo-edge/www/static/style.css
NOT NEEDED|shingo-edge/www/static/pages/operator.js
NOT NEEDED|shingo-edge/www/templates/layout.html
NOT NEEDED|.github/workflows/edge.yml
NOT NEEDED|deploy/shingo-core.service
NOT NEEDED|install-edge.sh
NOT NEEDED|Makefile
NOT NEEDED|shingo-edge/engine/engine_test.go
NOT NEEDED|shared/shared_test.go
"

rows="$(printf '%s\n' "$ROWS" | sed '/^$/d')"
out="$(printf '%s\n' "$rows" | cut -d'|' -f2 | bash "$ROOT/scripts/gate.sh" scope - 2>&1)"
case $? in 0|1) ;; *) echo "FAIL docker scope: gate.sh scope - errored:"; printf '%s\n' "$out" | sed 's/^/  /'; exit 1 ;; esac
reached="$(printf '%s\n' "$out" | awk '/^scope: docker NEEDED/ { on = 1; next } on && /^    / { sub(/^    /, ""); print }')"

n="$(printf '%s\n' "$rows" | wc -l | tr -d ' ')"
# One pass, not a grep per row: a subprocess per row costs seconds on Windows.
bad="$(awk -F'|' 'NR == FNR { r[$0] = 1; next }
  { got = ($2 in r) ? "NEEDED" : "NOT NEEDED"
    if (got != $1) print "FAIL docker scope: " $2 " — want " $1 ", got " got }' \
  <(printf '%s\n' "$reached") <(printf '%s\n' "$rows"))"
if [ -n "$bad" ]; then
  printf '%s\n' "$bad"
  exit 1
fi

# THE TREE, NOT THE COMMITS (G11). `scope BASE` must read what the dev tier's
# diff reads (write_diff): commits since the merge base, staged and unstaged
# edits, and untracked files. It used to read `git diff BASE...HEAD`, so a tree
# level with its base and an uncommitted Go edit printed "docker NOT needed".
# Each row runs the real gate.sh against a throwaway repo through GIT_DIR and
# GIT_WORK_TREE: a committed base, then one uncommitted change, scoped against
# HEAD. Every row must say NEEDED; the last says NEEDED because there is no
# merge base to diff against (an unknown base fails safe).
lvl="$(mktemp -d "${TMPDIR:-${TEMP:-/tmp}}/docker-scope.XXXXXX")" || { echo "FAIL docker scope: mktemp"; exit 1; }
trap 'rm -rf "$lvl"' EXIT
lvl_git() { git -C "$lvl" -c user.name=gate -c user.email=gate@example.invalid "$@" >/dev/null 2>&1; }
lvl_scope() { GIT_DIR="$lvl/.git" GIT_WORK_TREE="$lvl" bash "$ROOT/scripts/gate.sh" scope "$1" >/dev/null 2>&1; }
lvl_reset() {
  rm -rf "$lvl/.git" "$lvl/m"
  mkdir -p "$lvl/m"
  printf 'package m\n' >"$lvl/m/a.go"
  lvl_git init -q && lvl_git add -A && lvl_git commit -qm base
}
tree_bad=""
lvl_reset; printf '// edit\n' >>"$lvl/m/a.go"
lvl_scope HEAD || tree_bad="$tree_bad unstaged-go-edit"
lvl_reset; printf '// edit\n' >>"$lvl/m/a.go"; lvl_git add m/a.go
lvl_scope HEAD || tree_bad="$tree_bad staged-go-edit"
lvl_reset; printf 'package m\n' >"$lvl/m/b.go"
lvl_scope HEAD || tree_bad="$tree_bad untracked-go-file"
lvl_reset; rm "$lvl/m/a.go"
lvl_scope HEAD || tree_bad="$tree_bad deleted-go-file"
lvl_reset
lvl_scope no-such-base || tree_bad="$tree_bad unknown-base"
if [ -n "$tree_bad" ]; then
  for r in $tree_bad; do echo "FAIL docker scope: level tree, $r — want NEEDED, got NOT NEEDED"; done
  exit 1
fi
echo "ok   docker scope ($n rows, 5 level-tree rows)"
