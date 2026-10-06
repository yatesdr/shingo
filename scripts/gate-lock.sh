# shellcheck shell=bash
#
# gate-lock.sh — one gate per machine at a time. Sourced by scripts/gate.sh
# (and by scripts/check-gate-lock.sh, which pins it); not run on its own.
#
# WHY. Several lanes running `gate.sh` at once on a 16 GB laptop exhaust its
# memory: each gate is a handful of Go compiles, five concurrent `go test`
# modules, golangci-lint and a Postgres container. Run one after another they
# all finish; run together they swap, time out and fail for reasons outside the
# diff. So the heavy steps queue on a lock and the queue says who is in front.
#
# WHERE. A directory in the clone's git COMMON dir —
# `$(git rev-parse --git-common-dir)/gate.lock` — so every worktree of a clone
# shares one lock, WSL sees the Windows clone's lock through /mnt/c, and a
# container that mounts the clone at the same path shares it too. The scope is
# therefore PER CLONE, not per kernel: two separate clones on one machine do
# not see each other. One clone per machine is the norm here.
#
# ACQUIRE is `mkdir`, which is atomic on every filesystem this runs on. The
# holder writes `owner` (who, where, which commit, which step, since when, and
# a token) and keeps `heartbeat` fresh from a background loop.
#
# SELF-RELEASE IF THE GATE DIES. The normal exit releases through the caller's
# EXIT trap. A SIGKILLed gate runs no trap — so the heartbeat loop watches the
# holder's pid and stops touching the file when it is gone, and a waiter treats
# a heartbeat older than GATE_LOCK_STALE seconds as abandoned and takes the
# lock. Staleness is judged from the heartbeat's AGE, never from a pid: pids
# mean nothing across hosts, containers or the WSL/Windows boundary. The age is
# measured against a probe file the waiter touches in the same directory, not
# against `date`, so both timestamps come from the same filesystem's clock (WSL's
# clock drifts from Windows' after a sleep).
#
# NESTED RUNS. The holder exports GATE_LOCK_HELD=<token>. A gate started with
# that token in its environment, while the lock's owner carries the same token,
# is part of the holder's run and does not wait on itself. A token that does not
# match (a stale export in some shell) is ignored and the run queues normally.
#
# Knobs (seconds unless noted), mostly for the pin's short windows:
#   GATE_LOCK_DIR        lock directory (default: <git common dir>/gate.lock)
#   GATE_LOCK_HEARTBEAT  heartbeat interval              (20)
#   GATE_LOCK_STALE      heartbeat age that frees a lock (120)
#   GATE_LOCK_POLL       how often a waiter retries      (2)
#   GATE_LOCK_REPORT     how often a waiter says who it waits for (60)
#   GATE_LOCK_TIMEOUT    give up after this long; 0 = wait forever (0)
#   GATE_NO_LOCK=1       EMERGENCY ONLY: skip the lock, loudly

gate_lock_dir() {
  if [ -n "${GATE_LOCK_DIR:-}" ]; then
    printf '%s' "$GATE_LOCK_DIR"
    return
  fi
  local common
  # --git-common-dir is relative (".git") in a main checkout and absolute in a
  # worktree; resolve it either way so the path is the same from any cwd.
  common="$(cd "${ROOT:-.}" && cd "$(git rev-parse --git-common-dir 2>/dev/null)" 2>/dev/null && pwd)"
  if [ -z "$common" ]; then
    # Not a git checkout (an exported tarball): per tree is the best available.
    common="${ROOT:-.}/.gate"
    mkdir -p "$common" 2>/dev/null
  fi
  printf '%s/gate.lock' "$common"
}

# _gate_mtime FILE — modification time in epoch seconds, or empty.
_gate_mtime() {
  stat -c %Y "$1" 2>/dev/null || date -r "$1" +%s 2>/dev/null
}

# _gate_lock_field DIR KEY — one value from DIR/owner.
_gate_lock_field() {
  sed -n "s/^$2=//p" "$1/owner" 2>/dev/null | head -1
}

# _gate_lock_age DIR — seconds since the holder's last heartbeat, judged
# against a probe touched now in the same parent directory. Empty when the
# lock vanished mid-measurement.
_gate_lock_age() {
  local d="$1" probe hb now
  probe="$(dirname "$d")/gate.lock.probe.$$"
  : >"$probe" 2>/dev/null && touch "$probe" 2>/dev/null
  now="$(_gate_mtime "$probe")"
  rm -f "$probe"
  [ -n "$now" ] || now="$(date +%s)"
  # Missing heartbeat = the holder died between mkdir and its first touch;
  # the directory's own mtime then stands in.
  hb="$(_gate_mtime "$d/heartbeat")"
  [ -n "$hb" ] || hb="$(_gate_mtime "$d")"
  [ -n "$hb" ] || return 0
  local age=$((now - hb))
  [ "$age" -lt 0 ] && age=0
  printf '%s' "$age"
}

# _gate_lock_who DIR — the one-line description of the holder.
_gate_lock_who() {
  local d="$1"
  printf '%s@%s %s %s %s since %s' \
    "$(_gate_lock_field "$d" user)" "$(_gate_lock_field "$d" host)" \
    "$(_gate_lock_field "$d" worktree)" "$(_gate_lock_field "$d" head)" \
    "$(_gate_lock_field "$d" steps)" "$(_gate_lock_field "$d" started)"
}

# gate_lock_status — print the holder, or "free". Never waits.
gate_lock_status() {
  local d age
  d="$(gate_lock_dir)"
  if [ ! -d "$d" ]; then
    echo "gate lock: free ($d)"
    return 0
  fi
  age="$(_gate_lock_age "$d")"
  echo "gate lock: held ($d)"
  sed 's/^/    /' "$d/owner" 2>/dev/null
  if [ -n "$age" ] && [ "$age" -ge "${GATE_LOCK_STALE:-120}" ]; then
    echo "    heartbeat ${age}s ago — STALE; the next gate to start will take it"
  else
    echo "    heartbeat ${age:-?}s ago"
  fi
}

_gate_heartbeat_loop() {
  local d="$1" token="$2" holder="$3" every="$4"
  while kill -0 "$holder" 2>/dev/null; do
    # Lock taken over as stale (holder suspended past the window): stop
    # refreshing a lock that now belongs to someone else.
    [ "$(_gate_lock_field "$d" token)" = "$token" ] || exit 0
    touch "$d/heartbeat" 2>/dev/null || exit 0
    sleep "$every"
  done
}

GATE_LOCK_MINE=""      # the token this process holds, if any
GATE_LOCK_HB_PID=""

# gate_lock_acquire STEPS — block until this run holds the lock (or is nested
# inside a run that does). Returns 1 only on GATE_LOCK_TIMEOUT.
gate_lock_acquire() {
  local steps="$1" d
  if [ "${GATE_NO_LOCK:-}" = 1 ]; then
    {
      echo "################################################################"
      echo "# GATE_NO_LOCK=1 — RUNNING WITHOUT THE MACHINE GATE LOCK."
      echo "# Emergency use only: a concurrent gate on this machine can"
      echo "# exhaust its memory and fail both runs. Say so in your report."
      echo "################################################################"
    } >&2
    return 0
  fi
  d="$(gate_lock_dir)"

  if [ -n "${GATE_LOCK_HELD:-}" ] && [ -d "$d" ] \
      && [ "$(_gate_lock_field "$d" token)" = "$GATE_LOCK_HELD" ]; then
    return 0   # nested inside the holder's own run
  fi

  local stale="${GATE_LOCK_STALE:-120}" poll="${GATE_LOCK_POLL:-2}"
  local report="${GATE_LOCK_REPORT:-60}" timeout="${GATE_LOCK_TIMEOUT:-0}"
  local start last=-1 shown="" now age tok moved
  start="$(date +%s)"
  while ! mkdir "$d" 2>/dev/null; do
    now="$(date +%s)"
    if [ ! -d "$d" ]; then continue; fi   # released between mkdir and here
    age="$(_gate_lock_age "$d")"
    if [ -n "$age" ] && [ "$age" -ge "$stale" ]; then
      # Take the abandoned lock. Rename first (atomic, and only one waiter's
      # rename can succeed), then check the renamed lock is the one judged
      # stale — another waiter may have taken over and re-created it between
      # the age check and the rename — and put it back if not.
      tok="$(_gate_lock_field "$d" token)"
      moved="$d.stale.$$"
      if mv "$d" "$moved" 2>/dev/null; then
        if [ "$(_gate_lock_field "$moved" token)" = "$tok" ]; then
          echo "gate lock: heartbeat ${age}s old — taking the lock from $(_gate_lock_who "$moved")" >&2
          rm -rf "$moved"
        else
          mv "$moved" "$d" 2>/dev/null || rm -rf "$moved"
        fi
      fi
      continue
    fi
    if [ -z "$shown" ]; then
      echo "gate lock: held by another gate on this machine ($d):" >&2
      sed 's/^/    /' "$d/owner" >&2 2>/dev/null
      shown=1
    fi
    if [ "$last" -lt 0 ] || [ $((now - last)) -ge "$report" ]; then
      echo "gate lock: waiting for $(_gate_lock_who "$d") (heartbeat ${age:-?}s ago)" >&2
      last="$now"
    fi
    if [ "$timeout" -gt 0 ] && [ $((now - start)) -ge "$timeout" ]; then
      echo "gate lock: gave up after ${timeout}s (GATE_LOCK_TIMEOUT)" >&2
      return 1
    fi
    sleep "$poll"
  done

  local started token
  started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  token="$(hostname 2>/dev/null || echo host):$$:$(date +%s):$RANDOM$RANDOM"
  {
    echo "user=${USER:-${USERNAME:-$(id -un 2>/dev/null)}}"
    echo "host=$(hostname 2>/dev/null)"
    echo "pid=$$"
    echo "worktree=${ROOT:-$(pwd)}"
    echo "head=$(git -C "${ROOT:-.}" rev-parse --short HEAD 2>/dev/null)"
    echo "steps=$steps"
    echo "started=$started"
    echo "token=$token"
  } >"$d/owner"
  touch "$d/heartbeat"
  GATE_LOCK_MINE="$token"
  export GATE_LOCK_HELD="$token"
  # stdio detached: the loop must not hold a `gate.sh | tee` pipeline open.
  # NOT A JOB OF THIS SHELL: gate.sh's tests step collects its module runs
  # with a bare `wait`, which waits for every child job — and a loop that
  # stops only when this shell dies would hold it forever. The subshell starts
  # the loop and exits at once, so the loop is nobody's job; its pid comes
  # back through a file. ($$ in the subshell is still this shell's pid.)
  ( _gate_heartbeat_loop "$d" "$token" "$$" "${GATE_LOCK_HEARTBEAT:-20}" \
      </dev/null >/dev/null 2>&1 &
    echo $! >"$d/heartbeat.pid" )
  GATE_LOCK_HB_PID="$(cat "$d/heartbeat.pid" 2>/dev/null)"
  [ -n "$shown" ] && echo "gate lock: acquired after $(( $(date +%s) - start ))s" >&2
  return 0
}

# gate_lock_release — idempotent; safe from an EXIT trap. Removes the lock only
# if it is still ours (a takeover after a suspend must not be undone).
gate_lock_release() {
  [ -n "$GATE_LOCK_MINE" ] || return 0
  local d
  d="$(gate_lock_dir)"
  [ -n "$GATE_LOCK_HB_PID" ] && kill "$GATE_LOCK_HB_PID" 2>/dev/null
  if [ "$(_gate_lock_field "$d" token)" = "$GATE_LOCK_MINE" ]; then
    rm -rf "$d"
  fi
  GATE_LOCK_MINE=""
  GATE_LOCK_HB_PID=""
}
