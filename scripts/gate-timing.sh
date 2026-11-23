# shellcheck shell=bash
#
# gate-timing.sh — the gate's side of the timing tool (shared/scripts/gatetime).
# Sourced by scripts/gate.sh after gate-lock.sh (it uses _gate_lock_age); not
# run on its own. Pinned by scripts/check-gate-lock.sh.
#
# THE TIMING FLAG. While `gatetime run` times a command it holds a directory,
# `<git common dir>/gate.timing` (GATE_TIMING_DIR names another), whose `owner`
# file names the holder and whose `heartbeat` it touches every few seconds. A
# gate that started beside a timed run would be load in the timing, and the
# gate lock cannot stop it: GATE_NO_LOCK=1 skips the lock. So every heavy mode
# refuses to start while the flag is held, GATE_NO_LOCK=1 included, and names
# the holder. The timed run's own gate passes: gatetime hands its command the
# flag's token in GATE_TIMING_TOKEN. A flag whose heartbeat is older than
# GATE_TIMING_STALE seconds (120) belongs to a run that died, or to a machine
# that slept; the gate says so and goes on. fmt, scope, order, cachescope and
# lock-status never look: they are cheap, and the pre-push hook runs fmt.

gate_timing_dir() {
  if [ -n "${GATE_TIMING_DIR:-}" ]; then
    printf '%s' "$GATE_TIMING_DIR"
    return
  fi
  local common
  common="$(cd "${ROOT:-.}" && cd "$(git rev-parse --git-common-dir 2>/dev/null)" 2>/dev/null && pwd)"
  [ -n "$common" ] || return 0   # not a git checkout: no flag to honour
  printf '%s/gate.timing' "$common"
}

# gate_timing_check — 0 when this gate may start, 1 (printing who holds the
# flag) when a timed run that is not this gate's own holds it.
gate_timing_check() {
  local d age
  d="$(gate_timing_dir)"
  { [ -n "$d" ] && [ -d "$d" ]; } || return 0
  if [ -n "${GATE_TIMING_TOKEN:-}" ] \
      && [ "$(sed -n 's/^token=//p' "$d/owner" 2>/dev/null | head -1 | tr -d '\r')" = "$GATE_TIMING_TOKEN" ]; then
    return 0   # this gate is the timed run's own
  fi
  age="$(_gate_lock_age "$d")"
  if [ -n "$age" ] && [ "$age" -ge "${GATE_TIMING_STALE:-120}" ]; then
    echo "gate: the timing flag $d has a heartbeat ${age}s old: its run is gone; going on" >&2
    return 0
  fi
  {
    echo "gate: REFUSED — a timed run holds the timing flag ($d):"
    sed 's/^/    /' "$d/owner" 2>/dev/null
    echo "  Every heavy gate mode waits its turn behind a timing, GATE_NO_LOCK=1 included:"
    echo "  start this gate again when the timed run ends (bash scripts/gate.sh lock-status shows it)."
  } >&2
  return 1
}

# gate_timing_status — lock-status's line for the flag. Never waits.
gate_timing_status() {
  local d age
  d="$(gate_timing_dir)"
  if [ -z "$d" ] || [ ! -d "$d" ]; then
    echo "timing flag: free${d:+ ($d)}"
    return 0
  fi
  age="$(_gate_lock_age "$d")"
  echo "timing flag: held ($d), heartbeat ${age:-?}s ago"
  sed 's/^/    /' "$d/owner" 2>/dev/null
}

# ── The steps log's selection line and load stamps ─────────────────────────
#
# A timing compares two gates only when they chose the same work: two runs
# that skipped different guards, or ran docker in one and not the other, differ
# by what they ran, not by how fast. So every heavy gate run writes one
# `selection` line to the steps log before its `total`:
#
#   sha mode selection - run tier=T docker=in|out modules=all|a,b p=M:N,...|-
#       guards=check-a,check-b|- reused=N
#
# (`-` stands in the seconds field). docker is whether the docker step was in
# this run; p is the -p each docker module ran at, as step_docker wrote it;
# guards the scripts step's guards that ran, as step_scripts wrote them;
# reused the packages Go's test cache served (the dev tier's cached unit step,
# read from a unit.summary this run wrote), else 0. The `total` line carries
# the load stamps after its five fields (`gatetime stamp`): ac, samples,
# standby, load, foreign30. Readers: `gatetime compare`
# (shared/scripts/gatetime), which refuses unequal selections and stamped
# loads; docker_order reads fields 1-5 only (scripts/check-docker-order.sh
# pins that the extra fields change nothing).
#
# PER RUN, NOT PER WORKTREE. The steps write what the selection line reads
# into GATE_SEL_DIR (.gate/selection.<pid>), and each run builds its own
# gatetime (.gate/gatetime-<pid>): the gate-lock pin runs nested gates in
# this worktree's .gate while the gate that runs the pin is still going, and
# Windows will not overwrite an executable another process has open. A nested
# gate (one that inherits GATETIME_BIN) uses its parent's binary.

# GATETIME_BIN is the built gatetime, as a path native programs can open.
GATETIME_BIN="${GATETIME_BIN:-}"
GATE_SEL_DIR=""
GATETIME_OWN=""

# gatetime_prepare — once per heavy run, before any step. A build that fails
# costs the stamps only (they read `unread`); lint and modbuild report the
# compile error.
gatetime_prepare() {
  GATE_SEL_DIR="$ROOT/.gate/selection.$$"
  mkdir -p "$GATE_SEL_DIR" && rm -f "$GATE_SEL_DIR/docker.p" "$GATE_SEL_DIR/scripts.ran"
  : >"$GATE_SEL_DIR/start"
  [ -n "$STEPS_LOG" ] || return 0
  if [ -n "$GATETIME_BIN" ] && [ -x "$GATETIME_BIN" ]; then return 0; fi
  # binaries a killed gate left behind
  find "$ROOT/.gate" -maxdepth 1 -name 'gatetime-[0-9]*' -mmin +240 -exec rm -f {} + 2>/dev/null
  local ext bin
  ext="$(go env GOEXE 2>/dev/null | tr -d '\r')"
  bin="gatetime-$$$ext"
  if ( cd "$ROOT/shared" && go build -o "$ROOT/.gate/$bin" ./scripts/gatetime ) >"$GATE_SEL_DIR/build.log" 2>&1; then
    GATETIME_BIN="$(cd "$ROOT/.gate" && { pwd -W 2>/dev/null || pwd; })/$bin"
    GATETIME_OWN="$ROOT/.gate/$bin"
    export GATETIME_BIN
  else
    echo "gate: WARNING — could not build shared/scripts/gatetime; the steps log's load stamps read unread:" >&2
    sed 's/^/    /' "$GATE_SEL_DIR/build.log" | head -10 >&2
  fi
}

# gatetime_done — after the total line: removes this run's own files.
gatetime_done() {
  [ -n "$GATETIME_OWN" ] && rm -f "$GATETIME_OWN"
  [ -n "$GATE_SEL_DIR" ] && rm -f "$GATE_SEL_DIR/docker.p" "$GATE_SEL_DIR/scripts.ran" \
    "$GATE_SEL_DIR/start" "$GATE_SEL_DIR/build.log" && rmdir "$GATE_SEL_DIR" 2>/dev/null
  return 0
}

# gate_selection_fields — the selection line's key=value words.
gate_selection_fields() {
  local docker=out p="-" guards="" modules=all reused="" d="$GATE_SEL_DIR"
  { [ "${docker_in:-0}" = 1 ] || [ "$GATE_MODE" = docker ]; } && docker=in
  [ -n "$d" ] && [ -s "$d/docker.p" ] && p="$(paste -sd, "$d/docker.p")"
  [ -n "$d" ] && [ -s "$d/scripts.ran" ] && guards="$(sed '/^$/d' "$d/scripts.ran" | paste -sd, -)"
  [ -n "$gate_scope" ] && modules="$(echo "$gate_scope" | tr ' ' ',')"
  if [ -n "$d" ] && [ "$ROOT/.gate/unit.summary" -nt "$d/start" ]; then
    reused="$(sed -n 's/^cached: \([0-9]*\) of .*/\1/p' "$ROOT/.gate/unit.summary" | head -1)"
  fi
  printf 'tier=%s docker=%s modules=%s p=%s guards=%s reused=%s' \
    "$GATE_TIER" "$docker" "$modules" "$p" "${guards:--}" "${reused:-0}"
}

# gate_total_stamps — the total line's load stamps.
gate_total_stamps() {
  local s=""
  [ -n "$GATETIME_BIN" ] && s="$("$GATETIME_BIN" stamp -since "$GATE_T0" 2>/dev/null | tr -d '\r')"
  printf '%s' "${s:-ac=unread samples=0 standby=unread load=unmeasured}"
}
