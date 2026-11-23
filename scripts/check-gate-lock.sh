#!/usr/bin/env bash
# tier: dev
# inputs: scripts/gate.sh scripts/gate-lock.sh scripts/gate-timing.sh scripts/check-gate-lock.sh shared/scripts/gatetime/*
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
#   6. while a timed run holds the timing flag (scripts/gate-timing.sh) a heavy
#      mode refuses and names the holder, GATE_NO_LOCK=1 included; the timed
#      run's own gate (its token) runs; fmt, scope, order, cachescope and
#      lock-status never refuse; a stale heartbeat does not refuse; and the
#      real writer, `gatetime run`, holds the flag gate.sh reads
#   7. a heavy run writes its selection line and stamps its total line, with
#      the timed run's load under `gatetime run`, and `gatetime compare`
#      reads four such runs by their selection lines
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export ROOT
cd "$ROOT" || exit 1

TMP="$(mktemp -d 2>/dev/null || mktemp -d -t gatelock)"
trap 'rm -f "$TMP/stop"; touch "$TMP/stop"; sleep 0.5; rm -rf "$TMP"' EXIT
unset GATE_LOCK_HELD GATE_NO_LOCK GATE_LOCK_TIMEOUT
export GATE_LOCK_DIR="$TMP/gate.lock"
# The nested gates these cases run are not a schedule: keep them out of the
# real tree's .gate/steps.log.
export GATE_STEPS_LOG=
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

# ── 6. the timing flag: heavy modes refuse, the timed run's own passes ──────
# A private flag (GATE_TIMING_DIR) planted the way gatetime writes it
# (shared/scripts/gatetime, flag.go), then the real writer: `gatetime run`
# holding the flag that gate.sh reads, so the two sides of the format agree.
export GATE_TIMING_DIR="$TMP/gate.timing"
unset GATE_TIMING_TOKEN GATETIME_RUN_DIR
plant_flag() { # TOKEN HEARTBEAT-AGE-SECONDS
  mkdir -p "$GATE_TIMING_DIR"
  printf 'holder=pin@host\npid=1\ncwd=/x\nout=/y\ncmd=gatetime run -- the pin\nstarted=now\ntoken=%s\n' "$1" \
    >"$GATE_TIMING_DIR/owner"
  touch -d "@$(( $(date +%s) - $2 ))" "$GATE_TIMING_DIR/heartbeat"
}
plant_flag pin-token 0
for nolock in "" 1; do
  out="$(GATE_NO_LOCK=$nolock GATE_MODULES=protocol bash scripts/gate.sh simvet 2>&1)"; rc=$?
  { [ "$rc" -ne 0 ] && echo "$out" | grep -q 'REFUSED' && echo "$out" | grep -q 'cmd=gatetime run -- the pin'; } \
    || fail "gate.sh simvet${nolock:+ with GATE_NO_LOCK=1} started beside a timed run (exit $rc): $out"
  echo "$out" | grep -q '^ok   sim compile' && fail "gate.sh simvet ran its step under the timing flag: $out"
done
out="$(GATE_TIMING_TOKEN=pin-token GATE_MODULES=protocol bash scripts/gate.sh simvet 2>&1)"
echo "$out" | grep -q '^ok   sim compile' || fail "the timed run's own gate (its token) was refused: $out"
for mode in "fmt" "scope -" "order" "cachescope -" "lock-status"; do
  # shellcheck disable=SC2086 # mode is a mode and its argument
  out="$(echo protocol/x.go | bash scripts/gate.sh $mode 2>&1)"
  echo "$out" | grep -q 'REFUSED' && fail "gate.sh $mode refused under the timing flag: $out"
done
bash scripts/gate.sh lock-status 2>&1 | grep -q 'timing flag: held' || fail "lock-status did not show the timing flag"
plant_flag pin-token 300
out="$(GATE_MODULES=protocol bash scripts/gate.sh simvet 2>&1)"
{ echo "$out" | grep -q '^ok   sim compile' && echo "$out" | grep -q 'its run is gone'; } \
  || fail "a flag with a 300 s old heartbeat still refused the gate: $out"
rm -rf "$GATE_TIMING_DIR"
# The real writer, built once (go caches the build). bash by its full path: a
# native program looking up "bash" on Windows may find the WSL launcher.
BASH_BIN="$(cygpath -w "$BASH" 2>/dev/null || echo "$BASH")"
GT="$TMP/gatetime$(go env GOEXE 2>/dev/null | tr -d '\r')"
if ( cd "$ROOT/shared" && go build -o "$GT" ./scripts/gatetime ) >"$TMP/gtbuild.log" 2>&1; then
  out="$("$GT" run -out "$TMP/run1" -every 200ms -repo "$ROOT" -- "$BASH_BIN" -c \
    'GATE_TIMING_TOKEN= GATE_MODULES=protocol bash scripts/gate.sh simvet' 2>&1)"
  grep -q 'REFUSED' "$TMP/run1/cmd.log" 2>/dev/null \
    || fail "a gate without the token ran inside gatetime run: $out $(cat "$TMP/run1/cmd.log" 2>/dev/null)"
  out="$("$GT" run -out "$TMP/run2" -every 200ms -repo "$ROOT" -- "$BASH_BIN" -c \
    'GATE_MODULES=protocol bash scripts/gate.sh simvet' 2>&1)"
  grep -q '^ok   sim compile' "$TMP/run2/cmd.log" 2>/dev/null \
    || fail "gatetime run's own gate was refused: $out $(cat "$TMP/run2/cmd.log" 2>/dev/null)"
  [ -e "$GATE_TIMING_DIR" ] && fail "gatetime run left its timing flag behind"
else
  fail "could not build shared/scripts/gatetime: $(cat "$TMP/gtbuild.log")"
fi
unset GATE_TIMING_DIR

# ── 7. the steps log's selection line and total stamps, and their reader ────
# A heavy run writes one `selection` line and stamps its total line; under
# `gatetime run` the stamps carry the run's load judgement, and `gatetime
# compare` reads the selection lines of four such runs.
unset GATE_TIMING_DIR GATE_TIMING_TOKEN GATETIME_RUN_DIR
out="$(GATE_STEPS_LOG="$TMP/steps7.log" GATE_MODULES=protocol bash scripts/gate.sh simvet 2>&1)"
grep -qE '^[^ ]+ simvet selection - [^ ]+ tier=step docker=out modules=protocol p=- guards=- reused=0$' "$TMP/steps7.log" \
  || fail "no selection line, or not the one simvet chose: $(cat "$TMP/steps7.log" 2>/dev/null) $out"
grep -qE '^[^ ]+ simvet total [0-9.]+ [^ ]+ ac=(on|off|unknown) samples=1 standby=([0-9]+|unread) load=unmeasured$' "$TMP/steps7.log" \
  || fail "the total line carries no load stamps outside gatetime run: $(cat "$TMP/steps7.log" 2>/dev/null)"
if [ -x "$GT" ]; then
  export GATE_TIMING_DIR="$TMP/gate.timing7"
  for n in 1 2 3 4; do
    "$GT" run -out "$TMP/cmp$n" -every 200ms -repo "$ROOT" -- "$BASH_BIN" -c \
      "GATE_STEPS_LOG='$TMP/steps7-$n.log' GATE_MODULES=protocol bash scripts/gate.sh simvet" >/dev/null 2>&1
    grep -qE ' simvet total [0-9.]+ [^ ]+ ac=[a-z]+ samples=[0-9]+ standby=[0-9a-z]+ load=(clean|void) foreign30=[0-9.]+' "$TMP/steps7-$n.log" \
      || fail "under gatetime run the total line does not carry the run's load: $(cat "$TMP/steps7-$n.log" 2>/dev/null)"
    cp "$TMP/steps7-$n.log" "$TMP/cmp$n/steps.tsv" 2>/dev/null   # the gate logged to a private file, not .gate/steps.log
  done
  out="$("$GT" compare "$TMP/cmp1" "$TMP/cmp2" "$TMP/cmp3" "$TMP/cmp4" 2>&1)"
  { echo "$out" | grep -q '^selection: by the selection line' && ! echo "$out" | grep -q 'selection differs'; } \
    || fail "gatetime compare did not compare four equal selection lines: $out"
  unset GATE_TIMING_DIR
fi

if [ "$fails" -eq 0 ]; then
  echo "ok   gate-lock (acquire, wait, SIGKILL takeover, nested, fmt exempt, timing flag, selection and stamps)"
  exit 0
fi
exit 1
