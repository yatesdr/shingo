#!/usr/bin/env bash
# bash scripts/kanban-rig.sh <R1..R6|W|W5|W6|P1..P6> — one run of the Kanban certification rig
# on the visible dev stack: compose project shingo-dev, plants/master.yaml (R and W: its [kanban]
# section; P1..P6: its [kanban5]), Core :8083, Edge :8081. docs/dev-env/kanban-rig.md says
# what each run injects and catches. The master's other sections run beside the loop, so every
# order count below is the loop's own (orders_count).
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
#   orders.tsv          every order at the run's end, order-history.tsv every history row: a
#                       long order is read from these, the database goes with the next run
#   family-watch.tsv    every 3 s, K's families: pool, slot, line, window, flight, found
#   cart-watch.tsv      every 3 s, each cart of the loop: where it stands, its hard claim, its
#                       live orders, its live legs, the last order that named it
#
# W, W5 and W6, the walk, add, every 3 wall-seconds from the run's start:
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
#      SHINGO_BIND (default 127.0.0.1), KANBAN_RIG_P5_CANCEL_ON_ROBOT=1 (P5's CANCEL waits
#      for a 32x30 on a robot bound for press 2).
set -uo pipefail
export MSYS_NO_PATHCONV=1

# ── THE NAMES, in one place: the rename pass moves these ────────────────────────────────
KANBAN_API=/api/nodes/kanban/levels
FAMILY_API=/api/nodes/kanban/families
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
POSITIONS="'PLN_021','PLN_022','PLN_023','PLN_024'"
# The weld's own loop beside the press's, in the [kanban] section: its nodes and its unloader.
WELD_NODES="'ALN_021','SLN_021'"
WELD_UNLOAD=WELD-UNLOAD
LIT_NONE_SHORT='"none_short":true'  # the card's "no group is short", on a tile's assignment
PARKED_AT_LEVEL=ngrp-at-level       # dispatch.CauseNGRPAtLevel, a stranger refused at a level
PARKED_NO_SLOT=waiting_for_slot     # the history code of a push no slot admits
NO_EMPTY=finder-no-empty-of-type    # the queue cause of an ask with nothing to fetch

SIM_SPEED=10            # shingo-*/*.dev.yaml sim.speed; 1 sim-minute is 6 wall-seconds
CORE=http://127.0.0.1:8083
EDGE=http://127.0.0.1:8081
PLANT=/plants/master.yaml
PROJECT=shingo-dev

RUN="${1:-}"
case "$RUN" in
  R1|R2|R3|R4|R5|R6|W|W5|W6) ;;
  P1|P2|P3|P4|P5|P6)
    # [kanban5]: the five presses' loop. Its raid supermarket is [kanban]'s KP-SM.
    K=P5-KANBAN
    K2=""
    WELD_NODES="''"
    WELD_UNLOAD=""
    UNLOAD_GROUP=P5-UNLOAD
    PRESS_OPS=P5-PRESS-1-OPS
    UNLOAD_OPS=P5-UNLOAD-OPS
    POSITIONS="$(for p in 1 2 3 4 5; do for i in 1 2 3 4; do printf "'PLN_5%d%d'," "$p" "$i"; done; done | sed 's/,$//')"
    ;;
  *) echo "usage: bash scripts/kanban-rig.sh <R1..R6|W|W5|W6|P1..P6>" >&2; exit 2 ;;
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

# step_node <order alias> <step expr>: the SQL for where a plan step happens, as Core's
# helpers.StepNodeSQL spells it (an order's carts are its order_bins rows, one per lift, and a
# row names steps, not nodes): the step's node, else its group; a plain order's 0 and 1 are its
# source and delivery.
step_node() {
  local o="$1" i="$2"
  printf "(CASE WHEN (%s) IS NULL OR (%s) < 0 THEN '' WHEN left(COALESCE(%s.steps_json,''),1) = '[' AND jsonb_array_length(%s.steps_json::jsonb) > 0 THEN COALESCE(NULLIF(%s.steps_json::jsonb->(%s)->>'node',''), %s.steps_json::jsonb->(%s)->>'group','') WHEN (%s) = 0 THEN COALESCE(%s.source_node,'') WHEN (%s) = 1 THEN COALESCE(%s.delivery_node,'') ELSE '' END)" \
    "$i" "$i" "$o" "$o" "$o" "$i" "$o" "$i" "$i" "$o" "$i" "$o"
}
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
  say "$PRESS_OPS is Edge process $PROCESS_ID"
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
    printf '{"at":"%s","cards":%s,"families":%s}\n' "$(date -u +%H:%M:%SZ)" "$(curl -s -m 10 "$CORE$KANBAN_API")" \
      "$(curl -s -m 10 "$CORE$FAMILY_API?group_node_id=$K_ID")" >>"$OUT/kanban-cards.jsonl"
    [ -n "$unload_id" ] && printf '{"at":"%s","view":%s}\n' "$(date -u +%H:%M:%SZ)" \
      "$(curl -s -m 10 "$EDGE/api/operator-stations/$unload_id/view")" >>"$OUT/lit.jsonl"
    sql "SELECT to_char(now(),'HH24:MI:SS'), n.name, COALESCE(b.label,''), COALESCE(t.code,''), COALESCE(b.payload_code,''),
                (SELECT count(*) FROM order_bins ob JOIN orders o ON o.id = ob.order_id WHERE ob.bin_id = b.id
                  AND o.status NOT IN ('confirmed','failed','cancelled','skipped'))
           FROM nodes n JOIN nodes g ON g.id = n.parent_id
           LEFT JOIN bins b ON b.node_id = n.id LEFT JOIN bin_types t ON t.id = b.bin_type_id
          WHERE g.name IN ('$UNLOAD_GROUP','WELD-UNLOAD') ORDER BY n.name" >>"$OUT/windows.tsv" 2>/dev/null
    sleep 30
  done
}

# loop_watcher: found by place and every cart, every 3 s. Found is read before the carts, so a
# row of family-watch.tsv reads against the cart-watch.tsv rows with the same `at`.
loop_watcher() {
  local at
  printf 'at\tfamily\tpool\tslot\tline\twindow\tflight\tfound\n' >"$OUT/family-watch.tsv"
  printf 'at\ttype\tfamily\tlabel\tnode\tgroup\tclaimed_order\tlive_orders\tlive_legs\tlast_named\n' >"$OUT/cart-watch.tsv"
  while :; do
    at="$(date -u +%H:%M:%SZ)"
    curl -s -m 5 "$CORE$FAMILY_API?group_node_id=$K_ID" \
      | grep -o '"family":"[^"]*","family_id":[0-9]*,"pool":[0-9]*,"pool_declared":[a-z]*,"minimum":[0-9]*,"mix":[0-9]*,"slot":[0-9]*,"line":[0-9]*,"window":[0-9]*,"flight":[0-9]*,"found":[0-9]*' \
      | sed -E "s/.*\"family\":\"([^\"]*)\".*\"pool\":([0-9]+),.*\"slot\":([0-9]+),\"line\":([0-9]+),\"window\":([0-9]+),\"flight\":([0-9]+),\"found\":([0-9]+)/$at\t\1\t\2\t\3\t\4\t\5\t\6\t\7/" \
      >>"$OUT/family-watch.tsv"
    sql "SELECT '$at', t.code, f.code, b.label, COALESCE(n.name,''), COALESCE(g.name,''), COALESCE(b.claimed_by::text,''),
                COALESCE((SELECT string_agg(o.id||':'||o.status||':'||COALESCE(o.source_node,'')||'>'||COALESCE(o.delivery_node,''), ',')
                            FROM orders o WHERE (o.named_bin_id = b.id
                                 OR EXISTS (SELECT 1 FROM order_bins ob WHERE ob.order_id = o.id AND ob.bin_id = b.id))
                             AND o.status NOT IN ('confirmed','failed','cancelled','skipped')),''),
                COALESCE((SELECT string_agg(o.id||':'||o.status||':'||$(step_node o ob.pick_step)||'>'||$(step_node o ob.drop_step), ',')
                            FROM order_bins ob JOIN orders o ON o.id = ob.order_id WHERE ob.bin_id = b.id
                             AND o.status NOT IN ('confirmed','failed','cancelled','skipped')),''),
                COALESCE((SELECT x.id||':'||x.status||':'||x.s||'>'||x.d FROM (
                            SELECT o.id, o.status, COALESCE(o.source_node,'') s, COALESCE(o.delivery_node,'') d
                              FROM orders o WHERE o.named_bin_id = b.id
                                AND NOT EXISTS (SELECT 1 FROM order_bins ob WHERE ob.order_id = o.id)
                            UNION ALL
                            SELECT o.id, o.status, $(step_node o ob.pick_step), $(step_node o ob.drop_step)
                              FROM order_bins ob JOIN orders o ON o.id = ob.order_id WHERE ob.bin_id = b.id
                          ) x ORDER BY x.id DESC LIMIT 1),'')
           FROM bins b JOIN bin_types t ON t.id = b.bin_type_id JOIN carrier_families f ON f.id = t.family_id
           LEFT JOIN nodes n ON n.id = b.node_id LEFT JOIN nodes g ON g.id = n.parent_id
          WHERE b.label NOT LIKE '%-SM-%' ORDER BY f.code, b.label" >>"$OUT/cart-watch.tsv" 2>/dev/null
    sleep 3
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
  sql "SELECT id, order_type, status, COALESCE(source_node,''), COALESCE(delivery_node,''), COALESCE(queue_cause,''),
              COALESCE(error_detail,''), created_at, updated_at, completed_at,
              COALESCE((SELECT string_agg(ob.bin_id::text, '+' ORDER BY ob.id) FROM order_bins ob WHERE ob.order_id = orders.id),''),
              COALESCE(parent_order_id::text,''), COALESCE(edge_uuid,'') FROM orders ORDER BY id" >"$OUT/orders.tsv"
  sql "SELECT * FROM order_history ORDER BY order_id, id" >"$OUT/order-history.tsv"
  ( cd shingo-core && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$OUT/soakstat" ./cmd/soakstat ) \
    || { fail "soakstat did not build"; return; }
  local cid; cid="$(dc ps -q core)"
  docker cp "$OUT/soakstat" "$cid:/tmp/soakstat" >/dev/null 2>&1 || { fail "could not place soakstat"; return; }
  docker exec "$cid" /tmp/soakstat -config /etc/shingo/shingocore.dev.yaml -stranded >"$OUT/stranded.txt" 2>&1
  docker exec "$cid" /tmp/soakstat -config /etc/shingo/shingocore.dev.yaml >"$OUT/soakstat.txt" 2>&1
  rm -f "$OUT/soakstat"

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

# orders_count <where>: the loop's orders matching a condition, since the run began. The
# master runs every other section beside the loop, so an order is the loop's when it starts
# or ends at one of its nodes (loop_nodes).
orders_count() { sql "SELECT count(*) FROM orders WHERE ($1) AND (source_node IN $(loop_nodes) OR delivery_node IN $(loop_nodes))"; }

# loop_nodes: the SQL list of the loop's nodes: the press's positions, the weld's, and every
# node of its kanbans, its supermarket and its unloaders, two levels down as in_group reads.
loop_nodes() {
  local groups="'$K','$K2','$SM','$UNLOAD_GROUP','$WELD_UNLOAD'"
  echo "(SELECT unnest(ARRAY[$POSITIONS,$WELD_NODES,$groups])
         UNION SELECT n.name FROM nodes n JOIN nodes g ON g.id = n.parent_id WHERE g.name IN ($groups)
         UNION SELECT n.name FROM nodes n JOIN nodes l ON l.id = n.parent_id
                                          JOIN nodes g ON g.id = l.parent_id WHERE g.name IN ($groups))"
}

# in_group <group>: the SQL list of a group's own name, its children's and, for a group of
# lanes, its lanes' slots: SM's orders deliver two levels down.
in_group() {
  echo "(SELECT '$1'
         UNION SELECT n.name FROM nodes n JOIN nodes g ON g.id = n.parent_id WHERE g.name = '$1'
         UNION SELECT n.name FROM nodes n JOIN nodes l ON l.id = n.parent_id
                                          JOIN nodes g ON g.id = l.parent_id WHERE g.name = '$1')"
}

never_to_sm() { check_zero "no order delivered to $SM" "$(orders_count "delivery_node IN $(in_group "$SM")")"; }

# lit_other_than <type>: unloader assignments naming any other type, across the samples. A blank
# assignment is "no group is short", which lights nothing.
lit_other_than() {
  grep -o '"assignment":{"bin_type":"[^"]*"' "$OUT/lit.jsonl" 2>/dev/null | grep -v '"bin_type":""' | grep -vc "\"$1\"" || true
}

# ── THE RUNS. A criterion the fixture must meet stays a check, failing if it fails. ──────

# mix_is <small> <big> <totes> <32x30>: K stands exactly that many empties of each.
mix_is() {
  [ "$(empties "$K" "$SMALL")" = "$1" ] && [ "$(empties "$K" "$BIG")" = "$2" ] \
    && [ "$(empties "$K" "$TOTES")" = "$3" ] && [ "$(empties "$K" "$C32")" = "$4" ]
}
mix_said() {
  echo "K S$(empties "$K" "$SMALL") B$(empties "$K" "$BIG") T$(empties "$K" "$TOTES") C$(empties "$K" "$C32")"
}
# expect_mix <what> <small> <big> <totes> <32x30>: waited for, a refill's carts leave K between two
# samples.
expect_mix() {
  local what=$1; shift
  if wait_for 180 "$what: K S$1 B$2 T$3 C$4" "mix_is $1 $2 $3 $4"; then
    pass "$what: K S$1 B$2 T$3 C$4"
  else
    fail "$what: $(mix_said), want S$1 B$2 T$3 C$4"
  fi
}

# expect_on_press <what> <type> <n>: waited for, a swap takes a position's cart between two samples.
expect_on_press() {
  if wait_for 180 "$1" "[ \"\$(on_press \"$2\")\" = \"$3\" ]"; then
    pass "$1 ($3)"
  else
    fail "$1: got $(on_press "$2"), want $3"
  fi
}

# found_of <family>: K's found for one family, now, off the families read.
found_of() {
  curl -s -m 10 "$CORE$FAMILY_API?group_node_id=$K_ID" \
    | grep -o "\"family\":\"$1\",\"family_id\":[0-9]*,\"pool\":[0-9]*,\"pool_declared\":[a-z]*,\"minimum\":[0-9]*,\"mix\":[0-9]*,\"slot\":[0-9]*,\"line\":[0-9]*,\"window\":[0-9]*,\"flight\":[0-9]*,\"found\":[0-9]*" \
    | sed -n 's/.*"found":\([0-9]*\)$/\1/p' | head -1
}

# found_held: found equals the pool at every sample of the run, per (kanban, family).
found_held() {
  local bad
  bad="$(grep -o '"family":"[^"]*","family_id":[0-9]*,"pool":[0-9]*,"pool_declared":[a-z]*,"minimum":[0-9]*,"mix":[0-9]*,"slot":[0-9]*,"line":[0-9]*,"window":[0-9]*,"flight":[0-9]*,"found":[0-9]*' \
    "$OUT/kanban-cards.jsonl" 2>/dev/null \
    | sed -E 's/.*"family":"([^"]*)".*"pool":([0-9]+),.*"found":([0-9]+)/\1 \2 \3/' \
    | awk '$2 != $3 { n++ } END { print n + 0 }')"
  check_zero "samples where found differs from the pool" "$bad"
  check_zero "3 s samples where found differs from the pool" \
    "$(awk -F'\t' 'NR > 1 && $3 != $8 { n++ } END { print n + 0 }' "$OUT/family-watch.tsv")"
}

# sm_unchanged: the plain supermarket stands the 4 carts its plant file stocks it with.
sm_unchanged() {
  check "$SM still holds what it was stocked with" "$(sql "SELECT count(*) FROM bins b JOIN nodes n ON n.id = b.node_id
      JOIN nodes l ON l.id = n.parent_id JOIN nodes g ON g.id = l.parent_id WHERE g.name = '$SM' AND b.label LIKE '%-SM-%'")" 4
  check_zero "no keeper's ask outside the loop" "$(orders_count "edge_uuid LIKE 'core-mnt-%'")"
}

run_R1() {
  say "R1: steady state on small, 20 wall-minutes"
  sleep $(( 20 * 60 ))
  check "K wants small 6" "$(card "$K" "$SMALL" want)" 6
  check "K2 wants small 3" "$(card "$K2" "$SMALL" want)" 3
  check_zero "unloader carts lit anything but small" "$(lit_other_than "$SMALL")"
  if wait_for 120 "K's small rests at 2" '[ "$(card "$K" "$SMALL" resident)" = 2 ]'; then
    pass "K's small line rests at want 6, resident 2, held on the line"
  else
    fail "K's small line never rested at 2: resident $(card "$K" "$SMALL" resident)"
  fi
  check "K's small reads short 0" "$(card "$K" "$SMALL" gap)" 0
  check_zero "pushes parked at a level" "$(orders_count "queue_cause = '$PARKED_AT_LEVEL'")"
  check_zero "orders live and queued on a kanban at the end" \
    "$(orders_count "status IN ('queued','sourcing') AND delivery_node IN $(in_group "$K")")"
  found_held
  sm_unchanged
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
  expect_mix "on totes" 2 2 2 6
  expect_on_press "the 4 positions hold totes" "$TOTES" 4
  check_zero "K2's keeper took from K" "$(orders_count "delivery_node IN $(in_group "$K2") AND source_node IN $(in_group "$K")")"
  found_held
  sm_unchanged
  never_to_sm
}

run_R3() {
  say "R3: totes -> 32x30 -> totes, each to cutover + 10 sim-min"
  sim_min 10
  changeover_to "$STYLE_TOTES" || return
  changeover_to "$STYLE_32" || return
  check "K wants 32x30 6" "$(card "$K" "$C32" want)" 6
  if wait_for 120 "the positions hold 32x30" '[ "$(on_press "$C32")" = 4 ]'; then
    pass "the 4 positions hold 32x30"
  else
    fail "the positions hold $(on_press "$C32") of 4 32x30"
  fi
  # The 4 off the positions have no level to land on and land all the same: a return is never
  # refused at a level. Their slots stand free for them.
  expect_mix "on 32x30, the family's extras stand as what they came off as" 2 2 6 2
  check_zero "carts parked at K's level on the windows under $STYLE_32" "$(parked_on_windows)"
  changeover_to "$STYLE_TOTES" || return
  expect_mix "back on totes" 2 2 2 6
  check_zero "carts parked at K's level on the windows, back on $STYLE_TOTES" "$(parked_on_windows)"
  expect_on_press "the 4 positions hold totes" "$TOTES" 4
  found_held
  sm_unchanged
  never_to_sm
}

# parked_on_windows: the pushes standing parked at a level, off the press's windows.
parked_on_windows() {
  orders_count "status = 'queued' AND queue_cause = '$PARKED_AT_LEVEL' AND source_node IN $(in_group "$UNLOAD_GROUP")"
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
  check_zero "carts parked at K's level after the CANCEL" "$(parked_on_windows)"
  start "$STYLE_BIG" || { fail "START big refused"; return; }
  cutover 600 || { fail "small -> big did not reach its cutover"; return; }
  pass "cut over to big"
  sim_min 10
  check_zero "totes left on the positions: the next START lifts them" "$(on_press "$TOTES")"
  found_held
  sm_unchanged
  never_to_sm
}

# on_press <type>: carts of a type on the press's four positions.
on_press() {
  sql "SELECT count(*) FROM bins b JOIN nodes n ON n.id = b.node_id JOIN bin_types t ON t.id = b.bin_type_id
       WHERE n.name IN ($POSITIONS) AND t.code = '$1'"
}

# plant_at_rest: no order of the loop moving and none on its way, staged swaps aside. A cart stood on a slot by
# hand ahead of a cart already filed to it holds that cart on its robot (STOPS 15).
plant_at_rest() {
  wait_for 300 "the plant at rest" \
    '[ "$(orders_count "status NOT IN ('"'"'confirmed'"'"','"'"'failed'"'"','"'"'cancelled'"'"','"'"'skipped'"'"','"'"'staged'"'"')")" = 0 ]' \
    || fail "the plant never came to rest"
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

# R5: every slot admitting the family taken, the next cart parks visible and lands when a slot
# frees. Then a cart carried out of the loop: the family is one short, and its keeper takes one
# from SM, one ask for one cart, and nothing ever repays SM.
run_R5() {
  say "R5: K full parks the next cart; a cart lost is one ask; SM is not repaid"
  sim_min 10

  local i slot
  for i in 1 2 3 4; do
    plant_at_rest
    slot="$(free_child "$K" "$SMALL")"
    [ -n "$slot" ] || die "no free slot in K for a small"
    hand_register "KB-HAND-$i-" "$SMALL" "$slot" || fail "could not register a cart on $slot"
  done
  if wait_for 180 "K holds 6 small" '[ "$(card "$K" "$SMALL" resident)" = 6 ]'; then
    pass "K over-handed reads resident 6"
  else
    fail "K never read 6: resident $(card "$K" "$SMALL" resident)"
  fi
  check_zero "asks outside with K above its pool" "$(orders_count "edge_uuid LIKE 'core-mnt-%'")"

  # The next cart: one more stood on a window, as the next off the process would. The card stands
  # only while the cart is on its window, so the board is read every second here.
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
    # From the order's history: a park can last seconds, and a slot freeing ends it between samples.
    parked="$(sql "SELECT o.id FROM orders o JOIN bins b ON (b.id = o.named_bin_id
                     OR b.id IN (SELECT ob.bin_id FROM order_bins ob WHERE ob.order_id = o.id))
                     JOIN order_history h ON h.order_id = o.id
                   WHERE b.label = 'KB-HAND-5-0001' AND h.status = 'queued' AND h.code = '$PARKED_NO_SLOT'
                     AND h.detail LIKE 'No slot of $K admits%' LIMIT 1")"
    [ -n "$parked" ] && [ "$saw" = yes ] && break
    sleep 1
  done
  check "the card says no group is short" "$saw" yes
  check "the cart parks named at K, no slot" "$([ -n "$parked" ] && echo yes || echo no)" yes
  if [ -n "$parked" ]; then
    if wait_for 180 "the parked cart left when a slot freed" \
        "[ \"\$(sql \"SELECT status NOT IN ('queued','pending','sourcing') FROM orders WHERE id = $parked\")\" = t ]"; then
      pass "the parked cart left when a slot freed"
    else
      fail "the parked cart stayed parked: $(sql "SELECT status, queue_cause FROM orders WHERE id = $parked")"
    fi
  fi
  check_zero "pushes parked at a level" "$(orders_count "queue_cause = '$PARKED_AT_LEVEL'")"
  never_to_sm

  # The over-hand leaves the floor, each once it stands free, so the family is whole; then one
  # cart is carried out of the loop.
  local left=""
  for _ in $(seq 1 150); do
    sql "UPDATE bins SET node_id = NULL WHERE label LIKE 'KB-HAND-%' AND claimed_by IS NULL
          AND node_id IN (SELECT id FROM nodes WHERE parent_id = (SELECT id FROM nodes WHERE name = '$K'))" >/dev/null
    left="$(sql "SELECT count(*) FROM bins WHERE label LIKE 'KB-HAND-%' AND node_id IS NOT NULL")"
    [ "$left" = 0 ] && break
    sleep 2
  done
  check_zero "over-hand carts still on the floor" "$left"
  sim_min 2
  local asks; asks="$(orders_count "edge_uuid LIKE 'core-mnt-%'")"
  local cart spot
  for _ in $(seq 1 30); do
    cart="$(oldest_empty "$K" "$SMALL")"; spot="$(free_child_lane)"
    if [ -n "$cart" ] && [ -n "$spot" ] && move_bin "$cart" "$spot"; then break; fi
    cart=""
    sleep 2
  done
  [ -n "$cart" ] || die "could not carry a cart to $SM"
  if wait_for 180 "K's keeper asked for the one it lost" \
      '[ "$(orders_count "edge_uuid LIKE '"'"'core-mnt-%'"'"' AND status = '"'"'confirmed'"'"'")" -gt '"$asks"' ]'; then
    pass "K's keeper took one off $SM: $(sql "SELECT source_node || ' -> ' || delivery_node FROM orders WHERE edge_uuid LIKE 'core-mnt-%' ORDER BY id DESC LIMIT 1")"
  else
    fail "K's keeper never fetched the cart it lost"
  fi
  sim_min 2
  never_to_sm
  check_zero "asks standing queued on an empty that does not exist" \
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

# R6: a cart in the wrong buffer goes home. K2's two carried into K, K stands above its pool and
# K2 below. K2's keeper takes what stands above K's pool before any other cart, and nothing K owns;
# K never drops below its pool. SM holds carts too and gives none: K ends at 10 of 10.
run_R6() {
  say "R6: K2's carts carried into K: K2's keeper takes K's carts above its pool first"
  sim_min 10
  carry_small "$K2" "$K" || fail "could not take K2's first cart"
  carry_small "$K2" "$K" || fail "could not take K2's second cart"
  if wait_for 240 "K2 whole again" '[ "$(empties "$K2" "$SMALL")" = 2 ]'; then
    pass "K2 fetched back what it lost"
  else
    fail "K2 holds $(empties "$K2" "$SMALL"), want 2"
  fi
  check_zero "a cart K owns given away: K below its pool of small" \
    "$([ "$(empties "$K" "$SMALL")" -lt 2 ] && echo 1 || echo 0)"
  sim_min 10
  check "K's 45x48 ends at its pool" "$(found_of 45x48)" 10
  check "$SM gave none: K's extras went first" "$(sql "SELECT count(*) FROM bins b JOIN nodes n ON n.id = b.node_id
      JOIN nodes l ON l.id = n.parent_id JOIN nodes g ON g.id = l.parent_id WHERE g.name = '$SM' AND b.label LIKE '%-SM-%'")" 4
  never_to_sm
}

# free_child_lane: a mouth in SM, where a cart taken out of the loop stands.
free_child_lane() {
  sql "SELECT n.name FROM nodes n JOIN nodes l ON l.id = n.parent_id JOIN nodes g ON g.id = l.parent_id
       WHERE g.name = '$SM' AND NOT EXISTS (SELECT 1 FROM bins b WHERE b.node_id = n.id)
         AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.delivery_node = n.name AND o.status NOT IN ('confirmed','failed','cancelled','skipped'))
       ORDER BY n.depth, n.name LIMIT 1"
}

# ── THE WALK. Cart by cart, and its acceptance. No step's failure stops it. ────────
mark() {
  local line; line="$(date -u +%H:%M:%SZ) sim=$(sim_clock) $*"
  echo "$line" >>"$OUT/walk-log.txt"
  say "MARK $*"
}
sim_clock() { curl -s -m 5 "$CORE/api/sim/status" | sed -n 's/.*"sim_now":"\([^"]*\)".*/\1/p'; }

# walk_sampler: walk.tsv, walk-orders.tsv and walk-cards.jsonl, every 3 wall-seconds.
walk_sampler() {
  local unload_id="" id at sim
  for id in $(seq 1 12); do
    case "$(curl -s -m 10 "$EDGE/api/operator-stations/$id/view")" in
      *"\"code\":\"$UNLOAD_OPS\""*) unload_id="$id"; break ;;
    esac
  done
  printf 'at\tsim\tlabel\ttype\tnode\tgroup\tclaimed_order\n' >"$OUT/walk.tsv"
  printf 'at\tsim\tid\ttype\tpayload\tsource\tdelivery\tstatus\tqueue_cause\tage_s\n' >"$OUT/walk-orders.tsv"
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
          WHERE (o.status NOT IN ('confirmed','failed','cancelled','skipped') OR o.updated_at > clock.now - interval '30 seconds')
            AND (o.source_node IN $(in_group "$K") OR o.delivery_node IN $(in_group "$K")
              OR o.source_node IN ($POSITIONS) OR o.delivery_node IN ($POSITIONS)
              OR o.source_node IN $(in_group "$UNLOAD_GROUP") OR o.delivery_node IN $(in_group "$UNLOAD_GROUP"))
          ORDER BY o.id" >>"$OUT/walk-orders.tsv" 2>/dev/null
    printf '{"at":"%s","sim":"%s","cards":%s,"families":%s,"board":%s}\n' "$at" "$sim" \
      "$(curl -s -m 5 "$CORE$KANBAN_API")" \
      "$(curl -s -m 5 "$CORE$FAMILY_API?group_node_id=$K_ID")" \
      "$(curl -s -m 5 "$EDGE/api/operator-stations/$unload_id/view" || echo null)" >>"$OUT/walk-cards.jsonl"
    sleep 3
  done
}

# walk_gate_and_cutover: wait for the gate, 15 wall-minutes at most, and press CUTOVER.
walk_gate_and_cutover() {
  local deadline=$(( $(date +%s) + 900 )) code begin
  begin=$(date +%s)
  while ! gate_open; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      curl -s -m 10 "$EDGE/api/processes/$PROCESS_ID/changeover/gate-status" >"$OUT/gate-at-timeout-$1.json"
      mark "GATE NEVER OPENED $1 (15 wall-min): gate-at-timeout-$1.json"
      fail "the gate never opened for $1"
      return 1
    fi
    sleep 2
  done
  mark "GATE-OPEN $1 after $(( $(date +%s) - begin )) wall-s"
  pass "gate opened for $1 after $(( $(date +%s) - begin )) wall-s"
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
    fail "START $1 refused"
  fi
}

# windows_clear <what>: every cart off the windows, within the unloader's own handling.
windows_clear() {
  local n="$(sql "SELECT count(*) FROM bins b JOIN nodes n ON n.id = b.node_id JOIN nodes g ON g.id = n.parent_id
                  WHERE g.name = '$UNLOAD_GROUP' AND b.payload_code = ''")"
  check_zero "$1: empties standing on the windows" "$n"
}

run_W() {
  say "W: small settle, -> 32x30, steady, -> big"
  mark "BEGIN"
  sim_min 20
  mark "STEP1 settled on small"
  expect_mix "step 1, steady on small" 2 2 2 6
  expect_on_press "step 1: the line holds 4 small" "$SMALL" 4
  check "step 1: small reads short 0" "$(card "$K" "$SMALL" gap)" 0
  walk_start "$STYLE_32" STEP2-START
  walk_gate_and_cutover 32x30
  sim_min 20
  mark "STEP3 20 sim-min on 32x30"
  expect_mix "step 3, steady on 32x30: S2 B2 T2 held, the 4 extras small" 6 2 2 2
  expect_on_press "step 3: the line holds 4 32x30" "$C32" 4
  check_zero "step 3: the arm moves nothing with the mix whole" "$(orders_count "edge_uuid LIKE 'core-sp-%' AND status NOT IN ('confirmed','failed','cancelled','skipped')")"
  windows_clear "step 3"
  walk_start "$STYLE_BIG" STEP4-START
  walk_gate_and_cutover big
  sim_min 20
  mark "END"
  expect_mix "step 4 ends S2 B2 T2 C6, floaters 0" 2 2 2 6
  expect_on_press "step 4: the line holds 4 big" "$BIG" 4
  check_zero "pushes parked at a level" "$(orders_count "queue_cause = '$PARKED_AT_LEVEL'")"
  found_held
  sm_unchanged
  never_to_sm
}

# re_type <type>: an admin stamping every empty standing in K of the family, on the bin page's door.
re_type() {
  local bt label code
  bt="$(sql "SELECT id FROM bin_types WHERE code = '$1'")"
  for label in $(sql "SELECT b.label FROM bins b JOIN nodes n ON n.id = b.node_id JOIN nodes g ON g.id = n.parent_id
                       JOIN bin_types t ON t.id = b.bin_type_id
                      WHERE g.name = '$K' AND b.payload_code = '' AND b.claimed_by IS NULL AND t.code IN ('$SMALL','$BIG','$TOTES')"); do
    code="$(core_post /api/bins/action "{\"id\":$(bin_id "$label"),\"action\":\"update\",\"params\":{\"bin_type_id\":$bt}}")"
    http_ok "$code" || fail "re-type of $label: $code $(cut -c1-160 "$OUT/.last")"
  done
}

# W6: the owner's case: all ten of the family set big, then big -> small. 8 re-types: the
# line's 4 at the windows on their way back, K's 4 extras through the arm.
run_W6() {
  say "W6: -> big, every cart of the family set big, -> small"
  mark "BEGIN"
  sim_min 10
  walk_start "$STYLE_BIG" STEP-TO-BIG
  walk_gate_and_cutover big
  sim_min 10
  re_type "$BIG"
  local all_big; all_big="$(mix_said)"
  mark "ALL BIG: $all_big"
  check "all ten set big" "$all_big" "K S0 B6 T0 C6"
  walk_start "$STYLE_SMALL" STEP-TO-SMALL
  walk_gate_and_cutover small
  sim_min 20
  mark "END"
  expect_mix "back on small" 2 2 2 6
  expect_on_press "the line holds 4 small" "$SMALL" 4
  check_zero "pushes parked at a level" "$(orders_count "queue_cause = '$PARKED_AT_LEVEL'")"
  found_held
  sm_unchanged
  never_to_sm
}

run_W5() {
  say "W5: small settle, -> 32x30, CANCEL with two positions held, -> big"
  mark "BEGIN"
  sim_min 20
  mark "STEP1 settled on small"
  walk_start "$STYLE_32" STEP2-START
  local deadline=$(( $(date +%s) + 900 ))
  while [ "$(on_press "$C32")" -lt 2 ]; do
    if [ "$(date +%s)" -ge "$deadline" ]; then mark "TWO POSITIONS NEVER HELD 32x30 (15 wall-min)"; fail "two positions never held 32x30"; break; fi
    sleep 2
  done
  mark "$(on_press "$C32") positions hold 32x30"
  if cancel_changeover; then mark "CANCEL"; else mark "CANCEL REFUSED: $(cut -c1-200 "$OUT/.last")"; fail "CANCEL refused"; fi
  sim_min 10
  mark "10 sim-min after the CANCEL"
  check_zero "the CANCEL parked nothing" "$(parked_on_windows)"
  walk_start "$STYLE_BIG" STEP4-START
  walk_gate_and_cutover big
  sim_min 10
  mark "END"
  expect_on_press "the line holds 4 big" "$BIG" 4
  found_held
  sm_unchanged
  never_to_sm
}

# ── FIVE PRESSES ON ONE UNLOADER ───────────────────────
# press_n <n>: aim START, CANCEL and the gate at press n.
press_n() {
  PRESS_OPS="P5-PRESS-$1-OPS"
  local view; view="$(station_view "$PRESS_OPS")" || die "no operator station $PRESS_OPS on the Edge"
  PROCESS_ID="$(printf '%s' "$view" | grep -o '"process":{"id":[0-9]*' | grep -o '[0-9]*$' | head -1)"
  [ -n "$PROCESS_ID" ] || die "$PRESS_OPS names no process"
}
press_gate_cut() {
  press_n "$1"
  walk_gate_and_cutover "press $1 to $2"
}

run_P1() {
  say "P1: five presses steady on small, 20 wall-minutes"
  sleep $(( 20 * 60 ))
  check "the kanban wants small 22" "$(card "$K" "$SMALL" want)" 22
  expect_on_press "the positions hold 20 small" "$SMALL" 20
  check_zero "unloader carts lit anything but small" "$(lit_other_than "$SMALL")"
  check_zero "pushes parked at a level" "$(orders_count "queue_cause = '$PARKED_AT_LEVEL'")"
  found_held
  sm_unchanged
  never_to_sm
}

# P2: two presses leaving 45x48 in one cycle, while a third returns to it from 32x30.
run_P2() {
  say "P2: press 3 onto 32x30, then presses 1 and 2 onto 32x30 as press 3 comes back"
  sim_min 10
  press_n 3; walk_start P5-3-32 P3-OFF; walk_gate_and_cutover "press 3 to 32x30"
  sim_min 10
  press_n 1; start P5-1-32 || fail "START press 1"
  press_n 2; start P5-2-32 || fail "START press 2"
  press_n 3; start P5-3-SMALL || fail "START press 3 back"
  press_gate_cut 1 32x30
  press_gate_cut 2 32x30
  press_gate_cut 3 small
  sim_min 20
  expect_on_press "8 positions hold 32x30" "$C32" 8
  check_zero "pushes parked at a level" "$(orders_count "queue_cause = '$PARKED_AT_LEVEL'")"
  found_held
  sm_unchanged
  never_to_sm
}

# P3: the all-bigs case, on one press.
run_P3() {
  say "P3: press 1 onto big, every standing cart set big, back to small"
  sim_min 10
  press_n 1; walk_start P5-1-BIG P1-BIG; walk_gate_and_cutover "press 1 to big"
  sim_min 10
  re_type "$BIG"
  mark "ALL BIG: $(mix_said)"
  sim_min 2
  press_n 1; walk_start P5-1-SMALL P1-SMALL; walk_gate_and_cutover "press 1 to small"
  sim_min 20
  expect_mix "back on small" 2 2 2 22
  check_zero "pushes parked at a level" "$(orders_count "queue_cause = '$PARKED_AT_LEVEL'")"
  found_held
  sm_unchanged
  never_to_sm
}

# P4: a hand move out of the kanban's buffer into the plain supermarket: the family is one short, and
# one ask brings a cart home. Nothing is above any pool on this plant, so nothing else is reachable.
run_P4() {
  say "P4: a cart carried out by hand; one ask brings it home"
  sim_min 10
  local cart spot
  for _ in $(seq 1 30); do
    cart="$(oldest_empty "$K" "$BIG")"; spot="$(free_child_lane)"
    if [ -n "$cart" ] && [ -n "$spot" ] && move_bin "$cart" "$spot"; then break; fi
    cart=""
    sleep 2
  done
  [ -n "$cart" ] || die "could not carry a cart out"
  sim_min 10
  check "one ask brought the family home" "$(orders_count "edge_uuid LIKE 'core-mnt-%' AND status = 'confirmed'")" 1
  check_zero "pushes parked at a level" "$(orders_count "queue_cause = '$PARKED_AT_LEVEL'")"
  never_to_sm
}

# P5: CANCEL on one press while another changes over.
run_P5() {
  say "P5: presses 1 and 2 onto 32x30, press 2 cancels with two positions held"
  sim_min 10
  press_n 1; start P5-1-32 || fail "START press 1"
  press_n 2; start P5-2-32 || fail "START press 2"
  local deadline=$(( $(date +%s) + 600 ))
  while [ "$(on_press "$C32")" -lt 2 ]; do
    [ "$(date +%s)" -lt "$deadline" ] || { fail "no position took 32x30"; break; }
    sleep 2
  done
  press_n 2
  # KANBAN_RIG_P5_CANCEL_ON_ROBOT=1 holds the CANCEL for a 32x30 on a robot bound for press 2.
  if [ -n "${KANBAN_RIG_P5_CANCEL_ON_ROBOT:-}" ]; then
    deadline=$(( $(date +%s) + 300 ))
    until [ "$(sql "SELECT count(*) FROM bins b JOIN bin_types t ON t.id = b.bin_type_id JOIN nodes n ON n.id = b.node_id
                     JOIN order_bins ob ON ob.bin_id = b.id JOIN orders o ON o.id = ob.order_id
                    WHERE t.code = '$C32' AND (n.name = '_TRANSIT' OR n.name LIKE '\_ROBOT:%')
                      AND $(step_node o ob.drop_step) LIKE 'PLN_52%' AND o.status NOT IN ('confirmed','failed','cancelled','skipped')")" != 0 ]; do
      [ "$(date +%s)" -lt "$deadline" ] || { say "no 32x30 on a robot bound for press 2 in time"; break; }
    done
    say "CANCEL with a 32x30 on a robot"
  fi
  cancel_changeover || fail "CANCEL press 2"
  press_gate_cut 1 32x30
  sim_min 10
  check_zero "pushes parked at a level" "$(orders_count "queue_cause = '$PARKED_AT_LEVEL'")"
  found_held
  sm_unchanged
  never_to_sm
}

# P6: two presses STARTing onto BIG in one cycle.
run_P6() {
  say "P6: presses 1 and 2 onto big in one cycle"
  sim_min 10
  press_n 1; start P5-1-BIG || fail "START press 1"
  press_n 2; start P5-2-BIG || fail "START press 2"
  press_gate_cut 1 big
  press_gate_cut 2 big
  sim_min 20
  expect_on_press "8 positions hold big" "$BIG" 8
  expect_mix "after both" 2 2 2 22
  check_zero "pushes parked at a level" "$(orders_count "queue_cause = '$PARKED_AT_LEVEL'")"
  found_held
  sm_unchanged
  never_to_sm
}

# ── THE RUN ─────────────────────────────────────────────────────────────────────────────
. scripts/gate-lock.sh
gate_lock_acquire "kanban-rig $RUN" || { echo "no gate lock" >&2; exit 3; }
SAMPLER=""
WALKER=""
WATCHER=""
cleanup() {
  [ -n "$WATCHER" ] && kill "$WATCHER" 2>/dev/null
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
K_ID="$(node_id "$K")"
resolve_edge_ids
START_S=$(date +%s)
sample_loop & SAMPLER=$!
loop_watcher & WATCHER=$!
case "$RUN" in W|W5|W6|P2|P3|P5|P6) walk_sampler & WALKER=$! ;; esac
say "run $RUN: Core $CORE, Edge $EDGE, out $OUT"

"run_$RUN"

collect
say "done; the stack stays up"
echo
cat "$OUT/verdict.txt"
exit "$FAILED"
