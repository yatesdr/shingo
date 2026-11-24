#!/usr/bin/env bash
#
# The gate: what CI enforces, run locally, in three tiers on the three
# invocations already in use. A run is only as strong as its tier, and the
# sentence it prints names the tier, so a dev pass cannot be pasted as a cert.
#
#   dev  = bash scripts/gate.sh             fmt, then at once: modbuild, lint, the
#          dev-tier guards (script pins only when their inputs are in the diff),
#          the sim compile, the unit tests through Go's test cache. Seconds to a
#          minute; run it on every change.
#   push = bash scripts/gate.sh full        fmt, then at once: modbuild, lint, every
#          guard but the script pins whose inputs are unchanged and the
#          `alone` ones (`gate.sh scripts` runs those), the sim compile,
#          the unit tests (-count=1), the docker suites (scoped by diff), the
#          sim-only tests natively without -race. Before a push. race: CI.
#   cert = bash scripts/gate.sh full --unscoped   push's steps at full strength:
#          every guard (but the `alone` ones, which `gate.sh scripts` runs),
#          docker whatever the diff, race over core's engine and www, the
#          sim-only tests under -race. Certifies a tip.
#
# WHERE EACH TAG SET COMPILES AND RUNS (every tag compiles somewhere before a
# push and runs somewhere before origin):
#
#   tag set            dev                 push               cert             CI
#   untagged+lockcheck run (cached)        run                run              run, and -race
#   docker,lockcheck   compile (lint)      run (scoped)       run (unscoped)   run, and -race shards
#   sim                compile (sim vet)   run, no -race      run, -race       build; run -race (sim job)
#   race (core engine, www, with docker)   —                  —                run       docker-race shards
#   shots, explain, deps, plc's race file  —   compile (tagged-vet)  compile    compile (guards job)
#
# The minor tags compile in push and in CI's guards job, not in dev; the
# screenshot (shots) and explain tests run by hand.
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
# -race IS SCOPED, NOT ABSENT — and it used to be absent. This header said
# "NO -race HERE, DELIBERATELY", on the reasoning that module-wide race runs
# cost 25-35 minutes and CI runs them anyway. The cost argument was right and
# the conclusion was wrong: a data race on Engine.lastSceneSync shipped on the
# localization branch behind a comment asserting a serialisation the call graph
# did not provide, and no invocation of this script could have found it. "Run it
# targeted if you think you touched concurrency" only works on someone who
# already knows they did.
#
# So `race` runs the detector over ./engine/... and ./www/... — the two packages
# where a concurrent caller arrives WITHOUT anyone writing a goroutine (the
# reconnect path, and one goroutine per HTTP request) — with their docker tests,
# as CI's docker-race shards run them, against a throwaway Postgres of its own.
# Scoped to two packages it is minutes; module-wide it is half an hour. See
# step_race.
#
# It is in the cert tier and in CI on every PR; the push tier leaves it to CI,
# whose docker-race shards are a strict superset of it.
#
# DOCKER SUITES ARE SCOPED, NOT SKIPPED. See `scope` below.
#
# Usage:
#   bash scripts/gate.sh                  the dev tier (above)
#   bash scripts/gate.sh full [BASE]      the push tier; docker if scope against
#                                         BASE (default: upstream) says so. The
#                                         sentence names the base and its sha.
#   bash scripts/gate.sh full --unscoped  the cert tier: docker whatever the diff.
#                                         On a pushed tip or a merged one the diff
#                                         is empty and scope skips docker, so
#                                         certify with this.
#   bash scripts/gate.sh scope [BASE]     say whether the docker suites are needed
#                                         (exit 0 = needed, 1 = not needed)
#   bash scripts/gate.sh order            print the order each docker module's
#                                         packages would run in (docker_order)
#   bash scripts/gate.sh race            -race -tags docker over ./engine/... and
#                                         ./www/... (borrows WSL's cgo on Windows;
#                                         needs docker for its Postgres)
#   bash scripts/gate.sh docker           run the docker suites (no -race)
#   bash scripts/gate.sh sim              the sim-only tests (-tags sim, with -race),
#                                         in every package that holds one
#   bash scripts/gate.sh simvet           the sim compile alone
#   bash scripts/gate.sh scripts          every scripts/check-*.sh guard, whatever
#                                         its tier (CI's guards job runs this)
#   bash scripts/gate.sh modbuild         GOWORK=off go build, per module
#   bash scripts/gate.sh fmt|vet|modbuild|lint|scripts|test|race|sim|simvet  one step
#   bash scripts/gate.sh lint|test [MODULE...]
#                                         lint or unit-test only the named modules
#                                         (protocol shared shingo-core shingo-edge
#                                         integration); an unknown name is a usage
#                                         error, and the gate sentence names the
#                                         scope so a scoped pass cannot be pasted
#                                         as a whole-repo one
#
#   bash scripts/gate.sh lock-status      who holds the machine gate lock, or "free"
#   bash scripts/gate.sh identity         print the external identity a run records
#                                         beside its test manifests (gate-timing.sh)
#
#   GATE_SIDE=windows bash scripts/gate.sh ...   from Windows, run on the Windows
#                                         side, as before the Linux side existed
#   GATE_SIDE=linux bash scripts/gate.sh ...     the Linux side or FAILED; unset,
#                                         the Linux side when it is set up, else
#                                         the Windows side, saying why (below)
#
# THE GATE LOCK. Every step except fmt, scope, order and lock-status takes a
# lock shared by all worktrees of this clone, and a second gate waits for it,
# printing who holds it. fmt is exempt because the pre-push hook runs it and
# must never wait. The lock frees itself if the holding gate dies (its
# heartbeat stops). See scripts/gate-lock.sh. A gate in a worktree's mirror on
# the Linux side takes the same lock (scripts/gate-mirror.sh).
#
# WHAT KEEPS IT (owner, 2026-11-21): the runs whose verdict or timing must be
# clean. `full`, `full --unscoped` and `race`; proofs, sims and rigs (they hold
# it through scripts/gate-lock.sh); any gate-timing measurement; anything that
# certifies a tip. It began as one gate per machine when a 16 GB laptop ran out
# of memory under concurrent gates; memory is no longer the reason, load-
# sensitive docker tests and timing studies are. A parallel lane MAY skip it
# (GATE_NO_LOCK=1, said in its report) for the dev tier and for docker tests of
# only the packages it changed (-p 1). A red from an unlocked run that could be
# load (a timeout, a slow-path assertion, a container start) is re-run alone
# under the lock before anyone debugs it.
#

set -uo pipefail
cd "$(dirname "$0")/.."
ROOT="$(pwd)"

# START IT FROM THE WINDOWS WORKTREE; IT RUNS ON THE LINUX SIDE (R37, 2026-11-24).
#
# Started from a Windows worktree (Git Bash, or PowerShell through it), every
# mode but fmt, scope, order, cachescope, identity and lock-status runs in a
# mirror of that worktree on WSL's own ext4 filesystem, and its output and exit
# code come back to the caller; scripts/gate-mirror.sh says how the mirror is
# held equal to the working tree and how one gate lock covers both sides.
# GATE_SIDE=windows runs it on the Windows side as before, and a Linux side
# that is not set up (scripts/gate-linux-setup.sh --check) falls back to the
# Windows side, the sentence saying so. Off Windows nothing changes.
#
# WHY, MEASURED. The same tree's gate, same tool versions, Windows clone against
# a WSL ext4 clone (R31 and the round 2 close-out, work/loc-census-2026-11-20/
# REPORT-gate-fast.md): push 348.9 against 127.1 s, cert 353.5 against 177.4 s,
# dev 109.5 against 40.5 s. This block used to say Windows-native was within
# ~2x of Linux-native so the repo did not want moving to ext4; per command that
# held, for the whole gate it did not, and the mirror is the ext4 copy while
# the editing stays on Windows.
#
# WHAT STILL HOLDS: never gate a Windows path from inside WSL (/mnt/c). WSL
# reaches it through a 9p/virtio-fs bridge, and Go's build and test-compile
# phases are metadata-heavy; crossing it costs 10-20x, MEASURED on this host
# with a warm cache:
#
#                        WSL -> /mnt/c   Windows -> C:   WSL -> ext4
#   go build ./...          48.6s            2.4s           1.2s
#   go vet ./...            24.1s            2.2s           0.9s
#   test compile            28.2s            7.6s           3.3s
#
# On the Windows side `-race` still borrows WSL's cgo (step_race, step_sim
# cross for their runs); on the Linux side they run natively in the mirror.
if [ -n "${WSL_DISTRO_NAME:-}" ] && case "$ROOT" in /mnt/*) true ;; *) false ;; esac; then
  echo "WARNING: running under WSL against a Windows path ($ROOT)." >&2
  echo "         This is 10-20x slower than starting the same gate from Git Bash," >&2
  echo "         which runs it in a mirror on WSL's own filesystem (scripts/gate-mirror.sh)." >&2
  echo >&2
fi

# A MIRROR (scripts/gate-mirror.sh) knows itself by .git/gate-mirror, which
# names the Windows worktree it copies. A gate in it, synced by that worktree's
# gate or started here by hand, takes the Windows clone's lock and timing flag,
# whose paths the mirror records, never a private one (mirror_prelock refuses
# a mirror that records none), and uses the Linux side's pinned tools.
GATE_IN_MIRROR="" GATE_LINUX_ENV_MISSING=""
if [ -f "$ROOT/.git/gate-mirror" ]; then
  GATE_IN_MIRROR=1
  [ -n "${GATE_LOCK_DIR:-}" ] || GATE_LOCK_DIR="$(git config --get gate.lockdir 2>/dev/null)"
  [ -n "${GATE_TIMING_DIR:-}" ] || GATE_TIMING_DIR="$(git config --get gate.timingdir 2>/dev/null)"
  [ -n "$GATE_LOCK_DIR" ] && export GATE_LOCK_DIR
  [ -n "$GATE_TIMING_DIR" ] && export GATE_TIMING_DIR
  _env="${GATE_LINUX_ENV:-$HOME/gate-tools/env.sh}"
  # shellcheck source=/dev/null
  if [ -r "$_env" ]; then . "$_env"; else GATE_LINUX_ENV_MISSING="$_env"; fi
fi

ALL_MODULES="protocol shared shingo-core shingo-edge integration"
usage() {
  echo "usage: bash scripts/gate.sh [fmt|vet|modbuild|lint|scripts|test|race|sim|simvet|scope|order|docker|full|lock-status|identity] [BASE]" >&2
  echo "       bash scripts/gate.sh full --unscoped                 docker whatever the diff (certification)" >&2
  echo "       bash scripts/gate.sh lint|test [MODULE...]   MODULE in: $ALL_MODULES" >&2
  echo "       GATE_SIDE=windows|linux bash scripts/gate.sh ...   from Windows: which side runs it" >&2
  exit 2
}

# SCOPED RUNS. `gate.sh lint|test shingo-core shared` is the interface;
# GATE_MODULES is how it is carried to the steps (and still honoured when set
# directly). Either way every name is checked against ALL_MODULES — a typo
# that silently scoped the run to nothing would print "ok" over no modules.
if [ $# -gt 1 ]; then
  case "$1" in
    lint|test) GATE_MODULES="${*:2}" ;;
  esac
fi
MODULES="${GATE_MODULES:-$ALL_MODULES}"
for _m in $MODULES; do
  case " $ALL_MODULES " in
    *" $_m "*) ;;
    *) echo "gate.sh: unknown module '$_m'" >&2; usage ;;
  esac
done
[ -n "${MODULES// /}" ] || { echo "gate.sh: empty module list" >&2; usage; }
case "${GATE_DOCKER_P:-}" in
  "") ;;
  0|*[!0-9]*) echo "gate.sh: GATE_DOCKER_P must be a positive integer, got '$GATE_DOCKER_P'" >&2; usage ;;
esac
# The scope the sentence names: empty when every module ran.
gate_scope=""
[ "$(echo $MODULES)" = "$ALL_MODULES" ] || gate_scope="$(echo $MODULES)"
rc=0

# Which modules actually carry docker-tagged tests. COMPUTED, not listed,
# and computed in ONE place because two copies of this answer is exactly the
# rot it exists to prevent: `scope` used to derive it while `docker` carried
# a hardcoded triple, so the first `go:build docker` test to land in
# `protocol` or `shared` would have had scope say NEEDED and docker silently
# not run it. Today the two lists agree, which is why nobody noticed.
#
# A BUILD LINE, NOT THE WORDS. The guards' own fixture tests (shared/scripts)
# write "//go:build docker" into Go strings, and an unanchored grep counted
# shared as a docker module on the strength of them: its untagged tests moved
# into the docker step and a shared _test.go change scoped docker in.
docker_modules() {
  local m
  for m in $MODULES; do
    if grep -rlqE '^//go:build .*\bdocker\b' --include='*_test.go' "$ROOT/$m" 2>/dev/null; then
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

# EACH MODULE MUST BUILD WITHOUT THE WORKSPACE, and until 2026-08-19 one did
# not. go.work resolves every module's imports against the sibling directories,
# so an import with no matching `require` in that module's own go.mod builds
# fine in the workspace and fails for anyone consuming the module on its own.
# That is what hid protocol -> shared/clock: protocol imported it with no
# require, shared required protocol back, and the two were circular behind a
# workspace that made the cycle invisible. README's "each module builds
# independently" was false for months and nothing could tell you.
#
# GOWORK=off is the whole check. It costs one extra build per module and it is
# the only thing in this gate that sees a missing require or a missing go.sum
# entry — shingo-edge was also short a go.sum line for klauspost/compress, via
# minio-go, which only the workspace build was covering.
#
# THE FIVE BUILDS RUN AT ONCE (2026-11-23). One after another they were a sum,
# 47-50 s in the dev gate, the longest one (shingo-core) 18.6 s of it (the
# gate-time baseline of 2026-11-22). They share only Go's build cache, which
# the unit step's per-module `go test` runs already share at once. Each build
# writes its own log and verdict under .gate/; the logs print in $MODULES
# order whatever order the builds finish in, each followed by its FAIL line,
# as the serial loop printed them. A module with no verdict file (its build
# was killed) reads as a failure.
step_modbuild() {
  local m failed=0 pids="" rc secs
  for m in $MODULES; do
    rm -f "$ROOT/.gate/modbuild-$m.log" "$ROOT/.gate/modbuild-$m.rc"
    (
      t0="$EPOCHREALTIME"
      ( cd "$ROOT/$m" && GOWORK=off go build ./... ) >"$ROOT/.gate/modbuild-$m.log" 2>&1
      echo "$? $(_secs "$t0")" >"$ROOT/.gate/modbuild-$m.rc"
    ) &
    pids="$pids $!"
  done
  # By pid, so this waits for these five builds and nothing else.
  # shellcheck disable=SC2086 # pids
  wait $pids
  for m in $MODULES; do
    cat "$ROOT/.gate/modbuild-$m.log" 2>/dev/null
    rc=1 secs=0
    [ -e "$ROOT/.gate/modbuild-$m.rc" ] && read -r rc secs <"$ROOT/.gate/modbuild-$m.rc"
    [ "$rc" = 0 ] || { echo "  FAIL $m — does not build standalone (GOWORK=off)"; failed=1; }
    steps_log "modbuild:$m" "$secs"
  done
  [ "$failed" -eq 0 ] && echo "ok   modbuild" || echo "FAIL modbuild"
  return $failed
}

# NOT IN `all` OR `full`: LINT IS THE VET STEP THERE. golangci-lint v2.11.4's
# govet runs the same analyzers `go vet` runs by default — the two lists were
# compared name by name, 35 each — and this config neither narrows govet nor
# excludes any of its findings. Lint also runs them over the docker-tagged
# files, which this step never compiles. So in a gate that runs lint, this
# was a second compile of every module to ask a subset of the same question:
# ~25s on the Windows host, ~75s on the 4-core gate box. It stays as
# `gate.sh vet`, for asking that one question quickly.
#
# NOT parallelised across modules, unlike step_test, and that is a measured
# decision rather than an oversight: 8s serial against 7s concurrent. Vet's
# wall time is one module (shingo-core) compiling, and the other four are
# rounding error — so the concurrency buys a second and costs five per-module
# log files and an interleaving problem to solve. Not worth it.
step_vet() {
  local m failed=0
  for m in $MODULES; do
    ( cd "$ROOT/$m" && go vet ./... && go vet -tags lockcheck ./... ) || failed=1
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
  #
  # AND A HANDOFF CAN STILL FAIL WITH THE FLAG ON: the laptop probe's second
  # cert run (2026-11-21) had its scripts step exit 3 with it. So the run goes through
  # golangci_run (scripts/golangci-run.sh), which retries exactly that exit and
  # message a bounded number of times, and says when it did.
  local m failed=0 t0 err
  mkdir -p "$ROOT/.gate"
  for m in $MODULES; do
    t0="$EPOCHREALTIME"
    err="$ROOT/.gate/lint-$m.err"
    ( cd "$ROOT/$m" && golangci_run "$err" run --allow-serial-runners --build-tags docker,lockcheck \
        --config="$ROOT/.golangci.yml" --issues-exit-code=1 ./... ) || failed=1
    cat "$err" >&2
    steps_log "lint:$m" "$(_secs "$t0")"
  done
  [ "$failed" -eq 0 ] && echo "ok   lint" || echo "FAIL lint"
  return $failed
}

# ONE RUN, NOT TWO. This used to run each module to /dev/null and then, if it
# failed, run the whole module AGAIN to have something to print — so a red gate
# cost double a green one, on the run you are least willing to wait through.
# The log is written the first time and the excerpt comes out of it.
# Per-worktree under $ROOT/.gate for the same reason step_docker's logs are:
# see the note there about /tmp being machine-global on Git Bash.
#
# CONCURRENT, AFTER A SERIAL INTERLUDE. The modules run in parallel job
# control, one log per module. Serial was chosen for one named reason —
# shingocore/rds's TestPollerStopHaltsPolling asserted a 30ms wall-clock drain
# window and failed ~1 run in 6 under concurrent load — and that test is now
# gone: Stop() became synchronous (it waits on doneChan) and
# TestPollerStop_IsSynchronous asserts the same property with no timing window
# at all. MEASURED serial vs concurrent on the 20-core host of the time (not
# re-run on the 24-thread laptop): 45s -> ~26s bare
# (all five modules), 29.5s -> ~18s in `full` (the three modules the docker
# step does not cover).
#
# THE FAILURE MODE THIS CAN STILL FIND is a second load-fragile test that
# serial execution was hiding. That is a finding, not a reason to go back:
# the fix is to harden that test the way the poller's was hardened, not to
# serialize five modules forever.
# Which modules still need an untagged `go test` of their own.
#
# NOT ALL OF THEM, WHEN THE DOCKER STEP IS ALSO RUNNING. `-tags=docker` ADDS
# files and removes none, and nothing in this tree carries a `!docker`
# constraint — so `go test -tags=docker ./...` runs every test the untagged run
# would, plus the docker ones.
#
# The property is structural, so no count proves it: `go test -list` per
# package shows every untagged test again under the tag (shingo-core's engine:
# 87 untagged, 738 tagged), and the `!docker` search below keeps it true.
#
# A docker-tagged TestMain can still break it, by deciding whether the untagged
# tests run at all. internal/schemadump has one: when its Postgres will not
# start it runs the binary anyway (Postgres tests skipped) and prints the
# SHINGO-DOCKER-DOWN line step_docker refuses, rather than exiting 0 with its
# untagged tests unrun. A new one must do the same.
#
# So running both duplicates every untagged test in shingo-core and
# integration, and cold it duplicates a whole second compile of shingo-core.
# protocol, shared and shingo-edge carry no docker tests and run here.
#
# THE `!docker` SEARCH IS THE ENTIRE SAFETY OF THIS. The moment one file
# excludes itself from the docker build, the superset property is false and
# skipping the untagged run would silently stop testing that file. Recomputed
# on every invocation so it cannot go stale, and it fails SAFE: anything found,
# and every module runs.
untagged_only_modules() {
  if grep -rlq 'go:build.*!docker' --include='*.go' "$ROOT" 2>/dev/null; then
    printf '%s' "$MODULES"
    return
  fi
  local m d dockermods covered
  dockermods="$(docker_modules)"
  for m in $MODULES; do
    covered=0
    for d in $dockermods; do
      [ "$m" = "$d" ] && covered=1
    done
    [ "$covered" -eq 0 ] && printf '%s ' "$m"
  done
}

# ── The tiers and the diff ──────────────────────────────────────────────
#
# GATE_TIER is dev (the bare gate), push (`full`), cert (`full --unscoped`), or
# step (one step by name, e.g. CI's `gate.sh scripts` and `gate.sh sim`), which
# runs at full strength.
#
# write_diff records, once per run, the paths this tree changes against its
# base: `git diff --name-only <merge-base HEAD origin/main>` (commits since the
# base, staged and unstaged edits, deletions) plus untracked files. With no
# origin/main it records the single line `*`: every pin runs and the unit step
# does not trust the cache. A stale origin/main widens the diff, which fails
# the same safe way.
DIFF_FILE="$ROOT/.gate/diff.files"

# tree_diff <base> — the paths this tree changes against <base>, one a line,
# sorted: `git diff --name-only <merge-base HEAD base>` (commits since the
# base, staged and unstaged edits, deletions) plus untracked files. Returns 1,
# printing nothing, when there is no merge base. The ONE reading of "what this
# tree changes": write_diff and step_scope both take it, so the dev tier's pins
# and the push tier's docker scope cannot see different trees (G11: scope read
# `git diff BASE...HEAD`, the commits alone, and a level tree with an
# uncommitted Go edit skipped docker; scripts/check-docker-scope.sh pins it).
tree_diff() {
  local mb
  mb="$(git merge-base HEAD "$1" 2>/dev/null)" && [ -n "$mb" ] || return 1
  { git diff --name-only "$mb" 2>/dev/null
    git ls-files --others --exclude-standard 2>/dev/null
  } | sed '/^$/d' | LC_ALL=C sort -u
}

# ALWAYS origin/main, whatever `full BASE` scopes the docker suites against: the
# wider diff is the safe one (more pins run).
write_diff() {
  local files
  mkdir -p "$ROOT/.gate"
  if files="$(tree_diff origin/main)"; then
    printf '%s\n' "$files" | sed '/^$/d' >"$DIFF_FILE"
  else
    echo '*' >"$DIFF_FILE"
  fi
}

# diff_touches <glob...> — true when a path in the diff matches one of the globs
# (`*` crosses `/`), or when there was no base to diff against.
diff_touches() {
  [ -e "$DIFF_FILE" ] || write_diff
  local p g
  while IFS= read -r p; do
    [ "$p" = '*' ] && return 0
    for g in "$@"; do
      # shellcheck disable=SC2254 # g is a glob on purpose
      case "$p" in $g) return 0 ;; esac
    done
  done <"$DIFF_FILE"
  return 1
}

# diff_go_only — true when every path in the diff is a .go, go.mod or go.sum
# under one of the five modules (an empty diff counts). The bare gate's unit
# step trusts Go's test cache only then. The cache keys on what a test's own
# process opens; a file a test reads through a subprocess (node, over the edge's
# JS) or a census reads in a module the test does not import is invisible to it,
# so any other path in the diff (JS, assets, YAML, templates, docs, scripts)
# reruns everything. What is left uncovered — a .go file in one module read as
# text by a census test in another that does not import it — `full` catches.
diff_go_only() {
  [ -e "$DIFF_FILE" ] || write_diff
  local p
  while IFS= read -r p; do
    case "$p" in
      '*') return 1 ;;
      protocol/*.go|shared/*.go|shingo-core/*.go|shingo-edge/*.go|integration/*.go) ;;
      protocol/go.mod|protocol/go.sum|shared/go.mod|shared/go.sum) ;;
      shingo-core/go.mod|shingo-core/go.sum|shingo-edge/go.mod|shingo-edge/go.sum) ;;
      integration/go.mod|integration/go.sum) ;;
      *) return 1 ;;
    esac
  done <"$DIFF_FILE"
  return 0
}

# module_uncached <module> — why the dev tier must run <module>'s unit tests
# -count=1, or nothing when Go's test cache may serve them. Today: whenever
# the diff is not Go-only, for every module (diff_go_only). `gate.sh
# cachescope -` prints it per module for a path list on stdin
# (scripts/check-cache-scope.sh).
module_uncached() {
  diff_go_only && return 0
  echo "the diff has non-Go files"
}

# guard_header <file> <key> — a guard's `# key: value` header line (first 30
# lines), or empty.
guard_header() {
  sed -n "1,30s/^# $2:[[:space:]]*//p" "$1" | head -1 | tr -d '\r'
}

# THE GREP-BASED GUARDS, WHICH RAN ONLY IN CI UNTIL 2026-08-19.
#
# The swap-mode-literals guard (then scripts/check-swap-mode-literals.sh, now a
# rule of shared/scripts/repolint) had a job of its own in lint.yml and no
# call anywhere in this file, so `gate.sh full` could go green on a change that
# CI then rejected. A pre-push gate that does not run a CI gate is not telling
# you what CI will say, which is the only thing it is for.
#
# DISCOVERED, NOT LISTED. Every scripts/check-*.sh runs. The alternative is a
# hardcoded name here plus a job over in lint.yml, which is exactly the pairing
# that let the first one go missing — a new guard would have to be remembered
# in two places by whoever writes it. Globbing means it only has to be named
# once, by its filename.
#
# ALL AT ONCE. Each guard reads the tree and writes nothing it shares (the
# gate-lock pin works in a private lock directory), so they run side by side,
# each into its own log under .gate/scripts/, and the logs are printed in name
# order afterwards. In series the step was its guards' sum, which a process
# spawn per grep makes dear on Windows: ~170 s on the laptop against ~27 s on
# the Linux box, serially before the unit tests in both `all` and `full`. Side by side it
# is about its slowest guard. Each guard's seconds go to the steps log as
# `scripts:<name>`.
#
# TIERED BY WHAT CAN CHANGE THEIR ANSWER. Each guard's header names its tier:
#   # tier: dev    runs in every gate (the default, so a guard that names no
#                  tier, or misspells it, is slower and never blind)
#   # tier: push   runs in `full` and `full --unscoped`, not in the bare gate
#                  (tagged-vet compiles every minor tag; the discard ratchet is a
#                  second golangci-lint pass, which queues behind lint's own on
#                  golangci-lint's machine lock, --allow-serial-runners)
#   # inputs: ...  a script pin: it tests a script, so it can change its answer
#                  only when those paths change; the bare gate and `full` skip it
#                  when none is in the diff (write_diff, diff_touches); `full --unscoped` and
#                  `gate.sh scripts` (CI's guards job) always run it
#   # tier: alone  runs only in `gate.sh scripts` (CI's guards job), never in a
#                  tier: it times tests (check-test-cost.sh), and a tier's own
#                  build and test steps beside it are the load it must not time
# The step writes its account to .gate/scripts.summary for the sentence.
step_scripts() {
  local f n s failed=0 ran=0 total=0 logdir="$ROOT/.gate/scripts" pids="" names=""
  local tier="${GATE_TIER:-step}" gtier inputs skipped_inputs="" skipped_push="" skipped_alone=""
  mkdir -p "$logdir" || { echo "FAIL scripts — cannot create $logdir"; return 1; }
  for f in "$ROOT"/scripts/check-*.sh; do
    [ -e "$f" ] || continue
    total=$((total + 1))
    n="$(basename "$f" .sh)"
    s="${n#check-}"
    gtier="$(guard_header "$f" tier | cut -d' ' -f1)"
    inputs="$(guard_header "$f" inputs)"
    if [ "$gtier" = alone ] && [ "$tier" != step ]; then
      skipped_alone="${skipped_alone:+$skipped_alone, }$s"; continue
    fi
    case "$tier" in
      dev|push)
        if [ "$tier" = dev ] && [ "$gtier" = push ]; then
          skipped_push="${skipped_push:+$skipped_push, }$s"; continue
        fi
        # shellcheck disable=SC2086 # inputs is a list of pathspecs
        if [ -n "$inputs" ] && ! diff_touches $inputs; then
          skipped_inputs="${skipped_inputs:+$skipped_inputs, }$s"; continue
        fi ;;
    esac
    ran=$((ran + 1))
    names="$names $n"
    rm -f "$logdir/$n.rc"
    ( t="$EPOCHREALTIME"; bash "$f" >"$logdir/$n.out" 2>&1; echo $? >"$logdir/$n.rc"
      steps_log "scripts:$n" "$(_secs "$t")" ) &
    pids="$pids $!"
  done
  s="$ran of $total"
  [ -n "$skipped_inputs" ] && s="$s; skipped: $skipped_inputs — inputs unchanged"
  [ -n "$skipped_push" ] && s="$s; push-only: $skipped_push"
  [ -n "$skipped_alone" ] && s="$s; alone, in \`gate.sh scripts\`: $skipped_alone"
  echo "$s" >"$ROOT/.gate/scripts.summary"
  # The guards that ran, for the steps log's selection line (gate-timing.sh).
  # shellcheck disable=SC2086 # names is a list of guard names
  [ -n "${GATE_SEL_DIR:-}" ] && printf '%s\n' $names >"$GATE_SEL_DIR/scripts.ran"
  if [ "$total" -eq 0 ]; then
    echo "ok   scripts (none found)"
    return 0
  fi
  # These jobs by pid: a bare wait would also wait for anything else this
  # shell has started.
  # shellcheck disable=SC2086
  [ -n "$pids" ] && wait $pids
  for n in $names; do
    cat "$logdir/$n.out"
    [ "$(cat "$logdir/$n.rc" 2>/dev/null)" = 0 ] || failed=1
  done
  [ "$failed" -eq 0 ] && echo "ok   scripts ($s)" || echo "FAIL scripts"
  return $failed
}


# excerpt_failures <log> — THE failure excerpt, for every step that runs tests:
# unit (step_test), docker, race and sim all print through this one function,
# so a red step reads the same whichever of them went red. In order:
#
#   1. each LEAF `--- FAIL:` (a failing test with no failing subtest under it;
#      a parent's block is only its children's headers) with its indented
#      block. Inside the block the `*_test.go:N:` assertion lines are kept and
#      the engine's t.Log chatter around them is dropped; a block with no
#      assertion line (a race report, a helper without t.Helper) is kept
#      whole. Capped per test and in the number of tests.
#   2. the package-level lines: `FAIL <pkg>`, `panic:`, `DATA RACE`, and the
#      compiler's `# pkg` / `file.go:L:C:` lines when a package did not build.
#   3. the log path, always.
#
# No `ok` lines and no unindented app logging: the 30-line `grep -Ev` excerpts
# this replaced interleaved both and buried the index of what broke.
# Pinned against the preserved docker fixture (evidence-lead/a3-proof.sh).
excerpt_failures() {
  # Caps: at most 30 leaf tests, 8 block lines each. Inside the function so it
  # can be lifted out alone (evidence-lead/a3-proof.sh sources just this).
  local log="$1" maxt=30 maxl=8
  awk -v maxt="$maxt" -v maxl="$maxl" '
    function indent(s) { match(s, /^ */); return RLENGTH }
    { line[NR] = $0 }
    END {
      shown = 0; extra = 0
      for (i = 1; i <= NR; i++) {
        if (line[i] !~ /^ *--- FAIL: /) continue
        ind = indent(line[i])
        leaf = 1
        for (j = i + 1; j <= NR && line[j] != "" && indent(line[j]) > ind; j++)
          if (line[j] ~ /^ *--- FAIL: /) leaf = 0
        if (!leaf) continue
        if (shown >= maxt) { extra++; continue }
        shown++
        # Keep the assertion lines (and their deeper continuation lines).
        n = 0; keep = 0; any = 0
        for (k = i + 1; k < j; k++) if (line[k] ~ /_test\.go:[0-9]+:/) any = 1
        for (k = i + 1; k < j; k++) {
          if (!any) { sel[++n] = line[k]; continue }
          if (indent(line[k]) == ind + 4) keep = (line[k] ~ /_test\.go:[0-9]+:/)
          if (keep) sel[++n] = line[k]
        }
        hdr = line[i]; sub(/^ +/, "", hdr)
        print "  " hdr
        for (k = 1; k <= n && k <= maxl; k++) {
          s = sel[k]; sub(/^ +/, "", s)
          print "      " substr(s, 1, 400)
        }
        if (n > maxl) print "      ... (" n - maxl " more lines in the log)"
      }
      if (extra > 0) print "  ... and " extra " more failing tests (see the log)"
    }' "$log"
  grep -E '^FAIL[[:space:]]+[^[:space:]]|^panic: |DATA RACE|^# |^[^ ]+\.go:[0-9]+:[0-9]+: ' "$log" \
    | awk '!seen[$0]++' | head -20 | sed 's/^/  /'
  echo "  (full log: $log)"
}

# harness_verdict <log> — connect-shaped failure classifier for docker logs.
# A docker run that fails because the shared Postgres was unavailable shows
# every failing test with a connect/auth error; those are re-runs, not bugs.
# An "other" failure that fails again alone is real. The verdict names the
# split so the reader re-runs before debugging.
# A3's pin: A3-block-classification.txt — 36 of 37 CONNECT, 1 OTHER.
harness_verdict() {
  local log="$1" name total=0 other=0
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    total=$((total + 1))
    if ! awk -v t="$name" '
      index($0, "--- FAIL: " t) { infail=1; next }
      /^--- FAIL: / { infail=0 }
      infail && /failed to connect|failed SASL auth|connection refused|dial tcp/ { found=1; exit }
      END { exit !found }
    ' "$log"; then
      other=$((other + 1))
      echo "  other failures (not connect-shaped):"
      echo "    $name"
      # The message is the test's own t.Error/t.Fatal output: the block's
      # `_test.go:NN:` lines (up to 3). Engine and store log lines in the same
      # block come first and say nothing about the failure; only when the
      # block has no _test.go line does its first line stand in.
      awk -v t="$name" '
        index($0, "--- FAIL: " t) { infail=1; next }
        /^--- FAIL: / { infail=0 }
        infail && first == "" { first = $0 }
        infail && /_test\.go:[0-9]+:/ { print "      " $0; n++; if (n >= 3) exit }
        END { if (n == 0 && first != "") print "      " first }
      ' "$log"
    fi
  done < <(grep -E '^--- FAIL:' "$log" | sed 's/^--- FAIL: //; s/ (.*//' | sort -u)
  if [ "$total" -gt 0 ]; then
    echo "HARNESS: $((total - other)) of $total failing tests are connect failures to the shared Postgres — re-run before debugging; an \"other\" test that fails again alone is real."
  fi
}

step_test() {
  local m failed=0 logdir mods
  mods="${1:-$MODULES}"
  # NODE IS A PRECONDITION, NOT A NICETY. The JS is tested under plain node
  # through Go wrappers, and each wrapper skips when node is missing
  # (`t.Skipf("node not on PATH ...")`): 96 test files at 2026-11-23 (shared 10,
  # Core engine 1 and www 23, Edge cmd/shingoedge 2 and www 60, among them
  # TestProcessSetupPagePins, the dev tier's longest test). This said "five
  # suites"; it was a hand list, and it had rotted by the costliest member.
  # Skipping is right for a machine that genuinely has no node; it is wrong for
  # a gate, which would then report ok on JS it never executed. CI's
  # ubuntu-latest ships node, so this fires only on a dev box, which is exactly
  # where the silence was. Count them: grep -rl 'node not on PATH' --include=*_test.go
  if ! command -v node >/dev/null 2>&1; then
    echo "FAIL tests — node not on PATH"
    echo "  The JS tests skip silently without it (shared, Core engine and www,"
    echo "  Edge cmd/shingoedge and www)."
    echo "  Install Node LTS (nodejs.org) — CI's ubuntu-latest already has it."
    return 1
  fi
  if [ -z "$mods" ]; then
    echo "ok   tests (every module's untagged tests run inside the docker step)"
    return 0
  fi
  # GATE_TEST_LOG_DIR: a private directory for a nested gate's unit logs
  # (scripts/check-gate-exec.sh), so they cannot overwrite the logs of the gate
  # running it.
  logdir="${GATE_TEST_LOG_DIR:-$ROOT/.gate}"
  mkdir -p "$logdir" || { echo "FAIL tests — cannot create $logdir"; return 1; }
  # THE BARE GATE MAY READ GO'S TEST CACHE; every other tier runs -count=1. This
  # said the cache is "content-keyed — the test binary and every file and env
  # var the test's own process touched". It is not (go1.25.x
  # cmd/go/internal/test/test.go): an opened file is keyed on size, mode and
  # mtime, not content, and one under 2 s old makes the run uncacheable; opens
  # and stats outside the package's module root are dropped; a child process's
  # reads and anything read before m.Run are not seen; GODEBUG is in every key.
  # And under -exec, which the record-only wrapper passes (scripts/gate-timing.sh),
  # Go asks for no testlog and caches nothing, so with the wrapper on the dev tier
  # always runs -count=1 (it engaged 1 of 57 recorded runs before: docket D21).
  # With the wrapper off (GATE_EXEC=off): FAIL-SAFE, any path in the diff that is
  # not Go under a module (diff_go_only) reruns everything. A flake cached green
  # re-rolls at `full`.
  # Reuse that is safe from those holes is the wrapper's (GATE_REUSE=on,
  # scripts/gate-reuse.sh), not Go's cache.
  local count="-count=1" how=""
  if [ "${GATE_TIER:-}" = dev ]; then
    # Under the -exec wrapper Go's test cache is off whatever the diff, so the
    # dev tier runs -count=1 too (and its argv is push's: reuse keys on argv).
    if [ "${#GATE_EXEC_ARGS[@]}" -gt 0 ] && [ -n "${GATETIME_REUSE:-}" ]; then how="-count=1 under the -exec wrapper (GATETIME_REUSE=$GATETIME_REUSE)"
    elif [ "${#GATE_EXEC_ARGS[@]}" -gt 0 ]; then how="-count=1: the record-only -exec wrapper turns Go's test cache off"
    elif diff_go_only; then count=""; else how="-count=1: the diff has non-Go files"; fi
  fi
  # One background job per module; collect failures when the last lands. The
  # log stays per-module ($logdir/test-$m.log) so the excerpt printed on a
  # failure is that module's own, not another's interleaving.
  # Each job records its own exit code: the verdict is go test's, not a guess
  # read back out of the log text.
  for m in $mods; do
    # -tags lockcheck: every lock goes through protocol/lockcheck's guard, which fails a
    # re-entry or a backwards take loudly (LOCKS.md). The docker and race steps carry it too.
    # shellcheck disable=SC2086 # count is one flag or none
    ( cd "$ROOT/$m" && go test -tags lockcheck $count "${GATE_EXEC_ARGS[@]}" ./... >"$logdir/test-$m.log" 2>&1
      echo $? >"$logdir/test-$m.rc" ) &
  done
  wait
  for m in $mods; do
    if [ "$(cat "$logdir/test-$m.rc" 2>/dev/null)" != 0 ] \
        || grep -qE '^(FAIL|--- FAIL)' "$logdir/test-$m.log" 2>/dev/null; then
      echo "  --- $m ---"
      excerpt_failures "$logdir/test-$m.log"
      failed=1
    fi
  done
  # What the sentence says about the cache: reused out of run, from go test's
  # own `ok pkg (cached)` lines.
  if [ "${GATE_TIER:-}" = dev ] && [ -z "$how" ]; then
    local reused ran
    reused="$(for m in $mods; do cat "$logdir/test-$m.log"; done 2>/dev/null \
      | grep -cE '^ok[[:space:]]+[^[:space:]]+[[:space:]]+\(cached\)')"
    ran="$(for m in $mods; do cat "$logdir/test-$m.log"; done 2>/dev/null \
      | grep -cE '^(ok|FAIL)[[:space:]]+[^[:space:]]+[[:space:]]')"
    how="cached: $reused of $ran packages reused"
  fi
  echo "$how" >"$logdir/unit.summary"
  [ "$failed" -eq 0 ] && echo "ok   tests${how:+ ($how)}" || echo "FAIL tests"
  return $failed
}

# ── The sim step ──────────────────────────────────────────────────────
#
# `-tags sim` WAS COMPILED BY NOTHING HERE, AND THAT IS HOW A RACE SHIPPED.
# The unit step builds untagged and the race step is scoped to shingo-core's
# ./engine/... and ./www/..., so every file behind `//go:build sim` — the edge
# sim operator, the core fleet simulator, the simwarlink fake — was written,
# reviewed and pushed without one line of it ever being run by the gate. Two
# defects reached origin through that hole: a concurrent map write in the edge
# sim operator that survived from the commit that introduced it to the commit
# that fixed it, and a nil-store panic in the same file that broke the
# plant-agnostic harness for the whole series.
#
# THE RACE DETECTOR IS THE POINT, not just compilation. The sim operator is
# goroutine-driven by construction — timers firing against shared maps — so a
# plain `go test -tags sim` would have compiled the bug and passed. This runs
# the detector where cgo is available, borrows WSL's where it is not, and FAILS
# where neither is, rather than reporting a pass it did not earn.
#
# THE SIM-ONLY TESTS, NOT THE PACKAGES THAT HOLD THEM. `-tags sim` adds files
# and removes none, so a tagged run over a package re-runs every untagged test
# in it, which the unit and docker steps already ran. That was where this
# step's time went: the edge engine's ~1,400 untagged tests under the detector
# were ~185s of a ~200s step, around ~50 sim tests. So per package the step
# runs, anchored, exactly the tests `go test -list` names under `-tags sim` and
# not under `-tags lockcheck` (the unit step's build). What that trades away is
# a local -race run of the edge engine's untagged suite; CI's edge docker-race
# shards race the whole edge module on every PR (.github/workflows/edge.yml).
#
# THE PACKAGES ARE DERIVED FROM THE TREE, as docker_modules is: every package
# holding a _test.go whose //go:build names sim. A hard-coded list sat here
# called "the two packages that carry sim tests" while five sim test files in
# three more packages (core and edge www, edge cmd/shingoedge) ran nowhere.

# sim_test_dirs <module> — ./dir of every package in <module> holding a
# _test.go whose //go:build expression names sim positively (`sim`,
# `lockcheck || sim`; not `!sim`). Prints `OS <file>` for one that also names
# an OS: the listing below runs on this host while the run may be in WSL, so
# such a file could be listed on one side and built on the other.
sim_test_dirs() {
  ( cd "$ROOT/$1" && grep -rE --include='*_test.go' --exclude-dir=node_modules -m1 '^//go:build ' . 2>/dev/null ) \
    | awk '{
        i = index($0, ":"); f = substr($0, 1, i - 1); e = substr($0, i + 1); sub(/^\/\/go:build /, "", e)
        if (e !~ /(^|[^!A-Za-z0-9_])sim([^A-Za-z0-9_]|$)/) next
        if (e ~ /(^|[^A-Za-z0-9_])(unix|linux|windows|darwin|freebsd)([^A-Za-z0-9_]|$)/) { print "OS " f; next }
        sub(/\/[^\/]*$/, "", f); print f }' \
    | LC_ALL=C sort -u
}

# test_names <tags> <pkg...> — `importpath<TAB>Name` for every test, example,
# fuzz target and benchmark `go test -list` finds. Run from inside the module.
# A package that does not build fails the call, and its compiler output goes to
# stderr: a listing that silently lost a package would run none of its tests.
test_names() {
  local tags="$1" out
  shift
  out="$(go test -list '.' -tags "$tags" "$@" 2>&1)" || { printf '%s\n' "$out" >&2; return 1; }
  printf '%s\n' "$out" | awk '
    /^(Test|Example|Fuzz|Benchmark)[A-Za-z0-9_]*$/ { names[++n] = $0; next }
    /^ok[ \t]/ { for (i = 1; i <= n; i++) print $2 "\t" names[i]; n = 0 }'
}

# sim_plan — one line per module with sim-only tests:
# `module<TAB>pkg pkg ...<TAB>^(Name|Name|...)$`. Fails when a listing fails,
# when a sim test file names an OS, or when the whole tree yields no sim-only
# test (a discovery that finds nothing is broken, not finished).
sim_plan() {
  local m dirs d with without only total=0 os=""
  for m in $MODULES; do
    dirs="$(sim_test_dirs "$m")"
    [ -n "$dirs" ] || continue
    os="$os$(printf '%s\n' "$dirs" | sed -n "s#^OS \./#$m/#p")"
    dirs="$(printf '%s\n' "$dirs" | grep -v '^OS ')"
    [ -n "$dirs" ] || continue
    # shellcheck disable=SC2086 # dirs is a list of package directories
    with="$(cd "$ROOT/$m" && test_names sim $dirs)" || return 1
    # A package whose every file is sim-tagged (plc/simwarlink) does not build
    # untagged at all: it has no tests there, and listing it would fail.
    # shellcheck disable=SC2086
    d="$(cd "$ROOT/$m" && go list -e -tags lockcheck \
          -f '{{if or .GoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' $dirs)" || return 1
    without=""
    # shellcheck disable=SC2086
    [ -z "$d" ] || without="$(cd "$ROOT/$m" && test_names lockcheck $d)" || return 1
    only="$(LC_ALL=C comm -23 <(printf '%s\n' "$with" | LC_ALL=C sort -u) \
                              <(printf '%s\n' "$without" | LC_ALL=C sort -u) | sed '/^$/d')"
    [ -n "$only" ] || continue
    total=$((total + $(printf '%s\n' "$only" | wc -l)))
    printf '%s\t%s\t^(%s)$\n' "$m" \
      "$(printf '%s\n' "$only" | cut -f1 | LC_ALL=C sort -u | tr '\n' ' ' | sed 's/ $//')" \
      "$(printf '%s\n' "$only" | cut -f2 | LC_ALL=C sort -u | paste -sd'|' -)"
  done
  if [ -n "$os" ]; then
    echo "sim: these sim test files also constrain the OS, which the host-side listing cannot see past:" >&2
    printf '%s\n' "$os" | sed 's/^/    /' >&2
    return 1
  fi
  [ "$total" -gt 0 ] || { echo "sim: no sim-only test found anywhere; the discovery is broken" >&2; return 1; }
}

# GATE_GO is the toolchain CI runs: every workflow's setup-go pins it, and
# scripts/check-gate-toolchain.sh holds those pins to go.mod's go line, which is
# where it is read from here. The steps that borrow WSL's cgo (race, sim) run it
# too, through GOTOOLCHAIN: WSL's own go is newer (go1.26.8 on 2026-11-21), the
# Windows steps run the host's go1.25.x, and a race verdict from a third
# compiler is not CI's verdict (docket D7). On the Windows side two toolchains
# remain in one local verdict: the host's for every native step, and GATE_GO
# for race and sim in WSL. Only the second is CI's; nothing pins the first. The
# toolchain is fetched once into WSL's module cache; wsl_go_check fails the
# step, saying why, if it cannot be. On the Linux side (a mirror's gate,
# scripts/gate-mirror.sh) race and sim run natively under GOTOOLCHAIN=GATE_GO
# too (native_go_check), and the other steps under ~/gate-tools/env.sh's go.
GATE_GO="go$(sed -n 's/^go[[:space:]]*//p' "$ROOT/shingo-core/go.mod" 2>/dev/null | tr -d '\r')"

# wsl_go_check <step> — passes when WSL's go under GOTOOLCHAIN=$GATE_GO is
# $GATE_GO; otherwise prints what it is and fails.
wsl_go_check() {
  local v
  v="$(wsl.exe -d Ubuntu -- bash -lc "GOTOOLCHAIN=$GATE_GO go env GOVERSION" 2>&1 | tr -d '\r\0' | tail -1)"
  [ "$v" = "$GATE_GO" ] && return 0
  echo "FAIL $1 — WSL's go under GOTOOLCHAIN=$GATE_GO reports '$v', not $GATE_GO (CI's toolchain)"
  return 1
}

# native_go_check <step> — wsl_go_check for a gate already inside WSL.
native_go_check() {
  local v
  v="$(GOTOOLCHAIN="$GATE_GO" go env GOVERSION 2>&1 | tr -d '\r' | tail -1)"
  [ "$v" = "$GATE_GO" ] && return 0
  echo "FAIL $1 — go under GOTOOLCHAIN=$GATE_GO reports '$v', not $GATE_GO (CI's toolchain)"
  return 1
}

# sim_cmd <root> <plan> — the tagged runs, one `go test` per module, each with
# the module's sim-only tests as its -run. Every module runs even after one
# fails, and the string exits non-zero if any did. The root is INLINED rather
# than passed as a shell variable: this string also crosses the wsl.exe
# boundary, where an outer `R=...` assignment does not reliably survive Windows
# argument splitting (it silently produced `cd /shingo-core`). The -run pattern
# is single-quoted: it is names, `^`, `(`, `|`, `)` and `$`, none of which a
# single-quoted word expands.
sim_cmd() {
  local r="$1" plan="$2" mode="${3:-race}" m pkgs re s="rc=0;" flags="CGO_ENABLED=1 go test -race"
  [ "$mode" = plain ] && flags="go test"
  # plain runs natively, so under the record-only -exec wrapper when it is on
  # (scripts/gate-timing.sh); the -race runs cross into WSL and stay unwrapped.
  if [ "$mode" = plain ] && [ "${#GATE_EXEC_ARGS[@]}" -gt 0 ]; then
    flags="$flags$(printf ' %q' "${GATE_EXEC_ARGS[@]}")"
  fi
  while IFS=$'\t' read -r m pkgs re; do
    [ -n "$m" ] || continue
    s="$s ( cd '$r/$m' && $flags -count=1 -timeout=15m -tags sim -run '$re' $pkgs ) || rc=1;"
  done <<< "$plan"
  printf '%s' "$s exit \$rc"
}

# step_sim [plain] — the sim-only tests. Under the detector by default (cert,
# CI's sim job, `gate.sh sim`). `plain` is `full`'s run: natively, no -race, no
# WSL — the tests still run and still fail on a wrong answer, and the detector
# runs on them in CI's sim job on every PR and in `full --unscoped`. A named
# choice of the tier, printed in the step's ok line and the sentence, so it is
# not the silent downgrade the branch below refuses.
step_sim() {
  local logdir rrc how plan ntests npkgs mode="${1:-race}"
  logdir="$ROOT/.gate"
  mkdir -p "$logdir" || { echo "FAIL sim — cannot create $logdir"; return 1; }
  if ! plan="$(sim_plan 2>"$logdir/sim-plan.err")"; then
    echo "FAIL sim — could not work out which tests are sim-only:"
    sed 's/^/  /' "$logdir/sim-plan.err" | head -30
    return 1
  fi
  ntests="$(printf '%s\n' "$plan" | cut -f3 | tr -cd '|\n' | awk '{ n += length($0) + 1 } END { print n }')"
  npkgs="$(printf '%s\n' "$plan" | cut -f2 | wc -w | tr -d ' ')"

  if [ "$mode" = plain ]; then
    ( eval "$(sim_cmd "$ROOT" "$plan" plain)" >"$logdir/sim.log" 2>&1 ); rrc=$?
    how="no -race: CI's sim job races them"
  elif [ -n "${WSL_DISTRO_NAME:-}" ]; then
    # Inside WSL (a mirror's gate, scripts/gate-mirror.sh): natively, under the
    # toolchain the wsl.exe branch below runs, CI's.
    native_go_check sim || return 1
    ( export GOTOOLCHAIN="$GATE_GO"; eval "$(sim_cmd "$ROOT" "$plan")" >"$logdir/sim.log" 2>&1 ); rrc=$?
    how="-race, $GATE_GO"
  elif command -v go >/dev/null 2>&1 && [ "${CGO_ENABLED:-}" = "1" ] && command -v gcc >/dev/null 2>&1; then
    ( eval "$(sim_cmd "$ROOT" "$plan")" >"$logdir/sim.log" 2>&1 ); rrc=$?
    how="-race"
  elif command -v wsl.exe >/dev/null 2>&1; then
    # Windows: borrow WSL's cgo, exactly as step_race does, and translate the
    # path rather than assuming /mnt/c — a worktree is not always under it.
    local wslroot
    wslroot="$(wsl.exe -d Ubuntu -- wslpath -a "$(pwd -W 2>/dev/null || pwd)" 2>/dev/null | tr -d '\r\0')"
    if [ -z "$wslroot" ]; then
      echo "FAIL sim — could not translate $ROOT into a WSL path"
      return 1
    fi
    wsl_go_check sim || return 1
    ( wsl.exe -d Ubuntu -- bash -lc "export GOTOOLCHAIN=$GATE_GO; $(sim_cmd "$wslroot" "$plan")" >"$logdir/sim.log" 2>&1 ); rrc=$?
    how="-race, via WSL, $GATE_GO"
  else
    # NO SILENT DOWNGRADE. A plain `-tags sim` run compiles the sim files and
    # would have passed with the concurrent map write in place, so reporting it
    # as a green sim step is the failure this step exists to prevent. Say what
    # is missing instead.
    echo "FAIL sim — no cgo toolchain and no WSL to borrow one from"
    echo "  the detector is the point here; a plain -tags sim run passes with a race in place"
    return 1
  fi

  if [ "$rrc" -eq 0 ]; then
    echo "ok   sim ($how; $ntests sim-only tests in $npkgs packages)"
    return 0
  fi
  echo "FAIL sim"
  excerpt_failures "$logdir/sim.log"
  return 1
}

# step_simvet — the sim compile: `go vet -tags sim` over every package holding a
# file whose //go:build line names sim (either way: a `!sim` stub's package is
# vetted under the tag that replaces it). It is check-tagged-vet's sim half, run
# on its own so the bare gate, which leaves tagged-vet to `full`, still compiles
# every sim file and every sim test before a push. Vet's results are cached, so
# a warm run costs the packages that changed.
step_simvet() {
  local m pkgs failed=0
  for m in $MODULES; do
    pkgs="$( cd "$ROOT/$m" && grep -rlE --include='*.go' --exclude-dir=node_modules '^//go:build .*\bsim\b' . 2>/dev/null \
      | sed 's#/[^/]*$##' | LC_ALL=C sort -u )"
    [ -n "$pkgs" ] || continue
    # shellcheck disable=SC2086 # pkgs is a list of package directories
    ( cd "$ROOT/$m" && go vet -tags sim $pkgs ) || { echo "  FAIL $m — go vet -tags sim"; failed=1; }
  done
  [ "$failed" -eq 0 ] && echo "ok   sim compile (go vet -tags sim)" || echo "FAIL sim compile"
  return $failed
}

# ── Docker scope ─────────────────────────────────────────────────────
#
# The question is NOT "does a path filter match" -- that answers which
# workflow triggers, which is a different question and a narrower one. It is
# "can this diff reach a Postgres-backed test at all".
#
# NEEDED IS THE DEFAULT. A file is out of scope only if it is on the allow-list
# below, each entry with the reason no docker-tagged test can reach it. The
# rule used to run the other way: anything not named as NEEDED was skipped, on
# a comment that said no docker-tagged test reads a .css, .js or .svg. Docker
# tests in shingo-core read their own package's static/pages/*.js, templates,
# testdata goldens and ../kanban/testdata JSON (some through filepath.Join, so
# the comment's own grep could not see them), and a JS- or golden-only diff
# skipped the only tests that read the file. Every new kind of file is now
# NEEDED until someone argues it onto the list. scripts/check-docker-scope.sh
# pins the rule through `scope -`.
#
#   *_test.go                        NEEDED only in a module that carries
#                                    docker tests: a _test.go is compiled into
#                                    its own package's test binary alone.
#   *.go *.sql go.mod go.sum go.work NEEDED. Any Go change in any module can
#                                    be compiled into a docker test binary
#                                    (shared/ has none of its own; core links it).
#   allow-list (NOT NEEDED):
#     *.md, outside static/, templates/ and testdata/ dirs
#                                    no docker test reads one, and none sits in
#                                    an embedded directory
#     shingo-edge/www/static/*, shingo-edge/www/templates/*
#                                    embedded by shingoedge/www alone, which no
#                                    docker test binary links (integration
#                                    links shingocore/www and shared, not it),
#                                    and no docker test reads an edge path
#     docs/ .github/ deploy/, the root's launch and install scripts, Makefile,
#     Dockerfile.dev, docker-compose.dev.yml, .golangci.yml, LICENSE, .git*
#                                    outside every module; no test reads them
#   everything else                  NEEDED -- shingo-core's static, templates
#                                    and testdata, shared's embedded assets and
#                                    vectors, plants/*.yaml (the planner golden
#                                    and the Kanban rig seed them), scripts/.
step_scope() {
  local base="${1:-}"
  if [ -z "$base" ]; then
    base="$(git rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' 2>/dev/null)" || base=""
  fi
  [ -z "$base" ] && base="origin/main"
  # What the gate sentence names: the base as given, and the commit it was.
  scope_against="$base"
  [ "$base" = "-" ] || scope_against="$base @ $(git rev-parse --short "$base" 2>/dev/null || echo '?')"

  local files
  if [ "$base" = "-" ]; then
    # Classify an explicit file list on stdin instead of a git range, so the
    # rule can be exercised directly:
    #   printf 'a.css\nb/templates/c.html\n' | bash scripts/gate.sh scope -
    files="$(cat)"
  elif ! files="$(tree_diff "$base")"; then
    # No merge base (an unknown base, a shallow clone): nothing says the tree
    # is level, so docker runs. This read as an empty diff and skipped it.
    echo "scope: cannot diff against $scope_against (no merge base) — docker NEEDED"
    return 0
  fi
  if [ -z "$files" ]; then
    echo "scope: no diff against $scope_against — docker NOT needed"
    [ "$base" = "-" ] || echo '  (an empty diff is what a pushed or merged tip has: certify with `full --unscoped`)'
    return 1
  fi

  local dockermods m
  dockermods="$(docker_modules)"

  local reaching="" f
  while IFS= read -r f; do
    [ -z "$f" ] && continue
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
      *.go|*.sql|go.mod|go.sum|*/go.mod|*/go.sum|go.work|go.work.sum)
        reaching="$reaching$f"$'\n'; continue ;;
    esac
    # The allow-list (see the table above). Anything it does not name reaches.
    case "$f" in
      */static/*|*/templates/*|*/testdata/*) ;;
      *.md) continue ;;
    esac
    case "$f" in
      shingo-edge/www/static/*|shingo-edge/www/templates/*) continue ;;
      docs/*|.github/*|deploy/*) continue ;;
      install-*.sh|start-*.sh|Makefile|Dockerfile.dev|docker-compose.dev.yml) continue ;;
      .golangci.yml|LICENSE|.gitignore|.gitattributes) continue ;;
    esac
    reaching="$reaching$f"$'\n'
  done <<< "$files"
  reaching="$(printf '%s' "$reaching" | sed '/^$/d')"
  if [ -n "$reaching" ]; then
    echo "scope: docker NEEDED — $(echo "$reaching" | wc -l | tr -d ' ') of $(echo "$files" | wc -l | tr -d ' ') changed files can reach a Postgres-backed test:"
    # A list on stdin is being exercised, not gated: name every file that
    # reaches, so check-docker-scope.sh can classify its rows in one call.
    if [ "$base" = "-" ]; then
      echo "$reaching" | sed 's/^/    /'
    else
      echo "$reaching" | head -8 | sed 's/^/    /'
      [ "$(echo "$reaching" | wc -l)" -gt 8 ] && echo "    ..."
    fi
    return 0
  fi
  echo "scope: docker NOT needed — none of the $(echo "$files" | wc -l | tr -d ' ') changed files can reach a Postgres-backed test:"
  echo "$files" | head -8 | sed 's/^/    /'
  [ "$(echo "$files" | wc -l)" -gt 8 ] && echo "    ..."
  return 1
}

# No -race here. step_race runs the detector over core's engine and www with
# their docker tests, beside this step in `full`; the rest of docker+race is
# CI's (the docker-race shards).
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
# ── The shared Postgres ──────────────────────────────────────────────
#
# ONE SERVER FOR THE WHOLE DOCKER STEP, not one container per Go package.
#
# Every Go package is its own test process, and shingo-core/internal/testdb
# used to start a Postgres container per process — so per PACKAGE, and
# shingo-core has 31 packages carrying docker-tagged tests. MEASURED on the
# Windows dev host, mid-run:
#
#   container boot (create -> "ready to accept connections")   ~3.0s
#   migration stack replayed into the template                 ~2.4s
#   actual query work in a small package (store/admin)         ~0.2s
#
# store/admin's four tests each reported 5.49s against a 5.667s package total:
# they run concurrently and all of them were sitting on the container. Summed
# over 31 packages that is ~167s of a ~274s suite — 61% of the docker run spent
# booting Postgres and rebuilding the same schema — and `-p 1` makes every
# second of it additive.
#
# It compounded, too. testdb's reaper only collects a container five minutes
# past its creator's deadline (reaper.go, reapSlack), which is longer than the
# whole suite, so containers accumulate: 19 were alive at once, and packages
# late in the alphabet paid for the pile-up. shingocore/uop measured 36.5s
# inside the suite against ~4.5s run on its own.
#
# So: start one server here, hand its address to every package through
# $SHINGO_TEST_PG, and let the first process that needs a template build it
# under an advisory lock while the rest wait and reuse it. Per-package setup
# becomes one CREATE DATABASE ... TEMPLATE: a clone written through the WAL
# (Postgres 16's default STRATEGY WAL_LOG), not a file copy.
#
# THE TEMPLATE NAME IS SCOPED TO THIS RUN ($$). testdb defaults the name to the
# migration version, which is enough to stop a new migration reusing an old
# template — but a baseline-DDL edit that adds no migration would not change
# that name, and this container is thrown away at the end of the run anyway.
# Naming it per run means the full gate is never the thing that discovers a
# stale-template bug.
#
# TUNED FOR DATA NOBODY KEEPS. fsync/synchronous_commit/full_page_writes exist
# to survive a crash; this database does not survive the script. PGDATA on
# tmpfs for the same reason — via a SUBDIRECTORY of the mount, because the
# entrypoint creates that subdirectory with the 0700 postgres-owned permissions
# initdb insists on, while the mount point itself lands world-writable and
# initdb refuses it. BUT NOT FROM GIT BASH: MSYS rewrites the --tmpfs path
# into a Windows path, so on the laptop PGDATA is not on tmpfs (docket R16; the
# fix is held on wip/test-gate-time/pgtmp by the orc's R16 ruling, untested with
# cert's two servers). max_connections is raised because every package's pool
# lands on this one server now.
sharedPG="shingo-gate-docker-$$"
SHARED_PG_MARK="$ROOT/.gate/docker-pg.$$"

# pg_up starts one such server as container $1 and prints its address once it
# answers. NAMED, so gate_on_exit can take it down by name: the race step's
# server is started inside `full`'s side job, a subshell whose variables the
# parent never sees, and $$ is the gate's pid in both. PG_IMAGE is a floating
# tag: the gate run's identity (gate_identity, scripts/gate-timing.sh) records
# the image id it named, and pg_up's docker run line, at each run.
PG_IMAGE="postgres:16-alpine"
pg_up() {
  local name=$1 port deadline
  docker run -d --name "$name" \
      -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=postgres \
      -e PGDATA=/var/lib/postgresql/data/pgdata \
      --tmpfs /var/lib/postgresql/data:rw,size=4g \
      -p 127.0.0.1::5432 \
      "$PG_IMAGE" \
      -c fsync=off -c synchronous_commit=off -c full_page_writes=off \
      -c max_connections=500 -c shared_buffers=256MB >/dev/null 2>&1 || return 1

  deadline=$(( $(date +%s) + 60 ))
  until docker exec "$name" pg_isready -U test -q 2>/dev/null; do
    [ "$(date +%s)" -gt "$deadline" ] && return 1
    sleep 0.5
  done

  port="$(docker port "$name" 5432/tcp 2>/dev/null | head -1 | sed 's/.*://')"
  [ -z "$port" ] && return 1
  echo "127.0.0.1:$port"
}

start_shared_pg() {
  local addr
  # Marked before the start, so an interrupted run's gate_on_exit (the script's
  # one EXIT trap, which also releases the gate lock — a second `trap ... EXIT`
  # here would replace that one and leave the lock behind) removes it. A file,
  # not a variable: in `full` this step runs in a background job, whose
  # variables the parent's exit trap never sees.
  sharedPG="shingo-gate-docker-$$"
  : >"$SHARED_PG_MARK"
  addr="$(pg_up "$sharedPG")" || return 1
  export SHINGO_TEST_PG="$addr"
  export SHINGO_TEST_PG_TEMPLATE="template_gate_$$"
  return 0
}

stop_shared_pg() {
  [ -e "$SHARED_PG_MARK" ] || return 0
  docker rm -f "$sharedPG" >/dev/null 2>&1
  rm -f "$SHARED_PG_MARK"
}

# The race step's own server (step_race): not the docker step's, because the
# two run side by side in `full` and the docker step stops its server when it
# is done.
RACE_PG="shingo-gate-race-$$"
# The marker file says the server may exist: the exit trap asks docker only
# when a race step started one, not on every run.
RACE_PG_MARK="$ROOT/.gate/race-pg.$$"
stop_race_pg() {
  [ -e "$RACE_PG_MARK" ] || return 0
  docker rm -f "$RACE_PG" >/dev/null 2>&1
  rm -f "$RACE_PG_MARK"
}

# docker_p answers how many test binaries to run at once.
#
# It used to be a hardcoded `-p 1`, to stop packages fighting over containers.
# One shared server removes that reason, but the obvious follow-up — turn the
# packages loose — is WRONG, and measured wrong rather than argued wrong. On
# the 20-core host the gate ran on then, shingo-core's docker suite from a
# fresh container (today's 24-thread laptop re-ran part of it on 2026-11-22:
# the docker step at -p 4 216-237 s against 259.7 s at -p 2, no wall
# difference by the timing rule; work/loc-census-2026-11-20 raw/base/2.7, and
# GATE_DOCKER_P exists for the next series):
#
#   -p 1                183s
#   -p 2                151s
#   -p 4                116s   <- best
#   -p 8                116s, and every package individually 4-6x slower
#                              (shingocore/engine 16.3s -> 95.3s)
#   -p 4 -parallel 5    204s
#   -p 6 -parallel 4    277s
#
# The reason -p 8 buys nothing is that THE BOX IS ALREADY BUSY AT -p 1: tests
# inside a package run up to GOMAXPROCS-parallel, so eight packages at once is
# ~160 concurrent tests on 20 cores and they thrash. The reason capping
# -parallel is worse still is the mirror image — the big packages (engine,
# dispatch, www) get their speed FROM in-package parallelism, and throttling it
# to hand cores to other packages trades a large win for a small one.
#
# So: oversubscribe by about 4x and no more, which is what nproc/4 is. Tests
# here are mostly waiting on Postgres round-trips rather than burning CPU, which
# is why some oversubscription wins at all. Derived rather than hardcoded
# because 4 is this host's answer, not everyone's — on an 8-core laptop the same
# rule gives 2, and on CI's 2-core runners it gives 1, which is where a
# hardcoded 4 would thrash hardest.
# AD-HOC `go test -tags=docker ./...` IS NOT THIS, AND WILL LIE TO YOU.
#
# Run by hand it uses the DEFAULT -p, which is GOMAXPROCS — 20 on the dev host
# against the 4 below. With $SHINGO_TEST_PG set, that is twenty packages' worth
# of connection pools, each running up to twenty parallel tests, all on one
# server. OBSERVED: three tests failing at 48-74s that pass in isolation at
# 25s. Nothing was wrong with them; they are timing-sensitive and lost their
# margin under load, the same way TestPollerStopHaltsPolling did.
#
# So: the gate is the runner to trust. A hand-run of one package is fine and
# fast — it is `./...` at default parallelism that manufactures failures. If
# you want the whole suite by hand, pass the same -p this computes.
# docker_packages <module...> — how many packages carry a go:build docker
# test, i.e. how many would each start their own container without the shared
# server. Counted from the tree, for the fallback warning's arithmetic.
docker_packages() {
  local m
  for m in "$@"; do
    grep -rlE '^//go:build .*\bdocker\b' --include='*_test.go' "$ROOT/$m" 2>/dev/null
  done | sed 's#/[^/]*$##' | sort -u | wc -l | tr -d ' '
}

#
# GATE_DOCKER_P, when set, is the -p instead (checked at startup: a positive
# integer or a usage error). It exists for the -p series a timing study runs,
# and the gate sentence names it, so a run at a hand-set -p is never read as a
# default one. scripts/check-docker-order.sh pins it.
docker_p() {
  if [ -n "${GATE_DOCKER_P:-}" ]; then echo "$GATE_DOCKER_P"; return; fi
  local cores
  cores="$(nproc 2>/dev/null || echo "${NUMBER_OF_PROCESSORS:-4}")"
  case "$cores" in *[!0-9]*|"") cores=4 ;; esac
  local p=$(( cores / 4 ))
  [ "$p" -lt 1 ] && p=1
  [ "$p" -gt 4 ] && p=4
  echo "$p"
}

# docker_order <module> — the packages to hand `go test` for one module's docker
# run: longest first, by each package's MEDIAN seconds over the last 5 clean runs
# the steps log holds (`docker:<importpath>` lines of a run that also logged
# `total`, not `total-FAILED`), then every other package in `go list` order.
# LONGEST FIRST because `go test -p` starts binaries in the order it is given
# them, and in-gate the module ended on whichever slow package happened to start
# late. A MEDIAN OF CLEAN RUNS because the last line alone made the order the
# noise of one run: a package that ran short beside a quiet machine, or ended a
# failed run early, dropped to the back, and the next run's wall was not
# comparable with the last. A package the log names that the tree no longer has
# is dropped. `./...` when the log has no clean line for this module (a first
# run, a fresh clone, GATE_STEPS_LOG off): the order is a speedup, never the list
# of what runs. Pinned by scripts/check-docker-order.sh.
#
# GATE_DOCKER_ORDER_LOG names a steps log to rank from instead (a snapshot), so a
# timing before/after runs both sides in one order however the live log moves.
docker_order() {
  local m="$1" mod timed all src="${GATE_DOCKER_ORDER_LOG:-$STEPS_LOG}"
  { [ -n "$src" ] && [ -s "$src" ]; } || { echo "./..."; return; }
  mod="$(sed -n 's/^module[[:space:]]*//p' "$ROOT/$m/go.mod" | tr -d '\r')"
  timed="$(awk -v pre="docker:$mod" '
      NR == FNR { if ($3 == "total") clean[$5] = 1; next }
      ($3 == pre || index($3, pre "/") == 1) && $4 ~ /^[0-9.]+$/ && ($5 in clean) {
        p = $3; sub(/^docker:/, "", p); n[p]++; v[p, n[p]] = $4 }
      END {
        for (p in n) {
          k = 0
          for (i = (n[p] > 5 ? n[p] - 4 : 1); i <= n[p]; i++) s[++k] = v[p, i] + 0
          for (i = 2; i <= k; i++) { x = s[i]; j = i - 1; while (j >= 1 && s[j] > x) { s[j + 1] = s[j]; j-- } s[j + 1] = x }
          print (k % 2 ? s[(k + 1) / 2] : (s[k / 2] + s[k / 2 + 1]) / 2), p
        }
      }' "$src" "$src" | LC_ALL=C sort -k1,1gr | awk '{ print $2 }')"
  [ -n "$timed" ] || { echo "./..."; return; }
  all="$(cd "$ROOT/$m" && go list -tags docker,lockcheck ./... 2>/dev/null)" || { echo "./..."; return; }
  [ -n "$all" ] || { echo "./..."; return; }
  awk 'NR == FNR { have[$0] = 1; next } NF && have[$0] && !seen[$0]++' \
    <(printf '%s\n' "$all") <(printf '%s\n%s\n' "$timed" "$all") | tr '\n' ' ' | sed 's/ $//'
}

step_docker() {
  local m failed=0 mods logdir p pids=""
  mods="$(docker_modules)"
  p="$(docker_p)"
  if [ -z "$mods" ]; then
    echo "ok   docker (no module carries a go:build docker test)"
    return 0
  fi
  logdir="$ROOT/.gate"
  mkdir -p "$logdir" || { echo "FAIL docker — cannot create $logdir"; return 1; }

  # A shared server is a SPEEDUP, NOT A GATE. If it cannot be had — no image
  # pulled yet, a daemon that just came up, a host with the port range locked
  # down — say so once and let every package fall back to making its own
  # container, which is what they did before this existed. The one thing not to
  # do is fail the gate: the verdict this script returns is about the code.
  if start_shared_pg; then
    echo "  docker: shared postgres at $SHINGO_TEST_PG (template $SHINGO_TEST_PG_TEMPLATE, -p $p)"
  else
    stop_shared_pg
    unset SHINGO_TEST_PG SHINGO_TEST_PG_TEMPLATE
    # The cost is PRINTED AS ITS ARITHMETIC, not asserted as a fixed phrase:
    # how many packages will each start a container is a count of this tree,
    # and a reader deciding whether to wait or fix docker needs the total.
    local npk per=5
    npk="$(docker_packages $mods)"
    echo "  docker: WARNING — could not start a shared postgres;" >&2
    echo "          each package will start its own: $npk packages x ~${per}s = ~$((npk * per))s added." >&2
  fi

  # ── THE SKIP-TOGETHER TRAP (fix-batch 2a) ─────────────────────────────
  #
  # testdb.Skipf's when the container fails with a docker error, and `go test`
  # exits 0 when every test skips: a docker step that runs green while the
  # daemon is down is a lying green, and the exit code cannot tell them apart.
  # SHINGO_TEST_REQUIRE_DOCKER turns the skip into a failure inside every
  # package, AND the sentinel line the packages emit is checked here against
  # the logs the run itself produced — two spellings of the same answer, so
  # neither can go quietly blind.
  #
  # The untagged step_test keeps the old behavior deliberately: a developer
  # without Docker still gets a useful unit run from `bash scripts/gate.sh`.
  # Requiring docker there would turn every laptop-unit-run into a wall of
  # failures and teach people to ignore the gate.
  export SHINGO_TEST_REQUIRE_DOCKER=1
  # THE MODULES RUN AT ONCE, NOT ONE AFTER ANOTHER. integration is one package,
  # integration/scenarios, which no -p can split and which is most of the
  # step's length on its own; in series shingo-core's suite waited out all of
  # it. Each module keeps its own -p, its own log and its own exit code, and
  # the excerpts are printed afterwards in module order, so a red run reads as
  # it always did.
  local order
  for m in $mods; do
    order="$(docker_order "$m")"
    echo "  docker: $m ($( [ "$order" = "./..." ] && echo "./..." || echo "longest first, from the steps log"))"
    # The -p this module runs at, for the steps log's selection line (gate-timing.sh).
    [ -n "${GATE_SEL_DIR:-}" ] && echo "$m:$p" >>"$GATE_SEL_DIR/docker.p"
    rm -f "$logdir/docker-$m.rc"
    # shellcheck disable=SC2086 # order is a package list
    ( cd "$ROOT/$m" && go test -tags=docker,lockcheck -timeout=20m -count=1 -p "$p" "${GATE_EXEC_ARGS[@]}" $order >"$logdir/docker-$m.log" 2>&1
      echo $? >"$logdir/docker-$m.rc" ) &
    pids="$pids $!"
  done
  # These jobs by pid: a bare wait would also wait for the race-and-sim job
  # `full` runs beside this step.
  wait $pids
  for m in $mods; do
    [ "$(cat "$logdir/docker-$m.rc" 2>/dev/null)" = 0 ] \
      || { failed=1
           echo "  --- $m ---"
           excerpt_failures "$logdir/docker-$m.log"; }
    # `ok  pkg  1.2s` only prints when that package's binary ran and passed, and
    # a package whose tests all skipped still prints ok, so the exit code cannot
    # tell a skipped run from a green one: the check below is on the sentinel
    # being ABSENT. (This comment named a "Sunday smoke" that asserts on the
    # ok-count; no such workflow or script exists: docket L8.)
    if grep -q '^SHINGO-DOCKER-DOWN' "$logdir/docker-$m.log"; then
      echo "  FAIL docker ($m): the run reported docker down —" \
           "the exit code was 0 and the tests skipped. Not a green gate." >&2
      failed=1
    fi
  done
  # Explicit teardown as well as the trap: the trap covers the interrupted run,
  # this covers the normal one, and it takes the server down before the verdict
  # is printed rather than after the script has already returned.
  stop_shared_pg
  if [ "$failed" -ne 0 ]; then
    # A3: classify the failures before the verdict — a connect-shaped wall is
    # a wedged Postgres, not a broken diff. See harness_verdict above.
    for m in $mods; do
      if grep -qE '^(FAIL|--- FAIL)' "$logdir/docker-$m.log" 2>/dev/null; then
        echo "  $m:"
        harness_verdict "$logdir/docker-$m.log"
      fi
    done
  fi
  [ "$failed" -eq 0 ] && echo "ok   docker" || echo "FAIL docker"
  return $failed
}

# ── Race ─────────────────────────────────────────────────────────────
#
# SCOPED, NOT MODULE-WIDE, AND THAT IS WHY IT CAN EXIST AT ALL.
#
# The header above says module-wide race runs cost 25-35 minutes, which is why
# this script had no race mode and why the local gate is "compiles and behaves"
# while CI is "and it is race-free". That reasoning is sound and it left a real
# hole: a data race on Engine.lastSceneSync shipped on this branch behind a
# comment asserting a serialisation the call graph did not provide, and NO MODE
# OF THIS SCRIPT COULD HAVE FOUND IT.
#
# So the answer is not "add -race to step_test" — it is to run the detector over
# the two packages where this codebase actually spawns goroutines against shared
# engine state:
#
#   engine/  the connection loop, the reconnect goroutine, the refresh loops
#   www/     HTTP handlers, which are one goroutine per request by construction
#
# Those two are where a concurrent caller arrives without anyone writing a
# goroutine, which is the shape that gets missed. Everything else in the tree is
# CI's job.
#
# CGO IS REQUIRED AND WINDOWS DOES NOT HAVE IT. `go test -race` needs a C
# toolchain; the dev host has none, so this step re-enters through WSL and pays
# the 9p bridge crossing the header measures at 10-20x. That is affordable
# because the scope is two packages and unaffordable module-wide, which is the
# same trade the header already makes.
#
# WITH THE DOCKER TESTS, BECAUSE CI RUNS THEM SO. This step used to be untagged
# and the docker step runs without -race, so the combination CI's docker-race
# shards run existed nowhere here. A docker test that read its own handler's
# second write before it landed (TestStepsDone_TheSubscriberCountsThePlanSteps-
# TheFleetFinished, main 9c86018f, 2026-10-30) passed every local gate and
# failed CI's. So the step runs -tags docker,lockcheck against its own
# throwaway Postgres (RACE_PG), and a run whose tests skipped for want of
# docker is a failure, as in step_docker.
RACE_PKGS="./engine/... ./www/..."

step_race() {
  local rrc addr pgenv="" inner racego=""
  touch "$RACE_PG_MARK" 2>/dev/null
  if addr="$(pg_up "$RACE_PG")"; then
    pgenv="SHINGO_TEST_PG=$addr SHINGO_TEST_PG_TEMPLATE=template_gate_race_$$"
  else
    # As in step_docker: a speedup, not a gate. testdb starts a container per
    # package instead, and the sentinel check below still refuses a skip.
    stop_race_pg
    echo "  race: WARNING — could not start its postgres; each docker package starts its own" >&2
  fi
  inner="$pgenv SHINGO_TEST_REQUIRE_DOCKER=1 CGO_ENABLED=1 go test -race -tags docker,lockcheck -count=1 -timeout=20m $RACE_PKGS"
  local rcmd="cd \"$ROOT/shingo-core\" && $inner"

  if [ -n "${WSL_DISTRO_NAME:-}" ]; then
    # Already inside WSL (a mirror's gate, scripts/gate-mirror.sh): run
    # directly, under the toolchain the wsl.exe branch below runs, CI's.
    native_go_check race || { stop_race_pg; return 1; }
    racego=", $GATE_GO"
    ( export GOTOOLCHAIN="$GATE_GO"; eval "$rcmd" >"$ROOT/.gate/race.log" 2>&1 ); rrc=$?
  elif command -v go >/dev/null 2>&1 && [ "${CGO_ENABLED:-}" = "1" ] && command -v gcc >/dev/null 2>&1; then
    # A host that genuinely has a C toolchain (a Linux or macOS dev box).
    ( eval "$rcmd" >"$ROOT/.gate/race.log" 2>&1 ); rrc=$?
  elif command -v wsl.exe >/dev/null 2>&1; then
    # Windows: re-enter through WSL, which is the only place cgo exists here.
    # The path is translated rather than assumed — a worktree is not always
    # under /mnt/c.
    local wslroot
    wslroot="$(wsl.exe -d Ubuntu -- wslpath -a "$(pwd -W 2>/dev/null || pwd)" 2>/dev/null | tr -d '\r\0')"
    if [ -z "$wslroot" ]; then
      echo "FAIL race — could not translate $ROOT into a WSL path"
      return 1
    fi
    wsl_go_check race || { stop_race_pg; return 1; }
    racego=", via WSL, $GATE_GO"
    ( wsl.exe -d Ubuntu -- bash -lc "export GOTOOLCHAIN=$GATE_GO; cd '$wslroot/shingo-core' && $inner" \
        >"$ROOT/.gate/race.log" 2>&1 ); rrc=$?
  else
    stop_race_pg
    echo "FAIL race — no cgo toolchain and no WSL to borrow one from"
    echo "  install WSL, or run on a host with gcc: go test -race -tags docker $RACE_PKGS"
    return 1
  fi
  stop_race_pg

  if grep -q '^SHINGO-DOCKER-DOWN' "$ROOT/.gate/race.log"; then
    echo "FAIL race — the run reported docker down: the docker tests skipped. Not a green gate." >&2
    return 1
  fi
  if [ "$rrc" -eq 0 ]; then
    echo "ok   race (-tags docker, $RACE_PKGS$racego)"
    return 0
  fi
  echo "FAIL race"
  # A race fails its test with "race detected during execution of test", so
  # the leaf block names the test and the DATA RACE line says why; the stacks
  # are in the log the excerpt ends by naming.
  excerpt_failures "$ROOT/.gate/race.log"
  return 1
}

mkdir -p "$ROOT/.gate" 2>/dev/null || true

# ── The machine gate lock ─────────────────────────────────────────────────
# One trap for everything the script must undo on the way out, however it
# leaves: the two throwaway Postgres servers (step_docker's, step_race's) and
# the gate lock. INT and TERM exit explicitly so the EXIT trap runs for them
# too.
. "$ROOT/scripts/gate-lock.sh"
# golangci_run: golangci-lint with a bounded retry on its lock handoff (step_lint).
. "$ROOT/scripts/golangci-run.sh"
# gate_timing_check / gate_timing_status: the timing flag a `gatetime run`
# holds (shared/scripts/gatetime), which every heavy mode below honours.
. "$ROOT/scripts/gate-timing.sh"
# reuse_setup / reuse_summary / reuse_audit: test-result reuse, OFF unless
# GATE_REUSE=on (dev and push serve, cert audits; shared/scripts/gatetime/reuse).
. "$ROOT/scripts/gate-reuse.sh"
# mirror_prelock / mirror_route / mirror_on_exit: which side a heavy mode runs
# on, and the mirror on WSL's ext4 that the Linux side runs it in.
. "$ROOT/scripts/gate-mirror.sh"
# FAN_PIDS are the steps `all` and `full` run at once (fan): an interrupted
# gate takes them down too, rather than leaving them running against a tree
# nobody is gating.
FAN_PIDS=""
gate_on_exit() {
  # shellcheck disable=SC2086 # pids
  [ -n "${FAN_PIDS:-}" ] && kill $FAN_PIDS 2>/dev/null
  # The mirror's gate before the lock: it runs under this run's lock.
  stop_shared_pg; stop_race_pg; mirror_on_exit; gate_lock_release
}
trap gate_on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

case "${1:-all}" in
  # fmt: the pre-push hook runs it and must never wait. scope: reads git only. order:
  # reads the steps log and go list only. cachescope: reads stdin. identity:
  # prints what gate_identity would record for a run (scripts/gate-timing.sh).
  fmt|scope|order|cachescope) ;;
  identity) gate_identity; exit 0 ;;
  lock-status) gate_lock_status; gate_timing_status; exit 0 ;;
  vet|modbuild|lint|scripts|test|race|sim|simvet|docker|full|all)
    mirror_prelock || exit 1
    # The timing flag first, GATE_NO_LOCK=1 or not: a gate beside a timed run
    # is load in the timing (scripts/gate-timing.sh).
    gate_timing_check || { echo "gate: FAILED (a timed run holds the timing flag)"; exit 1; }
    gate_lock_acquire "${*:-all}" || { echo "gate: FAILED (gate lock not acquired)"; exit 1; }
    # Under the lock: from Windows, sync the mirror and run this gate there
    # (its exit code is this one's); in a mirror, check its tree is the one
    # the caller named before any step runs (scripts/gate-mirror.sh).
    mirror_route "$@"
    [ -n "$MIRROR_RC" ] && exit "$MIRROR_RC" ;;
esac

# ── THE GATE SENTENCE ─────────────────────────────────────────────────────
#
# Every run ends by printing the exact sentence to paste into the commit
# message, naming ONLY the steps that ran and passed in that invocation.
#
# It exists because the sentence used to be hand-typed, and a hand-typed claim
# about what a tool did is a second spelling of a fact the tool already holds.
# Ten commits on this branch claimed "fmt, vet, lint, tests, docker suites" when
# the docker suites had not run: `gate.sh` with no argument is the four, and
# `gate.sh full` SKIPS docker whenever scope finds no diff — which is always true
# for work already pushed. CI found the break the docker suites would have caught
# (a test asserting a contract two commits had already changed). So the sentence
# also names what decided docker: "(unscoped)" for `full --unscoped`, "(scoped
# against BASE @ sha)" otherwise, and the base again when scope skipped them.
#
# So the sentence is fact-carried, per standing law 4: accumulated by the steps
# themselves as they pass, never assembled from what the invocation was supposed
# to do. A "docker suites" that appears in a commit message and not in this
# output is now a visible contradiction rather than an invisible one.
gate_steps=""
gate_docker=""   # "" = not attempted | "ran" | "skipped"
scope_against=""  # set by step_scope: "BASE @ sha"
gate_unscoped=""  # 1 = `full --unscoped`

# ── The steps log ────────────────────────────────────────────────────────
#
# Every timed step appends `sha mode step seconds run` to .gate/steps.log, and
# each step that runs tests adds a `step:pkg` line per package, read from go
# test's own `ok pkg Ns` lines in the logs that step wrote — the format
# shingo-core/scripts/shardplan's rePkgTime parses. lint and modbuild add a
# `step:module` line each. `run` is the gate's start (UTC) and groups one
# invocation's lines; a sha ending `+` had uncommitted changes. A heavy run
# also writes one `selection` line, and its `total` line carries load stamps
# after the five fields (scripts/gate-timing.sh). Its readers: docker_order
# (the docker step's package order, fields 1-5), `gatetime compare` (the
# selection lines and stamps of timed runs), and people. In-gate package
# times overlap one another, so they attribute nothing on their own: a
# package's cost is measured solo, and a step's wall judges the schedule.
GATE_MODE="${1:-all}"
[ "$GATE_MODE" = full ] && [ "${2:-}" = "--unscoped" ] && GATE_MODE="full-unscoped"
# The tier (see "The tiers and the diff"): what the bare gate, `full` and
# `full --unscoped` are for, which the steps read to decide what they skip.
case "$GATE_MODE" in
  all) GATE_TIER=dev ;;
  full) GATE_TIER=push ;;
  full-unscoped) GATE_TIER=cert ;;
  *) GATE_TIER=step ;;
esac
export GATE_TIER
GATE_RUN="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
GATE_SHA="$(git rev-parse --short HEAD 2>/dev/null || echo nogit)"
git diff --quiet HEAD 2>/dev/null || GATE_SHA="$GATE_SHA+"
# GATE_STEPS_LOG names another file; set and empty, nothing is logged (the
# gate-lock pin runs nested gates whose times are not a schedule).
STEPS_LOG="${GATE_STEPS_LOG-$ROOT/.gate/steps.log}"
GATE_T0="$EPOCHREALTIME"
# The timing tool, built once for the run, and the record-only -exec wrapper
# for the modes whose Go tests run natively (scripts/gate-timing.sh).
case "${1:-all}" in
  vet|modbuild|lint|scripts|test|race|sim|simvet|docker|full|all)
    gatetime_prepare "${1:-all}"
    gatetime_exec_prepare "${1:-all}" || exit 1
    # Reuse rides on that wrapper and its identity; off unless GATE_REUSE=on.
    reuse_setup ;;
esac

# _secs <from> — seconds since an $EPOCHREALTIME reading, to 0.1s.
_secs() {
  awk -v a="$1" -v b="$EPOCHREALTIME" 'BEGIN { printf "%.1f", b - a }'
}

# steps_log <step> <seconds> [words] — one line, appended; words (key=value)
# follow the five fields on the selection and total lines.
steps_log() {
  [ -n "$STEPS_LOG" ] || return 0
  printf '%s %s %s %s %s%s\n' "$GATE_SHA" "$GATE_MODE" "$1" "$2" "$GATE_RUN" "${3:+ $3}" >>"$STEPS_LOG"
}

# The logs each test-running step writes, under .gate/.
step_logs() {
  case "$1" in
    step_test) echo "test-*.log" ;;
    step_docker) echo "docker-*.log" ;;
    step_race) echo "race.log" ;;
    step_sim) echo "sim.log" ;;
  esac
}

# timed <step_fn> — runs one step, prints how long it took, and logs it with
# its packages. Only logs the step itself rewrote are read (newer than a mark
# made as it started): a module's log from an earlier, wider run can still be
# lying in .gate/.
timed() {
  local t0="$EPOCHREALTIME" r mark pat f name="${1#step_}" s
  mark="$ROOT/.gate/.timed.$BASHPID.$1"
  : >"$mark"
  "$@"; r=$?
  s="$(_secs "$t0")"
  echo "     ($1: ${s}s)"
  steps_log "$name" "$s"
  pat="$(step_logs "$1")"
  if [ -n "$pat" ] && [ -n "$STEPS_LOG" ]; then
    for f in "$ROOT"/.gate/$pat; do
      { [ -e "$f" ] && [ "$f" -nt "$mark" ]; } || continue
      # A package the bare gate took from Go's test cache is logged as `cached`,
      # so the log does not read a reused package as one that was not there.
      awk -v sha="$GATE_SHA" -v mode="$GATE_MODE" -v step="$name" -v run="$GATE_RUN" '
        /^(ok|FAIL)[ \t]+[^ \t]+[ \t]+[0-9.]+s/ {
          t = $3; sub(/s$/, "", t); print sha, mode, step ":" $2, t, run; next }
        /^ok[ \t]+[^ \t]+[ \t]+\(cached\)/ { print sha, mode, step ":" $2, "cached", run }' "$f" >>"$STEPS_LOG"
    done
  fi
  rm -f "$mark"
  return $r
}
note_step() { gate_steps="${gate_steps:+$gate_steps, }$1"; }

# ── Fan-out ──────────────────────────────────────────────────────────────
#
# fan <label> <cmd...> runs one step as a background job whose output goes to
# .gate/fan-<label>.out and whose exit code to .gate/fan-<label>.rc; fan_join
# waits for those jobs by pid (so it waits for these steps and nothing else the
# gate started; the lock's heartbeat is no job of it: check-gate-lock.sh case
# 1b); fan_print prints a step's held output, so the log reads in the
# order the steps always printed whatever order they finished in; fan_ok is the
# step's verdict. A missing rc reads as a failure, so fan_reset clears last
# run's first.
fan_reset() {
  local l
  for l in "$@"; do rm -f "$ROOT/.gate/fan-$l.rc" "$ROOT/.gate/fan-$l.out"; done
}
fan() {
  local label="$1"
  shift
  ( "$@" >"$ROOT/.gate/fan-$label.out" 2>&1; echo $? >"$ROOT/.gate/fan-$label.rc" ) &
  FAN_PIDS="$FAN_PIDS $!"
}
fan_join() {
  # shellcheck disable=SC2086 # pids
  [ -n "$FAN_PIDS" ] && wait $FAN_PIDS
  FAN_PIDS=""
}
fan_print() { [ -e "$ROOT/.gate/fan-$1.out" ] && cat "$ROOT/.gate/fan-$1.out"; return 0; }
fan_ok() { [ "$(cat "$ROOT/.gate/fan-$1.rc" 2>/dev/null)" = 0 ]; }

# cert_side — the cert tier's race then sim, one after the other beside the
# docker step as `full` always ran them, each with its own verdict file.
cert_side() {
  timed step_race; echo $? >"$ROOT/.gate/fan-race.rc"
  timed step_sim;  echo $? >"$ROOT/.gate/fan-sim.rc"
}

gate_sentence() {
  local s="$gate_steps"
  case "$gate_docker" in
    # "AND" only joins something to something. `gate.sh docker` runs the suites
    # alone, and the first version of this printed a dangling "Gate: AND the
    # docker suites." — caught by the mutation this rider was specified with.
    ran)     if [ -n "$s" ]; then s="$s AND the docker suites"; else s="the docker suites"; fi
             # `full` says what put them in scope; `gate.sh docker` runs them
             # by name and needs no qualifier.
             if [ -n "$gate_unscoped" ]; then s="$s (unscoped)"
             elif [ -n "$scope_against" ]; then s="$s (scoped against $scope_against)"; fi
             [ -n "${GATE_DOCKER_P:-}" ] && s="$s at -p $GATE_DOCKER_P (GATE_DOCKER_P)" ;;
    skipped) : ;;
  esac
  [ -n "$s" ] || { echo "Gate sentence: (nothing ran)"; return; }
  # A scoped run says so in the sentence itself, so "Gate: lint." can only
  # ever mean all five modules.
  [ -n "$gate_scope" ] && s="$s (scoped to: $gate_scope)"
  # Capitalised the way it lands in a commit message, ready to paste. THE TIER
  # IS IN THE SENTENCE: a dev or push pass reads "Gate (dev)" / "Gate (push)",
  # names what it skipped or took from the cache, and so cannot be pasted as a
  # certification, which is the only run that reads plain "Gate:".
  local label="Gate"
  case "${GATE_TIER:-}" in
    dev) label="Gate (dev)" ;;
    push) label="Gate (push)" ;;
  esac
  local out="$label: $s."
  [ "$gate_docker" = skipped ] && out="$out Docker suites SKIPPED by scope against $scope_against (nothing in this diff can reach one)."
  [ "${GATE_TIER:-}" = push ] && out="$out race: CI (the docker-race shards and the sim job)."
  [ "${GATE_TIER:-}" = dev ] && out="$out Not run here: the docker suites (compiled by lint), the sim-only tests (compiled), race — \`full\` and CI run them."
  out="$out$(gate_exec_sentence)"
  local r
  r="$(reuse_summary)"
  [ -n "$r" ] && out="$out ${r^}."
  # Which side ran it, why, and the tree it gated (scripts/gate-mirror.sh): set
  # only where there are two sides (from Windows, and in a mirror).
  [ -n "$gate_side_note" ] && out="$out Ran $gate_side_note."
  echo "$out"
}

case "${1:-all}" in
  fmt)   if step_fmt;  then note_step fmt;  else rc=1; fi ;;
  vet)   if timed step_vet;  then note_step vet;  else rc=1; fi ;;
  modbuild) if timed step_modbuild; then note_step modbuild; else rc=1; fi ;;
  lint)  if timed step_lint; then note_step lint; else rc=1; fi ;;
  scripts) if timed step_scripts; then note_step scripts; else rc=1; fi ;;
  test)  if timed step_test; then note_step unit; else rc=1; fi ;;
  race)  if timed step_race; then note_step race; else rc=1; fi ;;
  sim)   if timed step_sim;  then note_step sim;  else rc=1; fi ;;
  simvet) if timed step_simvet; then note_step "sim compile"; else rc=1; fi ;;
  # EXIT CODE IS THE VERDICT: 0 = docker needed, 1 = not needed. It used to
  # `exit 0` unconditionally, which made the answer readable only by a human
  # reading the text — so nothing could gate on it. `if bash scripts/gate.sh
  # scope; then ...` now works, and matches step_scope's own return
  # convention, which `full` has always branched on.
  scope) step_scope "${2:-}"; exit $? ;;
  cachescope)
    # Which modules the dev tier would run -count=1 for a diff given as paths on
    # stdin (module_uncached), one line per module: `gate.sh cachescope -`.
    [ "${2:-}" = - ] || { echo "usage: bash scripts/gate.sh cachescope -   (paths on stdin)" >&2; exit 2; }
    DIFF_FILE="$ROOT/.gate/cachescope.$$"
    cat >"$DIFF_FILE"
    for m in $MODULES; do why="$(module_uncached "$m")"; echo "$m: ${why:-cached}"; done
    rm -f "$DIFF_FILE"
    exit 0 ;;
  order)
    # The order each docker module's packages would be handed to go test
    # (docker_order), one line per module; reads only the steps log.
    for m in $(docker_modules); do echo "$m: $(docker_order "$m")"; done
    echo "docker -p: $(docker_p)${GATE_DOCKER_P:+ (GATE_DOCKER_P)}"
    exit 0 ;;
  docker) if timed step_docker; then gate_docker=ran; else rc=1; fi ;;
  full)
    if timed step_fmt;  then note_step fmt;  else rc=1; fi
    # Scope is decided BEFORE the steps start, because it decides what the unit
    # step has to cover: when the docker step runs it already runs every untagged
    # test in the modules that carry docker tests, so only the modules it does
    # not reach need their own run (untagged_only_modules). When docker is out
    # of scope nothing else covers them and every module runs.
    #
    # --unscoped puts docker in scope whatever the diff says. A tip that is
    # level with its base (pushed, or merged) has an empty diff, and a scoped
    # run then skips the docker suites; certification is that tip, so it
    # certifies unscoped, and the sentence says which ran.
    full_base="${2:-}"
    if [ "$full_base" = "--unscoped" ]; then
      gate_unscoped=1
      full_base=""
    fi
    docker_in=0
    if [ -n "$gate_unscoped" ]; then
      echo "scope: --unscoped — the docker suites run whatever the diff"
      docker_in=1
    elif timed step_scope "$full_base"; then
      docker_in=1
    fi
    write_diff
    # EVERY STEP AT ONCE AFTER fmt, printed afterwards in the order the steps
    # have always printed (fan, fan_print). The push tier (`full`) leaves -race to CI
    # (the docker-race shards race core and edge on every PR, and the sim job
    # races the sim tests) and runs the sim-only tests natively; the cert tier
    # (`full --unscoped`) runs race then sim under the detector, as before.
    fan_reset modbuild lint scripts simvet unit docker race sim side
    fan modbuild timed step_modbuild
    fan lint timed step_lint
    fan scripts timed step_scripts
    [ "$GATE_TIER" = push ] && fan simvet timed step_simvet
    if [ "$docker_in" -eq 1 ]; then
      fan unit timed step_test "$(untagged_only_modules)"
      fan docker timed step_docker
    else
      fan unit timed step_test
    fi
    if [ "$GATE_TIER" = cert ]; then
      fan side cert_side
    else
      fan sim timed step_sim plain
    fi
    fan_join
    for l in modbuild lint scripts simvet unit docker side sim; do fan_print "$l"; done
    if fan_ok modbuild; then note_step modbuild; else rc=1; fi
    if fan_ok lint; then note_step lint; else rc=1; fi
    if fan_ok scripts; then
      if [ "$GATE_TIER" = push ]; then note_step "scripts ($(cat "$ROOT/.gate/scripts.summary" 2>/dev/null))"
      else note_step scripts; fi
    else rc=1; fi
    if [ "$GATE_TIER" = push ]; then
      if fan_ok simvet; then note_step "sim compile"; else rc=1; fi
    fi
    if fan_ok unit; then note_step unit; else rc=1; fi
    if [ "$GATE_TIER" = cert ]; then
      if fan_ok race; then note_step race; else rc=1; fi
      if fan_ok sim; then note_step sim; else rc=1; fi
    else
      if fan_ok sim; then note_step "sim (no -race)"; else rc=1; fi
    fi
    if [ "$docker_in" -eq 1 ]; then
      if fan_ok docker; then gate_docker=ran; else rc=1; fi
    else
      gate_docker=skipped
      echo "     (docker suites skipped — nothing in this diff can reach one)"
    fi
    # Cert, GATE_REUSE=on: a binary push would have served whose fresh run did
    # not pass fails the certification (scripts/gate-reuse.sh).
    reuse_audit || rc=1
    ;;
  all)
    # THE BARE GATE IS THE DEV TIER: fmt first (cheapest, and the only check that
    # still answers when the tree does not compile), then modbuild, lint, the
    # dev-tier guards, the sim compile and the unit tests (through Go's test
    # cache) all at once, printed in that order afterwards. Every step runs even
    # after one fails: a gate that stops at the first problem makes you re-run
    # the slow parts once per problem.
    if timed step_fmt;  then note_step fmt;  else rc=1; fi
    write_diff
    fan_reset modbuild lint scripts simvet unit
    fan modbuild timed step_modbuild
    fan lint timed step_lint
    fan scripts timed step_scripts
    fan simvet timed step_simvet
    fan unit timed step_test
    fan_join
    for l in modbuild lint scripts simvet unit; do fan_print "$l"; done
    if fan_ok modbuild; then note_step modbuild; else rc=1; fi
    if fan_ok lint; then note_step lint; else rc=1; fi
    if fan_ok scripts; then note_step "scripts ($(cat "$ROOT/.gate/scripts.summary" 2>/dev/null))"; else rc=1; fi
    if fan_ok simvet; then note_step "sim compile"; else rc=1; fi
    if fan_ok unit; then
      u="$(cat "$ROOT/.gate/unit.summary" 2>/dev/null)"
      note_step "unit${u:+ ($u)}"
    else rc=1; fi
    ;;
  *) usage ;;
esac

# The run's wall, marked when it failed: a red run is not a measurement. fmt
# is the pre-push hook's step and is left out. The selection line first, then
# the total with its load stamps (scripts/gate-timing.sh); the wall is read
# before the stamps are taken.
if [ "$GATE_MODE" != fmt ]; then
  steps_log selection - "$(gate_selection_fields)"
  if [ "$rc" -eq 0 ]; then steps_log total "$(_secs "$GATE_T0")" "$(gate_total_stamps)"
  else steps_log total-FAILED "$(_secs "$GATE_T0")" "$(gate_total_stamps)"; fi
  gatetime_done
fi
if [ "$rc" -eq 0 ]; then echo "gate: clean"; else echo "gate: FAILED"; fi
# Printed on PASS only: there is no sentence to paste for a gate that failed, and
# emitting one would be the same hand-waving this replaces.
[ "$rc" -eq 0 ] && gate_sentence
exit "$rc"
