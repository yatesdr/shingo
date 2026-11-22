#!/usr/bin/env bash
# golangci-run.sh — golangci_run, `golangci-lint` with a bounded retry on its
# lock handoff. Sourced by scripts/gate.sh (step_lint) and
# scripts/check-test-error-discard-ratchet.sh; pinned by
# scripts/check-golangci-run.sh.
#
# --allow-serial-runners makes golangci-lint WAIT for its machine-global lock
# instead of failing, and three runs started at one instant all pass with it
# (gate.sh step_lint's measurement). A handoff can still fail: the laptop
# probe's second cert run (REPORT-laptop-probe, 2026-11-21) had its scripts step
# exit 3, "parallel golangci-lint is running", with the flag on. Its reading of
# the sources, not re-verified here: the serial wait has no timeout
# (golangci-lint v2.11.4 pkg/commands/run.go), so the message means TryLock
# errored; on Windows gofrs/flock v0.13.0 reopens the lock file on every try
# and golangci-lint deletes it on release. The gate makes the contention itself:
# `full` runs lint and the push-tier ratchet side by side.
#
# golangci_run ERRFILE ARGS...
#   Runs `golangci-lint ARGS...` (GOLANGCI_LINT overrides the binary), with its
#   stderr in ERRFILE. Only exit 3 WITH that message is retried, at most
#   GOLANGCI_RETRIES runs in all (default 10), GOLANGCI_RETRY_SLEEP seconds
#   apart (default 2): any other exit is the verdict, and a genuine exit 3 still
#   fails after the bound. When it retried, a line saying how many runs it took
#   is appended to ERRFILE, so a retry is never silent. Returns the last exit.
golangci_run() {
  local err=$1 runs=1 rc
  shift
  while :; do
    rc=0
    "${GOLANGCI_LINT:-golangci-lint}" "$@" 2>"$err" || rc=$?
    if [ "$rc" -eq 3 ] && [ "$runs" -lt "${GOLANGCI_RETRIES:-10}" ] &&
        grep -q "parallel golangci-lint is running" "$err"; then
      runs=$((runs + 1))
      sleep "${GOLANGCI_RETRY_SLEEP:-2}"
      continue
    fi
    break
  done
  if [ "$runs" -gt 1 ]; then
    echo "golangci-lint: its lock handoff failed; took $runs runs (exit $rc)" >>"$err"
  fi
  return "$rc"
}
