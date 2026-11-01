#!/usr/bin/env bash
# bash scripts/kanban-rig.sh <R1|R2|R3|R4|R5|R6> — one run of the Kanban certification rig on the
# visible dev stack: compose project shingo-dev, plants/kanban-press.yaml, Core :8083,
# Edge :8081. docs/fleet-simulator/kanban.md says what each run injects and catches.
#
# It brings the stack up from nothing, seeds with Core stopped (docs/dev-env/sim.md),
# presses the run's buttons over Core and Edge HTTP, waits to the event plus settle,
# and writes under $KANBAN_RIG_OUT (default ../kanban-rig-runs/<run>-<stamp>):
#
#   verdict.txt         PASS / FAIL per criterion of the run; the exit code is theirs
#   long-orders.tsv     every order over 10 sim-min: id, payload, cause, route, minutes
#   stranded.txt        soakstat -stranded, read the moment the run ends
#   soakstat.txt        soakstat's full read
#   no-door.txt         carts standing on a window with no order to leave by
#   kanban-cards.jsonl  $KANBAN_API every 30 s
#   windows.tsv         each window's cart every 30 s, with its live orders
#   lit.jsonl           the unloader's board every 30 s, its assignments among it
#   log.txt             what the run pressed, and when
#
# The stack stays up afterwards, for the pages. A run owns the stack: this script
# refuses while shingo-dev has containers running, because its first act is `down -v`.
# KANBAN_RIG_REPLACE=1 says the running stack is yours to drop. It takes the gate lock,
# so a gate and a run never overlap.
#
# Env: KANBAN_RIG_OUT, KANBAN_RIG_REPLACE, KANBAN_RIG_NO_BUILD=1 (reuse the images),
#      SHINGO_BIND (default 127.0.0.1).
set -uo pipefail
export MSYS_NO_PATHCONV=1

# ── THE NAMES, in one place: the rename pass moves these ────────────────────────────────
KANBAN_API=/api/inventory/maintained-groups
CARD_GROUP=group_node
CARD_TYPE=bin_type_code
K=PRESS-KANBAN
K2=WELD-KANBAN
Z45=ZONE-45
Z32=ZONE-32
SPARE=ZONE-45-SPARE
SM=KP-SM
UNLOAD_GROUP=PRESS-UNLOAD
PRESS_OPS=KP-PRESS-OPS
UNLOAD_OPS=KP-UNLOAD-OPS
SMALL=SMALL-45
BIG=BIG-45
TOTES=TOTES-45
C32=CART-32
STYLE_SMALL=KP-SMALL
STYLE_BIG=KP-BIG
STYLE_TOTES=KP-TOTES
STYLE_32=KP-32
LIT_NONE_SHORT='"none_short":true'  # the card's "no group is short", on a tile's assignment

SIM_SPEED=10             # shingo-*/*.dev.yaml sim.speed; 1 sim-minute is 6 wall-seconds
CORE=http://127.0.0.1:8083
EDGE=http://127.0.0.1:8081
PLANT=/plants/kanban-press.yaml
PROJECT=shingo-dev

RUN="${1:-}"
case "$RUN" in
  R1|R2|R3|R4|R5|R6) ;;
  *) echo "usage: bash scripts/kanban-rig.sh <R1|R2|R3|R4|R5|R6>" >&2; exit 2 ;;
esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUT="${KANBAN_RIG_OUT:-$ROOT/../kanban-rig-runs/$RUN-$STAMP}"
mkdir -p "$OUT" || exit 1
OUT="$(cd "$OUT" && { pwd -W 2>/dev/null || pwd; })"
COOKIES="$OUT/.core-cookies"
FAILED=0

export SHINGO_PLANT="$PLANT"
dc() { docker compose -p "$PROJECT" -f docker-compose.dev.yml "$@"; }

say() { echo "$(date -u +%H:%M:%SZ) +$(( $(date +%s) - ${START_S:-$(date +%s)} ))s $*" | tee -a "$OUT/log.txt"; }
die() { say "STOP: $*"; exit 3; }
sim_min() { sleep $(( $1 * 60 / SIM_SPEED )); }

# pass/fail write the run's verdict; the exit code at the end is theirs.
pass() { echo "PASS  $*" | tee -a "$OUT/verdict.txt"; }
fail() { echo "FAIL  $*" | tee -a "$OUT/verdict.txt"; FAILED=1; }
check() { # check <description> <got> <want>
  if [ "$2" = "$3" ]; then pass "$1 ($2)"; else fail "$1: got $2, want $3"; fi
}
check_zero() { check "$1" "$2" 0; }

# ── READS ───────────────────────────────────────────────────────────────────────────────
sql() { dc exec -T postgres psql -U shingocore -d shingocore -At -F $'\t' -c "$1"; }

# empties <group> <type>: empty carts of a type standing in a group.
empties() {
  sql "SELECT count(*) FROM bins b JOIN nodes n ON n.id = b.node_id JOIN nodes g ON g.id = n.parent_id
       JOIN bin_types t ON t.id = b.bin_type_id
       WHERE g.name = '$1' AND t.code = '$2' AND b.payload_code = ''"
}
node_id() { sql "SELECT id FROM nodes WHERE name = '$1'"; }
bin_id() { sql "SELECT id FROM bins WHERE label = '$1'"; }

# oldest_empty <group> <type>: the label of one such cart nothing has claimed.
oldest_empty() {
  sql "SELECT b.label FROM bins b JOIN nodes n ON n.id = b.node_id JOIN nodes g ON g.id = n.parent_id
       JOIN bin_types t ON t.id = b.bin_type_id
       WHERE g.name = '$1' AND t.code = '$2' AND b.payload_code = '' AND b.claimed_by IS NULL
       ORDER BY b.id LIMIT 1"
}

# free_child <group>: a child of the group holding no bin and with nothing coming to it.
free_child() {
  sql "SELECT n.name FROM nodes n JOIN nodes g ON g.id = n.parent_id
       WHERE g.name = '$1' AND n.enabled
         AND NOT EXISTS (SELECT 1 FROM bins b WHERE b.node_id = n.id)
         AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.delivery_node = n.name
                         AND o.status NOT IN ('confirmed','failed','cancelled','skipped'))
       ORDER BY n.name LIMIT 1"
}

# card <group> <type> <field>: one number off the kanban's row in $KANBAN_API.
card() {
  curl -s -m 10 "$CORE$KANBAN_API" | tr '{' '\n' \
    | grep "\"$CARD_GROUP\":\"$1\"" | grep "\"$CARD_TYPE\":\"$2\"" \
    | sed -n "s/.*\"$3\":\([0-9-]*\).*/\1/p" | head -1
}

# wait_for <wall seconds> <description> <shell test>: polls every 2 s; false on timeout.
wait_for() {
  local budget=$1 what=$2 deadline=$(( $(date +%s) + $1 ))
  shift 2
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if eval "$*"; then say "ok: $what"; return 0; fi
    sleep 2
  done
  say "TIMED OUT after ${budget}s: $what"
  return 1
}

# ── THE STACK ───────────────────────────────────────────────────────────────────────────
bring_up() {
  if [ -n "$(dc ps -q 2>/dev/null)" ] && [ "${KANBAN_RIG_REPLACE:-0}" != 1 ]; then
    echo "$PROJECT has containers running, and this run starts with down -v." >&2
    echo "Set KANBAN_RIG_REPLACE=1 if that stack is yours to drop." >&2
    exit 3
  fi
  dc --profile tools down -v --remove-orphans >"$OUT/down.log" 2>&1
  bash scripts/sim-anchor.sh mint >>"$OUT/log.txt"
  if [ "${KANBAN_RIG_NO_BUILD:-0}" != 1 ]; then
    say "build core edge seed"
    dc --profile tools build core edge seed >"$OUT/build.log" 2>&1 || die "build failed, see build.log"
  fi
  say "up postgres kafka core edge"
  SHINGO_BIND="${SHINGO_BIND:-127.0.0.1}" dc up -d postgres kafka core edge >"$OUT/up.log" 2>&1 || die "up failed, see up.log"
  # Core first and then stopped: Edge migrates its database at boot and the seeder
  # writes into a migrated schema. docs/dev-env/sim.md, "Seed with Core STOPPED".
  wait_for 120 "Edge up" "curl -s -m 3 $EDGE/status >/dev/null" || die "Edge did not come up"
  dc stop core >>"$OUT/up.log" 2>&1
  say "seed $PLANT"
  dc run --no-deps --rm seed >"$OUT/seed.log" 2>&1 || die "seed failed, see seed.log"
  dc start core >>"$OUT/up.log" 2>&1
  dc restart edge >>"$OUT/up.log" 2>&1
  wait_for 120 "Core up" "curl -s -m 3 $CORE/login >/dev/null" || die "Core did not come up"
  wait_for 120 "Edge up" "curl -s -m 3 $EDGE/status >/dev/null" || die "Edge did not come up"
  local login
  login="$(curl -s -m 10 -o "$OUT/.last" -w '%{http_code}' -c "$COOKIES" -d "username=admin&password=admin" "$CORE/login")"
  case "$login" in 30*) ;; *) die "Core login refused ($login)" ;; esac
}

# station_view <code>: the Edge's public view of the operator station with that code.
station_view() {
  local id view
  for id in $(seq 1 12); do
    view="$(curl -s -m 10 "$EDGE/api/operator-stations/$id/view")"
    case "$view" in *"\"code\":\"$1\""*) printf '%s' "$view"; return 0 ;; esac
  done
  return 1
}

# Edge ids: a process's id and a style's, off the press's station view. The view is the
# screen's, so what it offers is what a person can press.
resolve_edge_ids() {
  local view
  view="$(station_view "$PRESS_OPS")" || die "no operator station $PRESS_OPS on the Edge"
  PROCESS_ID="$(printf '%s' "$view" | grep -o '"process":{"id":[0-9]*' | grep -o '[0-9]*$' | head -1)"
  [ -n "$PROCESS_ID" ] || die "the press's view names no process"
  say "KP-PRESS is Edge process $PROCESS_ID"
}
style_id() {
  station_view "$PRESS_OPS" | grep -o "\"id\":[0-9]*,\"name\":\"$1\"" | head -1 | sed 's/"id":\([0-9]*\).*/\1/'
}

# ── THE BUTTONS ─────────────────────────────────────────────────────────────────────────
edge_post() { curl -s -m 30 -o "$OUT/.last" -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "$2" "$EDGE$1"; }
core_post() { curl -s -m 30 -o "$OUT/.last" -w '%{http_code}' -b "$COOKIES" -X POST -H 'Content-Type: application/json' -d "$2" "$CORE$1"; }

http_ok() { case "$1" in 2*) return 0 ;; *) return 1 ;; esac; }

# start <style>: the station's START, pressed again while the screen refuses it: "a
# carrier is on its way" and a staged swap both clear on their own here, because the
# sim operator releases a staged swap when its bin is done (10 sim-min at the most).
start() {
  local id code deadline=$(( $(date +%s) + 180 ))
  id="$(style_id "$1")"
  [ -n "$id" ] || die "the press offers no style $1"
  while :; do
    code="$(edge_post "/api/processes/$PROCESS_ID/changeover/start" "{\"to_style_id\":$id,\"called_by\":\"kanban-rig\"}")"
    say "START $1: $code $(cut -c1-200 "$OUT/.last")"
    http_ok "$code" && return 0
    [ "$(date +%s)" -lt "$deadline" ] || return 1
    sleep 5
  done
}
cancel_changeover() {
  local code; code="$(edge_post "/api/processes/$PROCESS_ID/changeover/cancel" '{}')"
  say "CANCEL: $code $(cut -c1-200 "$OUT/.last")"
  http_ok "$code"
}
gate_open() { curl -s -m 10 "$EDGE/api/processes/$PROCESS_ID/changeover/gate-status" | grep -q '"can_complete":true'; }

# cutover <wall budget>: CUTOVER once the changeover's own gate allows it.
cutover() {
  wait_for "$1" "changeover gate open" gate_open || {
    curl -s -m 10 "$EDGE/api/processes/$PROCESS_ID/changeover/gate-status" >"$OUT/gate-at-timeout.json"
    return 1
  }
  local code; code="$(edge_post "/api/processes/$PROCESS_ID/changeover/cutover" '{}')"
  say "CUTOVER: $code $(cut -c1-200 "$OUT/.last")"
  http_ok "$code"
}

# changeover_to <style>: START, then CUTOVER, then 10 sim-min of settle.
changeover_to() {
  start "$1" || { fail "START $1 refused"; return 1; }
  cutover 600 || { fail "changeover to $1 did not reach its cutover"; return 1; }
  pass "changeover to $1 cut over"
  sim_min 10
}

# move_bin <label> <node>: the bin page's move, a person carrying a cart.
move_bin() {
  local id node code
  id="$(bin_id "$1")"; node="$(node_id "$2")"
  [ -n "$id" ] && [ -n "$node" ] || die "move_bin: no bin $1 or node $2"
  code="$(core_post /api/bins/action "{\"id\":$id,\"action\":\"move\",\"params\":{\"node_id\":$node}}")"
  say "move $1 -> $2: $code $(cut -c1-160 "$OUT/.last")"
  http_ok "$code"
}

# ── THE SAMPLERS AND THE COLLECTION ─────────────────────────────────────────────────────
sample_loop() {
  local unload_id="" id
  for id in $(seq 1 12); do
    case "$(curl -s -m 10 "$EDGE/api/operator-stations/$id/view")" in
      *"\"code\":\"$UNLOAD_OPS\""*) unload_id="$id"; break ;;
    esac
  done
  while :; do
    printf '{"at":"%s","cards":%s}\n' "$(date -u +%H:%M:%SZ)" "$(curl -s -m 10 "$CORE$KANBAN_API")" >>"$OUT/kanban-cards.jsonl"
    [ -n "$unload_id" ] && printf '{"at":"%s","view":%s}\n' "$(date -u +%H:%M:%SZ)" \
      "$(curl -s -m 10 "$EDGE/api/operator-stations/$unload_id/view")" >>"$OUT/lit.jsonl"
    sql "SELECT to_char(now(),'HH24:MI:SS'), n.name, COALESCE(b.label,''), COALESCE(t.code,''), COALESCE(b.payload_code,''),
                (SELECT count(*) FROM orders o WHERE o.bin_id = b.id
                  AND o.status NOT IN ('confirmed','failed','cancelled','skipped'))
           FROM nodes n JOIN nodes g ON g.id = n.parent_id
           LEFT JOIN bins b ON b.node_id = n.id LEFT JOIN bin_types t ON t.id = b.bin_type_id
          WHERE g.name IN ('$UNLOAD_GROUP','WELD-UNLOAD') ORDER BY n.name" >>"$OUT/windows.tsv" 2>/dev/null
    sleep 30
  done
}

# orders over 10 sim-min: minutes on the orders' own clock, the newest stamp as now.
long_orders() {
  sql "WITH clock AS (SELECT max(updated_at) AS now FROM orders)
       SELECT o.id, COALESCE(NULLIF(o.payload_code,''),'-'), o.status,
              COALESCE(NULLIF(o.queue_cause,''), NULLIF(o.error_detail,''), '-'),
              o.source_node || ' -> ' || o.delivery_node,
              round((EXTRACT(EPOCH FROM (COALESCE(o.completed_at, clock.now) - o.created_at)) / 60)::numeric, 1) AS minutes
         FROM orders o, clock
        WHERE COALESCE(o.completed_at, clock.now) - o.created_at > interval '10 minutes'
        ORDER BY minutes DESC" >"$OUT/long-orders.tsv"
}

collect() {
  say "collect"
  long_orders
  ( cd shingo-core && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$OUT/soakstat" ./cmd/soakstat ) \
    || { fail "soakstat did not build"; return; }
  local cid; cid="$(dc ps -q core)"
  docker cp "$OUT/soakstat" "$cid:/tmp/soakstat" >/dev/null 2>&1 || { fail "could not place soakstat"; return; }
  docker exec "$cid" /tmp/soakstat -config /etc/shingo/shingocore.dev.yaml -stranded >"$OUT/stranded.txt" 2>&1
  docker exec "$cid" /tmp/soakstat -config /etc/shingo/shingocore.dev.yaml >"$OUT/soakstat.txt" 2>&1

  # A STRANDED ORDER AND A WINDOW WITH NO DOOR ARE EVERY RUN'S STOPS (the brief's U9).
  local stranded
  stranded="$(sed -n 's/^VERDICT //p' "$OUT/stranded.txt" | head -1)"
  say "stranded verdict: ${stranded:-unread}"
  case "$stranded" in
    PASS*) pass "no order stranded with no releaser ($stranded)" ;;
    *) fail "stranded orders: ${stranded:-the table did not read}, see stranded.txt" ;;
  esac

  # An empty cart on a window with no live order to leave by, in 4 samples running (20
  # sim-min), that the operator is not about to work: nothing will ever move it.
  awk -F'\t' '$3 != "" && $5 == "" && $6 == 0 { run[$3]++; if (run[$3] >= 4 && !said[$3]++) print $2, $3, $4 }
              $3 == "" || $5 != "" || $6 != 0 { run[$3] = 0 }' "$OUT/windows.tsv" >"$OUT/no-door.txt"
  check_zero "carts on a window with no door" "$(wc -l <"$OUT/no-door.txt" | tr -d ' ')"

  # The process's fulls always have a window (R10): all four windows held, 2 samples running.
  awk -F'\t' '$2 ~ /^ULN_02/ { if ($1 != at) { if (n == 4) hit++; else hit = 0; if (hit >= 2) bad = 1; at = $1; n = 0 } if ($3 != "") n++ }
              END { print bad + 0 }' "$OUT/windows.tsv" >"$OUT/.full-windows"
  check_zero "the press's four windows all held across two samples" "$(cat "$OUT/.full-windows")"
}

# orders_count <where>: orders matching a condition, since the run began.
orders_count() { sql "SELECT count(*) FROM orders WHERE $1"; }

# in_group <group>: the SQL list of a group's own name and its children's.
in_group() {
  echo "(SELECT '$1' UNION SELECT n.name FROM nodes n JOIN nodes g ON g.id = n.parent_id WHERE g.name = '$1')"
}

never_to_sm() { check_zero "no order delivered to $SM" "$(orders_count "delivery_node IN $(in_group "$SM")")"; }

# lit_other_than <type>: unloader assignments naming any other type, across the samples.
lit_other_than() {
  grep -o '"assignment":{"bin_type":"[^"]*"' "$OUT/lit.jsonl" 2>/dev/null | grep -vc "\"$1\"" || true
}

# ── THE RUNS. Criteria are the brief's U9, written whether or not the code has landed. ──
run_R1() {
  say "R1: steady state on small, 20 wall-minutes"
  sleep $(( 20 * 60 ))
  check "K wants small 5" "$(card "$K" "$SMALL" want)" 5
  check "K2 wants small 2" "$(card "$K2" "$SMALL" want)" 2
  check_zero "unloader carts lit anything but small" "$(lit_other_than "$SMALL")"
  # A kanban with no supports says "no keeper" for as long as it is short; only the two
  # lineside kanbans can be held.
  check_zero "lineside cards with a holder" \
    "$(tr '{' '\n' <"$OUT/kanban-cards.jsonl" | grep -E "\"$CARD_GROUP\":\"($K|$K2)\"" | grep -c '"held_by"' || true)"
  check_zero "pushes parked at a level" "$(orders_count "queue_cause ILIKE '%level%'")"
  check_zero "orders live and queued on a kanban at the end" \
    "$(orders_count "status IN ('queued','sourcing') AND delivery_node IN $(in_group "$K")")"
  never_to_sm
}

run_R2() {
  say "R2: small -> totes, to cutover + 10 sim-min"
  sim_min 20
  start "$STYLE_TOTES" || { fail "START refused"; return; }
  sleep 12
  check "cover moved at START: K wants totes 5" "$(card "$K" "$TOTES" want)" 5
  check "cover moved at START: K wants small 1" "$(card "$K" "$SMALL" want)" 1
  cutover 600 || { fail "the changeover did not reach its cutover"; return; }
  pass "cut over to totes"
  sim_min 10
  check "K holds its totes want" "$(empties "$K" "$TOTES")" 5
  never_to_sm
}

run_R3() {
  say "R3: totes -> 32x30 -> totes, each to cutover + 10 sim-min"
  sim_min 10
  changeover_to "$STYLE_TOTES" || return
  changeover_to "$STYLE_32" || return
  check_zero "pulls of 32x30 into K from anywhere but $Z32" \
    "$(sql "SELECT count(*) FROM orders o JOIN bins b ON b.id = o.bin_id JOIN bin_types t ON t.id = b.bin_type_id
            WHERE t.code = '$C32' AND o.delivery_node IN $(in_group "$K")
              AND o.source_node NOT IN $(in_group "$Z32") AND o.source_node NOT IN $(in_group "$UNLOAD_GROUP")")"
  # 45x48 off the process goes to K, then Z45 if short, then the spare zone. Surplus residents
  # of K move only to a source with a level and room or to a window (the arm never goes to a
  # plain group), so with Z45 whole K keeps them and says why: all 17 carts accounted for.
  check "$Z45 holds its level" "$(family_in "$Z45")" 6
  check "K, $Z45 and $SPARE hold the family" "$(( $(family_in "$K") + $(family_in "$Z45") + $(family_in "$SPARE") ))" 17
  check "K's card says why its surplus stays" \
    "$(tr '{' '\n' <"$OUT/kanban-cards.jsonl" | grep "\"$CARD_GROUP\":\"$K\"" | grep -q '"surplus_cause"' && echo yes || echo no)" yes
  # Cover over the pool, not a defect: 8 wanted from 6 carts, 4 of them on the positions.
  check "K wants 32x30 4" "$(card "$K" "$C32" want)" 4
  check "K's card says what holds the other 2" \
    "$(tr '{' '\n' <"$OUT/kanban-cards.jsonl" | grep "\"$CARD_GROUP\":\"$K\"" | grep "\"$CARD_TYPE\":\"$C32\"" \
       | grep -q '"held_by"' && echo yes || echo no)" yes
  changeover_to "$STYLE_TOTES" || return
  # The arm fires only while K is full and short; the last cart can stay if K fills first.
  check_zero "32x30 back: at most one stays in K" "$(( $(empties "$K" "$C32") > 1 ))"
  check "the totes came back: K holds its want" "$(empties "$K" "$TOTES")" 5
  never_to_sm
}

# family_in <group>: the empty 45x48 carts standing in a group.
family_in() {
  sql "SELECT count(*) FROM bins b JOIN nodes n ON n.id = b.node_id JOIN nodes g ON g.id = n.parent_id
       JOIN bin_types t ON t.id = b.bin_type_id
       WHERE g.name = '$1' AND t.code IN ('$SMALL','$BIG','$TOTES') AND b.payload_code = ''"
}

run_R4() {
  say "R4: flip-flop: small -> totes, two positions fill, CANCEL, small -> big, cutover"
  sim_min 10
  start "$STYLE_TOTES" || { fail "START refused"; return; }
  wait_for 300 "two positions hold totes" '[ "$(totes_on_press)" -ge 2 ]' || fail "two positions never filled"
  cancel_changeover || fail "CANCEL refused"
  sleep 12
  check "CANCEL reverts cover within a tick: K wants small 5" "$(card "$K" "$SMALL" want)" 5
  check "CANCEL reverts cover within a tick: K wants totes 1" "$(card "$K" "$TOTES" want)" 1
  check_zero "changeover legs left live after the CANCEL settled" \
    "$(sim_min 2; orders_count "status NOT IN ('confirmed','failed','cancelled','skipped') AND queue_cause ILIKE '%changeover%'")"
  start "$STYLE_BIG" || { fail "START big refused"; return; }
  cutover 600 || { fail "small -> big did not reach its cutover"; return; }
  pass "cut over to big"
  sim_min 10
  check_zero "totes left on the positions: the next START lifts them" "$(totes_on_press)"
  never_to_sm
}

totes_on_press() {
  sql "SELECT count(*) FROM bins b JOIN nodes n ON n.id = b.node_id JOIN bin_types t ON t.id = b.bin_type_id
       WHERE n.name IN ('PLN_021','PLN_022','PLN_023','PLN_024') AND t.code = '$TOTES'"
}

run_R5() {
  say "R5: repayment"
  sim_min 10
  # K borrows from Z45: its cart leaves by hand, its keeper fetches from Z45, and the same
  # cart then reaches a window as the next small off the process would.
  local cart spot window
  cart="$(oldest_empty "$K" "$SMALL")"; spot="$(free_child_lane)"
  [ -n "$cart" ] && [ -n "$spot" ] || die "no small in K or no room in $SM"
  move_bin "$cart" "$spot" || fail "could not take a cart out of K"
  wait_for 120 "K borrowed: Z45 short a small" z45_short || fail "K did not borrow from Z45"
  window="$(free_child "$UNLOAD_GROUP")"
  [ -n "$window" ] || die "no free window"
  move_bin "$cart" "$window" || fail "could not stand the cart on a window"
  wait_for 180 "the cart repaid Z45" z45_whole || fail "Z45 was not repaid"
  never_to_sm

  # K2 borrows from Z45; K's next cart repays it.
  cart="$(oldest_empty "$K2" "$SMALL")"; spot="$(free_child_lane)"
  [ -n "$cart" ] && [ -n "$spot" ] || die "no small in K2 or no room in $SM"
  move_bin "$cart" "$spot" || fail "could not take a cart out of K2"
  wait_for 120 "K2 borrowed: Z45 short a small" z45_short || fail "K2 did not borrow from Z45"
  window="$(free_child "$UNLOAD_GROUP")"
  [ -n "$window" ] || die "no free window"
  move_bin "$cart" "$window" || fail "could not stand the cart on a press window"
  wait_for 180 "K's cart repaid Z45 for K2" z45_whole || fail "Z45 was not repaid across kanbans"
  never_to_sm

  # K borrows from SM by a hand move. SM holds no level, so nothing can repay it: K and
  # Z45 stay whole, the next cart goes to K's overflow, and the card says no group is short.
  wait_for 120 "K has a free child" '[ -n "$(free_child "$K")" ]' || die "K has no free child"
  move_bin KB-SM-S1 "$(free_child "$K")" || fail "could not hand K a cart from SM"
  wait_for 240 "the next cart went to $SPARE" '[ "$(empties "$SPARE" "$SMALL")" -ge 1 ]' \
    || fail "no cart reached $SPARE, K's overflow"
  never_to_sm
  check "the card says no group is short" \
    "$(grep -c "$LIT_NONE_SHORT" "$OUT/lit.jsonl" | awk '{ print ($1 > 0) ? "yes" : "no" }')" yes
}

z45_short() { [ "$(empties "$Z45" "$SMALL")" -lt 2 ]; }
z45_whole() { [ "$(empties "$Z45" "$SMALL")" = 2 ]; }
unclaimed() { [ -n "$(sql "SELECT 1 FROM bins WHERE label = '$1' AND claimed_by IS NULL")" ]; }

run_R6() {
  say "R6: raiding: K2 short, Z45 empty, K holding spares"
  sim_min 10
  local cart spot
  for cart in KB-Z45-S1 KB-Z45-S2; do
    spot="$(free_child_lane)"
    [ -n "$spot" ] || die "no room in $SM"
    wait_for 120 "$cart is unclaimed" "unclaimed $cart" || die "$cart stayed claimed"
    move_bin "$cart" "$spot" || fail "could not empty Z45 of $cart"
  done
  local mark; mark="$(sql "SELECT COALESCE(max(id),0) FROM orders")"
  for i in 1 2; do
    cart="$(oldest_empty "$K2" "$SMALL")"; spot="$(free_child_lane)"
    [ -n "$cart" ] && [ -n "$spot" ] || break
    move_bin "$cart" "$spot" || fail "could not take a cart out of K2"
  done
  sim_min 2
  check "K holds spares" "$([ "$(empties "$K" "$SMALL")" -ge 1 ] && echo yes || echo no)" yes
  check_zero "K2's keeper took from K" \
    "$(orders_count "id > $mark AND delivery_node IN $(in_group "$K2") AND source_node IN $(in_group "$K")")"
  check "K2 is short with its ask visible" \
    "$(grep -c '"oldest_ask_cause":"[^"]' "$OUT/kanban-cards.jsonl" | awk '{ print ($1 > 0) ? "yes" : "no" }')" yes
  sim_min 10
  check_zero "K2's keeper took from K, after settle" \
    "$(orders_count "id > $mark AND delivery_node IN $(in_group "$K2") AND source_node IN $(in_group "$K")")"
  never_to_sm
}

# free_child_lane: a mouth in SM, where a cart taken out of the loop stands.
free_child_lane() {
  sql "SELECT n.name FROM nodes n JOIN nodes l ON l.id = n.parent_id JOIN nodes g ON g.id = l.parent_id
       WHERE g.name = '$SM' AND n.depth = 1 AND NOT EXISTS (SELECT 1 FROM bins b WHERE b.node_id = n.id)
       ORDER BY n.name LIMIT 1"
}

# ── THE RUN ─────────────────────────────────────────────────────────────────────────────
. scripts/gate-lock.sh
gate_lock_acquire "kanban-rig $RUN" || { echo "no gate lock" >&2; exit 3; }
SAMPLER=""
cleanup() {
  [ -n "$SAMPLER" ] && kill "$SAMPLER" 2>/dev/null
  rm -f "$COOKIES" "$OUT/.last" "$OUT/.full-windows"
  gate_lock_release
}
trap cleanup EXIT

{
  echo "run:     $RUN"
  echo "plant:   $PLANT"
  echo "head:    $(git rev-parse --short HEAD)"
  echo "started: $STAMP"
} >"$OUT/run-meta.txt"

bring_up
resolve_edge_ids
START_S=$(date +%s)
sample_loop & SAMPLER=$!
say "run $RUN: Core $CORE, Edge $EDGE, out $OUT"

"run_$RUN"

collect
say "done; the stack stays up"
echo
cat "$OUT/verdict.txt"
exit "$FAILED"
