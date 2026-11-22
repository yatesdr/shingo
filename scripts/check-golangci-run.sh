#!/usr/bin/env bash
# tier: dev
# inputs: scripts/golangci-run.sh scripts/check-golangci-run.sh scripts/gate.sh scripts/check-test-error-discard-ratchet.sh
# check-golangci-run.sh — pins scripts/golangci-run.sh (golangci_run, the
# bounded retry on golangci-lint's lock handoff) against a fake binary, and its
# wiring into the two scripts that run golangci-lint.
#
#   1. exit 3 with "parallel golangci-lint is running", twice, then 0: passes,
#      and ERRFILE says it took 3 runs
#   2. exit 3 without the message: not retried
#   3. exit 1 (issues found): not retried
#   4. exit 3 with the message every time: stops at GOLANGCI_RETRIES and fails
#   5. gate.sh's step_lint and the ratchet run golangci-lint only through it
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d 2>/dev/null || mktemp -d -t golangcirun)"
trap 'rm -rf "$TMP"' EXIT
# shellcheck source=/dev/null
source "$ROOT/scripts/golangci-run.sh"
export GOLANGCI_RETRY_SLEEP=0 GOLANGCI_RETRIES=4

# fake MODE: a golangci-lint stand-in counting its runs in $TMP/n.
cat >"$TMP/fake" <<'FAKE'
#!/usr/bin/env bash
n=$(( $(cat "$FAKE_DIR/n" 2>/dev/null || echo 0) + 1 )); echo "$n" >"$FAKE_DIR/n"
case "$FAKE_MODE" in
  lock-twice) [ "$n" -le 2 ] && { echo "Error: parallel golangci-lint is running" >&2; exit 3; }; exit 0 ;;
  three-other) echo "Error: something else" >&2; exit 3 ;;
  issues) echo "x.go:1: issue"; exit 1 ;;
  lock-always) echo "Error: parallel golangci-lint is running" >&2; exit 3 ;;
esac
FAKE
chmod +x "$TMP/fake"
export GOLANGCI_LINT="$TMP/fake" FAKE_DIR="$TMP"

fail=0
run_case() { # name mode want-rc want-runs
  local rc=0
  rm -f "$TMP/n" "$TMP/err"
  FAKE_MODE=$2 golangci_run "$TMP/err" run >/dev/null || rc=$?
  local runs; runs=$(cat "$TMP/n")
  if [ "$rc" -ne "$3" ] || [ "$runs" -ne "$4" ]; then
    echo "FAIL golangci-run: $1: exit $rc after $runs run(s), want exit $3 after $4"
    fail=1
  fi
  if [ "$4" -gt 1 ] && ! grep -q "took $4 runs" "$TMP/err"; then
    echo "FAIL golangci-run: $1: the retry is not reported in ERRFILE"
    fail=1
  fi
}
run_case "a lock handoff twice" lock-twice 0 3
run_case "exit 3 for another reason" three-other 3 1
run_case "issues found" issues 1 1
run_case "the lock never frees" lock-always 3 4

for f in scripts/gate.sh scripts/check-test-error-discard-ratchet.sh; do
  if grep -nE '^[^#]*\bgolangci-lint run\b' "$ROOT/$f" >/dev/null; then
    echo "FAIL golangci-run: $f runs golangci-lint directly; run it through golangci_run (scripts/golangci-run.sh):"
    grep -nE '^[^#]*\bgolangci-lint run\b' "$ROOT/$f" | sed 's/^/       /'
    fail=1
  fi
  if ! grep -qE '^[^#]*\bgolangci_run\b' "$ROOT/$f"; then
    echo "FAIL golangci-run: $f does not call golangci_run"
    fail=1
  fi
done
[ "$fail" -eq 0 ] && echo "ok   golangci-run (retry bounded, reported; step_lint and the ratchet use it)"
exit "$fail"
