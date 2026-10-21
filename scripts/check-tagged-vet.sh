#!/usr/bin/env bash
# check-tagged-vet.sh — vet the Go files behind build tags no gate step builds.
#
# A file behind a build tag is compiled only when something passes that tag.
# The gate passes three: docker and lockcheck (lint, the docker step, the unit
# tests) and sim (the sim step). Everything else — today explain
# (store/bins/explain_plan_test.go), shots (the screenshot tests in both www
# packages), deps (shingo-core/deps.go) and race (plc/race_on_test.go, which
# only the -race run in WSL compiles, and only on the packages it names) — was
# compiled by nothing. A rename or a signature change in the code they call
# broke them silently, and the next person to run them found out.
#
# So each such tag is vetted here, one tag at a time (race and !race cannot be
# vetted together) and only over the packages that hold a file naming it, so
# the cost is seconds, not a module compile. The tags are DISCOVERED from the
# //go:build lines, not listed: a new tag is vetted the day its first file
# lands, with nothing to remember here.
#
# Exit 0 = every tagged package vets clean, 1 = a vet failed.

set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODULES="protocol shared shingo-core shingo-edge integration"

# Tags a gate step already builds, and platform or toolchain words that are
# not tags anyone passes by hand.
SKIP=" docker lockcheck sim ignore cgo unix windows linux darwin freebsd amd64 arm64 arm 386 "

failed=0
vetted=0
for m in $MODULES; do
  [ -d "$ROOT/$m" ] || continue
  # Every //go:build line in the module, as "file<TAB>expression".
  lines="$(cd "$ROOT/$m" && grep -rE --include='*.go' --exclude-dir=node_modules --exclude-dir=.git -m1 '^//go:build ' . 2>/dev/null \
    | grep -v '/node_modules/' | sed -E 's#^\./##; s#:(//go:build )#\t#')"
  [ -n "$lines" ] || continue
  tags="$(printf '%s\n' "$lines" | cut -f2 | grep -oE '[A-Za-z_][A-Za-z0-9_.]*' | sort -u)"
  for tag in $tags; do
    case "$SKIP" in *" $tag "*) continue ;; esac
    case "$tag" in go1*) continue ;; esac
    pkgs="$(printf '%s\n' "$lines" | awk -F'\t' -v t="$tag" '
      { n = split($2, w, /[^A-Za-z0-9_.]+/); for (i = 1; i <= n; i++) if (w[i] == t) { print $1; break } }' \
      | xargs -r -n1 dirname | sort -u | sed 's#^#./#')"
    [ -n "$pkgs" ] || continue
    vetted=$((vetted + 1))
    # shellcheck disable=SC2086 # pkgs is a list of package directories
    if ! out="$(cd "$ROOT/$m" && go vet -tags "$tag" $pkgs 2>&1)"; then
      echo "FAIL tagged vet: $m -tags $tag ($(echo $pkgs))"
      printf '%s\n' "$out" | sed 's/^/  /'
      echo "  reproduce: cd $m && go vet -tags $tag $(echo $pkgs)"
      failed=1
    fi
  done
done

if [ "$vetted" -eq 0 ]; then
  echo "FAIL tagged vet: found no tagged packages to vet — the discovery is broken and the check is vacuous"
  exit 1
fi
[ "$failed" -eq 0 ] && echo "ok   tagged vet ($vetted tag-module pairs)"
exit $failed
