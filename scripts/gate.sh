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
# reconnect path, and one goroutine per HTTP request). Scoped to two packages it
# is about a minute; module-wide it is half an hour. See step_race.
#
# It is in `full`, not in `all`. The bare gate stays the fast one.
#
# DOCKER SUITES ARE SCOPED, NOT SKIPPED. See `scope` below.
#
# Usage:
#   bash scripts/gate.sh                  fmt, vet, lint, unit tests
#   bash scripts/gate.sh scope [BASE]     say whether the docker suites are needed
#                                         (exit 0 = needed, 1 = not needed)
#   bash scripts/gate.sh race             -race over ./engine/... and ./www/...
#                                         (borrows WSL's cgo on Windows)
#   bash scripts/gate.sh docker           run the docker suites (no -race)
#   bash scripts/gate.sh full [BASE]      the four, then race, then docker if scope says so
#   bash scripts/gate.sh fmt|vet|lint|test|race  one step

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

# NOT parallelised across modules, unlike step_test, and that is a measured
# decision rather than an oversight: 8s serial against 7s concurrent. Vet's
# wall time is one module (shingo-core) compiling, and the other four are
# rounding error — so the concurrency buys a second and costs five per-module
# log files and an interleaving problem to solve. Not worth it.
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

# ONE RUN, NOT TWO. This used to run each module to /dev/null and then, if it
# failed, run the whole module AGAIN to have something to print — so a red gate
# cost double a green one, on the run you are least willing to wait through.
# The log is written the first time and the excerpt comes out of it.
# Per-worktree under $ROOT/.gate for the same reason step_docker's logs are:
# see the note there about /tmp being machine-global on Git Bash.
#
# STILL SERIAL, AND THAT IS THE SECOND THING TRIED HERE. Running the five
# modules at once works and is faster — 45s serial against ~26s, since
# shingo-edge alone is 26.2s and the other four total ~19s — but it also puts
# five modules' worth of tests on the CPU at once, and shingocore/rds's
# TestPollerStopHaltsPolling asserts on a 30ms wall-clock drain window after
# Stop(). Starve that window and the test reports "polls continued after Stop".
# MEASURED: 10/10 green in isolation, 4/4 green running this step alone, and a
# failure inside `gate.sh full` where lint's teardown overlaps the start of the
# tests. A gate that fails ~1 run in 6 for a reason outside the diff is not a
# gate, and 11s of a ~190s run does not buy that back. The test is genuinely
# load-fragile and would bite on a slow or busy machine without any help from
# here — worth hardening on its own terms, and then this can be revisited.
# Which modules still need an untagged `go test` of their own.
#
# NOT ALL OF THEM, WHEN THE DOCKER STEP IS ALSO RUNNING. `-tags=docker` ADDS
# files and removes none, and nothing in this tree carries a `!docker`
# constraint — so `go test -tags=docker ./...` runs every test the untagged run
# would, plus the docker ones.
#
# VERIFIED against `go test -list` rather than reasoned about: shingo-core's
# engine lists 37 tests untagged and 264 with the tag, rds 154 and 154,
# store/orders 1 and 28 — and in each, zero tests present untagged and absent
# with the tag. No docker-tagged file defines TestMain or init() either, so the
# tag does not change how the untagged tests get set up.
#
# So running both duplicates 1,929 tests, and cold it duplicates a whole second
# compile of shingo-core — 21s on this host against 43s for the docker-tagged
# one. protocol and shared carry no docker tests and are still run here; they
# are 185 tests and about two seconds.
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

step_test() {
  local m failed=0 logdir mods
  mods="${1:-$MODULES}"
  if [ -z "$mods" ]; then
    echo "ok   tests (every module's untagged tests run inside the docker step)"
    return 0
  fi
  logdir="$ROOT/.gate"
  mkdir -p "$logdir" || { echo "FAIL tests — cannot create $logdir"; return 1; }
  for m in $mods; do
    ( cd "$ROOT/$m" && go test -count=1 ./... >"$logdir/test-$m.log" 2>&1 ) || {
      echo "  $m:"
      grep -Ev '^ok |no test files' "$logdir/test-$m.log" | head -30
      failed=1
    }
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
# becomes one CREATE DATABASE ... TEMPLATE, which is a file copy.
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
# initdb refuses it. max_connections is raised because every package's pool
# lands on this one server now.
sharedPG=""

start_shared_pg() {
  local id port deadline
  id="$(docker run -d \
      -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=postgres \
      -e PGDATA=/var/lib/postgresql/data/pgdata \
      --tmpfs /var/lib/postgresql/data:rw,size=4g \
      -p 127.0.0.1::5432 \
      postgres:16-alpine \
      -c fsync=off -c synchronous_commit=off -c full_page_writes=off \
      -c max_connections=500 -c shared_buffers=256MB 2>&1)" || return 1
  case "$id" in *[!0-9a-f]*|"") return 1 ;; esac
  sharedPG="$id"
  trap stop_shared_pg EXIT INT TERM

  deadline=$(( $(date +%s) + 60 ))
  until docker exec "$id" pg_isready -U test -q 2>/dev/null; do
    [ "$(date +%s)" -gt "$deadline" ] && return 1
    sleep 0.5
  done

  port="$(docker port "$id" 5432/tcp 2>/dev/null | head -1 | sed 's/.*://')"
  [ -z "$port" ] && return 1

  export SHINGO_TEST_PG="127.0.0.1:$port"
  export SHINGO_TEST_PG_TEMPLATE="template_gate_$$"
  return 0
}

stop_shared_pg() {
  [ -z "$sharedPG" ] && return 0
  docker rm -f "$sharedPG" >/dev/null 2>&1
  sharedPG=""
}

# docker_p answers how many test binaries to run at once.
#
# It used to be a hardcoded `-p 1`, to stop packages fighting over containers.
# One shared server removes that reason, but the obvious follow-up — turn the
# packages loose — is WRONG, and measured wrong rather than argued wrong. On
# this 20-core host, shingo-core's docker suite from a fresh container:
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
docker_p() {
  local cores
  cores="$(nproc 2>/dev/null || echo "${NUMBER_OF_PROCESSORS:-4}")"
  case "$cores" in *[!0-9]*|"") cores=4 ;; esac
  local p=$(( cores / 4 ))
  [ "$p" -lt 1 ] && p=1
  [ "$p" -gt 4 ] && p=4
  echo "$p"
}

step_docker() {
  local m failed=0 mods logdir p
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
    echo "  docker: WARNING — could not start a shared postgres;" >&2
    echo "          each package will start its own (adds roughly 5s per package)." >&2
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
  for m in $mods; do
    echo "  docker: $m"
    ( cd "$ROOT/$m" && go test -tags=docker -timeout=20m -count=1 -p "$p" ./... >"$logdir/docker-$m.log" 2>&1 ) \
      || { failed=1; echo "  --- $m ($logdir/docker-$m.log) ---"; grep -Ev '^ok |no test files' "$logdir/docker-$m.log" | head -30; }
    # The per-package ok-count the Sunday smoke asserts on. `ok  pkg  1.2s`
    # only prints when that package's binary ran and passed; a package whose
    # tests all skipped still prints ok, so the smoke's assertion is on the
    # sentinel being ABSENT and the count being non-zero IN THE PACKAGES THAT
    # CARRY DOCKER TESTS — testdb's RanItsTests is the canary that cannot skip
    # green.
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
# because the scope is two packages — measured at roughly a minute — and
# unaffordable module-wide, which is the same trade the header already makes.
RACE_PKGS="./engine/... ./www/..."

step_race() {
  local rcmd rrc
  rcmd="cd \"$ROOT/shingo-core\" && CGO_ENABLED=1 go test -race -count=1 -timeout=15m $RACE_PKGS"

  if [ -n "${WSL_DISTRO_NAME:-}" ]; then
    # Already inside WSL — run directly.
    ( eval "$rcmd" >"$ROOT/.gate/race.log" 2>&1 ); rrc=$?
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
    ( wsl.exe -d Ubuntu -- bash -lc "cd '$wslroot/shingo-core' && CGO_ENABLED=1 go test -race -count=1 -timeout=15m $RACE_PKGS" \
        >"$ROOT/.gate/race.log" 2>&1 ); rrc=$?
  else
    echo "FAIL race — no cgo toolchain and no WSL to borrow one from"
    echo "  install WSL, or run on a host with gcc: go test -race $RACE_PKGS"
    return 1
  fi

  if [ "$rrc" -eq 0 ]; then
    echo "ok   race ($RACE_PKGS)"
    return 0
  fi
  echo "FAIL race ($ROOT/.gate/race.log)"
  # WARNING: DATA RACE is the line that matters and it is not near the end, so
  # grep for it rather than tailing.
  grep -A 24 'WARNING: DATA RACE' "$ROOT/.gate/race.log" | head -60
  grep -Ev '^ok |no test files' "$ROOT/.gate/race.log" | head -20
  return 1
}

mkdir -p "$ROOT/.gate" 2>/dev/null || true

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
# (a test asserting a contract two commits had already changed).
#
# So the sentence is fact-carried, per standing law 4: accumulated by the steps
# themselves as they pass, never assembled from what the invocation was supposed
# to do. A "docker suites" that appears in a commit message and not in this
# output is now a visible contradiction rather than an invisible one.
gate_steps=""
gate_docker=""   # "" = not attempted | "ran" | "skipped"
note_step() { gate_steps="${gate_steps:+$gate_steps, }$1"; }

gate_sentence() {
  local s="$gate_steps"
  case "$gate_docker" in
    # "AND" only joins something to something. `gate.sh docker` runs the suites
    # alone, and the first version of this printed a dangling "Gate: AND the
    # docker suites." — caught by the mutation this rider was specified with.
    ran)     if [ -n "$s" ]; then s="$s AND the docker suites"; else s="the docker suites"; fi ;;
    skipped) : ;;
  esac
  [ -n "$s" ] || { echo "Gate sentence: (nothing ran)"; return; }
  # Capitalised the way it lands in a commit message, ready to paste.
  local out="Gate: $s."
  [ "$gate_docker" = skipped ] && out="$out Docker suites SKIPPED by scope (nothing in this diff can reach one)."
  echo "$out"
}

case "${1:-all}" in
  fmt)   if step_fmt;  then note_step fmt;  else rc=1; fi ;;
  vet)   if step_vet;  then note_step vet;  else rc=1; fi ;;
  lint)  if step_lint; then note_step lint; else rc=1; fi ;;
  test)  if step_test; then note_step unit; else rc=1; fi ;;
  race)  if step_race; then note_step race; else rc=1; fi ;;
  # EXIT CODE IS THE VERDICT: 0 = docker needed, 1 = not needed. It used to
  # `exit 0` unconditionally, which made the answer readable only by a human
  # reading the text — so nothing could gate on it. `if bash scripts/gate.sh
  # scope; then ...` now works, and matches step_scope's own return
  # convention, which `full` has always branched on.
  scope) step_scope "${2:-}"; exit $? ;;
  docker) if step_docker; then gate_docker=ran; else rc=1; fi ;;
  full)
    if step_fmt;  then note_step fmt;  else rc=1; fi
    if step_vet;  then note_step vet;  else rc=1; fi
    if step_lint; then note_step lint; else rc=1; fi
    # Scope is decided BEFORE the tests here, because it decides what the tests
    # have to cover: when the docker step runs it already runs every untagged
    # test in the modules that carry docker tests, so only the modules it does
    # not reach need their own run. See untagged_only_modules. When docker is
    # out of scope nothing else covers them and every module runs, which is
    # what `all` does too.
    if step_scope "${2:-}"; then
      if step_test "$(untagged_only_modules)"; then note_step unit; else rc=1; fi
      if step_docker; then gate_docker=ran; else rc=1; fi
    else
      if step_test; then note_step unit; else rc=1; fi
      gate_docker=skipped
      echo "     (docker suites skipped — nothing in this diff can reach one)"
    fi
    if step_race; then note_step race; else rc=1; fi
    ;;
  all)
    # Every step runs even after one fails: a gate that stops at the first
    # problem makes you re-run the slow parts once per problem.
    if step_fmt;  then note_step fmt;  else rc=1; fi
    if step_vet;  then note_step vet;  else rc=1; fi
    if step_lint; then note_step lint; else rc=1; fi
    if step_test; then note_step unit; else rc=1; fi
    ;;
  *) echo "usage: bash scripts/gate.sh [fmt|vet|lint|test|race|scope|docker|full] [BASE]" >&2; exit 2 ;;
esac

if [ "$rc" -eq 0 ]; then echo "gate: clean"; else echo "gate: FAILED"; fi
# Printed on PASS only: there is no sentence to paste for a gate that failed, and
# emitting one would be the same hand-waving this replaces.
[ "$rc" -eq 0 ] && gate_sentence
exit "$rc"
