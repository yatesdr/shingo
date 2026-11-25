#!/usr/bin/env bash
# tier: dev
# inputs: scripts/gate-testsets.sh scripts/gate-os-tests.txt scripts/check-gate-testsets.sh
# side: linux
# check-gate-testsets.sh — pins testsets_compare (scripts/gate-testsets.sh), the
# guard that the Windows side and the Linux side of the gate give one tree's
# tests the same verdicts but for the tests scripts/gate-os-tests.txt names.
#
#   1. every test the list names exists in the tree, in its package's
#      directory (an expiry by symbol: renaming or deleting one fails here, so
#      its line goes in the same edit)
#   2. equal sets agree; the named tests in every shape they take (a skip on
#      the other side, a file not built there, subtests going with their
#      test) are excused, and the summary counts them
#   3. red, naming the test, on: a planted Linux-only skip; a test failing on
#      one side; a test only one side ran; a named test differing the other
#      way round; a named test that fails on its own side; a malformed line
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1
. "$ROOT/scripts/gate-testsets.sh"
LIST="$ROOT/scripts/gate-os-tests.txt"
TMP="$(mktemp -d 2>/dev/null || mktemp -d -t gatetestsets)"
trap 'rm -rf "$TMP"' EXIT

fails=0
fail() { echo "  FAIL gate-testsets: $*"; fails=$((fails + 1)); }

# ── 1. every named test exists ───────────────────────────────────────────
n=0
while read -r pkg test side _; do
  case "$pkg" in ''|'#'*) continue ;; esac
  n=$((n + 1))
  dir="${pkg#shingo/}"
  # Core's and the Edge's module paths are not their directories.
  case "$dir" in
    shingocore/*) dir="shingo-core/${dir#shingocore/}" ;;
    shingoedge/*) dir="shingo-edge/${dir#shingoedge/}" ;;
  esac
  grep -qs "^func $test(" "$dir"/*_test.go \
    || fail "gate-os-tests.txt names $pkg $test ($side), and $dir has no func $test: take its line out with the test"
done <"$LIST"
[ "$n" -ge 5 ] || fail "gate-os-tests.txt names $n tests; R31 found five"

# The shapes R31 saw, as gatetime testset records them.
T=$'\t'
base() {
  printf '%s\n' \
    "lockcheck${T}shingo/protocol/debuglog${T}-${T}pass" \
    "lockcheck${T}shingo/protocol/debuglog${T}TestRotate${T}pass" \
    "lockcheck${T}shingo/shared/scripts/gatetime${T}-${T}pass" \
    "lockcheck${T}shingo/shared/scripts/gatetime${T}TestStamps${T}pass" \
    "lockcheck${T}shingo/shared/scripts/gatetime/reuse${T}-${T}pass" \
    "docker,lockcheck${T}shingo/shingo-core/store${T}TestBins${T}pass"
}
win() {
  base
  printf '%s\n' \
    "lockcheck${T}shingo/protocol/debuglog${T}TestOpenUsesAppendMode${T}skip" \
    "lockcheck${T}shingo/shared/scripts/gatetime${T}TestProcs_ReadsThisProcessAndTheVM${T}pass" \
    "lockcheck${T}shingo/shared/scripts/gatetime${T}TestStayAwake_HoldsSystemAndDisplay${T}pass"
}
lin() {
  base
  printf '%s\n' \
    "lockcheck${T}shingo/protocol/debuglog${T}TestOpenUsesAppendMode${T}pass" \
    "lockcheck${T}shingo/protocol/debuglog${T}TestOpenUsesAppendMode/rotated${T}pass" \
    "lockcheck${T}shingo/shared/scripts/gatetime${T}TestOtherMachine_SaysWhatItCannotRead${T}pass" \
    "lockcheck${T}shingo/shared/scripts/gatetime/reuse${T}TestFilterProgram_EveryJumpLandsOnAReturn${T}pass"
}
cmp_sets() { testsets_compare "$TMP/w" "$TMP/l" "${1:-$LIST}" 2>&1; }

# ── 2. what agrees ──────────────────────────────────────────────────────
base >"$TMP/w"; base >"$TMP/l"
out="$(cmp_sets)"; rc=$?
{ [ "$rc" -eq 0 ] && echo "$out" | grep -q '^ok   testsets (6 verdicts compared; they differ only in the 0 tests'; } \
  || fail "two equal sets did not agree (exit $rc): $out"
win >"$TMP/w"; lin >"$TMP/l"
out="$(cmp_sets)"; rc=$?
{ [ "$rc" -eq 0 ] && echo "$out" | grep -q 'differ only in the 5 tests named in gate-os-tests.txt'; } \
  || fail "the five named differences were not excused, or not counted (exit $rc): $out"

# ── 3. what does not ────────────────────────────────────────────────────
# red_on LABEL WANT-IN-OUTPUT — the sets in $TMP/w and $TMP/l must not agree,
# and the output must name WANT.
red_on() {
  out="$(cmp_sets "${3:-}")"; rc=$?
  { [ "$rc" -ne 0 ] && echo "$out" | grep -qF -- "$2"; } || fail "$1: not red, or not naming '$2' (exit $rc): $out"
}
win >"$TMP/w"; { lin; printf '%s\n' "lockcheck${T}shingo/protocol/debuglog${T}TestRotate${T}skip"; } | grep -v "TestRotate${T}pass" >"$TMP/l"
red_on "a planted Linux-only skip" "shingo/protocol/debuglog TestRotate (-tags lockcheck): windows pass, linux skip"
win >"$TMP/w"; lin | sed "s/TestBins${T}pass/TestBins${T}fail/" >"$TMP/l"
red_on "a test failing on the Linux side only" "shingo/shingo-core/store TestBins (-tags docker,lockcheck): windows pass, linux fail"
{ win; printf '%s\n' "lockcheck${T}shingo/protocol/debuglog${T}TestNew${T}pass"; } >"$TMP/w"; lin >"$TMP/l"
red_on "a test only the Windows side ran" "TestNew (-tags lockcheck): windows pass, linux absent"
win | sed "s/TestOpenUsesAppendMode${T}skip/TestOpenUsesAppendMode${T}pass/" >"$TMP/w"
lin | sed "s/TestOpenUsesAppendMode${T}pass/TestOpenUsesAppendMode${T}skip/" | grep -v '/rotated' >"$TMP/l"
red_on "a named Linux test skipping on Linux" "TestOpenUsesAppendMode (-tags lockcheck): windows pass, linux skip; named as linux-only"
win >"$TMP/w"; lin | sed "s/TestOtherMachine_SaysWhatItCannotRead${T}pass/TestOtherMachine_SaysWhatItCannotRead${T}fail/" >"$TMP/l"
red_on "a named test failing on its own side" "TestOtherMachine_SaysWhatItCannotRead (-tags lockcheck): windows absent, linux fail"
win >"$TMP/w"; lin >"$TMP/l"
{ cat "$LIST"; echo "shingo/protocol/debuglog TestRotate solaris because"; } >"$TMP/list"
red_on "a list line whose side is neither linux nor windows" "TestRotate solaris because" "$TMP/list"

if [ "$fails" -eq 0 ]; then
  echo "ok   gate-testsets (the named OS tests exist and are excused; a planted Linux-only skip and four other differences are red)"
  exit 0
fi
exit 1
