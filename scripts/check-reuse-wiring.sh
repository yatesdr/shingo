#!/usr/bin/env bash
# tier: dev
# inputs: scripts/gate-reuse.sh scripts/check-reuse-wiring.sh
# side: linux
# check-reuse-wiring.sh — the gate's reuse switch (scripts/gate-reuse.sh), driven
# with a fake gatetime: what each tier exports on each side (on by default in a
# WSL mirror, never on the Windows side, off elsewhere unless asked: R38), that
# the canary's own exit code turns reuse off, that GitHub never serves, the
# sentence's counts, and cert's audit failing on a mismatch. The wrapper's own lookup is pinned by package
# reuse's tests and its probe set (shared/scripts/gatetime/reuse/probes).
#
# Exit 0 = every row holds, 1 = a row failed.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail=0
row() { # name want got
  if [ "$2" != "$3" ]; then echo "FAIL reuse wiring: $1: want '$2', got '$3'" >&2; fail=1; fi
}

# A fake gatetime: `canary` exits $FAKE_CANARY_RC with its one line.
cat >"$TMP/gatetime" <<'FAKE'
#!/usr/bin/env bash
if [ "$1" = canary ]; then
  if [ "${FAKE_CANARY_RC:-0}" = 0 ]; then echo "ok   reuse canary (fake)"; exit 0; fi
  echo "FAIL reuse canary: no open of testdata/in.txt by the binary"; echo; exit 1
fi
exit 2
FAKE
chmod +x "$TMP/gatetime"
: >"$TMP/identity"

# setup <side> <tier> [VAR=value...] — prints "mode|note|GATETIME_REUSE" after
# reuse_setup, as gate.sh would run it on <side>: windows (Git Bash's OSTYPE),
# mirror (a gate in a WSL mirror: GATE_IN_MIRROR, set by gate.sh) or other (any
# other host: CI, the houseserver). The side is named, never the host's own, so
# every row reads the same on Windows, in a mirror and on GitHub.
# The gate running this guard may itself have GATE_REUSE=on, and GitHub sets
# GITHUB_ACTIONS=true: each row clears both and passes only what it means.
setup() {
  local side="$1" tier="$2" os=linux-gnu mirror=""; shift 2
  [ "$side" = windows ] && os=msys
  [ "$side" = mirror ] && mirror=1
  env -u GATETIME_REUSE -u GATE_REUSE -u GITHUB_ACTIONS -u GATE_IN_MIRROR ${mirror:+GATE_IN_MIRROR=1} "$@" GATE_TIER="$tier" ROOT="$TMP" GATETIME_BIN="$TMP/gatetime" GATETIME_IDENTITY="$TMP/identity" \
    GATE_RUN=r GATE_SHA=s bash -c 'OSTYPE="$2"; . "$1"; reuse_setup; printf "%s|%s|%s" "$REUSE_MODE" "$REUSE_NOTE" "${GATETIME_REUSE-unset}"' _ "$ROOT/scripts/gate-reuse.sh" "$os"
}

row "off by default" "||unset" "$(setup other dev)"
row "an outer gate's switch does not leak into a gate that is off" "||unset" "$(setup other dev GATETIME_REUSE=reuse)"
row "dev serves" "reuse||reuse" "$(setup other dev GATE_REUSE=on)"
row "push serves" "reuse||reuse" "$(setup other push GATE_REUSE=on)"
row "cert audits" "audit||audit" "$(setup other cert GATE_REUSE=on)"
row "a single step is off" "|reuse: off for a single step|unset" "$(setup other step GATE_REUSE=on)"
row "GitHub never serves" "|reuse: off on GitHub|unset" "$(setup other push GATE_REUSE=on GITHUB_ACTIONS=true)"
row "a failed canary turns serving off" \
  "record|reuse: OFF this run, the canary differs: no open of testdata/in.txt by the binary|record" \
  "$(setup other dev GATE_REUSE=on FAKE_CANARY_RC=1)"
row "a failed canary leaves cert auditing" \
  "audit|reuse: OFF this run, the canary differs: no open of testdata/in.txt by the binary|audit" \
  "$(setup other cert GATE_REUSE=on FAKE_CANARY_RC=1)"
# R38: on by default on the Linux side (a mirror's gate), never on the Windows
# side, never on GitHub.
row "a mirror's dev serves by default" "reuse||reuse" "$(setup mirror dev)"
row "a mirror's push serves by default" "reuse||reuse" "$(setup mirror push)"
row "a mirror's cert audits by default, serving nothing" "audit||audit" "$(setup mirror cert)"
row "GATE_REUSE=off turns a mirror's reuse off, and says so" "|reuse: off (GATE_REUSE=off)|unset" "$(setup mirror dev GATE_REUSE=off)"
row "an outer gate's switch does not leak into a mirror gate that is off" "|reuse: off (GATE_REUSE=off)|unset" \
  "$(setup mirror push GATE_REUSE=off GATETIME_REUSE=reuse)"
row "a mirror's single step is off, quietly" "||unset" "$(setup mirror step)"
row "the Windows side never reuses, GATE_REUSE=on included" "|reuse: off on the Windows side (R38)|unset" \
  "$(setup windows dev GATE_REUSE=on)"
row "the Windows side's push says reuse is off" "|reuse: off on the Windows side (R38)|unset" "$(setup windows push)"
row "the Windows side's cert does not audit" "|reuse: off on the Windows side (R38)|unset" "$(setup windows cert GATE_REUSE=on)"
row "GitHub never reuses, even where the default is on" "|reuse: off on GitHub|unset" "$(setup mirror push GITHUB_ACTIONS=true)"
side() { # OSTYPE [VAR=value...] — reuse_side as gate.sh's shell would answer it
  local os="$1"; shift
  env -u GATE_IN_MIRROR "$@" bash -c 'OSTYPE="$2"; . "$1"; reuse_side' _ "$ROOT/scripts/gate-reuse.sh" "$os"
}
row "Git Bash is the Windows side" "windows windows windows" "$(side msys) $(side cygwin) $(side msys GATE_IN_MIRROR=1)"
row "a gate in a mirror is the Linux side; any other host is neither" "mirror other" "$(side linux-gnu GATE_IN_MIRROR=1) $(side linux-gnu)"

# settled AGE CALL — with a mirror's synced ref written AGE seconds ago ("now":
# just now), whether CALL (reuse_settle, or reuse_setup in a mirror's dev gate)
# "waited" or "went on". A fresh ref: "waited" when the call ended 2 s or more
# after the write, timed from the write (a lower bound, which load only adds
# to). An old ref: "waited" when the call itself took 2 s or more, timed over
# the call alone, after its shell has started and sourced the script: start-up
# is what is slow under load, and timing it made this row read "waited" on a
# laptop under 7 cores of other load (2026-11-24) and under a planted 2.5 s
# BASH_ENV, with no wait in the call.
settled() {
  local t0="$EPOCHREALTIME" s
  mkdir -p "$TMP/.git/refs/gate-mirror"
  if [ "$1" = now ]; then touch "$TMP/.git/refs/gate-mirror/synced"
  else touch -d "@$(( $(date +%s) - $1 ))" "$TMP/.git/refs/gate-mirror/synced"; fi
  s="$(env -u GATETIME_REUSE -u GATE_REUSE -u GITHUB_ACTIONS GATE_IN_MIRROR=1 GATE_TIER=dev ROOT="$TMP" \
    GATETIME_BIN="$TMP/gatetime" GATETIME_IDENTITY="$TMP/identity" GATE_RUN=r GATE_SHA=s \
    bash -c 'OSTYPE=linux-gnu; . "$1"; a="$EPOCHREALTIME"; $2 >/dev/null; echo "$a $EPOCHREALTIME"' _ "$ROOT/scripts/gate-reuse.sh" "$2")"
  rm -rf "$TMP/.git"
  awk -v t0="$t0" -v s="$s" -v age="$1" 'BEGIN {
    split(s, x, " "); w = (age == "now") ? (x[2] - t0 >= 2) : (x[2] - x[1] >= 2)
    print (w ? "waited" : "went on") }'
}
row "a mirror's gate waits out the 2 s slack after a sync that just wrote" "waited" "$(settled now reuse_settle)"
row "and goes on after one a minute old" "went on" "$(settled 60 reuse_settle)"
row "a mirror's dev gate settles before its canary" "waited" "$(settled now reuse_setup)"

row "no wrapper, no reuse" "|reuse: off (no gatetime binary or identity file this run)|unset" \
  "$(env -u GITHUB_ACTIONS -u GATE_IN_MIRROR GATE_TIER=dev ROOT="$TMP" GATE_REUSE=on bash -c 'OSTYPE=linux-gnu; . "$1"; GATETIME_BIN= reuse_setup; printf "%s|%s|%s" "$REUSE_MODE" "$REUSE_NOTE" "${GATETIME_REUSE-unset}"' _ "$ROOT/scripts/gate-reuse.sh")"

# The sentence and the audit, over a decision log.
cat >"$TMP/log" <<'LOG'
{"package":"p/a","decision":"reuse","exit":0}
{"package":"p/b","decision":"run","exit":0,"stored":true}
{"package":"p/c","decision":"reuse","exit":0}
LOG
summary() { # mode
  REUSE_MODE="$1" REUSE_NOTE="" GATETIME_REUSE_LOG="$TMP/log" bash -c '. "$1"; REUSE_MODE='"$1"'; reuse_summary' _ "$ROOT/scripts/gate-reuse.sh"
}
row "the sentence counts served binaries" "reuse: 2 of 3 test binaries served from .gate/reuse (canary ok)" "$(summary reuse)"
cat >"$TMP/log" <<'LOG'
{"package":"p/a","decision":"run","exit":0,"audit":"agree"}
{"package":"p/b","decision":"run","exit":1,"audit":"mismatch"}
{"package":"p/c","decision":"run","exit":0,"audit":"no-hit"}
LOG
row "the audit sentence" "reuse audit: 3 test binaries run fresh, 1 would-be hits agreed, 1 disagreed" "$(summary audit)"
out="$(GATETIME_REUSE_LOG="$TMP/log" bash -c '. "$1"; REUSE_MODE=audit; reuse_audit' _ "$ROOT/scripts/gate-reuse.sh")"; rc=$?
row "a mismatch fails cert" "1" "$rc"
row "the audit names the binary" "  p/b" "$(printf '%s\n' "$out" | sed -n 2p)"
GATETIME_REUSE_LOG="$TMP/log" bash -c '. "$1"; REUSE_MODE=reuse; reuse_audit' _ "$ROOT/scripts/gate-reuse.sh"
row "push does not audit" "0" "$?"

[ "$fail" -eq 0 ] && echo "ok   reuse wiring (on by default in a mirror, GATE_REUSE=off its opt-out; never on the Windows side or GitHub; elsewhere off unless asked; dev and push serve, cert audits, steps off; the canary's exit decides; counts; the audit fails a mismatch)"
exit "$fail"
