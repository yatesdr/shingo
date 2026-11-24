#!/usr/bin/env bash
# tier: dev
# inputs: .github/workflows/*.yml */go.mod scripts/gate.sh scripts/check-gate-toolchain.sh scripts/gate-linux-setup.sh
# side: linux
# check-gate-toolchain.sh — one Go toolchain for CI and the gate's WSL steps.
#
# Two toolchains remain in a local gate: the native steps run the host's go,
# which this does not check.
#
# CI's setup-go pins its version by hand in each workflow (`go-version:`), and
# gate.sh reads GATE_GO, the toolchain its WSL steps (race, sim) run under
# GOTOOLCHAIN, from shingo-core/go.mod's go line. This holds every pin and every
# module's go line to one version, so the two cannot drift apart unseen.
#
# The Linux side's setup (scripts/gate-linux-setup.sh --pins) installs that go
# and the golangci-lint every golangci-lint-action in CI downloads, and checks
# each download against a sha256 of its own: this holds both pins to CI's and
# fails a pin bump that brought no sha256 row, which the setup would refuse.
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

# The Linux setup's pins. lint_pins: "<workflow>: <version>" for every
# golangci-lint-action step's version, v dropped.
lint_pins="$(awk 'FNR == 1 { n = 0 }
  /golangci\/golangci-lint-action@/ { n = 8; next }
  n > 0 && /^[[:space:]]*version:/ { v = $2; gsub(/["\047]/, "", v); sub(/^v/, "", v); print FILENAME ": " v; n = 0; next }
  n > 0 { n-- }' .github/workflows/*.yml | tr -d '\r')"
[ -n "$lint_pins" ] || { echo "FAIL gate-toolchain: no golangci-lint-action version found in .github/workflows; the check read nothing"; fail=1; }
setup="$(bash scripts/gate-linux-setup.sh --pins 2>&1)" || { echo "FAIL gate-toolchain: scripts/gate-linux-setup.sh --pins failed: $setup"; fail=1; }
rows=0 setup_lint=""
while read -r tool pin file sha; do
  [ -n "${tool:-}" ] || continue
  rows=$((rows + 1))
  case "$tool" in
    go) [ "$pin" = "$want" ] || { echo "FAIL gate-toolchain: gate-linux-setup.sh installs go $pin, go.mod's go line is $want"; fail=1; } ;;
    golangci-lint)
      setup_lint="$pin"
      while IFS= read -r lp; do
        [ -n "$lp" ] && [ "${lp##* }" != "$pin" ] && { echo "FAIL gate-toolchain: ${lp%%: *}: golangci-lint-action version ${lp##* }, gate-linux-setup.sh installs $pin (lint.yml is its pin)"; fail=1; }
      done <<<"$lint_pins" ;;
  esac
  [ "$sha" != none ] || { echo "FAIL gate-toolchain: gate-linux-setup.sh has no sha256 for $file: add its row to SHA256 there, from the vendor's checksum list its header names"; fail=1; }
done <<<"$setup"
[ "$rows" -eq 3 ] || { echo "FAIL gate-toolchain: gate-linux-setup.sh --pins printed $rows rows, want 3 (go, golangci-lint, node)"; fail=1; }
[ "$fail" -eq 0 ] && echo "ok   gate-toolchain (go $want: $(printf '%s\n' "$pins" | wc -l | tr -d ' ') CI pins, every go.mod, gate.sh's WSL steps; the Linux setup's go and golangci-lint ${setup_lint:-?} ($(printf '%s\n' "$lint_pins" | wc -l | tr -d ' ') CI pins), a sha256 for each of its downloads)"
exit "$fail"
