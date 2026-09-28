#!/usr/bin/env bash
# check-plant-addresses.sh — no tracked file carries a private IPv4 address.
#
# THE RULE IS GENERIC ON PURPOSE: any RFC 1918 address (10/8, 172.16/12,
# 192.168/16) in a tracked text file fails, with ONE exemption — 192.168.1.0/24,
# the example range the docs and example configs use for "your server here".
# A guard that listed the networks it was protecting would itself be the leak it
# exists to stop, and it would bless every network it did not list.
#
# WHY. Fixtures captured from a live fleet carry the site's addressing (robot
# IPs, the fleet server's own address in every lock record), and it looks like
# test data. A private address in a new capture must fail here rather than ride
# in on that trust. Test literals and fixtures use the documentation ranges,
# TEST-NET-1/2/3 (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, RFC 5737),
# which are none of the above. To derive a committable /robotsStatus fixture
# from a capture, see scripts/anonymise-robotsstatus-fixture.py.
#
# The Go twin is shingo-core/fleet/seerrds/testdata_scan_test.go (same rule,
# over that package's fixtures, in the default test suite).
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

# An octet is 0-255, spelled without {m,n} intervals so any awk can run it.
O='(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])'
# Bounded both sides: not preceded by a digit or dot (so 110.x and 1.10.x.y
# are not read as 10/8), not followed by a digit or by ".digit" (so a longer
# dotted run is not an address). A sentence-ending "." is allowed.
PRIV="(10\\.$O\\.$O|172\\.(1[6-9]|2[0-9]|3[01])\\.$O|192\\.168\\.$O)\\.$O"
RE="(^|[^0-9.])$PRIV([^0-9.]|\\.[^0-9]|\\.?\$)"
export RE

# scan: reads "path:line:content" on stdin, prints each non-exempt address hit
# as "path:line: address". The exemption is decided per ADDRESS, not per line,
# so an example address cannot shelter a real one on the same line. The pattern
# goes in through ENVIRON, not -v: -v processes backslashes as string escapes
# and would turn every "\." into "any character".
scan() {
  awk '
    BEGIN { re = ENVIRON["RE"] }
    {
      p = index($0, ":"); rest = substr($0, p + 1); q = index(rest, ":")
      where = substr($0, 1, p + q); s = substr(rest, q + 1)
      while (match(s, re)) {
        a = substr(s, RSTART, RLENGTH)
        sub(/^[^0-9]/, "", a); sub(/[^0-9]+$/, "", a)
        if (a !~ /^192\.168\.1\./) print where " " a
        s = substr(s, RSTART + RLENGTH - 1)
        if (length(s) <= 1) break
      }
    }'
}

# LIVENESS FIRST: a pattern that matches nothing prints the same "ok" as a
# clean tree. Every probe is ASSEMBLED at run time so this file passes its own
# scan, and it goes through the same scan() the tree does.
dq() { printf '%s.%s.%s.%s' "$@"; }
bad=("$(dq 10 0 0 5)" "$(dq 10 255 1 9)" "$(dq 172 16 0 1)" "$(dq 172 31 255 254)"
     "$(dq 192 168 0 1)" "$(dq 192 168 2 30)" "url http://$(dq 10 1 2 3):8080/x"
     "$(dq 192 168 1 7) then $(dq 10 9 9 9)")
good=("$(dq 192 168 1 76)" "$(dq 192 0 2 1)" "$(dq 198 51 100 7)" "$(dq 203 0 113 9)"
      "$(dq 172 15 0 1)" "$(dq 172 32 0 1)" "$(dq 127 0 0 1)" "$(dq 110 1 2 3)"
      "v1.$(dq 10 0 0 1)" "$(dq 10 1 2 3).4" "$(dq 10 1 2 300)")
for t in "${bad[@]}"; do
  [ -n "$(printf 'probe:1:%s\n' "$t" | scan)" ] \
    || { echo "PROBE FAIL: private-address pattern missed: $t" >&2; exit 1; }
done
for t in "${good[@]}"; do
  [ -z "$(printf 'probe:1:%s\n' "$t" | scan)" ] \
    || { echo "PROBE FAIL: private-address pattern flagged: $t" >&2; exit 1; }
done

hits="$(git grep -nIE "$RE" 2>/dev/null | scan)"
if [ -n "$hits" ]; then
  echo "FAIL private IPv4 addresses in tracked files (use TEST-NET 192.0.2.0/24, 198.51.100.0/24 or 203.0.113.0/24; 192.168.1.0/24 is reserved for doc examples):"
  echo "$hits" | head -30
  exit 1
fi

echo "ok   plant addresses (no private IPv4 in tracked files; pattern proved live)"
