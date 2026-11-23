#!/usr/bin/env bash
# tier: dev
# inputs: scripts/gate.sh scripts/check-docker-order.sh
# check-docker-order.sh — pins docker_order in scripts/gate.sh: which order a
# docker module's packages are handed to `go test -p`, longest first, from the
# steps log (`gate.sh order` prints it). The order is a speedup, never the list
# of what runs, so every case also checks no package is lost.
#
# Each case writes a steps log of its own (GATE_STEPS_LOG) for shingo-core's
# packages store and www and asks which comes first.
#
#   1. one clean run each: the longer goes first
#   2. a later run's short line does not decide alone: the median of the
#      clean runs does (under the old last-line rule www went first here)
#   3. a failed run's lines are not counted
#   4. only the last 5 clean runs count
#   5. GATE_DOCKER_ORDER_LOG ranks from the snapshot it names, not the live log
#   6. a `selection` line, and key=value stamps after a total line's five
#      fields (scripts/gate-timing.sh writes both), change nothing
#   7. GATE_DOCKER_P sets the -p; a bad value is a usage error
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d 2>/dev/null || mktemp -d -t dockerorder)"
trap 'rm -rf "$TMP"' EXIT

fail=0
# first LOG: which of store and www gate.sh order puts first for shingo-core.
first() {
  local line
  line="$(GATE_STEPS_LOG="$1" GATE_DOCKER_ORDER_LOG="${GATE_DOCKER_ORDER_LOG:-}" bash "$ROOT/scripts/gate.sh" order | sed -n 's/^shingo-core: //p')"
  if ! printf '%s\n' $line | grep -qx 'shingocore/engine'; then
    echo "FAIL docker-order: a package is missing from the order: $line" >&2
    fail=1
  fi
  printf '%s\n' $line | grep -xE 'shingocore/(store|www)' | head -1
}
want() { # name log want-first
  local got; got="$(first "$2")"
  if [ "$got" != "$3" ]; then
    echo "FAIL docker-order: $1: $got first, want $3"
    fail=1
  fi
}

cat >"$TMP/one.log" <<'LOG'
a1 full docker:shingocore/store 100.0 2026-11-01T00:00:00Z
a1 full docker:shingocore/www 50.0 2026-11-01T00:00:00Z
a1 full total 300.0 2026-11-01T00:00:00Z
LOG
want "one clean run" "$TMP/one.log" shingocore/store

cat >"$TMP/last.log" <<'LOG'
a1 full docker:shingocore/store 100.0 2026-11-01T00:00:00Z
a1 full docker:shingocore/www 50.0 2026-11-01T00:00:00Z
a1 full total 300.0 2026-11-01T00:00:00Z
a2 full docker:shingocore/store 5.0 2026-11-02T00:00:00Z
a2 full total 300.0 2026-11-02T00:00:00Z
LOG
want "a later run's line" "$TMP/last.log" shingocore/store

cat >"$TMP/failed.log" <<'LOG'
a1 full docker:shingocore/store 10.0 2026-11-01T00:00:00Z
a1 full docker:shingocore/www 50.0 2026-11-01T00:00:00Z
a1 full total 300.0 2026-11-01T00:00:00Z
a2 full docker:shingocore/store 900.0 2026-11-02T00:00:00Z
a2 full docker:shingocore/store 900.0 2026-11-02T00:00:00Z
a2 full total-FAILED 300.0 2026-11-02T00:00:00Z
LOG
want "a failed run" "$TMP/failed.log" shingocore/www

: >"$TMP/window.log"
for d in 01 02 03 04 05; do
  printf 'a1 full docker:shingocore/store 1.0 2026-11-%sT00:00:00Z
a1 full total 9.0 2026-11-%sT00:00:00Z
' "$d" "$d" >>"$TMP/window.log"
done
for d in 06 07 08; do
  printf 'a1 full docker:shingocore/store 100.0 2026-11-%sT00:00:00Z
a1 full total 9.0 2026-11-%sT00:00:00Z
' "$d" "$d" >>"$TMP/window.log"
done
printf 'a1 full docker:shingocore/www 50.0 2026-11-08T00:00:00Z
' >>"$TMP/window.log"
want "the last 5 clean runs" "$TMP/window.log" shingocore/store

GATE_DOCKER_ORDER_LOG="$TMP/failed.log" want "the pinned snapshot" "$TMP/one.log" shingocore/www

cat >"$TMP/stamped.log" <<'LOG'
a1 full selection - 2026-11-01T00:00:00Z tier=push docker=in modules=all p=shingo-core:4 guards=check-x reused=0
a1 full docker:shingocore/store 100.0 2026-11-01T00:00:00Z
a1 full docker:shingocore/www 50.0 2026-11-01T00:00:00Z
a1 full total 300.0 2026-11-01T00:00:00Z exec=on ac=on samples=1 standby=0 load=unmeasured
a2 full selection - 2026-11-02T00:00:00Z tier=push docker=in modules=all p=shingo-core:4 guards=check-x reused=0
a2 full docker:shingocore/www 900.0 2026-11-02T00:00:00Z
a2 full total-FAILED 300.0 2026-11-02T00:00:00Z exec=on ac=on samples=1 standby=0 load=unmeasured
LOG
want "stamped total lines and selection lines" "$TMP/stamped.log" shingocore/store

# 7. GATE_DOCKER_P sets the docker step's -p (the -p series, 2.4); unset, the
#    derived value stands; a value that is not a positive integer is a usage
#    error (exit 2), never a silent fallback to the derived value.
p_line() { GATE_STEPS_LOG= bash "$ROOT/scripts/gate.sh" order 2>/dev/null | sed -n 's/^docker -p: //p'; }
got="$(GATE_DOCKER_P=3 p_line)"
[ "$got" = "3 (GATE_DOCKER_P)" ] || { echo "FAIL docker-order: GATE_DOCKER_P=3 gave -p '$got', want '3 (GATE_DOCKER_P)'" >&2; fail=1; }
got="$(GATE_DOCKER_P= p_line)"
case "$got" in [1-9]|[1-9][0-9]) ;; *) echo "FAIL docker-order: unset GATE_DOCKER_P gave -p '$got', want the derived number" >&2; fail=1 ;; esac
for bad in 0 x 2x -1; do
  GATE_DOCKER_P="$bad" GATE_STEPS_LOG= bash "$ROOT/scripts/gate.sh" order >/dev/null 2>&1
  [ $? -eq 2 ] || { echo "FAIL docker-order: GATE_DOCKER_P=$bad was not refused (want exit 2)" >&2; fail=1; }
done

[ "$fail" -eq 0 ] && echo "ok   docker-order (longest first from the steps log; no package lost; GATE_DOCKER_P)"
exit "$fail"
