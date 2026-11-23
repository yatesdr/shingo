#!/usr/bin/env bash
# tier: dev
# inputs: scripts/gate-reuse.sh scripts/check-reuse-wiring.sh
# check-reuse-wiring.sh — the gate's reuse switch (scripts/gate-reuse.sh), driven
# with a fake gatetime: what each tier exports, that the canary's own exit code
# turns reuse off, that GitHub never serves, the sentence's counts, and cert's
# audit failing on a mismatch. The wrapper's own lookup is pinned by package
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

# setup <tier> [VAR=value...] — prints "mode|note|GATETIME_REUSE" after reuse_setup.
# The gate running this guard may itself have GATE_REUSE=on, and GitHub sets
# GITHUB_ACTIONS=true: each row clears both and passes only what it means.
setup() {
  local tier="$1"; shift
  env -u GATETIME_REUSE -u GATE_REUSE -u GITHUB_ACTIONS "$@" GATE_TIER="$tier" ROOT="$TMP" GATETIME_BIN="$TMP/gatetime" GATETIME_IDENTITY="$TMP/identity" \
    GATE_RUN=r GATE_SHA=s bash -c '. "$1"; reuse_setup; printf "%s|%s|%s" "$REUSE_MODE" "$REUSE_NOTE" "${GATETIME_REUSE-unset}"' _ "$ROOT/scripts/gate-reuse.sh"
}

row "off by default" "||unset" "$(setup dev)"
row "an outer gate's switch does not leak into a gate that is off" "||unset" "$(setup dev GATETIME_REUSE=reuse)"
row "dev serves" "reuse||reuse" "$(setup dev GATE_REUSE=on)"
row "push serves" "reuse||reuse" "$(setup push GATE_REUSE=on)"
row "cert audits" "audit||audit" "$(setup cert GATE_REUSE=on)"
row "a single step is off" "|reuse: off for a single step|unset" "$(setup step GATE_REUSE=on)"
row "GitHub never serves" "|reuse: off on GitHub|unset" "$(setup push GATE_REUSE=on GITHUB_ACTIONS=true)"
row "a failed canary turns serving off" \
  "record|reuse: OFF this run, the canary differs: no open of testdata/in.txt by the binary|record" \
  "$(setup dev GATE_REUSE=on FAKE_CANARY_RC=1)"
row "a failed canary leaves cert auditing" \
  "audit|reuse: OFF this run, the canary differs: no open of testdata/in.txt by the binary|audit" \
  "$(setup cert GATE_REUSE=on FAKE_CANARY_RC=1)"
row "no wrapper, no reuse" "|reuse: off (no gatetime binary or identity file this run)|unset" \
  "$(env -u GITHUB_ACTIONS GATE_TIER=dev ROOT="$TMP" GATE_REUSE=on bash -c '. "$1"; GATETIME_BIN= reuse_setup; printf "%s|%s|%s" "$REUSE_MODE" "$REUSE_NOTE" "${GATETIME_REUSE-unset}"' _ "$ROOT/scripts/gate-reuse.sh")"

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

[ "$fail" -eq 0 ] && echo "ok   reuse wiring (off by default; dev and push serve, cert audits, steps and GitHub off; the canary's exit decides; counts; the audit fails a mismatch)"
exit "$fail"
