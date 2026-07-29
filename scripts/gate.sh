#!/usr/bin/env bash
#
# The pre-push gate: what CI enforces, minus the docker suites.
#
# A script rather than a checklist, because a gate re-derived by hand from
# `git diff --name-only` is a gate a step can quietly go missing from.
#
# A script rather than a make target, because `make` is installed neither on
# the Windows dev host nor in its WSL distro. The root Makefile's `gate` target
# delegates here for anyone who does have make.
#
# ALL FIVE MODULES. `shared` and `integration` are the two people forget:
# shared is small and embedded by the other two, integration compiles nothing
# without `-tags docker`. Both entered lint.yml's matrix in 91ce6355, and
# shared had been in no workflow at all before that — its seven drift-guard
# tests ran nowhere.
#
# NO -race HERE, DELIBERATELY. Race detection costs 2-4x and CI runs it on
# every push, so locally you would be paying that multiple for a signal
# arriving ninety seconds later anyway. The local gate's job is "this
# compiles and behaves"; CI's job is "and it is race-free". If you are
# actually touching lock acquisition, goroutine spawns, or map/channel/atomic
# access, run it TARGETED -- `go test -race -run TestRace_ ./thatpkg/...` --
# rather than module-wide. Stated explicitly because the cautious instinct is
# to add -race back, and module-wide race runs cost 25-35 minutes each.
#
# DOCKER SUITES ARE SCOPED, NOT SKIPPED. See `scope` below.
#
# Usage:
#   bash scripts/gate.sh                  fmt, vet, lint, unit tests
#   bash scripts/gate.sh scope [BASE]     say whether the docker suites are needed
#                                         (exit 0 = needed, 1 = not needed)
#   bash scripts/gate.sh docker           run the docker suites (no -race)
#   bash scripts/gate.sh full [BASE]      the four, then docker only if scope says so
#   bash scripts/gate.sh fmt|vet|lint|test  one step

set -uo pipefail
cd "$(dirname "$0")/.."
ROOT="$(pwd)"

# RUN THIS FROM WINDOWS (Git Bash / PowerShell), NOT FROM WSL.
#
# WSL reaches a Windows path through a 9p/virtio-fs bridge, and Go's build
# and test-compile phases are metadata-heavy — thousands of stat calls on
# files that have not changed. Crossing that bridge costs 10-20x, MEASURED on
# this host with a warm cache, which is the normal case:
#
#                        WSL -> /mnt/c   Windows -> C:   WSL -> ext4
#   go build ./...          48.6s            2.4s           1.2s
#   go vet ./...            24.1s            2.2s           0.9s
#   test compile            28.2s            7.6s           3.3s
#
# Windows-native is within ~2x of Linux-native, so the repo does NOT want
# moving to ext4 — the penalty is not where the files live, it is the
# crossing. The only reason to be in WSL at all is `-race` (Windows has no
# gcc), and the local gate deliberately does not run -race; see below.
#
# Docker Desktop serves Windows directly, so the docker suites do not need
# WSL either.
if [ -n "${WSL_DISTRO_NAME:-}" ] && case "$ROOT" in /mnt/*) true ;; *) false ;; esac; then
  echo "WARNING: running under WSL against a Windows path ($ROOT)." >&2
  echo "         This is 10-20x slower than running the same gate from Git Bash." >&2
  echo "         Nothing here needs WSL — Docker Desktop serves Windows directly." >&2
  echo >&2
fi

MODULES="protocol shared shingo-core shingo-edge integration"
rc=0

# Which modules actually carry docker-tagged tests. COMPUTED, not listed,
# and computed in ONE place because two copies of this answer is exactly the
# rot it exists to prevent: `scope` used to derive it while `docker` carried
# a hardcoded triple, so the first `go:build docker` test to land in
# `protocol` or `shared` would have had scope say NEEDED and docker silently
# not run it. Today the two lists agree, which is why nobody noticed.
docker_modules() {
  local m
  for m in $MODULES; do
    if grep -rlq 'go:build docker' --include='*_test.go' "$ROOT/$m" 2>/dev/null; then
      printf '%s ' "$m"
    fi
  done
}

step_fmt() {
  # FIRST, and on its own, for two reasons. It is the cheapest check here — a
  # second against golangci-lint's minutes — so a formatting slip should not
  # cost a full lint run to find. And it is the only one that still answers
  # when the tree does not compile: golangci-lint surfaces gofmt findings
  # through the same pipeline as its typecheck findings, so a build error can
  # hide a formatting error until the build error is gone. `gofmt -l` is
  # lexical and has no such failure mode.
  local bad
  # gofmt emits OS-native separators, so on Windows the paths come back as
  # shared\zlayer.go — and a backslash is an escape character in the very
  # shell this fix line gets pasted into, which would silently mangle it to
  # `sharedzlayer.go`. Normalise before printing: a failure message names what
  # to change, and a fix command that does not run names nothing.
  bad="$(gofmt -l $MODULES | tr '\\' '/')"
  if [ -n "$bad" ]; then
    echo "FAIL gofmt — not formatted:"
    echo "$bad" | sed 's/^/    /'
    echo "  fix: gofmt -w $(echo "$bad" | tr '\n' ' ')"
    return 1
  fi
  echo "ok   gofmt"
}

step_vet() {
  local m failed=0
  for m in $MODULES; do
    ( cd "$ROOT/$m" && go vet ./... ) || failed=1
  done
  [ "$failed" -eq 0 ] && echo "ok   vet" || echo "FAIL vet"
  return $failed
}

step_lint() {
  # --build-tags docker matches CI. Without it the linter compiles only the
  # default build tag and every //go:build docker test file is invisible, so
  # those files get linted for the first time in CI.
  if ! command -v golangci-lint >/dev/null 2>&1; then
    echo "FAIL lint — golangci-lint not on PATH"
    echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4"
    return 1
  fi
  # --allow-serial-runners IS NOT OPTIONAL WHEN LANES RUN CONCURRENTLY.
  #
  # golangci-lint takes a MACHINE-GLOBAL file lock at startup, and its own
  # --help says what happens without this flag: "golangci-lint exits with an
  # error if it fails to acquire file lock on start". So a second lane's gate
  # does not queue behind yours — it FAILS yours, or fails itself, with
  #
  #     Error: parallel golangci-lint is running
  #
  # and exit 3. That is indistinguishable in `gate.sh` output from a real lint
  # finding: `step_lint` sees a non-zero exit and prints "FAIL lint".
  #
  # MEASURED on this host, golangci-lint v2.11.4, three concurrent runs of the
  # same module, same instant:
  #
  #   without the flag:  exit 0, exit 0, exit 3 ("parallel golangci-lint is running")
  #   with the flag:     exit 0, exit 0, exit 0  ("0 issues." x3)
  #
  # IT IS INTERMITTENT, which is why it reads as flakiness rather than as this.
  # The lock is held over part of a run, not all of it, so two lanes offset by a
  # few seconds often both pass — and the same two lanes started together do
  # not. Four round-3 agents hit it independently.
  #
  # SERIAL AND NOT --allow-parallel-runners, deliberately. Parallel drops the
  # lock entirely and lets N instances write the shared content-addressed cache
  # at once; serial keeps the lock and WAITS for it. The cost is wall time on
  # whichever lane arrives second — bounded, because the lock is per
  # invocation and this loop makes five short ones per module set rather than
  # one long one. The thing being bought is that the gate's verdict is about
  # the code. A gate that can fail for a reason outside the diff is not a gate.
  local m failed=0
  for m in $MODULES; do
    ( cd "$ROOT/$m" && golangci-lint run --allow-serial-runners --build-tags docker \
        --config="$ROOT/.golangci.yml" --issues-exit-code=1 ./... ) || failed=1
  done
  [ "$failed" -eq 0 ] && echo "ok   lint" || echo "FAIL lint"
  return $failed
}

step_test() {
  local m failed=0
  for m in $MODULES; do
    ( cd "$ROOT/$m" && go test -count=1 ./... >/dev/null ) || { echo "  $m:"; ( cd "$ROOT/$m" && go test -count=1 ./... 2>&1 | grep -Ev '^ok |no test files' | head -30 ); failed=1; }
  done
  [ "$failed" -eq 0 ] && echo "ok   tests" || echo "FAIL tests"
  return $failed
}

# ── Docker scope ─────────────────────────────────────────────────────
#
# The question is NOT "does a path filter match" -- that answers which
# workflow triggers, which is a different question and a narrower one. It is
# "can this diff reach a Postgres-backed test at all".
#
# The rule below is derived from the tree, not assumed:
#
#   *.go / *.sql / go.mod / go.sum   NEEDED. Any Go change in any module can
#                                    be compiled into one of the three that
#                                    carry docker tests -- shared/ has zero
#                                    docker tests of its own but shingo-core
#                                    compiles it.
#   templates/**.html                NEEDED, and this one is the reason to
#                                    check rather than guess. It looks like an
#                                    asset. It is not:
#                                    shingo-core/www/handlers_payloads_test.go
#                                    is //go:build docker and does
#                                    `base.ParseFS(templateFS,
#                                    "templates/layout.html", ...)` plus a glob
#                                    over templates/*.html, so a malformed
#                                    template fails a Postgres-backed test.
#   everything else                  NOT NEEDED. Verified: no docker-tagged
#                                    test anywhere reads a .css, .js or .svg,
#                                    and none touches shared.Files. Re-check
#                                    with:
#                                      grep -rl 'go:build docker' --include='*_test.go' . \
#                                        | xargs grep -lE 'shared\.Files|static/|\.css|\.svg'
#
# Conservative on purpose: it says NEEDED for anything it is not sure about.
# The win is not that it skips often, it is that a docs-and-CSS round stops
# costing 35 minutes.
step_scope() {
  local base="${1:-}"
  if [ -z "$base" ]; then
    base="$(git rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' 2>/dev/null)" || base=""
  fi
  [ -z "$base" ] && base="origin/main"

  local files
  if [ "$base" = "-" ]; then
    # Classify an explicit file list on stdin instead of a git range, so the
    # rule can be exercised directly:
    #   printf 'a.css\nb/templates/c.html\n' | bash scripts/gate.sh scope -
    files="$(cat)"
  else
    files="$(git diff --name-only "$base"...HEAD 2>/dev/null)"
  fi
  if [ -z "$files" ]; then
    echo "scope: no diff against $base — docker NOT needed"
    return 1
  fi

  local dockermods m
  dockermods="$(docker_modules)"

  local reaching="" f
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    case "$f" in
      *.sql|go.mod|go.sum|*/go.mod|*/go.sum) reaching="$reaching$f"$'\n' ; continue ;;
    esac
    case "$f" in
      */templates/*.html) reaching="$reaching$f"$'\n' ; continue ;;
    esac
    case "$f" in
      *_test.go)
        # A _test.go file is compiled ONLY into its own package's test
        # binary -- it is not part of any importable surface -- so it can
        # reach a Postgres-backed test only if its own module has one.
        # This is the case that turns a palette round from 35 minutes into
        # 90 seconds: shared/ has seven test files and zero docker tags.
        for m in $dockermods; do
          case "$f" in "$m"/*) reaching="$reaching$f"$'\n' ;; esac
        done
        continue ;;
      *.go) reaching="$reaching$f"$'\n' ; continue ;;
    esac
  done <<< "$files"
  reaching="$(printf '%s' "$reaching" | sed '/^$/d')"
  if [ -n "$reaching" ]; then
    echo "scope: docker NEEDED — $(echo "$reaching" | wc -l | tr -d ' ') of $(echo "$files" | wc -l | tr -d ' ') changed files can reach a Postgres-backed test:"
    echo "$reaching" | head -8 | sed 's/^/    /'
    [ "$(echo "$reaching" | wc -l)" -gt 8 ] && echo "    ..."
    return 0
  fi
  echo "scope: docker NOT needed — none of the $(echo "$files" | wc -l | tr -d ' ') changed files can reach a Postgres-backed test:"
  echo "$files" | head -8 | sed 's/^/    /'
  [ "$(echo "$files" | wc -l)" -gt 8 ] && echo "    ..."
  return 1
}

# No -race: see the header. CI runs the race variant.
#
# The module list is docker_modules(), the SAME derivation `scope` uses. A
# module that starts carrying docker-tagged tests is picked up by both at
# once, or by neither — never by scope alone, which would report NEEDED and
# then run nothing.
#
# THE LOG GOES UNDER $ROOT, NOT UNDER /tmp, AND THAT IS A CORRECTNESS FIX.
#
# It used to be /tmp/gate-docker-$m.log. On Git Bash /tmp IS %TEMP% — one
# directory for the whole machine — and $m is a module name, which every
# worktree of this repo shares. So every concurrent lane wrote the SAME FILE:
#
#   C:/Users/<user>/AppData/Local/Temp/gate-docker-shingo-core.log
#
# `>` truncates on open, so the second lane to start emptied the first lane's
# log mid-write. A round-3 agent watched it go 5,455 -> 1,315 bytes and then
# printed another lane's stack traces as its own failure.
#
# THE EXIT CODES WERE ALWAYS RIGHT — `go test`'s status is not routed through
# this file, so no lane ever passed a gate it should have failed. What was
# wrong was the EXCERPT: the thirty lines a human reads to find out what broke
# could belong to somebody else's tree. A gate that reports the wrong reason is
# worse than one that reports none, because the reason is what gets acted on.
#
# $ROOT/.gate/ and not $$: a worktree is the unit that collides here, and the
# per-worktree path also keeps the log at a STABLE, findable place after the
# run — which is the whole reason to write a file instead of streaming. Two
# gates inside one worktree at the same moment would still share it, and are
# not worth designing for: they would already be fighting over the same
# testcontainers and the same Docker daemon, and the log is the least of it.
step_docker() {
  local m failed=0 mods logdir
  mods="$(docker_modules)"
  if [ -z "$mods" ]; then
    echo "ok   docker (no module carries a go:build docker test)"
    return 0
  fi
  logdir="$ROOT/.gate"
  mkdir -p "$logdir" || { echo "FAIL docker — cannot create $logdir"; return 1; }
  for m in $mods; do
    echo "  docker: $m"
    ( cd "$ROOT/$m" && go test -tags=docker -timeout=20m -count=1 -p 1 ./... >"$logdir/docker-$m.log" 2>&1 ) \
      || { failed=1; echo "  --- $m ($logdir/docker-$m.log) ---"; grep -Ev '^ok |no test files' "$logdir/docker-$m.log" | head -30; }
  done
  [ "$failed" -eq 0 ] && echo "ok   docker" || echo "FAIL docker"
  return $failed
}

case "${1:-all}" in
  fmt)   step_fmt  || rc=1 ;;
  vet)   step_vet  || rc=1 ;;
  lint)  step_lint || rc=1 ;;
  test)  step_test || rc=1 ;;
  # EXIT CODE IS THE VERDICT: 0 = docker needed, 1 = not needed. It used to
  # `exit 0` unconditionally, which made the answer readable only by a human
  # reading the text — so nothing could gate on it. `if bash scripts/gate.sh
  # scope; then ...` now works, and matches step_scope's own return
  # convention, which `full` has always branched on.
  scope) step_scope "${2:-}"; exit $? ;;
  docker) step_docker || rc=1 ;;
  full)
    step_fmt  || rc=1
    step_vet  || rc=1
    step_lint || rc=1
    step_test || rc=1
    if step_scope "${2:-}"; then
      step_docker || rc=1
    else
      echo "     (docker suites skipped — nothing in this diff can reach one)"
    fi
    ;;
  all)
    # Every step runs even after one fails: a gate that stops at the first
    # problem makes you re-run the slow parts once per problem.
    step_fmt  || rc=1
    step_vet  || rc=1
    step_lint || rc=1
    step_test || rc=1
    ;;
  *) echo "usage: bash scripts/gate.sh [fmt|vet|lint|test|scope|docker|full] [BASE]" >&2; exit 2 ;;
esac

if [ "$rc" -eq 0 ]; then echo "gate: clean"; else echo "gate: FAILED"; fi
exit "$rc"
