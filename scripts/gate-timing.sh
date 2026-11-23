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
