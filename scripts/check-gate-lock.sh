#!/usr/bin/env bash
# check-gate-lock.sh — pins scripts/gate-lock.sh, the one-gate-per-machine
# lock, and its wiring into scripts/gate.sh.
#
# Every case runs against a PRIVATE lock directory (GATE_LOCK_DIR in a temp
# dir) with second-scale windows, so this guard neither waits on nor disturbs
# the real lock — which the gate running it (`gate.sh scripts` / `full`) holds.
#
#   1. acquire writes the owner record; release removes the lock
#   1b. a bare `wait` in the holder returns (the heartbeat is no job of it)
#   2. a second acquirer waits, names the holder, and gets the lock on release
#   3. a SIGKILLed holder's heartbeat stops and a waiter takes the stale lock
#   4. a run nested inside the holder's (GATE_LOCK_HELD) does not wait on it,
#      through gate.sh too; an outsider times out and gate.sh says why
#   5. `gate.sh fmt` and `gate.sh lock-status` never take the lock
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export ROOT
cd "$ROOT" || exit 1

TMP="$(mktemp -d 2>/dev/null || mktemp -d -t gatelock)"
trap 'rm -f "$TMP/stop"; touch "$TMP/stop"; sleep 0.5; rm -rf "$TMP"' EXIT
unset GATE_LOCK_HELD GATE_NO_LOCK GATE_LOCK_TIMEOUT
export GATE_LOCK_DIR="$TMP/gate.lock"
export GATE_LOCK_HEARTBEAT=1 GATE_LOCK_STALE=4 GATE_LOCK_POLL=0.3 GATE_LOCK_REPORT=1
LIB="$ROOT/scripts/gate-lock.sh"

fails=0
fail() { echo "  FAIL gate-lock: $*"; fails=$((fails + 1)); }

# wait_for COND_CMD SECONDS — poll until the command succeeds.
wait_for() {
  local i=0 n=$(( $2 * 10 ))
  until eval "$1"; do
    i=$((i + 1)); [ "$i" -ge "$n" ] && return 1
    sleep 0.1
  done
}

# start_holder STEP — a background process that holds the lock until
# $TMP/stop exists. Sets HOLDER_PID.
start_holder() {
  rm -f "$TMP/stop"
  bash -c '. "$1"; trap gate_lock_release EXIT
           gate_lock_acquire "$2" || exit 1
           while [ ! -e "$3/stop" ]; do sleep 0.2; done' _ "$LIB" "$1" "$TMP" \
    >"$TMP/holder.out" 2>&1 &
  HOLDER_PID=$!
  # heartbeat, not owner: it is written last, so the holder is fully set up.
  wait_for '[ -e "$GATE_LOCK_DIR/heartbeat" ]' 10 || fail "holder never acquired"
}

# ── 1. acquire / release ────────────────────────────────────────────────
(
  . "$LIB"
  gate_lock_acquire "case-one" || exit 1
  grep -q '^steps=case-one$' "$GATE_LOCK_DIR/owner" || exit 2
  for k in user host pid worktree head started token; do
    grep -q "^$k=." "$GATE_LOCK_DIR/owner" || exit 3
  done
  [ -e "$GATE_LOCK_DIR/heartbeat" ] || exit 4
  gate_lock_release
  [ ! -e "$GATE_LOCK_DIR" ] || exit 5
)
rc=$?; [ "$rc" -eq 0 ] || fail "acquire/release (exit $rc)"

# ── 1b. a bare `wait` in the holder returns ─────────────────────────────
# gate.sh's tests step collects its module runs with a bare `wait`, which
# waits for EVERY child job of the shell; a heartbeat started as one of them
# lives as long as the holder, so the gate would wait on it forever.
rm -f "$TMP/waited"
bash -c '. "$1"; trap gate_lock_release EXIT
         gate_lock_acquire "bare-wait" || exit 1
         ( sleep 0.3 ) &
         wait
         touch "$2/waited"' _ "$LIB" "$TMP" >"$TMP/barewait.out" 2>&1 &
BW_PID=$!
wait_for '[ -e "$TMP/waited" ]' 5 || fail "a bare wait in the holder never returned (the heartbeat is a job of the gate's shell)"
wait "$BW_PID" 2>/dev/null   # it exits on its own; its EXIT trap releases
wait_for '[ ! -e "$GATE_LOCK_DIR" ]' 5 || { rm -rf "$GATE_LOCK_DIR"; fail "lock left behind after case 1b"; }

# ── 2. a second acquirer waits and names the holder ───────────────────────
start_holder "holder-step"
bash -c '. "$1"; gate_lock_acquire waiter && echo ACQUIRED; gate_lock_release' _ "$LIB" \
  >"$TMP/waiter.out" 2>&1 &
WAITER=$!
wait_for 'grep -q "waiting for .*holder-step" "$TMP/waiter.out"' 10 \
  || fail "waiter did not name the holder: $(cat "$TMP/waiter.out")"
grep -q ACQUIRED "$TMP/waiter.out" && fail "waiter acquired a held lock"
touch "$TMP/stop"
wait "$HOLDER_PID"
wait_for 'grep -q ACQUIRED "$TMP/waiter.out"' 10 || fail "waiter never acquired after release"
wait "$WAITER"
[ ! -e "$GATE_LOCK_DIR" ] || fail "lock left behind after case 2"

# ── 3. SIGKILLed holder: heartbeat stops, lock is taken as stale ─────────
start_holder "doomed"
kill -9 "$HOLDER_PID" 2>/dev/null
wait "$HOLDER_PID" 2>/dev/null
sleep 2.5   # > one heartbeat interval: the loop has seen the pid go
m1="$(stat -c %Y "$GATE_LOCK_DIR/heartbeat" 2>/dev/null)"
sleep 2.5
m2="$(stat -c %Y "$GATE_LOCK_DIR/heartbeat" 2>/dev/null)"
[ -n "$m1" ] && [ "$m1" = "$m2" ] || fail "heartbeat still moving after SIGKILL ($m1 -> $m2)"
bash -c '. "$1"; gate_lock_acquire rescuer && echo ACQUIRED; gate_lock_release' _ "$LIB" \
  >"$TMP/rescuer.out" 2>&1 &
RESCUER=$!
wait_for 'grep -q ACQUIRED "$TMP/rescuer.out"' 20 \
  || fail "stale lock never taken: $(cat "$TMP/rescuer.out")"
grep -q "taking the lock from .*doomed" "$TMP/rescuer.out" || fail "takeover did not name the dead holder: $(cat "$TMP/rescuer.out")"
wait "$RESCUER"
[ ! -e "$GATE_LOCK_DIR" ] || fail "lock left behind after case 3"

# ── 4. nested runs do not deadlock; outsiders time out ──────────────────
out="$(bash -c '. "$1"; trap gate_lock_release EXIT
  gate_lock_acquire outer || exit 1
  bash -c ". \"$1\"; GATE_LOCK_TIMEOUT=3 gate_lock_acquire inner && echo NESTED-OK"
  GATE_MODULES=protocol GATE_LOCK_TIMEOUT=30 bash "$2" vet 2>&1 | grep -E "^ok   vet|gate lock"
' _ "$LIB" "$ROOT/scripts/gate.sh" 2>&1)"
echo "$out" | grep -q NESTED-OK || fail "nested acquire waited on its own run: $out"
echo "$out" | grep -q '^ok   vet' || fail "nested gate.sh vet did not run: $out"
echo "$out" | grep -q 'gate lock' && fail "nested gate.sh waited on the lock: $out"

start_holder "busy"
bash -c '. "$1"; GATE_LOCK_TIMEOUT=2 gate_lock_acquire outsider' _ "$LIB" >/dev/null 2>&1 \
  && fail "an outsider acquired a held lock"
out="$(GATE_MODULES=protocol GATE_LOCK_TIMEOUT=2 bash scripts/gate.sh vet 2>&1)"; rc=$?
{ [ "$rc" -ne 0 ] && echo "$out" | grep -q 'gate lock not acquired'; } \
  || fail "gate.sh vet did not queue on the lock (exit $rc): $out"

# ── 5. fmt and lock-status never take the lock ──────────────────────────
out="$(GATE_LOCK_TIMEOUT=2 bash scripts/gate.sh fmt 2>&1)"
echo "$out" | grep -q 'gate lock' && fail "gate.sh fmt touched the lock: $out"
echo "$out" | grep -qE '^(ok|FAIL) +gofmt' || fail "gate.sh fmt did not run: $out"
out="$(bash scripts/gate.sh lock-status 2>&1)"
echo "$out" | grep -q 'steps=busy' || fail "lock-status did not name the holder: $out"
touch "$TMP/stop"
wait "$HOLDER_PID"
bash scripts/gate.sh lock-status 2>&1 | grep -q 'gate lock: free' || fail "lock-status not free after release"

if [ "$fails" -eq 0 ]; then
  echo "ok   gate-lock (acquire, wait, SIGKILL takeover, nested, fmt exempt)"
  exit 0
fi
exit 1
