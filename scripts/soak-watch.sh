#!/usr/bin/env bash
# soak-watch — the phase-2 soak's only moving part.
#
# Samples soakstat on an interval and appends to a journal, so the [M] measures
# become a SERIES rather than a snapshot. Catalog 8.1 (lane-shape drift over
# hours) and 8.3 (robots committed per group over time) are not answerable from
# one reading, and neither is "did it degrade" — which is most of what a soak is
# for.
#
# It also latches violations. soakstat exits non-zero when an invariant is
# broken; a run that breaks one at 02:00 and recovers by 06:00 is a finding, and
# a watcher that only reports the final state would miss it. Every sample's
# verdict goes in the journal and the first failing sample is copied out whole.
#
# Usage, on the box running the stack:
#
#	scripts/soak-watch.sh [interval_seconds] [journal_path]
#
# Defaults: 600s, ./soak-journal.txt. Runs until killed. Reading it back:
#
#	grep '^SOAK:' soak-journal.txt          # the one-line summary series
#	grep -c 'VIOLATION' soak-journal.txt    # how many samples were unhappy
#
# Deliberately a shell script and not a Go daemon: it has no state, nothing
# depends on it, and the thing it drives already exits non-zero on its own
# judgement. A daemon here would be machinery the campaign did not earn.
set -uo pipefail

INTERVAL="${1:-600}"
JOURNAL="${2:-./soak-journal.txt}"
CORE="${SOAK_CORE_CONTAINER:-shingo-dev-core-1}"
CONFIG="${SOAK_CORE_CONFIG:-/etc/shingo/shingocore.dev.yaml}"

printf '=== soak-watch started %s · interval %ss · container %s ===\n' \
  "$(date -Is)" "$INTERVAL" "$CORE" | tee -a "$JOURNAL"

first_violation_captured=0
sample=0

while true; do
  sample=$((sample + 1))
  ts="$(date -Is)"

  # The log measures (burial shadow, steals) come in on stdin: soakstat's
  # docker: form shells out to a docker CLI that is not in the image it ships
  # in, so piping is the form that works from outside.
  out="$(docker logs "$CORE" 2>&1 | docker exec -i "$CORE" soakstat -config "$CONFIG" -log - 2>&1)"
  rc=$?

  {
    printf '\n--- sample %d · %s · soakstat exit %d ---\n' "$sample" "$ts" "$rc"
    printf '%s\n' "$out" | grep -E '^SOAK:|VIOLATION'
  } >> "$JOURNAL"

  # The first unhappy sample is worth the whole report, not just its summary
  # line: whatever broke is most legible in the state that was live when it
  # broke, and by the next sample the system may have healed it.
  if [ "$rc" -ne 0 ] && [ "$first_violation_captured" -eq 0 ]; then
    first_violation_captured=1
    {
      printf '\n===== FIRST VIOLATION, FULL REPORT · sample %d · %s =====\n' "$sample" "$ts"
      printf '%s\n' "$out"
      printf '===== end first violation =====\n\n'
    } >> "$JOURNAL"
  fi

  printf '%s  sample %d  exit %d  %s\n' "$ts" "$sample" "$rc" \
    "$(printf '%s\n' "$out" | grep -m1 '^SOAK:')"

  sleep "$INTERVAL"
done
