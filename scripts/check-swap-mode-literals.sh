#!/usr/bin/env bash
# check-swap-mode-literals.sh — guards against raw swap-mode strings
# outside the canonical const definitions.
#
# Why: forbidigo (v2.11.4) only matches identifier expressions, not
# string literals, so we cannot enforce "use protocol.SwapModeXxx"
# through golangci-lint. This script does the grep instead.
#
# Catches the "added a new mode, forgot a comparison site" pattern that
# produced commits 4b749c9, bf206ba, 1e90764. New swap modes added to
# protocol/swap_mode.go must be added to MODES below as well.
#
# Go — allowed: protocol/swap_mode.go (the const definitions themselves) and
# *_test.go files (fixtures).
#
# JavaScript has no typed constant to spell a mode with, so there the rule is
# stated per surface. A mode literal is allowed where naming the mode IS the
# job, and nowhere else:
#   - *.test.js — fixtures, as for Go.
#   - shingo-edge/www/static/js/pages/processes.js — the claim editor. It
#     presents, defaults and validates each mode: the JS counterpart of
#     swap_mode.go's own validation.
#   - shingo-core/www/static/pages/test-orders.js — the test-order page, which
#     builds a swap for whichever mode the tester picks.
#   - "manual_swap" in the operator station. A hand-loaded node renders a
#     different board — no robot, no pair — so that check picks a surface; it
#     does not steer a robot.
# Everywhere else a JS gate reads the order graph (sibling_order_id) or a
# property the server declares (releases_as_pair). A 'two_robot' test in the
# operator modal's card is what kept a press-index pair from reading as one
# wait, and the Go half of this script could not see it.
#
# Exit 0 = clean, exit 1 = violations found.

set -euo pipefail

cd "$(dirname "$0")/.."

MODES=(
  '"two_robot_press_index"'
  '"two_robot"'
  '"single_robot"'
  '"manual_swap"'
  '"sequential"'
  '"simple"'
)

JS_ALLOWED='(\.test\.js:|^shingo-edge/www/static/js/pages/processes\.js:|^shingo-core/www/static/pages/test-orders\.js:)'
JS_MANUAL_SURFACE='^shingo-edge/www/static/operator-station/'

FAIL=0
for mode in "${MODES[@]}"; do
  hits=$(grep -RnE --include='*.go' \
    --exclude-dir=.git \
    "$mode" protocol/ shingo-core/ shingo-edge/ integration/ 2>/dev/null \
    | grep -vE '(_test\.go:|protocol/swap_mode\.go:)' \
    | grep -vE ':\s*//' \
    || true)
  if [[ -n "$hits" ]]; then
    echo "Raw swap-mode literal $mode found outside protocol/swap_mode.go and tests:"
    echo "$hits"
    echo "Use the typed constant from protocol/swap_mode.go (e.g. protocol.SwapModeTwoRobot)."
    echo
    FAIL=1
  fi

  bare=${mode//\"/}
  js_hits=$(grep -RnE --include='*.js' \
    --exclude-dir=.git --exclude-dir=node_modules \
    "['\"\`]${bare}['\"\`]" shingo-core/ shingo-edge/ 2>/dev/null \
    | grep -vE "$JS_ALLOWED" \
    | grep -vE ':\s*//' \
    || true)
  if [[ "$bare" == "manual_swap" && -n "$js_hits" ]]; then
    js_hits=$(printf '%s\n' "$js_hits" | grep -vE "$JS_MANUAL_SURFACE" || true)
  fi
  if [[ -n "$js_hits" ]]; then
    echo "Raw swap-mode literal '$bare' found in JavaScript outside the surfaces that name modes:"
    echo "$js_hits"
    echo "Key the gate on the order graph or a property the server declares (see releases_as_pair)."
    echo
    FAIL=1
  fi
done

exit "$FAIL"
