# shellcheck shell=bash
#
# gate-testsets.sh — the guard that the Windows side and the Linux side of the
# gate run the same tests to the same verdicts for one tree, but for the tests
# named, each with its reason, in scripts/gate-os-tests.txt (brief A 1.5; R31
# fired on exactly this: one test that failed on WSL only, beside five
# OS-conditional ones).
#
# A set is what `gatetime testset` records (shared/scripts/gatetime/testset.go),
# one verdict a line:
#     <tags> TAB <package> TAB <test> TAB <pass|fail|skip>
# `bash scripts/gate-testsets.sh compare WIN LINUX` compares two of them.
#
# WHAT A NAMED TEST MAY DO. It passes on the side its line names and skips, or
# is not built, on the other; its subtests go with it. Failing anywhere, or
# differing the other way round, is a difference like any other.

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

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  case "${1:-}" in
    compare) shift; ROOT="$(cd "$(dirname "$0")/.." && pwd)"; testsets_compare "$@"; exit $? ;;
    *) echo "usage: bash scripts/gate-testsets.sh compare WINDOWS_SET LINUX_SET [LIST]" >&2; exit 2 ;;
  esac
fi
