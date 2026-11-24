# shellcheck shell=bash
#
# gate-reuse.sh — the gate's side of test-result reuse (package reuse in
# shared/scripts/gatetime; work/loc-census-2026-11-20/design-round2 SYNTH
# section 2, stage 2). Sourced by scripts/gate.sh.
#
# WHERE IT IS ON (R38, 2026-11-24). On the Linux side, a gate in a WSL mirror
# (scripts/gate-mirror.sh: synced by a Windows worktree's gate, or started
# there by hand), reuse is ON unless GATE_REUSE=off: the probe set read 47 of
# 47 twice on this laptop's WSL ext4 (work/loc-census-2026-11-20/gate-fast/
# lanes-A/wsl). The Windows side NEVER reuses, GATE_REUSE=on included: nothing
# there traces a raw-syscall or package-var read (probes 23 and 25) or a
# child's read of an ignored file or one outside the repo (36, 45), so a stale
# pass could be served. Any other host (CI, the houseserver) is off unless
# GATE_REUSE=on. GitHub never reuses: that return comes before any default, no
# workflow sets GATE_REUSE (pinned by TestNoWorkflowSwitchesReuseOn), and the
# wrapper serves nothing under GITHUB_ACTIONS=true.
#
# Where it is on, every `go test` the gate runs through `gatetime exec` gets
# GATETIME_REUSE:
#   dev, push   reuse: a binary whose stored passing run matches on every
#               input, by content, is served (one marker line, then its stored
#               output); every other binary runs and, if it passes cleanly, is
#               stored in .gate/reuse.
#   cert        audit: nothing is served; for each binary the wrapper works
#               out what push would have served, runs it fresh, and a would-be
#               hit whose fresh run did not pass fails the gate (reuse_audit).
#   step        (one step by name: CI's `gate.sh scripts`, `gate.sh sim`) off.
#
# THE CANARY FIRST, EVERY GATE. `gatetime canary` runs one known package
# through the wrapper and compares the record with what that package is known
# to read, list and start. If they differ, reuse is off for this run (record
# only: nothing served, passing runs still stored) and the sentence says why.
#
# A FULL MISS EXPLAINS ITSELF (brief A2 step 4). A dev or push gate that served
# none of N > 0 binaries on the tree the store's last run had prints one line
# naming what in the key moved (reuse_explain), worded from the wrapper's
# key_diff: for a binary with no entry under its key, how that key differs
# from the nearest one stored for its package (package reuse, the recent
# index). The tree is GATE_TREE (the mirror's, from scripts/gate-mirror.sh);
# the store's last is kept in $GATETIME_STORE/last-run ("<tree> <run>").
#
# Needs GATETIME_BIN (the built wrapper) and GATETIME_IDENTITY (this run's
# identity file), both from gate-timing.sh; GATE_TIER, GATE_RUN, GATE_SHA, ROOT.

REUSE_MODE=""
REUSE_NOTE=""
REUSE_LAST_TREE=""   # the tree of the store's last run, read by reuse_setup

# reuse_side — where this gate's tests run: windows (Git Bash: the Windows
# side, by GATE_SIDE=windows or a fallback), mirror (the Linux side: a gate in a
# WSL mirror, GATE_IN_MIRROR from gate.sh's top), or other.
reuse_side() {
  case "${OSTYPE:-}" in msys*|cygwin*) echo windows; return ;; esac
  if [ -n "${GATE_IN_MIRROR:-}" ]; then echo mirror; else echo other; fi
}

# reuse_settle — in a mirror, waits out reuse's change-time slack (a run whose
# inputs changed less than 2 s before it started is never stored: package
# reuse's changedSince) after the sync's last write, its refs/gate-mirror/synced
# update (scripts/gate-mirror.sh). A mirror's gate starts the instant its sync
# has written the edited files, so without the wait the canary, whose key is the
# whole tree, refused to store its run and reuse was off for the gate after
# every edit (2026-11-24: "changed during the run: .../key.go", the gate started
# in the second its sync ended). Waits 2.2 s at most; no such ref, no wait.
reuse_settle() {
  local ref="$ROOT/.git/refs/gate-mirror/synced" t left
  [ -f "$ref" ] || return 0
  t="$(stat -c %.3Y "$ref" 2>/dev/null)"
  # A stat with no sub-second times: whole seconds, one more to be safe.
  [[ "$t" =~ ^[0-9]+\.[0-9]+$ ]] || t="$(( $(stat -c %Y "$ref" 2>/dev/null || echo 0) + 1 ))"
  left="$(awk -v t="$t" -v n="$EPOCHREALTIME" 'BEGIN { d = t + 2.2 - n; if (d > 0) printf "%.2f", d }')"
  [ -n "$left" ] && sleep "$left"
  return 0
}

# reuse_setup — decides this run's mode and exports the wrapper's switches.
# Prints nothing; the sentence reads REUSE_MODE and REUSE_NOTE.
reuse_setup() {
  REUSE_MODE="" REUSE_NOTE=""
  # Each gate decides for itself: a gate a guard runs inside this one must not
  # inherit this one's switches, store or log.
  unset GATETIME_REUSE GATETIME_STORE GATETIME_REUSE_LOG GATETIME_RUN GATETIME_SHA
  local side want
  side="$(reuse_side)"
  case "$side" in
    windows)
      case "${GATE_TIER:-}:${GATE_REUSE:-}" in
        dev:*|push:*|cert:*|*:on) REUSE_NOTE="reuse: off on the Windows side (R38)" ;;
      esac
      return 0 ;;
    mirror) want="${GATE_REUSE:-on}" ;;
    *) want="${GATE_REUSE:-off}" ;;
  esac
  # GitHub before any default can take effect.
  if [ "${GITHUB_ACTIONS:-}" = true ]; then
    [ "$want" = on ] && REUSE_NOTE="reuse: off on GitHub"
    return 0
  fi
  if [ "$want" != on ]; then
    # Off where the default is on: the sentence says who turned it off.
    [ "$side" = mirror ] && case "${GATE_TIER:-}" in dev|push|cert) REUSE_NOTE="reuse: off (GATE_REUSE=$want)" ;; esac
    return 0
  fi
  case "${GATE_TIER:-}" in
    dev|push) REUSE_MODE=reuse ;;
    cert) REUSE_MODE=audit ;;
    *)
      # A single step never reuses; it says so only when reuse was asked for.
      [ "${GATE_REUSE:-}" = on ] && REUSE_NOTE="reuse: off for a single step"
      return 0 ;;
  esac
  if [ -z "${GATETIME_BIN:-}" ] || [ -z "${GATETIME_IDENTITY:-}" ]; then
    REUSE_MODE="" REUSE_NOTE="reuse: off (no gatetime binary or identity file this run)"
    return 0
  fi
  [ "$side" = mirror ] && reuse_settle
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
  REUSE_LAST_TREE=""
  [ -r "$GATETIME_STORE/last-run" ] && read -r REUSE_LAST_TREE _ <"$GATETIME_STORE/last-run"
  return 0
}

# reuse_explain — at the end of a gate whose binaries went through the wrapper:
# when it served none of them in reuse mode, on the tree the store's last run
# had, the one line (A FULL MISS EXPLAINS ITSELF, above); then this run's tree
# becomes the store's last. On another tree the misses are its files'; cert's
# audit and a record-only run serve nothing by design.
reuse_explain() {
  local log="${GATETIME_REUSE_LOG:-}" c served all tree="${GATE_TREE:-}" m n why
  [ -n "$REUSE_MODE" ] && [ -n "$log" ] && [ -s "$log" ] || return 0
  c="$(reuse_counts)"; served="${c% *}"; all="${c#* }"
  if [ "$REUSE_MODE" = reuse ] && [ "$served" = 0 ] && [ -n "$tree" ] && [ "$tree" = "$REUSE_LAST_TREE" ]; then
    m="$(grep -c '"key_diff":"' "$log")"
    if [ "$m" -gt 0 ]; then
      # The most common reason; ties go to the first in sort order.
      read -r n why < <(sed -n 's/.*"key_diff":"\([^"]*\)".*/\1/p' "$log" | LC_ALL=C sort | uniq -c | LC_ALL=C sort -k1,1nr -s | head -1)
      why="the key's $why"
      [ "$n" -lt "$m" ] && why="$why ($n of the $m binaries compared)"
    else
      why="$(sed -n 's/.*"miss":"\([^"]*\)".*/\1/p' "$log" | grep -v '^no entry for this binary' | head -1)"
      if [ -n "$why" ]; then why="no part of the key differed ($why)"
      else why="no binary had an earlier key in the store to compare"; fi
    fi
    echo "reuse: 0 of $all served on the tree of the last stored run (${tree:0:12}): $why"
  fi
  [ -n "$tree" ] && printf '%s %s\n' "$tree" "${GATE_RUN:-}" >"${GATETIME_STORE:-$ROOT/.gate/reuse}/last-run" 2>/dev/null
  return 0
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
