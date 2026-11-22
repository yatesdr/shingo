#!/usr/bin/env bash
# tier: dev
# inputs: .github/workflows/*.yml */go.mod scripts/gate.sh scripts/check-gate-toolchain.sh
# check-gate-toolchain.sh — one Go toolchain for CI and the gate's WSL steps.
#
# CI's setup-go pins its version by hand in each workflow (`go-version:`), and
# gate.sh reads GATE_GO, the toolchain its WSL steps (race, sim) run under
# GOTOOLCHAIN, from shingo-core/go.mod's go line. This holds every pin and every
# module's go line to one version, so the two cannot drift apart unseen.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1
want="$(sed -n 's/^go[[:space:]]*//p' shingo-core/go.mod | tr -d '\r')"
fail=0
for f in */go.mod; do
  v="$(sed -n 's/^go[[:space:]]*//p' "$f" | tr -d '\r')"
  [ "$v" = "$want" ] || { echo "FAIL gate-toolchain: $f says go $v, shingo-core/go.mod says $want"; fail=1; }
done
pins="$(grep -nE '^[[:space:]]*go-version:' .github/workflows/*.yml)"
[ -n "$pins" ] || { echo "FAIL gate-toolchain: no go-version pin found in .github/workflows; the check read nothing"; exit 1; }
while IFS= read -r line; do
  v="$(printf '%s' "$line" | sed -E "s/.*go-version:[[:space:]]*['\"]?([^'\" ]+)['\"]?.*/\1/")"
  [ "$v" = "$want" ] || { echo "FAIL gate-toolchain: ${line%%:*}: go-version $v, go.mod's go line is $want (gate.sh GATE_GO)"; fail=1; }
done <<<"$pins"
grep -q 'GATE_GO="go$(sed -n' scripts/gate.sh || { echo "FAIL gate-toolchain: gate.sh no longer reads GATE_GO from go.mod"; fail=1; }
[ "$fail" -eq 0 ] && echo "ok   gate-toolchain (go $want: $(printf '%s\n' "$pins" | wc -l | tr -d ' ') CI pins, every go.mod, gate.sh's WSL steps)"
exit "$fail"
