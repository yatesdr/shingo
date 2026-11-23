# shellcheck shell=bash
#
# gate-reuse.sh — the gate's side of test-result reuse (package reuse in
# shared/scripts/gatetime; work/loc-census-2026-11-20/design-round2 SYNTH
# section 2, stage 2). Sourced by scripts/gate.sh. OFF unless GATE_REUSE=on.
#
# With GATE_REUSE=on, every `go test` the gate runs through `gatetime exec`
# gets GATETIME_REUSE:
#   dev, push   reuse: a binary whose stored passing run matches on every
#               input, by content, is served (one marker line, then its stored
#               output); every other binary runs and, if it passes cleanly, is
#               stored in .gate/reuse.
#   cert        audit: nothing is served; for each binary the wrapper works
#               out what push would have served, runs it fresh, and a would-be
#               hit whose fresh run did not pass fails the gate (reuse_audit).
#   step        (one step by name: CI's `gate.sh scripts`, `gate.sh sim`) off.
# GitHub never reuses: no workflow sets GATE_REUSE (pinned by
# TestNoWorkflowSwitchesReuseOn) and the wrapper serves nothing under
# GITHUB_ACTIONS=true.
#
# THE CANARY FIRST, EVERY GATE. `gatetime canary` runs one known package
# through the wrapper and compares the record with what that package is known
# to read, list and start. If they differ, reuse is off for this run (record
# only: nothing served, passing runs still stored) and the sentence says why.
#
# Needs GATETIME_BIN (the built wrapper) and GATETIME_IDENTITY (this run's
# identity file), both from gate-timing.sh; GATE_TIER, GATE_RUN, GATE_SHA, ROOT.

REUSE_MODE=""
REUSE_NOTE=""

# reuse_setup — decides this run's mode and exports the wrapper's switches.
# Prints nothing; the sentence reads REUSE_MODE and REUSE_NOTE.
reuse_setup() {
  REUSE_MODE="" REUSE_NOTE=""
  # Each gate decides for itself: a gate a guard runs inside this one must not
  # inherit this one's switches, store or log.
  unset GATETIME_REUSE GATETIME_STORE GATETIME_REUSE_LOG GATETIME_RUN GATETIME_SHA
  [ "${GATE_REUSE:-}" = on ] || return 0
  case "${GATE_TIER:-}" in
    dev|push) REUSE_MODE=reuse ;;
    cert) REUSE_MODE=audit ;;
    *) REUSE_NOTE="reuse: off for a single step"; return 0 ;;
  esac
  if [ "${GITHUB_ACTIONS:-}" = true ]; then
    REUSE_MODE="" REUSE_NOTE="reuse: off on GitHub"
    return 0
  fi
  if [ -z "${GATETIME_BIN:-}" ] || [ -z "${GATETIME_IDENTITY:-}" ]; then
    REUSE_MODE="" REUSE_NOTE="reuse: off (no gatetime binary or identity file this run)"
    return 0
  fi
  # The canary's own exit code decides, not the last command of a pipeline.
  local line rc
  line="$("$GATETIME_BIN" canary -root "$ROOT" -identity "$GATETIME_IDENTITY" 2>&1)"; rc=$?
  line="$(printf '%s\n' "$line" | tr -d '\r' | sed '/^$/d' | tail -1)"
  if [ "$rc" -ne 0 ]; then
    [ "$REUSE_MODE" = reuse ] && REUSE_MODE=record
    REUSE_NOTE="reuse: OFF this run, the canary differs: ${line#FAIL reuse canary: }"
  fi
  export GATETIME_REUSE="$REUSE_MODE"
  export GATETIME_STORE="$ROOT/.gate/reuse"
  export GATETIME_REUSE_LOG="$ROOT/.gate/reuse-$$.log"
  export GATETIME_RUN="$GATE_RUN" GATETIME_SHA="$GATE_SHA"
  rm -f "$GATETIME_REUSE_LOG"
}

# reuse_counts — "<served> <binaries>" from this run's decision log.
reuse_counts() {
  local log="${GATETIME_REUSE_LOG:-}" all served
  [ -n "$log" ] && [ -s "$log" ] || { echo "0 0"; return; }
  all="$(wc -l <"$log" | tr -d ' ')"
  served="$(grep -c '"decision":"reuse"' "$log")"
  echo "$served $all"
}

# reuse_summary — the sentence's words for reuse, or nothing when it was off
# and nobody asked for it.
reuse_summary() {
  local c served all
  if [ -z "$REUSE_MODE" ]; then
    [ -n "$REUSE_NOTE" ] && echo "$REUSE_NOTE"
    return 0
  fi
  c="$(reuse_counts)"; served="${c% *}"; all="${c#* }"
  case "$REUSE_MODE" in
    reuse)  echo "reuse: $served of $all test binaries served from .gate/reuse (canary ok)" ;;
    record) echo "$REUSE_NOTE; $all test binaries run, none served" ;;
    audit)  echo "reuse audit: $all test binaries run fresh, $(grep -c '"audit":"agree"' "$GATETIME_REUSE_LOG" 2>/dev/null) would-be hits agreed, $(grep -c '"audit":"mismatch"' "$GATETIME_REUSE_LOG" 2>/dev/null) disagreed" ;;
  esac
}

# reuse_audit — cert's verdict on what push would have served: fails, naming
# each binary, when any would-be hit did not pass fresh.
reuse_audit() {
  [ "$REUSE_MODE" = audit ] || return 0
  local log="${GATETIME_REUSE_LOG:-}" bad
  [ -n "$log" ] && [ -s "$log" ] || return 0
  bad="$(grep '"audit":"mismatch"' "$log" | sed -n 's/.*"package":"\([^"]*\)".*/\1/p' | sort -u)"
  [ -z "$bad" ] && return 0
  echo "FAIL reuse audit — push would have served a pass that this fresh run does not give:"
  printf '%s\n' "$bad" | sed 's/^/  /'
  echo "  (decisions: $log)"
  return 1
}
