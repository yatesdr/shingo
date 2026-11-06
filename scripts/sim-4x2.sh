#!/usr/bin/env bash
# bash scripts/sim-4x2.sh <S1..S8> [red] — one shape of the 4x2-shaped cell on the visible dev
# stack, for a person to watch: compose project shingo-dev on plants/master.yaml, whose [4x2]
# section is process WELD-6, five consume positions (ALN_061..065) and two styles, RUN (CATID
# 1001) and ALT (CATID 1002), sharing COMMON-1..3 and differing at the two mirror positions
# (MIRROR-L under RUN, MIRROR-R under ALT). EDGE 1 (:8081) serves WELD-6. Its fake PLC
# (shingoedge.dev.yaml) loads one assembly per 12 s tick into a 16-deep line, clicks the load
# station's STA_W6_LOAD.Load_Count per load and MES_WELD_6.Prod_Counter_01 as each load counts
# out, with the CATID beside the counter. Core is :8083.
#
#   S1        steady RUN, the five positions on the station counter; then all five on the cell
#             counter: the prepay profile (a landed cart reads L low until the line refills)
#   S2 [red]  changeover RUN->ALT; the carts land; the drain program change to 1002 before anyone
#             cuts over: the declaration completes it on a green gate within ~60 sim-s. `red`
#             holds the legs back, so the gate is red and the first counted ALT part forces it
#   S3        changeover RUN->ALT; "Mark as running" ALT at the landing while the PLC still says
#             1001; then the drain program change: no switch back to RUN, no hold
#   S4        no changeover, both styles pinned: the FORCED program change to 1002 under RUN;
#             holds at the mirror positions, the automatic switch to ALT after ~60 s and a
#             counted part, the holds released onto ALT's carts, the reorders
#   S5        the forced program change to 1003, which no style pins: every stroke's draw held;
#             Charge one (preview first), Write off another
#   S6        positions 1-3 on the station counter, 4-5 on the cell counter; RUN->ALT
#   S7        legs held, the Edge restarts mid-changeover; the forced program change; the holds
#             survive a second restart; the legs land and nothing is charged twice
#   S8        the PLC moves its count to the sibling slot Prod_Counter_02 (CATID_02 reads 1001;
#             Prod_Counter_01 stops): the strip's red "counts on Prod_Counter_02" item within the
#             Edge's slot watch (a pass a WALL minute; two when the move falls mid-pass), the click
#             re-points the counter; the record lacks what _02 counted before it, the carts nothing
#
# It brings the stack up from nothing as scripts/kanban-rig.sh does (down -v, a fresh sim anchor,
# build, seed with Core stopped), sets the PLC's CATID to 1001, drives the shape over Edge and
# Core HTTP at the stack's 10x, prints a line per step naming what to look at, leaves the stack
# and its pages up, and ends on the ledger (what each number is: see ledger):
#
#   carts Σ C = loads Σ E; holds open N; record RUN a / ALT b; Core received a+b of the Edge's T
#
# A run owns the stack: it refuses while shingo-dev has containers running, because its first
# act is `down -v`. SIM_4X2_REPLACE=1 says the running stack is yours to drop. It takes the gate
# lock (scripts/gate-lock.sh), so a gate and a run never overlap.
#
# Env: SIM_4X2_REPLACE, SIM_4X2_NO_BUILD=1 (reuse the images), SIM_4X2_OUT (default
#      ../sim-4x2-runs/<shape>-<stamp>), SIM_4X2_TICKS (one S1 window, default 40), SHINGO_BIND.
set -uo pipefail
export MSYS_NO_PATHCONV=1

# ── THE CELL, in one place ───────────────────────────────────────────────────────────────
PROC=WELD-6
PLC=WELD-6
COUNTER_TAG=MES_WELD_6.Prod_Counter_01
IDENTITY_TAG=MES_WELD_6.CATID_01
COUNTER_TAG2=MES_WELD_6.Prod_Counter_02   # the sibling slot S8 moves the count to
STYLE_RUN=WELD-6-RUN
STYLE_ALT=WELD-6-ALT
CATID_RUN=1001
CATID_ALT=1002
CATID_NONE=1003   # pinned by no style
POSITIONS="ALN_061 ALN_062 ALN_063 ALN_064 ALN_065"
MIRRORS="ALN_064 ALN_065"
POS_SQL="'ALN_061','ALN_062','ALN_063','ALN_064','ALN_065'"

# ── TIMING: sim seconds unless named wall ───────────────────────────────────────────────
SIM_SPEED=10                        # shingo-*/*.dev.yaml sim.speed, the stack's default
TICK_S=12                           # WELD-6's tick_interval
LINE_DEPTH=16                       # WELD-6's line_depth
DRAIN_S=$(( LINE_DEPTH * TICK_S ))  # a drain change lands once the line has counted out
DECLARE_S=60                        # the CATID monitor's stability window before it acts
RUN_TICKS="${SIM_4X2_TICKS:-40}"    # one S1 window
LAND_WALL=300                       # wall budget for a changeover's carts to land
SLOT_WATCH_WALL=60                  # the Edge's slot watch: a pass a WALL minute, not sim-paced

CORE=http://127.0.0.1:8083
EDGE=http://127.0.0.1:8081
PLANT=/plants/master.yaml
PROJECT=shingo-dev

usage() {
  echo "usage: bash scripts/sim-4x2.sh <S1..S8> [red]   (red: S2 only)" >&2
  echo "       make dev-sim-4x2 SHAPE=<S1..S8> [VARIANT=red]" >&2
  echo "The run starts with \`down -v\` of $PROJECT; set SIM_4X2_REPLACE=1 if the stack running now is yours to drop." >&2
  exit 2
}
SHAPE="${1:-}"
VARIANT="${2:-}"
case "$SHAPE" in S1|S2|S3|S4|S5|S6|S7|S8) ;; *) usage ;; esac
case "$VARIANT" in "") ;; red) [ "$SHAPE" = S2 ] || usage ;; *) usage ;; esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUT="${SIM_4X2_OUT:-$ROOT/../sim-4x2-runs/$SHAPE$VARIANT-$STAMP}"
mkdir -p "$OUT" || exit 1
OUT="$(cd "$OUT" && { pwd -W 2>/dev/null || pwd; })"
EDGE_COOKIES="$OUT/.edge-cookies"

export SHINGO_PLANT="$PLANT"
PID="" RUN_ID="" ALT_ID="" NODE_RE="" STATION=""   # resolve fills these off the Edge
dc() { docker compose -p "$PROJECT" -f docker-compose.dev.yml "$@"; }

say() { echo "$(date -u +%H:%M:%SZ) +$(( $(date +%s) - ${START_S:-$(date +%s)} ))s $*" | tee -a "$OUT/log.txt"; }
die() { say "STOP: $*"; exit 3; }
wall() { echo $(( ($1 + SIM_SPEED - 1) / SIM_SPEED )); }
sim_s() { sleep "$(wall "$1")"; }

# wait_for <wall seconds> <what> <predicate...>: polls every second; false, saying what it
# waited for, on timeout.
wait_for() {
  local budget=$1 what=$2 deadline=$(( $(date +%s) + $1 ))
  shift 2
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if eval "$*"; then say "ok: $what"; return 0; fi
    sleep 1
  done
  say "TIMED OUT after ${budget}s waiting for: $what"
  return 1
}

# ── HTTP ─────────────────────────────────────────────────────────────────────────────────
edge_get() { curl -s -m 15 -b "$EDGE_COOKIES" "$EDGE$1"; }
# edge_send <method> <path> <json>: the status code; the body lands in $OUT/.last.
edge_send() { curl -s -m 30 -o "$OUT/.last" -w '%{http_code}' -b "$EDGE_COOKIES" -X "$1" -H 'Content-Type: application/json' -d "$3" "$EDGE$2"; }
http_ok() { case "$1" in 2*) return 0 ;; *) return 1 ;; esac; }
last() { cut -c1-200 "$OUT/.last" 2>/dev/null; }
# field <json> <name>: the first number under that name.
field() { printf '%s' "$1" | grep -oE "\"$2\":-?[0-9]+" | head -1 | sed 's/.*://'; }
sql() { dc exec -T postgres psql -U shingocore -d shingocore -At -c "$1"; }

# Admin routes want a session. The Edge's first login creates its admin, as its login page does.
edge_login() {
  local code
  code="$(curl -s -m 10 -o "$OUT/.last" -w '%{http_code}' -c "$EDGE_COOKIES" -d "username=admin&password=admin" "$EDGE/login")"
  case "$code" in 30*) ;; *) die "Edge login refused ($code)" ;; esac
}

# ── READS ────────────────────────────────────────────────────────────────────────────────
proc_row() { edge_get /api/processes | tr '{' '\n' | grep "\"name\":\"$PROC\"" | head -1; }
active() { field "$(proc_row)" active_style_id; }
# The changeover's target is set at START and cleared when it completes or is cancelled.
target() { field "$(proc_row)" target_style_id; }
co_open() { [ -n "$(target)" ]; }
style_name() { case "${1:-}" in "") echo none ;; "$RUN_ID") echo RUN ;; "$ALT_ID") echo ALT ;; *) echo "style $1" ;; esac; }

gate_json() { edge_get "/api/processes/$PID/changeover/gate-status"; }
gate_green() { case "$(gate_json)" in *'"can_complete":true'*) return 0 ;; *) return 1 ;; esac; }
gate_word() {
  local g; g="$(gate_json)"
  case "$g" in *'"can_complete":true'*) echo green ;; *) echo "red, $(printf '%s' "$g" | grep -o '"reason":' | wc -l | tr -d ' ') blockers" ;; esac
}

# The fake PLC's own ledger of the line (GET /api/sim/line): loads entered, per identity, in
# flight, the identity, the counter. It lives in the Edge process and starts empty at its boot.
line_json() {
  curl -s -m 10 "$EDGE/api/sim/line" | grep -oE "\"plc_name\":\"$PLC\",\"tag_name\":\"${COUNTER_TAG//./[.]}\",\"loads\":[0-9]+,\"loads_by_identity\":(null|\{[^}]*\}),\"in_flight\":[0-9]+,\"identity\":-?[0-9]+,\"counter\":-?[0-9]+"
}
line_field() { field "$(line_json)" "$1"; }

# The fake's MES struct for WELD-6's line (its "slots" in GET /api/sim/line): "<n> <CATID_0n>
# <Prod_Counter_0n>" a line, n = 1..5.
slots_of() {
  curl -s -m 10 "$EDGE/api/sim/line" | grep -oE "\"tag_name\":\"${COUNTER_TAG//./[.]}\"[^]]*\]" \
    | grep -oE '\{"catid":-?[0-9]+,"counter":-?[0-9]+\}' | sed -E 's/.*"catid":(-?[0-9]+),"counter":(-?[0-9]+).*/\1 \2/' \
    | awk '{print NR, $0}'
}
slot_counter() { slots_of | awk -v n="$1" '$1 == n {print $3}'; }

# slot_item <kind>: the catid-health strip's first item of that kind (counter_slot_idle,
# counter_slot_lockstep), as JSON; empty when there is none.
slot_item() { edge_get "/api/processes/$PID/catid-health" | sed 's/{"kind":/\n{"kind":/g' | grep "^{\"kind\":\"$1\"" | head -1; }
counter_tag() { proc_row | grep -oE '"counter_tag_name":"[^"]*"' | head -1 | sed 's/.*:"//; s/"$//'; }

# The holds open on the process: the catid-health strip's "held" items, one "<hold id> <qty>"
# per line. Its automatic-decisions item names released holds too, so only held items count.
holds_list() {
  edge_get "/api/processes/$PID/catid-health" | sed 's/{"kind":/\n{"kind":/g' | grep '^{"kind":"held"' \
    | sed -E 's/.*"hold_id":([0-9]+).*"qty":(-?[0-9]+).*/\1 \2/'
}
holds_n() { holds_list | grep -c . ; }
hold_ids() { holds_list | cut -d' ' -f1 | tr '\n' ' ' | sed 's/ $//'; }

# Orders live at the five positions, off the Edge's active orders.
orders_at_positions() {
  edge_get /api/orders/active | grep -oE '"process_node_id":[0-9]+' | grep -cE ":($NODE_RE)\$"
}

status() {
  local lj co=""; lj="$(line_json)"
  co_open && co=" -> $(style_name "$(target)")"
  say "  now: style $(style_name "$(active)")$co | gate $(gate_word) | holds $(holds_n) | PLC CATID $(field "$lj" identity), in flight $(field "$lj" in_flight) | orders live at the positions $(orders_at_positions)"
}

# ── THE BUTTONS ──────────────────────────────────────────────────────────────────────────
# start_changeover <style id>: the station's START, pressed again while the screen refuses it
# ("a carrier is on its way" clears on its own).
start_changeover() {
  local code deadline=$(( $(date +%s) + 180 ))
  while :; do
    code="$(edge_send POST "/api/processes/$PID/changeover/start" "{\"to_style_id\":$1,\"called_by\":\"sim-4x2\"}")"
    say "START $(style_name "$1"): $code $(last)"
    http_ok "$code" && return 0
    [ "$(date +%s)" -lt "$deadline" ] || die "START refused for 180 s"
    sleep 5
  done
}

# mark_running <style id>: the admin switch ("Mark as running"); with a changeover open to that
# style it completes it through the door.
mark_running() {
  local code; code="$(edge_send PUT "/api/processes/$PID/active-style" "{\"style_id\":$1}")"
  say "Mark as running $(style_name "$1"): $code $(last)"
  http_ok "$code"
}

# program_change <value> <drain|forced>, written on the process's COUNTER tag.
program_change() {
  local code; code="$(edge_send POST /api/sim/program-change "{\"plc_name\":\"$PLC\",\"tag_name\":\"$COUNTER_TAG\",\"value\":$1,\"mode\":\"$2\"}")"
  say "PLC program change to $1 ($2): $code $(last)"
  http_ok "$code" || die "the program change was refused"
}

# counting_slot <n>: the PLC's program counts the part on slot n of MES_WELD_6, written on the
# process's configured counter tag: Prod_Counter_0n ticks from here, CATID_0n reads the part.
counting_slot() {
  local code; code="$(edge_send POST /api/sim/counting-slot "{\"plc_name\":\"$PLC\",\"tag_name\":\"$COUNTER_TAG\",\"slot\":$1}")"
  say "PLC counts on slot $1 of MES_WELD_6: $code $(last)"
  http_ok "$code" || die "the slot move was refused"
}

# set_catid <value>: the PLC's identity tag set directly; the fake starts every boot at 0.
set_catid() {
  local code; code="$(edge_send POST /api/sim/catid "{\"plc_name\":\"$PLC\",\"tag_name\":\"$IDENTITY_TAG\",\"value\":$1}")"
  say "PLC CATID set to $1: $code"
  http_ok "$code" || die "the CATID write was refused"
}

# counted_at <position> <station counter id | null>: null counts it at the cell counter.
counted_at() {
  local code; code="$(edge_send PUT "/api/process-nodes/${NODE_ID[$1]}/counted-at" "{\"line_counter_id\":$2}")"
  say "$1 counted at $([ "$2" = null ] && echo 'the cell counter' || echo "station counter $2"): $code"
  http_ok "$code" || die "counted-at refused: $(last)"
}

# pin <style id> <catid>: the health strip's one-click, the CATID joins the style's set.
pin() {
  local code; code="$(edge_send POST "/api/processes/$PID/styles/$1/catid" "{\"catid\":\"$2\",\"evidence\":\"sim-4x2 $SHAPE\"}")"
  say "pin $2 on $(style_name "$1"): $code $(last)"
  http_ok "$code"
}

# The hold verbs, one function each. The Preview is being folded into POST .../charge with a
# mode value, so a route rename is a one-line change here and nowhere else.
hold_preview() { edge_get "/api/catid-holds/$1/preview"; }
hold_charge() { edge_send POST "/api/catid-holds/$1/charge" '{}'; }
hold_write_off() { edge_send POST "/api/catid-holds/$1/write-off" '{}'; }

# legs_held <on|off>: Core's sim fleet refuses every create while on, and Core parks each refused
# order and retries it, so a changeover's legs stay unlanded and its gate red. It refuses every
# order in the plant, not only the cell's; cleanup turns it off.
LEGS_HELD=0
legs_held() {
  local on=0; [ "$1" = on ] && on=1
  curl -s -m 10 -o /dev/null -X POST "$CORE/api/sim/create-fails?on=$on"
  LEGS_HELD=$on
  say "Core's sim fleet refuses creates: $1"
}

# ── PREDICATES for wait_for ──────────────────────────────────────────────────────────────
landed() { co_open && gate_green; }
held_red() { co_open && ! gate_green; }
plc_on() { [ "$(line_field identity)" = "$1" ]; }
closed_on() { ! co_open && [ "$(active)" = "$1" ]; }
holds_at_least() { [ "$(holds_n)" -ge "$1" ]; }
no_holds() { [ "$(holds_n)" -eq 0 ]; }
no_orders_at_positions() { [ "$(orders_at_positions)" -eq 0 ]; }
line_live() { [ "${1:-0}" -lt "$(line_field loads)" ] 2>/dev/null; }
sim_epoch() { date -u -d "$(curl -s -m 5 "$EDGE/api/sim/status" | sed -n 's/.*"sim_now":"\([^"]*\)".*/\1/p')" +%s; }

# ── THE LEDGER ───────────────────────────────────────────────────────────────────────────
# mark opens a window and ledger closes it:
#   carts Σ   the units the five positions drew in the window: Core's applied count deltas
#             there (bin_uop_ledger, op bin_uop_delta, node-stamped) less the rise in their
#             lineside piles. No HTTP read sums a position's draws, so this one number is read
#             from Core's database, as kanban-rig.sh reads Core.
#   loads Σ   what the draws should be: a station-counted position draws once per load and a
#             cell-counted one once per load counted out (loads less the rise in flight), off
#             the PLC's line. Held draws are in it and not in the carts; the holds say how many.
#   record    the production ticks Core received for WELD-6 (GET /api/cells/<station>/heartbeat),
#             summed per style: the style the Edge attributed each tick to, as shipped. The
#             Edge's own per-style split has no HTTP read (its /api/hourly-counts sums styles),
#             so the Edge's T is that read: what it counted, against what Core received.
BANK_LOADS=0
BANK_COUNTED=0
days_around() { local o; for o in -1 0 1; do date -u -d "$1 $o day" +%Y-%m-%d; done; }
sim_date() { curl -s -m 5 "$EDGE/api/sim/status" | sed -n 's/.*"sim_now":"\([0-9-]*\)T.*/\1/p'; }
edge_total() {
  local d s=0 n
  for d in "$@"; do
    n="$(edge_get "/api/hourly-counts?process_id=$PID&date=$d" | grep -oE ':-?[0-9]+' | tr -d : | awk '{s += $1} END {print s + 0}')"
    s=$(( s + ${n:-0} ))
  done
  echo "$s"
}
cart_draws() {
  sql "SELECT COALESCE(sum(COALESCE(l.before_uop, l.after_uop) - l.after_uop), 0) FROM bin_uop_ledger l
       JOIN nodes n ON n.id = l.node_id WHERE l.op = 'bin_uop_delta' AND l.id > $1 AND n.name IN ($POS_SQL)"
}
pile_stock() { sql "SELECT COALESCE(sum(qty), 0) FROM lineside_buckets WHERE state = 'active' AND core_node_name IN ($POS_SQL)"; }

mark() {
  local lj; lj="$(line_json)"
  W_LOADS="$(field "$lj" loads)"; W_FLIGHT="$(field "$lj" in_flight)"
  W_LEDGER="$(sql 'SELECT COALESCE(max(id), 0) FROM bin_uop_ledger')"
  W_PILES="$(pile_stock)"
  W_SINCE="$(curl -s -m 5 "$CORE/api/sim/status" | sed -n 's/.*"sim_now":"\([^"]*\)".*/\1/p')"
  W_DATES="$(days_around "$(sim_date)")"
  # shellcheck disable=SC2086
  W_EDGE="$(edge_total $W_DATES)"
  BANK_LOADS=0
  BANK_COUNTED=0
}

# bank: the PLC's line restarts empty with the Edge, so the window's loads so far are kept here.
bank() {
  local lj loads flight; lj="$(line_json)"
  loads="$(field "$lj" loads)"; flight="$(field "$lj" in_flight)"
  BANK_LOADS=$(( BANK_LOADS + ${loads:-0} - ${W_LOADS:-0} ))
  BANK_COUNTED=$(( BANK_COUNTED + ${loads:-0} - ${W_LOADS:-0} - (${flight:-0} - ${W_FLIGHT:-0}) ))
  W_LOADS=0
  W_FLIGHT=0
}

ledger() {
  local lj loads flight dl counted nodes p ns=0 nc=0 expect drawn piles carts n q core a b other dates t
  lj="$(line_json)"
  loads="$(field "$lj" loads)"; flight="$(field "$lj" in_flight)"
  dl=$(( BANK_LOADS + ${loads:-0} - W_LOADS ))
  counted=$(( BANK_COUNTED + ${loads:-0} - W_LOADS - (${flight:-0} - W_FLIGHT) ))
  nodes="$(edge_get /api/process-nodes | tr '{' '\n' | grep "\"process_id\":$PID,")"
  for p in $POSITIONS; do
    case "$(printf '%s\n' "$nodes" | grep "\"core_node_name\":\"$p\"")" in
      *'"line_counter_id":'*) ns=$(( ns + 1 )) ;;
      *) nc=$(( nc + 1 )) ;;
    esac
  done
  expect=$(( ns * dl + nc * counted ))
  drawn="$(cart_draws "$W_LEDGER")"; piles="$(pile_stock)"
  carts=$(( ${drawn:-0} - (${piles:-0} - ${W_PILES:-0}) ))
  n="$(holds_n)"
  q="$(holds_list | awk '{s += $2} END {print s + 0}')"
  core="$(curl -s -m 60 "$CORE/api/cells/$STATION/heartbeat?since=$W_SINCE" | tr '{' '\n' | grep '"delta":' \
    | grep "\"process_id\":$PID,\"style_id\":" | sed -E 's/.*"delta":(-?[0-9]+).*"style_id":([0-9]+).*/\2 \1/' \
    | awk '{s[$1] += $2} END {for (k in s) print k, s[k]}')"
  a="$(printf '%s\n' "$core" | awk -v id="$RUN_ID" '$1 == id {print $2}')"
  b="$(printf '%s\n' "$core" | awk -v id="$ALT_ID" '$1 == id {print $2}')"
  other="$(printf '%s\n' "$core" | awk -v r="$RUN_ID" -v x="$ALT_ID" '$1 != r && $1 != x {s += $2} END {print s + 0}')"
  # shellcheck disable=SC2086
  dates="$(printf '%s\n' $W_DATES $(days_around "$(sim_date)") | sort -u)"
  # shellcheck disable=SC2086
  t=$(( $(edge_total $dates) - W_EDGE ))
  say "draws at the five positions: carts $carts (Core's deltas $drawn, piles +$(( ${piles:-0} - ${W_PILES:-0} ))); expected $expect = $ns station-counted x $dl loads + $nc cell-counted x $counted counted out; $n holds open, qty $q"
  [ "$other" -eq 0 ] || say "Core received $other more under neither style"
  echo "carts Σ $carts = loads Σ $expect; holds open $n; record RUN ${a:-0} / ALT ${b:-0}; Core received ${a:-0}+${b:-0} of the Edge's $t" | tee -a "$OUT/log.txt"
}

# ── THE STACK ────────────────────────────────────────────────────────────────────────────
bring_up() {
  if [ -n "$(dc ps -q 2>/dev/null)" ] && [ "${SIM_4X2_REPLACE:-0}" != 1 ]; then
    echo "$PROJECT has containers running, and this run starts with down -v." >&2
    echo "Set SIM_4X2_REPLACE=1 if that stack is yours to drop." >&2
    exit 3
  fi
  dc --profile tools down -v --remove-orphans >"$OUT/down.log" 2>&1
  bash scripts/sim-anchor.sh mint >>"$OUT/log.txt"
  if [ "${SIM_4X2_NO_BUILD:-0}" != 1 ]; then
    say "build core edge seed"
    dc --profile tools build core edge seed >"$OUT/build.log" 2>&1 || die "build failed, see build.log"
  fi
  say "up postgres kafka core edge"
  SHINGO_BIND="${SHINGO_BIND:-127.0.0.1}" dc up -d postgres kafka core edge >"$OUT/up.log" 2>&1 || die "up failed, see up.log"
  # Core stopped across the seed: Edge migrates its database at boot and the seeder writes into
  # a migrated schema. docs/dev-env/sim.md, "Seed with Core STOPPED".
  wait_for 120 "Edge up" "curl -s -m 3 $EDGE/status >/dev/null" || die "Edge did not come up"
  dc stop core >>"$OUT/up.log" 2>&1
  say "seed $PLANT"
  dc run --no-deps --rm seed >"$OUT/seed.log" 2>&1 || die "seed failed, see seed.log"
  dc start core >>"$OUT/up.log" 2>&1
  dc restart edge >>"$OUT/up.log" 2>&1
  wait_for 120 "Core up" "curl -s -m 3 $CORE/login >/dev/null" || die "Core did not come up"
  wait_for 120 "Edge up" "curl -s -m 3 $EDGE/status >/dev/null" || die "Edge did not come up"
}

# restart_edge <catid>: docker compose restart of WELD-6's Edge mid-run. The fake PLC lives in
# it and boots at CATID 0, so the PLC's program is set back once it is up.
restart_edge() {
  bank
  say "restart the Edge. Look at: the changeover, its legs and the holds when it is back"
  dc restart edge >>"$OUT/up.log" 2>&1 || die "edge restart failed, see up.log"
  wait_for 120 "Edge back" "curl -s -m 3 $EDGE/status >/dev/null" || die "Edge did not come back"
  edge_login
  wait_for 60 "WELD-6's line on the fake PLC" "[ -n \"\$(line_json)\" ]" || die "no WELD-6 line after the restart"
  set_catid "$1"
}

declare -A NODE_ID
resolve() {
  local styles nodes p
  PID="$(edge_get /api/processes | grep -oE "\"id\":[0-9]+,\"name\":\"$PROC\"" | grep -oE '[0-9]+' | head -1)"
  [ -n "$PID" ] || die "no process $PROC on the Edge at $EDGE"
  styles="$(edge_get "/api/processes/$PID/styles")"
  RUN_ID="$(printf '%s' "$styles" | grep -oE "\"id\":[0-9]+,\"name\":\"$STYLE_RUN\"" | grep -oE '[0-9]+' | head -1)"
  ALT_ID="$(printf '%s' "$styles" | grep -oE "\"id\":[0-9]+,\"name\":\"$STYLE_ALT\"" | grep -oE '[0-9]+' | head -1)"
  [ -n "$RUN_ID" ] && [ -n "$ALT_ID" ] || die "$PROC has no styles $STYLE_RUN / $STYLE_ALT"
  nodes="$(edge_get /api/process-nodes | tr '{' '\n' | grep "\"process_id\":$PID,")"
  NODE_RE=""
  for p in $POSITIONS; do
    NODE_ID[$p]="$(printf '%s\n' "$nodes" | grep "\"core_node_name\":\"$p\"" | grep -oE '"id":[0-9]+' | head -1 | sed 's/.*://')"
    [ -n "${NODE_ID[$p]}" ] || die "$PROC has no position $p"
    NODE_RE="${NODE_RE:+$NODE_RE|}${NODE_ID[$p]}"
  done
  COUNTER_ID="$(edge_get "/api/processes/$PID/station-counters" | grep -oE '"id":[0-9]+' | head -1 | sed 's/.*://')"
  STATION="$(curl -s -m 5 "$EDGE/status" | sed -n 's/.*"station_id":"\([^"]*\)".*/\1/p')"
  say "$PROC is Edge process $PID (RUN $RUN_ID, ALT $ALT_ID, station counter ${COUNTER_ID:-none}), station $STATION"
  # Both styles carry their pins from the seed (expected_catid); pinned here if a seed did not.
  pinned "$styles" "$STYLE_RUN" "$CATID_RUN" || pin "$RUN_ID" "$CATID_RUN" || die "could not pin $CATID_RUN on RUN"
  pinned "$styles" "$STYLE_ALT" "$CATID_ALT" || pin "$ALT_ID" "$CATID_ALT" || die "could not pin $CATID_ALT on ALT"
  [ "$(active)" = "$RUN_ID" ] || die "$PROC runs $(style_name "$(active)"), not RUN"
}
# pinned <styles json> <style name> <catid>: a count, not grep -q, which under pipefail can
# SIGPIPE its writer and answer no to a match.
pinned() {
  [ "$(printf '%s' "$1" | tr '{' '\n' | grep "\"name\":\"$2\"" | grep -c "\"expected_catid\":\"[^\"]*$3")" -gt 0 ]
}

run_ticks() {
  local i
  for (( i = 0; i < $1; i += 10 )); do sim_s $(( 10 * TICK_S )); status; done
}

# The waits more than one shape takes, each budgeted in sim terms.
await_landing() { wait_for "$LAND_WALL" "ALT's carts landed, the gate green" landed || die "the carts did not land"; }
await_plc() { wait_for $(( $(wall $(( DRAIN_S + 4 * TICK_S ))) + 30 )) "the PLC reports $1" plc_on "$1" || die "the PLC never reported $1"; }
await_holds() { wait_for $(( $(wall $(( 6 * TICK_S ))) + 30 )) "$2" holds_at_least "$1"; }
await_no_holds() { wait_for "$LAND_WALL" "no hold open" no_holds; }
# await_on_alt <sim s before it can act> <what>: the changeover (or the switch) lands on ALT.
await_on_alt() { wait_for $(( $(wall $(( $1 + 6 * TICK_S ))) + 30 )) "$2" closed_on "$ALT_ID"; }

# start_held: START to ALT with Core refusing creates, so its legs stay unlanded and its gate red.
# A reorder in flight at a position refuses START, and holding it would hold START with it, so
# the positions go quiet first.
start_held() {
  wait_for "$LAND_WALL" "no order live at the positions" no_orders_at_positions || die "the positions never went quiet"
  legs_held on
  start_changeover "$ALT_ID"
  wait_for 60 "ALT's legs held back, the gate red" held_red || die "the gate did not read red"
}

# ── THE SHAPES ───────────────────────────────────────────────────────────────────────────
shape_S1() {
  say "S1 steady RUN, the five positions on the station counter, $RUN_TICKS ticks. Look at: each cart drops one a load, no hold, every count under RUN"
  run_ticks "$RUN_TICKS"
  ledger
  say "S1 the five positions move to the cell counter (unbound from the station counter)"
  local p; for p in $POSITIONS; do counted_at "$p" null; done
  mark
  say "Look at: the draws follow the production counter now; a cart landed from here reads $LINE_DEPTH low until the line refills"
  run_ticks "$RUN_TICKS"
}

shape_S2() {
  local t0
  if [ "$VARIANT" = red ]; then
    start_held
  else
    start_changeover "$ALT_ID"
    say "Look at: the legs to the mirror positions; the sim operator releases them, nobody cuts over"
    await_landing
  fi
  status
  program_change "$CATID_ALT" drain
  say "Look at: loading stops, the line drains $LINE_DEPTH loads, then the PLC reports $CATID_ALT"
  await_plc "$CATID_ALT"
  t0="$(sim_epoch)"
  if [ "$VARIANT" = red ]; then
    say "Look at: the gate stays red; the line refills and the first counted ALT part forces the cutover"
    await_on_alt $(( DECLARE_S + DRAIN_S )) "the changeover forced onto ALT"
  else
    await_on_alt "$DECLARE_S" "the declaration completes the changeover onto ALT"
  fi
  say "completed $(( $(sim_epoch) - t0 )) sim-s after the PLC reported $CATID_ALT (look for ~$DECLARE_S)"
  status
  if [ "$VARIANT" = red ]; then
    legs_held off
    say "Look at: the held legs land as ordinary deliveries; the mirror positions' held draws land on ALT's carts"
    await_no_holds
    status
  fi
}

shape_S3() {
  start_changeover "$ALT_ID"
  await_landing
  say "S3 Mark as running ALT at the landing, the PLC still on $CATID_RUN"
  mark_running "$ALT_ID" || die "the admin switch was refused"
  status
  watch_stays "$ALT_ID" "$(wall $(( DECLARE_S + 6 * TICK_S )))" "the PLC still on $CATID_RUN"
  program_change "$CATID_ALT" drain
  await_plc "$CATID_ALT"
  watch_stays "$ALT_ID" "$(wall $(( DECLARE_S + 6 * TICK_S )))" "the PLC on $CATID_ALT"
}

# watch_stays <style id> <wall seconds> <while>: reads the style and the holds every 2 s.
watch_stays() {
  local end=$(( $(date +%s) + $2 )) s most=0 h
  say "Look at, $2 s ($3): the style stays $(style_name "$1"), no hold opens"
  while [ "$(date +%s)" -lt "$end" ]; do
    s="$(active)"; h="$(holds_n)"
    [ "$s" = "$1" ] || { say "SWITCHED to $(style_name "$s") ($3)"; status; return 1; }
    [ "$h" -gt "$most" ] && { most=$h; say "a hold opened ($3): $(hold_ids)"; }
    sleep 2
  done
  say "held $(style_name "$1") for $2 s ($3), $most holds at most"
}

shape_S4() {
  say "S4 no changeover, RUN active, both styles pinned: the FORCED program change to $CATID_ALT"
  program_change "$CATID_ALT" forced
  say "Look at: the loads in flight count out as $CATID_ALT; the mirror positions' draws are held (their carts are MIRROR-L)"
  await_holds 1 "holds open"
  status
  await_on_alt "$DECLARE_S" "the automatic switch to ALT"
  status
  say "Look at: reorders at the mirror positions fetching MIRROR-R; the holds land on those carts"
  await_no_holds
  status
}

shape_S5() {
  local first second preview code
  say "S5 the forced program change to $CATID_NONE, which no style pins"
  program_change "$CATID_NONE" forced
  await_holds 2 "two holds open on $CATID_NONE" || die "fewer than two holds opened"
  status
  say "holds open (id qty): $(holds_list | tr '\n' ';')"
  first="$(holds_list | sed -n 1p | cut -d' ' -f1)"; second="$(holds_list | sed -n 2p | cut -d' ' -f1)"
  preview="$(hold_preview "$first")"
  say "Preview Charge of hold $first: $(printf '%s' "$preview" | cut -c1-400)"
  case "$preview" in
    *'"refusal":"'*)
      say "the preview refuses; the health strip's one-click pins $CATID_NONE on RUN, and the preview again"
      pin "$RUN_ID" "$CATID_NONE"
      say "Preview Charge of hold $first: $(hold_preview "$first" | cut -c1-400)"
      ;;
  esac
  code="$(hold_charge "$first")"; say "Charge hold $first: $code $(last)"
  code="$(hold_write_off "$second")"; say "Write off hold $second: $code $(last)"
  say "Look at: the charged hold's quantity off RUN's carts; the written-off one charges nothing"
  status
}

shape_S6() {
  local p
  for p in $MIRRORS; do counted_at "$p" null; done
  mark
  say "S6 positions 1-3 on the station counter, 4-5 on the cell counter: changeover RUN->ALT"
  start_changeover "$ALT_ID"
  await_landing
  program_change "$CATID_ALT" drain
  await_plc "$CATID_ALT"
  await_on_alt "$DECLARE_S" "the declaration completes the changeover onto ALT"
  say "Look at: positions 1-3 draw at the load station, 4-5 as parts count out; the line refills"
  run_ticks $(( LINE_DEPTH + 4 ))
}

shape_S7() {
  local before after id lost=""
  start_held
  restart_edge "$CATID_RUN"
  status
  program_change "$CATID_ALT" forced
  say "Look at: ALT parts count; the mirror positions still hold MIRROR-L, so their draws are held"
  await_holds 1 "holds at the mirror positions" || die "no hold opened"
  before="$(hold_ids)"
  restart_edge "$CATID_ALT"
  after="$(hold_ids)"
  for id in $before; do case " $after " in *" $id "*) ;; *) lost="$lost $id" ;; esac; done
  if [ -z "$lost" ]; then say "the holds survived the restart: [$before] now [$after]"; else say "LOOK: holds$lost gone after the restart: [$before] now [$after]"; fi
  legs_held off
  await_on_alt "$DECLARE_S" "the changeover onto ALT completed"
  say "Look at: the legs land; each hold lands once on ALT's carts (the ledger's carts against its loads)"
  await_no_holds
  run_ticks 4
}

shape_S8() {
  local t0 info parts code
  say "S8 the PLC moves its count from Prod_Counter_01 to Prod_Counter_02 of MES_WELD_6: _02 ticks from here and CATID_02 reads $CATID_RUN; Prod_Counter_01 and CATID_01 stand"
  counting_slot 2
  t0=$(date +%s)
  say "Look at: WELD-6's counter stops. The Edge's slot watch compares two reads a wall minute apart, so the pass that straddles the move can say Prod_Counter_02 rises WITH _01 (info, one pass); the next is red: the PLC counts on Prod_Counter_02"
  wait_for $(( 2 * SLOT_WATCH_WALL + 30 )) "the strip's red counter_slot_idle item" '[ -n "$(slot_item counter_slot_idle)" ]' \
    || die "no counter_slot_idle item in two slot-watch passes"
  info="$(slot_item counter_slot_idle | grep -oE '"headline":"[^"]*"')"
  say "the item, $(( $(date +%s) - t0 )) wall-s after the move: $info"
  parts="$(slot_counter 2)"
  code="$(edge_send POST "/api/processes/$PID/counter" "{\"tag_name\":\"$COUNTER_TAG2\"}")"
  say "the click, count $COUNTER_TAG2: $code $(last)"
  http_ok "$code" || die "the re-point was refused"
  [ "$(counter_tag)" = "$COUNTER_TAG2" ] || die "$PROC counts $(counter_tag) after the click, not $COUNTER_TAG2"
  say "the record's gap: about ${parts:-?} parts counted on Prod_Counter_02 before the click, $(( $(date +%s) - t0 )) wall-s, read by no point; the carts, at the load station, lack none"
  say "Look at: the item gone; the record and Core's heartbeat resume under RUN at the next WarLink scan; the first part on _02 carries CATID_02's $CATID_RUN (no HTTP read: S8 in integration/scenarios asserts it)"
  run_ticks 10
  status
}

# ── THE RUN ──────────────────────────────────────────────────────────────────────────────
. scripts/gate-lock.sh
gate_lock_acquire "sim-4x2 $SHAPE $VARIANT" || { echo "no gate lock" >&2; exit 3; }
cleanup() {
  [ "$LEGS_HELD" = 1 ] && legs_held off
  rm -f "$EDGE_COOKIES" "$OUT/.last"
  gate_lock_release
}
trap cleanup EXIT

bring_up
edge_login
resolve
wait_for 120 "WELD-6's line loading" line_live 0 || die "WELD-6's line never loaded"
set_catid "$CATID_RUN"
say "settle: the monitor confirms $CATID_RUN under RUN"
sim_s $(( DECLARE_S + 2 * TICK_S ))
START_S=$(date +%s)
mark
say "run $SHAPE${VARIANT:+ $VARIANT}: Core $CORE, Edge $EDGE, out $OUT"
status
"shape_$SHAPE"
say "done; the stack stays up. Pages: $EDGE/changeover $EDGE/processes $EDGE/production $CORE/"
ledger
