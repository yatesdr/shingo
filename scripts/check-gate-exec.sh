#!/usr/bin/env bash
# tier: dev
# inputs: scripts/gate.sh scripts/gate-timing.sh scripts/check-gate-exec.sh shared/scripts/gatetime/*
# check-gate-exec.sh — pins the record-only -exec wrapper's wiring into
# scripts/gate.sh (scripts/gate-timing.sh) and the gate run's identity.
#
#   1. `gate.sh identity` names the Postgres image, pg_up's docker run line,
#      the tools, gatetime, and gate.sh with every file it sources (read from
#      gate.sh's own `. "$ROOT/scripts/..."` lines); editing a sourced script,
#      or sourcing a new one, changes it. A copy of scripts/ in a temp dir is
#      edited, never the tree.
#   2. `gate.sh test` (protocol) runs every test binary under the wrapper: one
#      manifest per package go test ran, each carrying the sha256 of the
#      identity file beside it; the total line stamps exec=on; the gate's
#      gatetime is the build of its source alone (no VCS stamp).
#   3. GATE_EXEC=off: no manifests, exec=off, and the gate sentence says so.
#
# Every nested gate runs with a private lock, timing flag, manifest directory,
# unit-log directory and steps log, so it neither waits on nor writes over the
# gate that runs this guard.
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1
TMP="$(mktemp -d 2>/dev/null || mktemp -d -t gateexec)"
trap 'rm -rf "$TMP"' EXIT
unset GATE_LOCK_HELD GATE_NO_LOCK GATE_TIMING_TOKEN GATETIME_RUN_DIR GATE_EXEC
export GATE_LOCK_DIR="$TMP/gate.lock" GATE_TIMING_DIR="$TMP/gate.timing"
# The nested gates run on the side this guard runs on (from Git Bash a gate
# would otherwise go to the mirror), so the gatetime they build and the one
# section 2 builds to compare with are one OS's.
case "${OSTYPE:-}" in msys*|cygwin*) export GATE_SIDE=windows ;; esac

fails=0
fail() { echo "  FAIL gate-exec: $*"; fails=$((fails + 1)); }

# ── 1. the identity, and what changes it ────────────────────────────────────
mkdir -p "$TMP/copy"
cp -r "$ROOT/scripts" "$TMP/copy/scripts"
identity_of() { bash "$TMP/copy/scripts/gate.sh" identity 2>/dev/null; }
before="$(identity_of)"
for k in pg_image=postgres pg_up='docker run' go=go node= git=git bash= gatetime= 'script=scripts/gate.sh ' \
    'script=scripts/gate-lock.sh ' 'script=scripts/golangci-run.sh ' 'script=scripts/gate-timing.sh '; do
  printf '%s\n' "$before" | grep -q "^$k" || fail "the identity has no $k line: $before"
done
printf '%s\n' "$before" | grep -q '^pg_up=.* -c fsync=off ' || fail "the identity's pg_up line lacks the server flags: $before"
echo '# an edit' >>"$TMP/copy/scripts/gate-lock.sh"
after="$(identity_of)"
[ "$(printf '%s\n' "$before" | grep -v '^script=scripts/gate-lock.sh ')" = "$(printf '%s\n' "$after" | grep -v '^script=scripts/gate-lock.sh ')" ] \
  || fail "an edit to gate-lock.sh moved other identity lines"
[ "$(printf '%s\n' "$before" | grep '^script=scripts/gate-lock.sh ')" != "$(printf '%s\n' "$after" | grep '^script=scripts/gate-lock.sh ')" ] \
  || fail "an edit to a sourced script (gate-lock.sh) left the identity unchanged"
printf '# shellcheck shell=bash\n' >"$TMP/copy/scripts/gate-extra.sh"
sed -i 's#^\. "\$ROOT/scripts/gate-timing\.sh"$#&\n. "$ROOT/scripts/gate-extra.sh"#' "$TMP/copy/scripts/gate.sh"
identity_of | grep -q '^script=scripts/gate-extra.sh ' || fail "a newly sourced script is not in the identity: $(identity_of)"

# ── 2. the unit step under the wrapper ──────────────────────────────────────
run_gate() { # LABEL ENV... -- MODE
  local label="$1"
  shift
  mkdir -p "$TMP/$label/logs"
  env GATE_MANIFEST_DIR="$TMP/$label/manifests" GATE_TEST_LOG_DIR="$TMP/$label/logs" \
    GATE_STEPS_LOG="$TMP/$label/steps.log" GATE_MODULES=protocol "$@" >"$TMP/$label/out" 2>&1
}
run_gate on bash scripts/gate.sh test
ran="$(grep -cE '^(ok|FAIL)[[:space:]]+shingo/protocol' "$TMP/on/logs/test-protocol.log" 2>/dev/null)"
mdir="$(find "$TMP/on/manifests" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | head -1)"
got="$(find "$mdir" -maxdepth 1 -name '*.json' 2>/dev/null | wc -l | tr -d ' ')"
{ [ "${ran:-0}" -gt 0 ] && [ "$got" = "$ran" ]; } \
  || fail "gate.sh test ran ${ran:-0} protocol packages and wrote $got manifests ($(tail -5 "$TMP/on/out"))"
want="$(sha256sum <"$mdir/identity" 2>/dev/null | cut -d' ' -f1)"
n="$(grep -l "\"identity_sha256\": \"$want\"" "$mdir"/*.json 2>/dev/null | wc -l | tr -d ' ')"
{ [ -n "$want" ] && [ "$n" = "$got" ]; } || fail "$n of $got manifests carry the identity hash $want"
# The wrapper's sha256 is in the identity and in every reuse key, so the gate
# builds it from its source alone: with go's VCS stamp in it, any edit anywhere
# in the tree (vcs.modified) or any new commit (vcs.revision) moved every key
# (2026-11-24: a mirror's dev gate after a one-line test edit served 0 of 112).
gt_sha="$(sed -n 's/^gatetime=//p' "$mdir/identity" 2>/dev/null)"
gt_src="$TMP/gatetime-source-only$(go env GOEXE 2>/dev/null | tr -d '\r')"
( cd "$ROOT/shared" && go build -buildvcs=false -o "$gt_src" ./scripts/gatetime ) >/dev/null 2>&1
gt_want="$(sha256sum <"$gt_src" 2>/dev/null | cut -d' ' -f1)"
{ [ -n "$gt_sha" ] && [ "$gt_sha" = "$gt_want" ]; } \
  || fail "the gate's gatetime (identity: ${gt_sha:-none}) is not the build of its source alone (${gt_want:-no build}): a VCS stamp moves every reuse key with each edit"
grep -qE ' test total [0-9.]+ [^ ]+ exec=on ' "$TMP/on/steps.log" || fail "the total line does not stamp exec=on: $(cat "$TMP/on/steps.log")"

# ── 3. GATE_EXEC=off (the docker mode, which over protocol runs nothing) ────
run_gate off GATE_EXEC=off bash scripts/gate.sh docker
[ -z "$(find "$TMP/off/manifests" -name '*.json' 2>/dev/null)" ] || fail "GATE_EXEC=off still wrote manifests"
grep -q 'Test binaries ran without the gatetime wrapper (GATE_EXEC=off)' "$TMP/off/out" \
  || fail "the gate sentence does not name GATE_EXEC=off: $(tail -3 "$TMP/off/out")"
grep -qE ' docker total [0-9.]+ [^ ]+ exec=off ' "$TMP/off/steps.log" || fail "the total line does not stamp exec=off: $(cat "$TMP/off/steps.log")"

if [ "$fails" -eq 0 ]; then
  echo "ok   gate-exec (identity names its inputs and moves with a sourced script; unit binaries under the wrapper; GATE_EXEC=off)"
  exit 0
fi
exit 1
