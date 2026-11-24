#!/usr/bin/env bash
# tier: dev
# inputs: scripts/gate.sh scripts/check-cache-scope.sh
# side: linux
# check-cache-scope.sh — pins which modules the dev tier runs -count=1 for a
# given diff (module_uncached in scripts/gate.sh, through `gate.sh cachescope
# -`), and so which it lets Go's test cache serve. Each row is one diff of one
# path, and the modules that must run uncached for it. A row that flips is
# either a rule change to argue in gate.sh or a hole.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ALL="protocol shared shingo-core shingo-edge integration"
# path|the modules that run -count=1 ("-" for none)
ROWS="
shingo-core/engine/engine.go|-
shingo-edge/go.mod|-
CHANGELOG.md|$ALL
docs/terminology.md|$ALL
shingo-edge/www/static/pages/x.js|$ALL
shared/components.css|$ALL
plants/demo.yaml|$ALL
scripts/gate.sh|$ALL
"
fail=0
while IFS='|' read -r path want; do
  [ -n "$path" ] || continue
  got="$(printf '%s\n' "$path" | bash "$ROOT/scripts/gate.sh" cachescope - | awk -F': ' '$2 != "cached" { printf "%s ", $1 }' | sed 's/ $//')"
  [ "$want" = "-" ] && want=""
  if [ "$got" != "$want" ]; then
    echo "FAIL cache-scope: $path: -count=1 in [${got}], want [${want}]"
    fail=1
  fi
done <<<"$ROWS"
[ "$fail" -eq 0 ] && echo "ok   cache-scope (which modules a non-Go path reruns in the dev tier)"
exit "$fail"
