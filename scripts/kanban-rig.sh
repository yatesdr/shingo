#!/usr/bin/env bash
# bash scripts/kanban-rig.sh <R1|R2|R3|R4|R5|R6|W|W4|W5> — one run of the Kanban certification rig
# on the visible dev stack: compose project shingo-dev, plants/kanban-press.yaml, Core :8083,
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
# W and W5, the walk, add, every 3 wall-seconds from the run's start:
#
#   walk.tsv            wall time, sim clock, and each carrier of the press's loop: label,
#                       type, node, its group, claimed order
#   walk-orders.tsv     each order touching K, the positions or the windows, live or just
#                       ended: id, type, payload, source, delivery, status, cause, age
#   walk-cards.jsonl    K's card and the unloader's board view (assignment per window)
#   walk-log.txt        the steps' marker lines
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
KANBAN_API=/api/nodes/kanban/levels
CARD_GROUP=group_node
CARD_TYPE=bin_type_code
K=PRESS-KANBAN
K2=WELD-KANBAN
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
PARKED_AT_LEVEL=ngrp-at-level       # dispatch.CauseNGRPAtLevel, a push refused at a level
NO_EMPTY=finder-no-empty-of-type    # the queue cause of an ask with nothing to fetch

# K's slots and what each allows, in one place. plantspec.Slot has no field for it and the
# seeder writes none (plants/kanban-press.yaml's head), so apply_slot_types posts this after
# the seed. cmd/seeddev rigSlotTypes is the docker tests' copy, and
# TestKanbanPressFixture_TheDriverStatesTheSameTable keeps the two lines below as they are.
SLOT_TYPES=(
  "PKB_001 PKB_002 PKB_003 PKB_004 PKB_005 PKB_006 PKB_007 PKB_008 PKB_009 PKB_010:$SMALL $BIG $TOTES"
  "PKB_011 PKB_012 PKB_013 PKB_014 PKB_015 PKB_016:$C32"
)

SIM_SPEED=10            # shingo-*/*.dev.yaml sim.speed; 1 sim-minute is 6 wall-seconds
CORE=http://127.0.0.1:8083
EDGE=http://127.0.0.1:8081
PLANT=/plants/kanban-press.yaml
PROJECT=shingo-dev

RUN="${1:-}"
case "$RUN" in
  R1|R2|R3|R4|R5|R6|W|W4|W5) ;;
  *) echo "usage: bash scripts/kanban-rig.sh <R1|R2|R3|R4|R5|R6|W|W4|W5>" >&2; exit 2 ;;
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

# free_child <group> [type]: a child of the group holding no bin and with nothing coming to
# it, and with a type, one that allows it: 10 of K's 16 slots refuse a 32x30 and 6 of them
# everything else.
free_child() {
  local allows=""
  [ -n "${2:-}" ] && allows="AND (NOT EXISTS (SELECT 1 FROM node_bin_types a WHERE a.node_id = n.id)
         OR EXISTS (SELECT 1 FROM node_bin_types a JOIN bin_types t ON t.id = a.bin_type_id
                    WHERE a.node_id = n.id AND t.code = '$2'))"
  sql "SELECT n.name FROM nodes n JOIN nodes g ON g.id = n.parent_id
       WHERE g.name = '$1' AND n.enabled
         AND NOT EXISTS (SELECT 1 FROM bins b WHERE b.node_id = n.id)
         AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.delivery_node = n.name
                         AND o.status NOT IN ('confirmed','failed','cancelled','skipped'))
         $allows
       ORDER BY n.name LIMIT 1"
}

# card <group> <type> <field>: one number off the kanban's row in $KANBAN_API.
card() {
  curl -s -m 10 "$CORE$KANBAN_API" | tr '{' '\n' \
    | grep "\"$CARD_GROUP\":\"$1\"" | grep "\"$CARD_TYPE\":\"$2\"" \
    | sed -n "s/.*\"$3\":\([0-9-]*\).*/\1/p" | head -1
}

# card_said <group> <type or ""> <pattern>: yes or no, whether any sampled card of that
# line carried the pattern. A count, not grep -q: under pipefail an early-exiting grep -q
# kills the writer upstream with SIGPIPE, and the pipeline answers no to a match.
card_said() {
  local n
  n="$(tr '{' '\n' <"$OUT/kanban-cards.jsonl" | grep "\"$CARD_GROUP\":\"$1\"" | grep "\"$CARD_TYPE\":\"$2" \
       | grep -c "$3" || true)"
  [ "${n:-0}" -gt 0 ] && echo yes || echo no
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
  apply_slot_types
}

# apply_slot_types: K's slots' allowed types, through two of the Nodes page's doors: the rows
# (/api/nodes/bin-types) and the mode (/api/nodes/properties/set). The modal's own post is
# /nodes/update, which also rewrites a slot's station mode and name; these write only this. No
# force: the holds-bins guard on a slot reads the slot's own children, of which it has none.
# Core and the Edge are up, and the press's first cart is 10 sim-min off: a minute's wall
# clock against these few posts. K is short and nothing outside it holds an empty, so no ask
# is holding a slot in the meantime.
apply_slot_types() {
  local entry slots types code ids id slot node posted=0
  for entry in "${SLOT_TYPES[@]}"; do
    slots="${entry%%:*}"; types="${entry##*:}"
    ids=""
    for code in $types; do
      id="$(sql "SELECT id FROM bin_types WHERE code = '$code'")"
      [ -n "$id" ] || die "no bin type $code"
      ids="${ids:+$ids,}$id"
    done
    for slot in $slots; do
      node="$(node_id "$slot")"
      [ -n "$node" ] || die "no slot $slot"
      http_ok "$(core_post /api/nodes/bin-types "{\"node_id\":$node,\"bin_type_ids\":[$ids]}")" \
        || die "the types of $slot refused: $(cut -c1-200 "$OUT/.last")"
      http_ok "$(core_post /api/nodes/properties/set "{\"node_id\":$node,\"key\":\"bin_type_mode\",\"value\":\"specific\"}")" \
        || die "the mode of $slot refused: $(cut -c1-200 "$OUT/.last")"
      posted=$(( posted + 1 ))
    done
  done
  say "slot types applied on $posted of $K's slots"
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
gate_open() {
  case "$(curl -s -m 10 "$EDGE/api/processes/$PROCESS_ID/changeover/gate-status")" in
    *'"can_complete":true'*) return 0 ;;
    *) return 1 ;;
  esac
}

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

# stand_on_window <label>: a person standing a cart on a free window of the press's
# unloader. free_child reads the windows free and a cart can land on one before the move
# reaches Core, which answers 400 "already has 1 bin(s)". So it tries again on whichever
# window is free next, and is a failure only after 120 s of that.
stand_on_window() {
  local window deadline=$(( $(date +%s) + 120 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    window="$(free_child "$UNLOAD_GROUP")"
    if [ -n "$window" ] && move_bin "$1" "$window"; then return 0; fi
    sleep 3
  done
  say "no window of $UNLOAD_GROUP took $1 in 120 s"
  return 1
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

# orders over 10 sim-min: minutes on the orders' own clock, the newest stamp as now. A
# cancelled order has no completed_at, so a terminal order ends at its last update; only a
# live one ages to the end of the run.
long_orders() {
  sql "WITH clock AS (SELECT max(updated_at) AS now FROM orders),
            aged AS (SELECT o.*, COALESCE(o.completed_at,
                       CASE WHEN o.status IN ('confirmed','failed','cancelled','skipped')
                            THEN o.updated_at ELSE clock.now END) AS ended
                       FROM orders o, clock)
       SELECT id, COALESCE(NULLIF(payload_code,''),'-'), status,
              COALESCE(NULLIF(queue_cause,''), NULLIF(error_detail,''), '-'),
              source_node || ' -> ' || delivery_node,
              round((EXTRACT(EPOCH FROM (ended - created_at)) / 60)::numeric, 1) AS minutes
         FROM aged
        WHERE ended - created_at > interval '10 minutes'
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

# in_group <group>: the SQL list of a group's own name, its children's and, for a group of
# lanes, its lanes' slots: SM's orders deliver two levels down.
in_group() {
  echo "(SELECT '$1'
         UNION SELECT n.name FROM nodes n JOIN nodes g ON g.id = n.parent_id WHERE g.name = '$1'
         UNION SELECT n.name FROM nodes n JOIN nodes l ON l.id = n.parent_id
                                          JOIN nodes g ON g.id = l.parent_id WHERE g.name = '$1')"
}

never_to_sm() { check_zero "no order delivered to $SM" "$(orders_count "delivery_node IN $(in_group "$SM")")"; }

# lit_other_than <type>: unloader assignments naming any other type, across the samples.
lit_other_than() {
  grep -o '"assignment":{"bin_type":"[^"]*"' "$OUT/lit.jsonl" 2>/dev/null | grep -vc "\"$1\"" || true
}

# ── THE RUNS. A criterion the fixture must meet stays a check, failing if it fails. ──────
# In this fixture K's card reads its running type short by the 4 on the positions, for as long
# as the press runs: 6 wanted, 2 standing, "no empty <type> outside the group".
line_of() { printf '"%s":"%s","%s":"%s"' "$CARD_GROUP" "$K" "$CARD_TYPE" "$1"; }

run_R1() {
  say "R1: steady state on small, 20 wall-minutes"
  sleep $(( 20 * 60 ))
  check "K wants small 6" "$(card "$K" "$SMALL" want)" 6
  check "K2 wants small 2" "$(card "$K2" "$SMALL" want)" 2
  check_zero "unloader carts lit anything but small" "$(lit_other_than "$SMALL")"
  # K's small rests at 2 with the pair home: read when it does, not on a sample mid-refill.
  if wait_for 120 "K's small rests at 2" '[ "$(card "$K" "$SMALL" resident)" = 2 ]'; then
    pass "K's small line rests at want 6, resident 2"
  else
    fail "K's small line never rested at 2: resident $(card "$K" "$SMALL" resident)"
  fi
  check "K's small line says what holds it" "$(card_said "$K" "$SMALL" "\"held_by\":\"no empty $SMALL outside the group")" yes
  # K's small is held by that for the whole run, so it is not one of the lines this judges. A
  # refill's two carts stand on K's slots until they are lifted, so another line can read
  # held for one tick: held across two samples is the defect. The one-tick ones are counted
  # in held-one-tick.txt.
  tr '{' '\n' <"$OUT/kanban-cards.jsonl" | grep -E "\"$CARD_GROUP\":\"($K|$K2)\"" | grep '"held_by"' \
    | grep -vF "$(line_of "$SMALL")" >"$OUT/held-one-tick.txt" || true
  check_zero "lineside lines other than K's small held across two samples" "$(awk -v a="\"$CARD_GROUP\":\"$K\"" -v b="\"$CARD_GROUP\":\"$K2\"" -v s="$(line_of "$SMALL")" '
    { n = split($0, card, "{"); delete now
      for (i = 1; i <= n; i++)
        if ((index(card[i], a) || index(card[i], b)) && index(card[i], "\"held_by\"") && !index(card[i], s)) {
          key = substr(card[i], 1, index(card[i], "\"want\"")); now[key] = 1; if (key in was) bad++
        }
      delete was; for (key in now) was[key] = 1 }
    END { print bad + 0 }' "$OUT/kanban-cards.jsonl")"
  check_zero "pushes parked at a level" "$(orders_count "queue_cause = '$PARKED_AT_LEVEL'")"
  check_zero "orders live and queued on a kanban at the end" \
    "$(orders_count "status IN ('queued','sourcing') AND delivery_node IN $(in_group "$K")")"
  never_to_sm
}

run_R2() {
  say "R2: small -> totes, to cutover + 10 sim-min"
  sim_min 20
  start "$STYLE_TOTES" || { fail "START refused"; return; }
  sleep 12
  check "cover moved at START: K wants totes 6" "$(card "$K" "$TOTES" want)" 6
  check "cover moved at START: K wants small 2" "$(card "$K" "$SMALL" want)" 2
  cutover 600 || { fail "the changeover did not reach its cutover"; return; }
  pass "cut over to totes"
  sim_min 10
  # The 4 positions hold totes and K its 2/2/2. A refill's carts leave K between two ticks, so
  # the picture is waited for, not sampled once.
  if wait_for 120 "K holds 2 of each and the positions totes" 'family_settled "$TOTES" 2 2 2'; then
    pass "K holds its 2/2/2 and the 4 positions totes"
  else
    fail "never K's 2/2/2 with totes on the positions: $(family_said "$TOTES")"
  fi
  check_zero "K2's keeper took from K" "$(orders_count "delivery_node IN $(in_group "$K2") AND source_node IN $(in_group "$K")")"
  never_to_sm
}

run_R3() {
  say "R3: totes -> 32x30 -> totes, each to cutover + 10 sim-min"
  sim_min 10
  changeover_to "$STYLE_TOTES" || return
  changeover_to "$STYLE_32" || return
  check "K wants 32x30 6" "$(card "$K" "$C32" want)" 6
  # K's 6 and the 4 positions want 10 of the 6 carts. K's card says so, and nothing fetches any.
  check "K's card says what holds its 32x30" "$(card_said "$K" "$C32" "\"held_by\":\"no empty $C32 outside the group")" yes
  if wait_for 120 "the positions hold 32x30" '[ "$(on_press "$C32")" = 4 ]'; then
    pass "the 4 positions hold 32x30"
  else
    fail "the positions hold $(on_press "$C32") of 4 32x30"
  fi
  # K wants 6 of the 10 45x48 and has no source and no overflow: the 4 off the positions have no
  # level to land on. simcalc's CHANGEOVER ROOM says so. This fails on the fixture as it stands.
  check_zero "45x48 carts parked at K's level on the windows under $STYLE_32" "$(parked_on_windows)"
  check_zero "windows held by a push that cannot land, under $STYLE_32" "$(windows_held)"
  changeover_to "$STYLE_TOTES" || return
  if wait_for 120 "K holds 2 of each and the positions totes" 'family_settled "$TOTES" 2 2 2'; then
    pass "back on totes: K holds its 2/2/2 and the 4 positions totes"
  else
    fail "back on totes, never K's 2/2/2 with totes on the positions: $(family_said "$TOTES")"
  fi
  # The mirror of the above: K wants 2 32x30, so those off the positions have no level either.
  check_zero "32x30 carts parked at K's level on the windows, back on $STYLE_TOTES" "$(parked_on_windows)"
  check "K's 32x30 back at 6" "$(empties "$K" "$C32")" 6
  never_to_sm
}

# family_settled <running type> <small> <big> <totes>: K holds those of the two types the press
# is not on and of the running type its 2, and the 4 positions hold the running type.
family_settled() {
  [ "$(on_press "$1")" = 4 ] && [ "$(empties "$K" "$SMALL")" = "$2" ] && [ "$(empties "$K" "$BIG")" = "$3" ] \
    && [ "$(empties "$K" "$TOTES")" = "$4" ]
}
family_said() {
  echo "positions $(on_press "$1"), K small $(empties "$K" "$SMALL") big $(empties "$K" "$BIG") totes $(empties "$K" "$TOTES")"
}

# parked_on_windows: the pushes standing parked at a level, off the press's windows.
parked_on_windows() {
  orders_count "status = 'queued' AND queue_cause = '$PARKED_AT_LEVEL' AND source_node IN $(in_group "$UNLOAD_GROUP")"
}
# windows_held: the press's windows standing with an empty whose push is parked.
windows_held() {
  sql "SELECT count(DISTINCT o.source_node) FROM orders o
       WHERE o.status = 'queued' AND o.queue_cause = '$PARKED_AT_LEVEL' AND o.source_node IN $(in_group "$UNLOAD_GROUP")"
}

run_R4() {
  say "R4: flip-flop: small -> totes, two positions fill, CANCEL, small -> big, cutover"
  sim_min 10
  start "$STYLE_TOTES" || { fail "START refused"; return; }
  wait_for 300 "two positions hold totes" '[ "$(on_press "$TOTES")" -ge 2 ]' || fail "two positions never filled"
  cancel_changeover || fail "CANCEL refused"
  sleep 12
  check "CANCEL reverts cover within a tick: K wants small 6" "$(card "$K" "$SMALL" want)" 6
  check "CANCEL reverts cover within a tick: K wants totes 2" "$(card "$K" "$TOTES" want)" 2
  check_zero "changeover legs left live after the CANCEL settled" \
    "$(sim_min 2; orders_count "status NOT IN ('confirmed','failed','cancelled','skipped') AND queue_cause ILIKE '%changeover%'")"
  start "$STYLE_BIG" || { fail "START big refused"; return; }
  cutover 600 || { fail "small -> big did not reach its cutover"; return; }
  pass "cut over to big"
  sim_min 10
  check_zero "totes left on the positions: the next START lifts them" "$(on_press "$TOTES")"
  never_to_sm
}

# on_press <type>: carts of a type on the press's four positions.
on_press() {
  sql "SELECT count(*) FROM bins b JOIN nodes n ON n.id = b.node_id JOIN bin_types t ON t.id = b.bin_type_id
       WHERE n.name IN ('PLN_021','PLN_022','PLN_023','PLN_024') AND t.code = '$1'"
}

# hand_register <label prefix> <type> [node]: a person bringing a new cart in, by the bin page's
# door. The label it answers is the prefix and 0001.
hand_register() {
  local bt node_json="" node code
  bt="$(sql "SELECT id FROM bin_types WHERE code = '$2'")"
  if [ -n "${3:-}" ]; then node="$(node_id "$3")"; node_json=",\"node_id\":$node"; fi
  code="$(core_post /api/bins/bulk-register "{\"bin_type_id\":$bt,\"count\":1,\"prefix\":\"$1\"$node_json}")"
  say "register $1 ($2) ${3:-off the floor}: $code"
  http_ok "$code"
}

# R5 keeps what the fixture can show. K over-handed first: K's asks past SM's one cart (below)
# would otherwise hold the slots the over-hand needs.
run_R5() {
  say "R5: K whole parks the next cart; K takes from SM and nothing repays it"
  sim_min 10

  local i slot
  for i in 1 2 3 4; do
    slot="$(free_child "$K" "$SMALL")"
    [ -n "$slot" ] || die "no free slot in K for a small"
    hand_register "KB-HAND-$i-" "$SMALL" "$slot" || fail "could not register a cart on $slot"
  done
  if wait_for 180 "K is whole of small" '[ "$(card "$K" "$SMALL" resident)" = 6 ]'; then
    pass "K over-handed reads resident 6 of its want of 6"
  else
    fail "K never read whole: resident $(card "$K" "$SMALL" resident)"
  fi

  # The next cart: one more stood on a window, as the next off the process would. It has no
  # source and no overflow. The card stands only while the cart is on its window, so the board
  # is read every second here; the 30 s samples can fall either side of it.
  hand_register "KB-HAND-5-" "$SMALL" || fail "could not register the next cart"
  stand_on_window "KB-HAND-5-0001" || fail "could not stand the cart on a window"
  local unload_id="" id saw=no parked=""
  for id in $(seq 1 12); do
    case "$(curl -s -m 10 "$EDGE/api/operator-stations/$id/view")" in
      *"\"code\":\"$UNLOAD_OPS\""*) unload_id="$id"; break ;;
    esac
  done
  for _ in $(seq 1 120); do
    case "$(curl -s -m 5 "$EDGE/api/operator-stations/$unload_id/view")" in *"$LIT_NONE_SHORT"*) saw=yes ;; esac
    parked="$(sql "SELECT o.id FROM orders o JOIN bins b ON b.id = o.bin_id
                   WHERE b.label = 'KB-HAND-5-0001' AND o.status = 'queued' AND o.queue_cause = '$PARKED_AT_LEVEL'
                     AND o.delivery_node = '$K' LIMIT 1")"
    [ -n "$parked" ] && [ "$saw" = yes ] && break
    sleep 1
  done
  check "the card says no group is short" "$saw" yes
  check "the cart parks named at K" "$([ -n "$parked" ] && echo yes || echo no)" yes
  # It leaves when K is short: the press's next refill makes it so, within a cycle.
  if [ -n "$parked" ]; then
    if wait_for 180 "the parked cart left when K was short" \
        "[ \"\$(sql \"SELECT status NOT IN ('queued','pending','sourcing') FROM orders WHERE id = $parked\")\" = t ]"; then
      pass "the parked cart left when K was short"
    else
      fail "the parked cart stayed parked: $(sql "SELECT status, queue_cause FROM orders WHERE id = $parked")"
    fi
  fi
  never_to_sm

  # K takes a cart from SM, plain, by its plant-wide search. Asked with the pair out so K is
  # short by more than the one cart SM holds: the keeper's forecast is per type, so it asks for
  # its whole gap, and the asks past the one cart stand queued, each holding a slot and counted
  # as asked. TestKanbanRig_R5_SMIsNotRepaid names it. The last check is that finding's.
  wait_for 120 "K is short by 2 or more" '[ "$(card "$K" "$SMALL" gap)" -ge 2 ]' \
    || say "K's small gap stayed at $(card "$K" "$SMALL" gap)"
  local cart="" spot=""
  for _ in $(seq 1 30); do
    cart="$(oldest_empty "$K" "$SMALL")"; spot="$(free_child_lane)"
    if [ -n "$cart" ] && [ -n "$spot" ] && move_bin "$cart" "$spot"; then break; fi
    cart=""
    sleep 2
  done
  [ -n "$cart" ] || die "could not carry a cart to $SM"
  # K2 searches plant-wide too and K is whole here, so whichever keeper is short takes it.
  if wait_for 180 "a keeper took the cart off $SM" \
      '[ "$(orders_count "order_type = '"'"'retrieve_empty'"'"' AND status = '"'"'confirmed'"'"' AND source_node IN $(in_group "$SM")")" -ge 1 ]'; then
    pass "a keeper took the cart off $SM by its plant-wide search: $(sql "SELECT delivery_node FROM orders WHERE order_type = 'retrieve_empty' AND source_node IN $(in_group "$SM") ORDER BY id DESC LIMIT 1")"
  else
    fail "no keeper took the cart off $SM"
  fi
  sim_min 2
  never_to_sm
  check_zero "K's asks standing queued on an empty that does not exist" \
    "$(orders_count "status = 'queued' AND queue_cause = '$NO_EMPTY' AND delivery_node IN $(in_group "$K")")"
}

# carry_small <from group> <to group>: a person taking one unclaimed small from one group to a
# free slot of another that allows it. A robot can claim the cart between the read and the move,
# so it asks again until a cart is taken.
carry_small() {
  local cart slot
  for _ in $(seq 1 30); do
    cart="$(oldest_empty "$1" "$SMALL")"; slot="$(free_child "$2" "$SMALL")"
    if [ -n "$cart" ] && [ -n "$slot" ] && move_bin "$cart" "$slot"; then return 0; fi
    sleep 2
  done
  say "could not carry a small from $1 to $2"
  return 1
}

run_R6() {
  say "R6: raiding: K2 short, nothing plant-wide, K holding carts"
  sim_min 10
  # K2's two carts, by hand, into K: nothing stands outside the two kanbans.
  local mark; mark="$(sql "SELECT COALESCE(max(id),0) FROM orders")"
  carry_small "$K2" "$K" || fail "could not take K2's first cart"
  carry_small "$K2" "$K" || fail "could not take K2's second cart"
  sim_min 2
  check "K holds carts" "$([ "$(empties "$K" "$SMALL")" -ge 1 ] && echo yes || echo no)" yes
  check_zero "K2's keeper took from K" \
    "$(orders_count "id > $mark AND delivery_node IN $(in_group "$K2") AND source_node IN $(in_group "$K")")"
  sim_min 10
  check_zero "K2's keeper took from K, after settle" \
    "$(orders_count "id > $mark AND delivery_node IN $(in_group "$K2") AND source_node IN $(in_group "$K")")"
  # Nothing can source it, so the keeper rightly asks nothing and K2's card says what holds it.
  # K2 has no sources, so the sentence is keeper.nothingToSource's "outside the group". Read
  # after the settle: the samples come every 30 s and the check before it is at 12.
  check "K2's card says no empty anywhere it may look" "$(card_said "$K2" "$SMALL" "\"held_by\":\"no empty $SMALL outside the group")" yes
  never_to_sm
  # The run starved the weld by hand, so its orders wait on carts nothing can bring. Hand the
  # carts back to K2: every order waiting on K2 moves, and the weld with them.
  local moved; moved="$(orders_count "status = 'confirmed' AND source_node IN $(in_group "$K2")")"
  carry_small "$K" "$K2" || fail "could not hand K2 its first cart back"
  carry_small "$K" "$K2" || fail "could not hand K2 its second cart back"
  wait_for 180 "the weld's waiting orders moved" \
    '[ "$(orders_count "status = '"'"'queued'"'"' AND source_node IN $(in_group "$K2")")" = 0 ]' \
    || fail "the weld's orders stayed queued after the carts came back"
  if wait_for 180 "the weld took a cart from K2 again" \
      '[ "$(orders_count "status = '"'"'confirmed'"'"' AND source_node IN $(in_group "$K2")")" -gt '"$moved"' ]'; then
    pass "the weld moved after the carts came back"
  else
    fail "the weld took nothing from K2 after the carts came back"
  fi
}

# free_child_lane: a mouth in SM, where a cart taken out of the loop stands.
free_child_lane() {
  sql "SELECT n.name FROM nodes n JOIN nodes l ON l.id = n.parent_id JOIN nodes g ON g.id = l.parent_id
       WHERE g.name = '$SM' AND n.depth = 1 AND NOT EXISTS (SELECT 1 FROM bins b WHERE b.node_id = n.id)
       ORDER BY n.name LIMIT 1"
}

# ── THE WALK. For watching cart by cart, not a verdict: no step's failure stops it. ─────────
mark() {
  local line; line="$(date -u +%H:%M:%SZ) sim=$(sim_clock) $*"
  echo "$line" >>"$OUT/walk-log.txt"
  say "MARK $*"
}
sim_clock() { curl -s -m 5 "$CORE/api/sim/status" | sed -n 's/.*"sim_now":"\([^"]*\)".*/\1/p'; }

# walk_sampler: walk.tsv, walk-orders.tsv and walk-cards.jsonl, every 3 wall-seconds.
walk_sampler() {
  local unload_id="" id at sim positions
  for id in $(seq 1 12); do
    case "$(curl -s -m 10 "$EDGE/api/operator-stations/$id/view")" in
      *"\"code\":\"$UNLOAD_OPS\""*) unload_id="$id"; break ;;
    esac
  done
  printf 'at\tsim\tlabel\ttype\tnode\tgroup\tclaimed_order\n' >"$OUT/walk.tsv"
  printf 'at\tsim\tid\ttype\tpayload\tsource\tdelivery\tstatus\tqueue_cause\tage_s\n' >"$OUT/walk-orders.tsv"
  positions="'PLN_021','PLN_022','PLN_023','PLN_024'"
  while :; do
    at="$(date -u +%H:%M:%SZ)"; sim="$(sim_clock)"
    # Every carrier but the weld's own 3. A cart in flight stands on its transit node.
    sql "SELECT '$at', '$sim', b.label, t.code, COALESCE(n.name,''), COALESCE(g.name,''), COALESCE(b.claimed_by::text,'')
           FROM bins b JOIN bin_types t ON t.id = b.bin_type_id
           LEFT JOIN nodes n ON n.id = b.node_id LEFT JOIN nodes g ON g.id = n.parent_id
          WHERE b.label NOT LIKE 'KB-K2-%' AND b.label <> 'KB-W-F' ORDER BY b.label" >>"$OUT/walk.tsv" 2>/dev/null
    # The order rows: live, or moved since the sample before. Age on the orders' own clock.
    sql "WITH clock AS (SELECT max(updated_at) AS now FROM orders)
         SELECT '$at', '$sim', o.id, o.order_type, COALESCE(NULLIF(o.payload_code,''),'-'), o.source_node, o.delivery_node,
                o.status, COALESCE(NULLIF(o.queue_cause,''),'-'),
                round(EXTRACT(EPOCH FROM (clock.now - o.created_at))::numeric, 0)
           FROM orders o, clock
          WHERE (o.status NOT IN ('confirmed','failed','cancelled','skipped') OR o.updated_at > clock.now - interval '60 seconds')
            AND (o.source_node IN $(in_group "$K") OR o.delivery_node IN $(in_group "$K")
              OR o.source_node IN ($positions) OR o.delivery_node IN ($positions)
              OR o.source_node IN $(in_group "$UNLOAD_GROUP") OR o.delivery_node IN $(in_group "$UNLOAD_GROUP"))
          ORDER BY o.id" >>"$OUT/walk-orders.tsv" 2>/dev/null
    printf '{"at":"%s","sim":"%s","cards":%s,"board":%s}\n' "$at" "$sim" \
      "$(curl -s -m 5 "$CORE$KANBAN_API")" \
      "$(curl -s -m 5 "$EDGE/api/operator-stations/$unload_id/view" || echo null)" >>"$OUT/walk-cards.jsonl"
    sleep 3
  done
}

# walk_gate_and_cutover: wait for the gate, 15 wall-minutes at most, and press CUTOVER. A gate
# that never opens is marked and the walk goes on: the press stays where it is and what the
# next step finds is what the walk is for.
walk_gate_and_cutover() {
  local deadline=$(( $(date +%s) + 900 )) code
  while ! gate_open; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      curl -s -m 10 "$EDGE/api/processes/$PROCESS_ID/changeover/gate-status" >"$OUT/gate-at-timeout-$1.json"
      mark "GATE NEVER OPENED $1 (15 wall-min): gate-at-timeout-$1.json"
      return 1
    fi
    sleep 2
  done
  mark "GATE-OPEN $1"
  code="$(edge_post "/api/processes/$PROCESS_ID/changeover/cutover" '{}')"
  mark "CUTOVER $1: $code $(cut -c1-200 "$OUT/.last")"
  http_ok "$code"
}

# walk_start <style> <marker>: START and its marker. Refused past start()'s 180 s is marked.
walk_start() {
  if start "$1"; then
    mark "$2 START $1"
  else
    mark "$2 START $1 REFUSED: $(cut -c1-200 "$OUT/.last")"
  fi
}

run_W() {
  say "W: small settle, -> 32x30, -> big, each to cutover + 20 sim-min"
  mark "BEGIN"
  sim_min 20
  mark "STEP1 settled on small"
  walk_start "$STYLE_32" STEP2-START
  walk_gate_and_cutover 32x30
  sim_min 20
  mark "STEP3 20 sim-min on 32x30"
  walk_start "$STYLE_BIG" STEP4-START
  walk_gate_and_cutover big
  sim_min 20
  mark "END"
}

# hand_clear_windows: a person's relief, for the walk only: every empty on the press's windows
# has its parked push terminated and is moved by hand onto a free child of K that admits it.
# Without this a change off the floor leaves all 4 windows held and nothing past it can be seen.
hand_clear_windows() {
  local node label type oid slot done
  while IFS=$'\t' read -r node label type <&3; do
    [ -n "$label" ] || continue
    done=""
    # The sim operator files a new push about 2 s after a terminate, so each try does both.
    for _ in $(seq 1 30); do
      for oid in $(sql "SELECT id FROM orders WHERE source_node = '$node' AND status NOT IN ('confirmed','failed','cancelled','skipped')"); do
        core_post /api/orders/terminate "{\"order_id\":$oid}" >/dev/null
      done
      slot="$(free_child "$K" "$type")"
      if [ -n "$slot" ] && move_bin "$label" "$slot"; then mark "HAND $label $type $node -> $slot"; done=1; break; fi
      sleep 1
    done
    [ -n "$done" ] || mark "HAND $label on $node could not be moved"
  done 3< <(sql "SELECT n.name, b.label, t.code FROM bins b JOIN nodes n ON n.id = b.node_id JOIN nodes g ON g.id = n.parent_id
                JOIN bin_types t ON t.id = b.bin_type_id
                WHERE g.name = '$UNLOAD_GROUP' AND b.payload_code = '' ORDER BY n.name")
}

# run_W4: W with the windows cleared by hand after the cutover, because W wedges there: what
# step 3 and step 4 do once a person has made room. It is labelled in the log as a person's act.
run_W4() {
  say "W4: W, with the windows cleared by hand after the cutover to 32x30"
  mark "BEGIN"
  sim_min 20
  mark "STEP1 settled on small"
  walk_start "$STYLE_32" STEP2-START
  walk_gate_and_cutover 32x30
  sim_min 2
  hand_clear_windows
  mark "HAND windows cleared"
  sim_min 20
  mark "STEP3 20 sim-min on 32x30"
  walk_start "$STYLE_BIG" STEP4-START
  walk_gate_and_cutover big
  sim_min 20
  mark "END"
}

run_W5() {
  say "W5: small settle, -> 32x30, CANCEL with two positions held, -> big"
  mark "BEGIN"
  sim_min 20
  mark "STEP1 settled on small"
  walk_start "$STYLE_32" STEP2-START
  local deadline=$(( $(date +%s) + 900 ))
  while [ "$(on_press "$C32")" -lt 2 ]; do
    if [ "$(date +%s)" -ge "$deadline" ]; then mark "TWO POSITIONS NEVER HELD 32x30 (15 wall-min)"; break; fi
    sleep 2
  done
  mark "$(on_press "$C32") positions hold 32x30"
  if cancel_changeover; then mark "CANCEL"; else mark "CANCEL REFUSED: $(cut -c1-200 "$OUT/.last")"; fi
  sim_min 10
  mark "10 sim-min after the CANCEL"
  walk_start "$STYLE_BIG" STEP4-START
  walk_gate_and_cutover big
  sim_min 10
  mark "END"
}

# ── THE RUN ─────────────────────────────────────────────────────────────────────────────
. scripts/gate-lock.sh
gate_lock_acquire "kanban-rig $RUN" || { echo "no gate lock" >&2; exit 3; }
SAMPLER=""
WALKER=""
cleanup() {
  [ -n "$SAMPLER" ] && kill "$SAMPLER" 2>/dev/null
  [ -n "$WALKER" ] && kill "$WALKER" 2>/dev/null
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
case "$RUN" in W|W4|W5) walk_sampler & WALKER=$! ;; esac
say "run $RUN: Core $CORE, Edge $EDGE, out $OUT"

"run_$RUN"

collect
say "done; the stack stays up"
echo
cat "$OUT/verdict.txt"
exit "$FAILED"
