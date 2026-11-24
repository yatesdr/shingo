# shellcheck shell=bash
#
# gate-testsets.sh — the guard that the Windows side and the Linux side of the
# gate (scripts/gate-mirror.sh) run the same tests to the same verdicts for one
# tree, but for the tests named, each with its reason, in
# scripts/gate-os-tests.txt (brief A 1.5; R31 fired on exactly this: one test
# that failed on WSL only, beside five OS-conditional ones).
#
# OPT IN, because it costs. GATE_TESTSET=on runs the gate's native Go test steps
# (unit, docker, and push's sim without -race) through `go test -json` and
# `gatetime testset` (shared/scripts/gatetime/testset.go), which writes the
# same text log the steps always wrote and records each verdict:
#     <tags> TAB <package> TAB <test> TAB <pass|fail|skip>
# The run's verdicts go to
#     $GATE_TESTSET_DIR/<tree>.<mode>.<side>.tsv   (default .gate/testsets;
#     the Linux side writes into the Windows worktree's, through /mnt/c)
# and when the other side's file for the same tree and mode is there, the two
# are compared: a difference this list does not name fails the gate. So
#     GATE_TESTSET=on GATE_SIDE=windows bash scripts/gate.sh full --unscoped
#     GATE_TESTSET=on bash scripts/gate.sh full --unscoped
# compare cert's sets; `bash scripts/gate-testsets.sh compare WIN LINUX` compares
# two files by hand. -json makes every test verbose, so a run with it on is not
# a timing. Race, and cert's -race sim, run in WSL on both sides (Linux both
# times), so they are not recorded: there is no second OS to compare.
#
# WHAT A NAMED TEST MAY DO. It passes on the side its line names and skips, or
# is not built, on the other; its subtests go with it. Failing anywhere, or
# differing the other way round, is a difference like any other.

TESTSET_DIR_RUN=""   # this run's per-step records, while GATE_TESTSET=on

# testsets_compare WINDOWS_SET LINUX_SET [LIST] — prints the comparison; 0 when
# every difference is one LIST names (default scripts/gate-os-tests.txt).
testsets_compare() {
  local win="$1" lin="$2" list="${3:-${ROOT:-.}/scripts/gate-os-tests.txt}" out
  [ -r "$win" ] && [ -r "$lin" ] && [ -r "$list" ] || { echo "testsets: cannot read $win, $lin or $list"; return 2; }
  out="$(awk -F'\t' -v list="$list" '
    BEGIN {
      while ((getline l < list) > 0) {
        if (l ~ /^#/ || l ~ /^[ \t]*$/) continue
        n = split(l, f, " ")
        if (n < 4 || (f[3] != "linux" && f[3] != "windows")) { print "LIST\t" l; continue }
        named[f[1] SUBSEP f[2]] = f[3]
      }
    }
    FILENAME == ARGV[1] { k = $1 SUBSEP $2 SUBSEP $3; w[k] = $4; seen[k] = 1; next }
    { k = $1 SUBSEP $2 SUBSEP $3; l2[k] = $4; seen[k] = 1 }
    END {
      for (k in seen) {
        total++
        a = (k in w) ? w[k] : "absent"; b = (k in l2) ? l2[k] : "absent"
        if (a == b) continue
        split(k, p, SUBSEP); top = p[3]; sub(/\/.*/, "", top)
        side = named[p[2] SUBSEP top]
        if (side == "linux" && b == "pass" && (a == "skip" || a == "absent")) { used[p[2] SUBSEP top] = 1; continue }
        if (side == "windows" && a == "pass" && (b == "skip" || b == "absent")) { used[p[2] SUBSEP top] = 1; continue }
        printf "BAD\t%s %s (-tags %s): windows %s, linux %s%s\n", p[2], p[3], p[1], a, b, \
          (side != "" ? "; named as " side "-only, which this is not" : "")
      }
      for (k in used) nused++
      printf "SUM %d %d\n", total, nused
    }' "$win" "$lin")"
  local bad total nused
  bad="$(printf '%s\n' "$out" | sed -n 's/^BAD\t//p' | LC_ALL=C sort)"
  read -r total nused <<<"$(printf '%s\n' "$out" | sed -n 's/^SUM //p')"
  if printf '%s\n' "$out" | grep -q '^LIST'; then
    echo "FAIL testsets — $list has lines that are not <package> <test> <linux|windows> <why>:"
    printf '%s\n' "$out" | sed -n 's/^LIST\t/    /p'
    return 1
  fi
  if [ -n "$bad" ]; then
    echo "FAIL testsets — the Windows side and the Linux side disagree on tests $list does not name:"
    printf '%s\n' "$bad" | sed 's/^/    /' | head -40
    echo "  A test that differs by design gets a line there, with its reason; anything else is a Linux-side (or Windows-side) bug to find."
    return 1
  fi
  echo "ok   testsets (${total:-0} verdicts compared; they differ only in the ${nused:-0} tests named in $(basename "$list"))"
}

# testset_setup MODE — at a run's start: under GATE_TESTSET=on, a fresh record
# directory for this run's test steps. Needs gatetime (gate-timing.sh builds it
# when GATE_TESTSET=on).
testset_setup() {
  TESTSET_DIR_RUN=""
  [ "${GATE_TESTSET:-}" = on ] || return 0
  # This run records; a gate a guard starts inside it does not.
  export -n GATE_TESTSET
  case "$1" in test|docker|full|all) ;; *) return 0 ;; esac
  if [ -z "${GATETIME_BIN:-}" ]; then
    echo "gate: FAILED — GATE_TESTSET=on records through shared/scripts/gatetime, which did not build"
    return 1
  fi
  TESTSET_DIR_RUN="$ROOT/.gate/testset.$$"
  rm -rf "$TESTSET_DIR_RUN" && mkdir -p "$TESTSET_DIR_RUN"
}

# gotest_to LOG TAGS ARGS... — `go test ARGS` with its output in LOG, as every
# test step ran it; under GATE_TESTSET=on through -json and `gatetime testset`,
# which writes the same text to LOG and records each verdict (TAGS names the
# -tags in the record). The exit code is go test's.
gotest_to() {
  local log="$1" tags="$2"
  shift 2
  if [ -z "$TESTSET_DIR_RUN" ]; then
    go test "$@" >"$log" 2>&1
    return
  fi
  go test -json "$@" 2>&1 \
    | "$GATETIME_BIN" testset -tags "$tags" -record "$TESTSET_DIR_RUN/$(basename "$log" .log).tsv" >"$log"
  return "${PIPESTATUS[0]}"
}

# testset_finish — at a run's end: this side's set into GATE_TESTSET_DIR, and
# the comparison with the other side's when it is there. 1 on a difference.
testset_finish() {
  [ -n "$TESTSET_DIR_RUN" ] || return 0
  local dir="${GATE_TESTSET_DIR:-$ROOT/.gate/testsets}" tree side other key f o
  tree="${GATE_TREE:-}"
  [ -n "$tree" ] || { gate_tree_id "$ROOT/.gate/testset.index" && tree="$GATE_TREE_WORK"; }
  if _mirror_on_windows; then side=windows other=linux; else side=linux other=windows; fi
  key="$GATE_MODE${gate_scope:+-${gate_scope// /,}}"
  mkdir -p "$dir" || return 1
  f="$dir/${tree:0:12}.$key.$side.tsv" o="$dir/${tree:0:12}.$key.$other.tsv"
  cat "$TESTSET_DIR_RUN"/*.tsv 2>/dev/null | LC_ALL=C sort -u >"$f"
  rm -rf "$TESTSET_DIR_RUN"
  if [ ! -s "$o" ]; then
    echo "testsets: recorded $(wc -l <"$f" | tr -d ' ') verdicts for tree ${tree:0:12} ($key) on the $side side; the $other side's are not recorded yet"
    return 0
  fi
  if [ "$side" = windows ]; then testsets_compare "$f" "$o"; else testsets_compare "$o" "$f"; fi || return 1
  note_step "test sets (the same on both sides but for the tests named in scripts/gate-os-tests.txt)"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  case "${1:-}" in
    compare) shift; ROOT="$(cd "$(dirname "$0")/.." && pwd)"; testsets_compare "$@"; exit $? ;;
    *) echo "usage: bash scripts/gate-testsets.sh compare WINDOWS_SET LINUX_SET [LIST]" >&2; exit 2 ;;
  esac
fi
