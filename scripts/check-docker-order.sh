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
#   2. a later run's line for a package: the rule as it stands, last line wins
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d 2>/dev/null || mktemp -d -t dockerorder)"
trap 'rm -rf "$TMP"' EXIT

fail=0
# first LOG: which of store and www gate.sh order puts first for shingo-core.
first() {
  local line
  line="$(GATE_STEPS_LOG="$1" bash "$ROOT/scripts/gate.sh" order | sed -n 's/^shingo-core: //p')"
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
want "a later run's line" "$TMP/last.log" shingocore/www

[ "$fail" -eq 0 ] && echo "ok   docker-order (longest first from the steps log; no package lost)"
exit "$fail"
