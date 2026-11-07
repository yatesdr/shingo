#!/usr/bin/env bash
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
echo "ok   docker scope ($n rows)"
