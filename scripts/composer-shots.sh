#!/usr/bin/env bash
#
# composer-shots.sh — render the station's cell picture from a dev edge seeded
# with the Hopkinsville pull, at 1280x800, one PNG per state, named like the
# flow-composer reference screenshots so they can be diffed by eye:
#
#   u4-press-index.png     style 7,  two index pairs   (U4's READ-ONLY picture)
#   u4-two-robot-swap.png  style 11, two staging moves (U4)
#   u4-schematic.png       style 7 with no scene cached (U4)
#
# The u4- prefix and not the reference's numbers: those three are the read-only
# picture, and while they carried 04/05/06 the folder looked complete with four
# of its names showing a screen the reference puts something else under.
#
# and U8's composer states, reached through the page's hash entry
# (#compose=<styleId>;state=<S2..S10>[;node=..]), which sets local UI state only
# and never writes:
#
#   02-picker.png   03-setup-card.png   04-composer-press-index.png
#   05-composer-two-robot-swap.png   06-position-panel.png   07-dock-panel.png
#   08-new-part-blank.png   09-finding-on-node-and-bar.png
#   10-confirm-rows-and-orders.png   11-started-auto-return.png (;hold=1)
#   12-part-picker.png (;addpart=1)  13-part-picker-on-position.png
#   14-lm-path-with-key-route.png
#
# and the DESKTOP Processes page at 1440x900, which this header did not list
# for three rounds while the harness took them:
#
#   P0-processes.png   D1-flows-selected.png   D1-flows-selected-light.png
#   D1-flows-running.png   D2-advanced.png   D3-routing-set.png
#   D4-operator-screens.png   D5-settings.png   D6-presets.png
#   D6-presets-member-expanded.png   D6-apply-modal.png
#   D6-apply-modal-answered.png
#
# 28 PNGs, and they are NOT committed — composer-shots/ is in .gitignore. They
# are a handoff artefact, they weigh ~2 MB, and they carry the fixture on
# screen. The copies that go with a report live beside it in the GitHub root.
#
# A HANDOFF GATE, NOT CI (PLAN-hmi-flow-composer §2.3): a pixel diff in CI
# would flake on fonts and buy nothing the eye does not. The renderer's
# correctness lives in operator-flow.test.js, run by `go test ./www/`.
#
# No npm, no Playwright: headless Chrome's own --screenshot, driven by a
# `shots`-tagged Go test (shingo-edge/www/composer_shots_test.go) that stands
# up the real engine, router and templates on a loopback port.
#
# Usage:
#   bash scripts/composer-shots.sh [OUTDIR]
#     OUTDIR defaults to ./composer-shots (created). Set COMPOSER_SHOTS_CHROME
#     to point at a Chrome/Chromium binary if the default is not found.

set -uo pipefail
cd "$(dirname "$0")/.."
ROOT="$(pwd)"

out="${1:-$ROOT/composer-shots}"
case "$out" in /*|[A-Za-z]:*) ;; *) out="$ROOT/$out" ;; esac
mkdir -p "$out" || { echo "composer-shots: cannot create $out" >&2; exit 1; }

# Go's exec on Windows wants a native path; Git Bash hands us a POSIX one.
if command -v cygpath >/dev/null 2>&1; then
  out_native="$(cygpath -w "$out")"
else
  out_native="$out"
fi

echo "composer-shots: rendering into $out"
# -timeout: eleven states at a 15 s virtual-time budget each runs past go test's
# 10-minute default, and the failure that produces is a panic with no shot list,
# which reads like a broken renderer rather than a clock.
#
# ONE LINE, no continuation: a comment line after a trailing backslash ends it,
# which once stopped the environment prefix reaching go test and skipped the
# whole run with "COMPOSER_SHOTS_OUT not set".
log="$(mktemp)"
( cd "$ROOT/shingo-edge" && COMPOSER_SHOTS_OUT="$out_native"     go test -tags shots -count=1 -timeout 30m -run '^TestComposerShots$' -v ./www/ ) 2>&1 | tee "$log"
rc=${PIPESTATUS[0]}

if [ "$rc" -ne 0 ]; then
  echo "composer-shots: FAILED (exit $rc)" >&2
  rm -f "$log"
  exit "$rc"
fi

# A MISSPELLED TAG OR -run PATTERN EXITS 0 WITH "no tests to run", and the only
# other sign is an output directory that did not change — which, on a re-run
# into a directory that already holds yesterday's PNGs, is no sign at all. So
# liveness is asserted rather than assumed, twice: the test ran, and it left a
# full set behind.
if ! grep -q 'RUN.*TestComposerShots' "$log"; then
  echo "composer-shots: FAILED — TestComposerShots never ran (build tag or -run pattern?)" >&2
  grep -E 'no tests to run|no test files|build constraints' "$log" >&2
  rm -f "$log"
  exit 1
fi
rm -f "$log"

shot_count=$(ls -1 "$out"/*.png 2>/dev/null | wc -l)
if [ "$shot_count" -lt 20 ]; then
  echo "composer-shots: FAILED — only $shot_count PNG(s) in $out; the set is 28" >&2
  exit 1
fi
ls -l "$out"/*.png 2>/dev/null
echo "composer-shots: done ($shot_count shots)"