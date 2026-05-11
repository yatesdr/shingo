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
# Allowed: protocol/swap_mode.go (the const definitions themselves) and
# *_test.go files (fixtures).
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
done

exit "$FAIL"
