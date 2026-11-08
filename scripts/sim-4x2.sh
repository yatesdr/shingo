#!/usr/bin/env bash
# bash scripts/sim-4x2.sh <S1..S9> [red|tap] — one shape of the 4x2-shaped cell on the visible dev
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
#   S2 [red]  changeover RUN->ALT on the running line; the carts land; the drain program change to
#             1002 before anyone cuts over: the declaration completes it on a green gate within
#             ~60 sim-s. `red` starts it with the legs held back (as S7 does), so the gate is red
#             and the first counted ALT part forces it
#   S3        changeover RUN->ALT on the running line; "Mark as running" ALT at the landing while
#             the PLC still says 1001; then the drain program change: no hold, and the common
#             positions supplied throughout (the request guards refuse by position)
#   S4 [tap]  no changeover, both styles pinned, from landed carts at the mirror positions: the
#             FORCED program change to 1002 under RUN; holds at the mirror positions; past the
#             ~60 sim-s window nothing has moved (RUN active, no changeover, no order at the
#             mirror positions) and the strip's red wrong-part item offers the changeover to ALT.
#             Untapped, the mirror holds grow and the record is ALT's by CATID. `tap` presses the
#             item's Start changeover: the legs land and the monitor completes it; the held shares
#             stay in the ledger for a person (decision 55: nothing lands a row by itself)
#   S5        the forced program change to 1003, which no style pins: every stroke's draw held;
#             Charge one position's hold (preview first), Write off another's
#   S6        positions 1-3 on the station counter, 4-5 on the cell counter; RUN->ALT on the
#             running line
#   S7        the forced program change to 1003 holds its draws, then back to 1001; Core holds
#             every create from a gap between the mirror positions' swaps, the line pauses its
#             loading and drains, and START goes in with its legs held; the Edge restarts
#             mid-changeover and loading resumes; the forced program change to 1002; the holds
#             survive both restarts; the legs land and nothing is charged twice
#   S8        the PLC moves its count to the sibling slot Prod_Counter_02 (CATID_02 reads 1001;
#             Prod_Counter_01 stops): the strip's red "counts on Prod_Counter_02" item within the
#             Edge's slot watch (a pass every 60 sim-s; two when the move falls mid-pass), the
#             click re-points the counter from _02's count before it rose: the record equals the
#             parts the line counted out (no gap), the carts lack nothing
#   S9 [tap]  the hand-placed cart: the mirror positions' carts are MIRROR-L in Core and their
#             seats record RUN's part assumed, not known (seeded with no part, as the bind records
#             a cart no source named, decision 57; the line stands paused from
#             boot until the shape, so those carts are still there); RUN draws; the FORCED
#             program change to 1002; nothing moves and the item offers the changeover, as in S4.
#             Untapped, ALT strokes and the holds grow; `tap` starts the changeover (the person
#             cancels RUN's swap staged at a mirror position first, as START's refusal says), the
#             person sends the hand-placed carts back (RELEASE: nobody named them, so the sim
#             operator waits), and it completes; the holds and the strokes logged with no cart of
#             their part stay in the ledger for a person (decision 55). It prints what the seats
#             record (part, known, source), the mirror holds, Core's mirror carts and the deltas
#             Core refused as the wrong part (none: the strokes between the tap and the release
#             are logged, the cart there being the departing part)
#
# It brings the stack up from nothing as scripts/kanban-rig.sh does (down -v, a fresh sim anchor,
# build, seed with Core stopped), sets the PLC's CATID to 1001, drives the shape over Edge and
# Core HTTP at the stack's 10x, prints a line per step naming what to look at, leaves the stack
# and its pages up, and ends on the ledger (what each number is: see ledger):
#
#   carts Σ C + logged G + written off W = loads Σ E; rows open N; record RUN a / ALT b; Core received a+b of the Edge's T
#   lost at the lift L
#   minimum cart M (node bin)
#   strip: <each no-cart row as the strip renders it>
#   overpack: <each release Core recorded below 0, and its timeline line>
#
# The loss is its own line, never a term of the sum (a sum that included it would be exact by
# construction): Core's stale drops of the cell's parts on a cart released in the window, and the
# draws a release's count undid (a released row that raised a cart's count). The final ledger
# asserts it 0, and every partial release fenced on both sides (the Edge's and Core's release
# lines).
#
# A window opens at rest and the final ledger is read at rest: loading paused, the line drained,
# then Core and the Edge's hourly store caught up (a window's opening then lets the line refill).
# The final ledger's three numbers are exact (carts + logged + written off = loads; the record = the
# parts the line counted out; Core received = the Edge's), and it says EXACT or NOT EXACT; a run
# whose final ledger is not exact exits 5. A mid-run ledger (S1's first window) is read on the
# running line and says so: it may carry one part in flight.
#
# A wait's budget is a floor, not a verdict: one that runs out while the cell still moved (the
# line's loads or counter, Core's ledger at the five positions, the orders there) waits its
# budget again, up to four times, and says so, so a slow host (on battery, throttled) is slow
# rather than stopped. The sim clock cannot do this: it is the wall clock times the speed. A wait
# that runs out on a cell that stood still stops the run, and the STOP writes the forensics a
# ledger that is not exact writes (the Edge's and Core's logs for the window, the line with the
# readiness gate's answer, the gate) before it exits.
#
# A run owns the stack: it refuses while shingo-dev has containers running, because its first
# act is `down -v`. SIM_4X2_REPLACE=1 says the running stack is yours to drop. It takes the gate
# lock (scripts/gate-lock.sh), so a gate and a run never overlap. INT, TERM or HUP stops the run:
# it kills what it started (its docker compose included), releases the lock and leaves the stack
# up.
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
MIRROR_RUN=MIRROR-L   # the mirror positions' part under RUN
MIRROR_ALT=MIRROR-R   # and under ALT
POS_SQL="'ALN_061','ALN_062','ALN_063','ALN_064','ALN_065'"
PARTS_SQL="'COMMON-1','COMMON-2','COMMON-3','MIRROR-L','MIRROR-R'"   # the cell's parts, its alone
MIRROR_SQL="'ALN_064','ALN_065'"

# ── TIMING: sim seconds unless named wall ───────────────────────────────────────────────
SIM_SPEED=10                        # shingo-*/*.dev.yaml sim.speed, the stack's default
TICK_S=12                           # WELD-6's tick_interval
LINE_DEPTH=16                       # WELD-6's line_depth
DRAIN_S=$(( LINE_DEPTH * TICK_S ))  # a drain change lands once the line has counted out
DECLARE_S=60                        # the CATID monitor's stability window before it acts
RUN_TICKS="${SIM_4X2_TICKS:-40}"    # one S1 window
LAND_WALL=300                       # wall budget for a changeover's carts to land
SLOT_WATCH_S=60                     # the Edge's slot watch: a pass every 60 sim-s
S9_RUN_TICKS=2                      # S9's RUN draws: its mirror carts stay above the reorder point

CORE=http://127.0.0.1:8083
EDGE=http://127.0.0.1:8081
PLANT=/plants/master.yaml
PROJECT=shingo-dev

usage() {
  echo "usage: bash scripts/sim-4x2.sh <S1..S9> [red|tap]   (red: S2 only; tap: S4 and S9)" >&2
  echo "       make dev-sim-4x2 SHAPE=<S1..S9> [VARIANT=red|tap]" >&2
  echo "The run starts with \`down -v\` of $PROJECT; set SIM_4X2_REPLACE=1 if the stack running now is yours to drop." >&2
  exit 2
}
SHAPE="${1:-}"
VARIANT="${2:-}"
case "$SHAPE" in S1|S2|S3|S4|S5|S6|S7|S8|S9) ;; *) usage ;; esac
case "$VARIANT" in
  "") ;;
  red) [ "$SHAPE" = S2 ] || usage ;;
  tap) case "$SHAPE" in S4|S9) ;; *) usage ;; esac ;;
  *) usage ;;
esac

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
die() {
  say "STOP: $*"
  [ -n "${WAIT_TIMED_OUT:-}" ] && forensics
  exit 3
}
wall() { echo $(( ($1 + SIM_SPEED - 1) / SIM_SPEED )); }
sim_s() { nap "$(wall "$1")"; }

# ── ITS CHILDREN ─────────────────────────────────────────────────────────────────────────
# bash runs a trap only between commands, so a foreground `sleep 120` or `docker compose build`
# holds a TERM until it ends. A long command runs as a job the shell waits on (bg): `wait`
# returns at the signal, and the trap (THE RUN, below) kills the job.
CHILD=""
bg() { "$@" & CHILD=$!; wait "$CHILD"; local rc=$?; CHILD=""; return "$rc"; }
nap() { bg sleep "$1"; }
# descendants <pid>: its children and theirs, off `ps -ef` (pid and ppid are its second and
# third columns in Git Bash and on Linux alike).
descendants() {
  ps -ef | awk -v root="$1" 'NR > 1 { kids[$3] = kids[$3] " " $2 }
    END { n = 1; q[1] = root; while (n) { p = q[n--]; m = split(kids[p], c, " "); for (i = 1; i <= m; i++) { print c[i]; q[++n] = c[i] } } }'
}
# kill_tree <pid>: a job and everything under it, listed before any is killed. In Git Bash
# `docker` is Docker Desktop's sh shim, which starts docker.exe, which starts docker-compose.exe;
# a native child outlives a kill of its MSYS parent, so on Windows each process's own Windows
# tree also goes with taskkill.
kill_tree() {
  local p wp
  # shellcheck disable=SC2046 # pids, one word each
  for p in "$1" $(descendants "$1"); do
    wp="$(cat "/proc/$p/winpid" 2>/dev/null)"
    [ -n "$wp" ] && command -v taskkill >/dev/null 2>&1 && taskkill /F /T /PID "$wp" >/dev/null 2>&1
    kill -TERM "$p" 2>/dev/null
  done
}
# shellcheck disable=SC2046 # pids, one word each
stop_children() { local p; for p in $CHILD $(jobs -p); do kill_tree "$p"; done; }

# wait_for <wall seconds> <what> <predicate...>: polls every second; false, saying what it
# waited for, on timeout. At the deadline a cell that moved since the wait began (or last
# stretched) gets the budget again, up to WAIT_STRETCHES times: the host is slow, not stuck.
# A timeout marks the run (WAIT_TIMED_OUT) so a die after it writes the forensics.
WAIT_STRETCHES=4
wait_for() {
  local budget=$1 what=$2 deadline=$(( $(date +%s) + $1 )) began=$(date +%s) stretched=0 seen now
  shift 2
  seen="$(cell_moves)"
  while :; do
    while [ "$(date +%s)" -lt "$deadline" ]; do
      if eval "$*"; then say "ok: $what"; return 0; fi
      nap 1
    done
    now="$(cell_moves)"
    [ "$stretched" -lt "$WAIT_STRETCHES" ] && [ "$now" != "$seen" ] || break
    stretched=$(( stretched + 1 )); seen="$now"; deadline=$(( $(date +%s) + budget ))
    say "LOOK: ${budget}s passed waiting for: $what; the cell is still moving, waiting ${budget}s more ($stretched of $WAIT_STRETCHES)"
  done
  local why="the cell stood still for the last ${budget}s"
  [ "$now" != "$seen" ] && why="the cell still moving at the last of them"
  say "TIMED OUT after $(( $(date +%s) - began ))s waiting for: $what ($stretched stretch(es); $why)"
  WAIT_TIMED_OUT=1
  return 1
}

# cell_moves: one word that changes while the cell does work: the fake line's loads and
# counter, Core's last ledger row at the five positions, and the orders live there with their
# states. The rest of the sim plant is not in it.
cell_moves() {
  printf '%s|%s|%s' "$(line_field loads):$(line_field counter):$(line_field in_flight)" \
    "$(sql "SELECT COALESCE(max(l.id), 0) FROM bin_uop_ledger l JOIN nodes n ON n.id = l.node_id WHERE n.name IN ($POS_SQL)" 2>/dev/null)" \
    "$(sql "SELECT COALESCE(string_agg(id || status, ',' ORDER BY id), '') FROM orders
      WHERE delivery_node IN ($POS_SQL) AND status NOT IN ('confirmed', 'cancelled', 'failed')" 2>/dev/null)"
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
# flight, the identity, the counter, its MES struct's slots, and whether its loading is paused.
# It lives in the Edge process and starts empty, and loading, at its boot.
line_json() {
  curl -s -m 10 "$EDGE/api/sim/line" | grep -oE "\"plc_name\":\"$PLC\",\"tag_name\":\"${COUNTER_TAG//./[.]}\",\"loads\":[0-9]+,\"loads_by_identity\":(null|\{[^}]*\}),\"in_flight\":[0-9]+,\"identity\":-?[0-9]+,\"counter\":-?[0-9]+,\"slot\":[0-9]+,\"slots\":\[[^]]*\],\"paused\":(true|false)"
}
line_field() { field "$(line_json)" "$1"; }
# line_raw: WELD-6's line as GET /api/sim/line has it, the readiness gate's answer ("ready") with it.
line_raw() { curl -s -m 10 "$EDGE/api/sim/line" | sed 's/},{"plc_name"/}\n{"plc_name"/g' | grep -a "\"plc_name\":\"$PLC\"" | cut -c1-600; }
# line_idle: loading paused and nothing in flight, so no stroke moves the line.
line_idle() { case "$(line_json)" in *'"in_flight":0,'*'"paused":true') return 0 ;; *) return 1 ;; esac; }

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

# The ledger's open rows on the process: the catid-health strip's "held" items (a held or off-plan
# share, and a stroke logged with no cart of its part), one "<hold id> <qty> <process node id>" per
# line. Node 0 is the production counter's held stroke, whole: a Charge draws it at every position
# counted there, so it holds one share per cell-counted position.
holds_list() {
  edge_get "/api/processes/$PID/catid-health" | sed 's/{"kind":/\n{"kind":/g' | grep '^{"kind":"held"' \
    | sed -E 's/.*"hold_id":([0-9]+).*"process_node_id":([0-9]+).*"qty":(-?[0-9]+).*/\1 \3 \2/'
}
holds_n() { holds_list | grep -c . ; }
hold_ids() { holds_list | cut -d' ' -f1 | tr '\n' ' ' | sed 's/ $//'; }

# Orders live at the five positions, off the Edge's active orders.
orders_at_positions() {
  edge_get /api/orders/active | grep -oE '"process_node_id":[0-9]+' | grep -cE ":($NODE_RE)\$"
}
# fleet_orders_at <position...>: orders at those positions already with the fleet (dispatched,
# in transit, staged at their wait, faulted): the ones that refuse START, which cancels the rest.
fleet_orders_at() {
  local re="" p
  for p in "$@"; do re="${re:+$re|}${NODE_ID[$p]}"; done
  edge_get /api/orders/active | sed 's/},{/}\n{/g' | grep -E "\"process_node_id\":($re)," | grep -cE '"status":"(dispatched|in_transit|staged|faulted)"'
}

status() {
  local lj co=""; lj="$(line_json)"
  co_open && co=" -> $(style_name "$(target)")"
  say "  now: style $(style_name "$(active)")$co | gate $(gate_word) | holds $(holds_n) | PLC CATID $(field "$lj" identity), in flight $(field "$lj" in_flight) | orders live at the positions $(orders_at_positions)"
}

# ── THE BUTTONS ──────────────────────────────────────────────────────────────────────────
# start_changeover <style id> [called by]: the station's START, pressed again while the screen
# refuses it ("a carrier is on its way" clears on its own).
start_changeover() {
  local code deadline=$(( $(date +%s) + 180 ))
  while :; do
    code="$(edge_send POST "/api/processes/$PID/changeover/start" "{\"to_style_id\":$1,\"called_by\":\"${2:-sim-4x2}\"}")"
    say "START $(style_name "$1")${2:+ ($2)}: $code $(last)"
    http_ok "$code" && return 0
    [ "$(date +%s)" -lt "$deadline" ] || die "START refused for 180 s"
    nap 5
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

# pause_loading <true|false>, written on the process's COUNTER tag: true stops the loads, the
# line drains and its counter ticks until it is empty; false loads again. cleanup resumes a line
# the run left paused.
PAUSED=0
pause_loading() {
  local code; code="$(edge_send POST /api/sim/pause-loading "{\"plc_name\":\"$PLC\",\"tag_name\":\"$COUNTER_TAG\",\"paused\":$1}")"
  say "PLC loading $([ "$1" = true ] && echo paused || echo resumed): $code $(last)"
  http_ok "$code" || return 1
  PAUSED=0; [ "$1" = true ] && PAUSED=1
  return 0
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

# The hold verbs, one function each. The Preview is POST .../charge?mode=preview.
hold_preview() { edge_send POST "/api/catid-holds/$1/charge?mode=preview" '{}' >/dev/null; cat "$OUT/.last"; }
hold_charge() { edge_send POST "/api/catid-holds/$1/charge" '{}'; }
hold_write_off() { edge_send POST "/api/catid-holds/$1/write-off" '{}'; }

# legs_held <on|off>: Core's sim fleet refuses every create while on, and Core parks each refused
# order and retries it, so a changeover's legs stay unlanded and its gate red. It refuses every
# order in the plant, not only the cell's; cleanup turns it off.
LEGS_HELD=0
legs_held() {
  local on=0; [ "$1" = on ] && on=1
  curl -s -m 10 -X POST "$CORE/api/sim/create-fails?on=$on" >/dev/null
  LEGS_HELD=$on
  say "Core's sim fleet refuses creates: $1"
}

# ── PREDICATES for wait_for ──────────────────────────────────────────────────────────────
landed() { co_open && gate_green; }
held_red() { co_open && ! gate_green; }
plc_on() { [ "$(line_field identity)" = "$1" ]; }
closed_on() { ! co_open && [ "$(active)" = "$1" ]; }
holds_at_least() { [ "$(holds_n)" -ge "$1" ]; }
# shellcheck disable=SC2086 # MIRRORS is a list of positions
mirrors_quiet() { [ "$(fleet_orders_at $MIRRORS)" -eq 0 ]; }
line_live() { [ "${1:-0}" -lt "$(line_field loads)" ] 2>/dev/null; }
sim_epoch() { date -u -d "$(curl -s -m 5 "$EDGE/api/sim/status" | sed -n 's/.*"sim_now":"\([^"]*\)".*/\1/p')" +%s; }

# ── THE LEDGER ───────────────────────────────────────────────────────────────────────────
# mark opens a window and ledger closes it:
#   carts Σ   the units the five positions drew in the window: Core's applied count deltas
#             there (bin_uop_ledger, op bin_uop_delta, node-stamped), and those on the cell's
#             parts Core booked at _TRANSIT, which must read 0 (a draw that reaches Core after its
#             cart's lift is booked at the node it was lifted from, v175), less the rise in their
#             lineside piles. No HTTP read sums a position's draws, so this one number is read
#             from Core's database, as kanban-rig.sh reads Core.
#   loads Σ   what the draws should be: a station-counted position draws once per load and a
#             cell-counted one once per load counted out (loads less the rise in flight), off
#             the PLC's line. Held draws are in it and not in the carts; the holds say how many.
#   record    the production ticks Core received for WELD-6 (cell_part_events, by the Edge's
#             process and style ids), summed per style: the style the Edge attributed each tick
#             to, as shipped. Core's cell heartbeat lists only a configured cell's primary
#             process, so it is read from the table. The Edge's own per-style split has no HTTP
#             read (its /api/hourly-counts sums styles), so the Edge's T is that read: what it
#             counted, against what Core received.
# A window's edges are read at rest (mark, and the final ledger: at_rest, settle_rest): loading
# paused, the line drained, then read once the line, what Core has applied and the Edge's hourly
# store have stood still at the run's speed; a mark then lets the line refill, so a shape starts
# from a full line. A mid-run ledger (S1's first window) and a bank before an Edge restart are read
# at 1x in a gap between strokes (settle, quiet_read), and a mid-run ledger says so: at 1x a
# stroke's draws reach Core up to ~16 s after it (the Edge's 5 sim-s delta flush, then its
# outbox), past the line's next stroke 12 s on, so no gap is sure to hold all of one stroke and
# none of the next.
#
# THE ONE-PART DIFFERENCES this rig used to print (Core's record one over the parts the line
# counted out; the Edge's hourly read one over Core's record) were its own reads straddling a
# stroke. It read the line, then Core, then the Edge's hourly store, over about 6 wall seconds
# that began up to 10 s after a stroke at 1x, whose next stroke comes 12 s after it. The Edge
# writes a stroke's hourly count when its poll reads the counter, before the stroke reaches Core,
# so an hourly read taken after that next stroke and a Core read taken before it differ by one.
# Neither store loses or doubles a part; the hourly bucket's clock is not in it (the rig sums the
# sim day and the days either side, which hold the wall's hour too).
BANK_LOADS=0
BANK_COUNTED=0
WRITTEN_OFF=0
LEDGER_EXACT=1
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
PILES_SQL="SELECT COALESCE(sum(qty), 0) FROM lineside_buckets WHERE state = 'active' AND core_node_name IN ($POS_SQL)"

# sim_speed <n>: both clocks, so Core's and the Edge's sim times stay together.
sim_speed() {
  curl -s -m 5 -X POST "$EDGE/api/sim/speed?speed=$1" >/dev/null
  curl -s -m 5 -X POST "$CORE/api/sim/speed?speed=$1" >/dev/null
}
# settle: both clocks to 1x (one line tick per TICK_S wall seconds), and the edge is read in a
# gap between strokes once Core and the Edge have caught up as far as can be seen: after the
# line's next stroke, once SETTLE_MIN_MS have passed, the line, what Core has applied (core_sig)
# and the Edge's hourly store (edge_total over the sim day and the days either side) have stood
# still for SETTLE_QUIET_MS, and the Edge's /status has nothing waiting to go (outbox_depth 0,
# production_ticks_pending 0; draws still in the Edge's flush window are not in it). A gap ends
# SETTLE_BY wall seconds after its stroke; one that does not settle passes to the next, up to
# SETTLE_GAPS, and the log says so (and settle fails). On an idle line no stroke comes: the gap
# is SETTLE_REST_S long. SETTLED_LINE is the line it settled on. It returns with the clocks at
# 1x; resume puts the run's speed back once the edge is read.
SETTLE_QUIET_MS=2000
SETTLE_MIN_MS=7000
SETTLE_BY=$(( TICK_S - 2 ))
SETTLE_REST_S=60
SETTLE_GAPS=5
ms() { date +%s%3N; }
# core_sig: what a stroke moves in Core, in one read: the five positions' applied draws (their
# bin_uop_delta rows), the process's part records (cell_part_events) and the positions' piles.
core_sig() {
  sql "SELECT (SELECT count(*) FROM bin_uop_ledger l JOIN nodes n ON n.id = l.node_id
                WHERE l.op = 'bin_uop_delta' AND n.name IN ($POS_SQL)),
              (SELECT count(*) FROM cell_part_events WHERE process_id = ${PID:-0}), ($PILES_SQL)"
}
edge_backlog() {
  local s; s="$(curl -s -m 5 "$EDGE/status")"
  echo "outbox $(field "$s" outbox_depth), ticks pending $(field "$s" production_ticks_pending)"
}
# next_stroke: returns at the line's next stroke, or after TICK_S + 8 wall seconds of none (a
# stalled machine); at once when the line is idle, whose state no stroke moves.
next_stroke() {
  local before deadline
  line_idle && return 0
  before="$(line_json)"
  deadline=$(( $(date +%s) + TICK_S + 8 ))
  while [ "$(date +%s)" -lt "$deadline" ] && [ "$(line_json)" = "$before" ]; do nap 0.3; done
}
SETTLED_LINE=""
settle() {
  local gap stroke sig last since backlog el by after ln dates
  sim_speed 1
  SETTLED_LINE=""
  dates="$(days_around "$(sim_date)")"
  for (( gap = 1; gap <= SETTLE_GAPS; gap++ )); do
    next_stroke
    by=$(( SETTLE_BY * 1000 )) after="a stroke"
    if line_idle; then by=$(( SETTLE_REST_S * 1000 )) after="the line stood idle"; fi
    stroke=$(ms) last="" since=0 backlog=""
    while :; do
      ln="$(line_json)"
      # shellcheck disable=SC2086 # dates, one word each
      sig="$ln $(core_sig) $(edge_total $dates)"
      backlog="$(edge_backlog)"
      el=$(( $(ms) - stroke ))
      [ "$el" -le "$by" ] || break
      [ "$sig" = "$last" ] || { last=$sig; since=$(ms); }
      if [ "$el" -ge "$SETTLE_MIN_MS" ] && [ "$backlog" = "outbox 0, ticks pending 0" ] \
          && [ $(( $(ms) - since )) -ge "$SETTLE_QUIET_MS" ]; then
        SETTLED_LINE=$ln
        say "the edge is read at 1x, settled $(( el / 1000 )).$(( el % 1000 / 100 )) s after $after$([ "$gap" -eq 1 ] || echo " (gap $gap)")"
        return 0
      fi
      nap 0.2
    done
    say "not settled $(( by / 1000 )) s after $after ($backlog; Core's draw rows, part rows and piles $(core_sig | tr '|' ' ')): the next gap"
  done
  say "NOT SETTLED in $SETTLE_GAPS gaps: this edge is read as it stands"
  return 1
}
# quiet_read <reader>: the reader's reads in a settled gap; again in the next gap when the line
# moved after the settle (a stroke between the settle and the last read), up to SETTLE_GAPS times.
# A read that never settles is taken as it stands and says so.
quiet_read() {
  local try
  for (( try = 1; try <= SETTLE_GAPS; try++ )); do
    if ! settle; then
      "$1"
      say "LOOK: read unsettled; this window's edge may be off by what Core had not applied"
      return 1
    fi
    "$1"
    [ "$(line_json)" = "$SETTLED_LINE" ] && return 0
    say "a stroke fell between the settle and the read: read again in the next gap"
  done
  say "LOOK: a stroke fell in each of $SETTLE_GAPS reads; this edge is read as it stands"
  return 1
}
resume() { sim_speed "$SIM_SPEED"; }
# record <cell_part_events id>: the production ticks Core received for WELD-6 past that row,
# "<style id> <parts>" a style.
record() { sql "SELECT style_id, sum(delta) FROM cell_part_events WHERE process_id = $PID AND id > ${1:-0} GROUP BY style_id" | tr '|' ' '; }
# refused <bin_uop_ledger id>: the deltas Core refused as the wrong part past that row
# (payload_mismatch_dropped, a row a refused delta), "<deltas> <parts>".
refused() {
  sql "SELECT count(*), COALESCE(-sum((metadata->>'delta')::int), 0) FROM bin_uop_ledger
       WHERE op = 'payload_mismatch_dropped' AND id > ${1:-0}" | tr '|' ' '
}

# strip_logged: the strip's items for the strokes logged with no cart of their part, as rendered
# (their headlines), one a line.
strip_logged() {
  edge_get "/api/processes/$PID/catid-health" | sed 's/{"kind":/\n{"kind":/g' | grep '^{"kind":"held"' \
    | grep '"reason":"no-cart"' | grep -oE '"headline":"[^"]*"' | sed 's/^"headline":"//; s/"$//'
}
# overpacks: each release at the five positions in the window that declared a count below 0, as
# Core recorded it (op released_overpack, decision 55's record half), "<node> bin <id> <before>
# -> 0 declared <N> | <the bins page's timeline line>".
overpacks() {
  sql "SELECT n.name || ' bin ' || l.bin_id || ' ' || COALESCE(l.before_uop::text, '?') || ' -> ' || l.after_uop
         || ' declared ' || COALESCE(l.detail->>'declared', '?') || ' | ' || COALESCE((SELECT a.action || ': ' || a.new_value
           FROM audit_log a WHERE a.entity_type = 'bin' AND a.entity_id = l.bin_id AND a.action LIKE 'released_overpack%'
           ORDER BY a.id DESC LIMIT 1), '(no timeline row)')
       FROM bin_uop_ledger l JOIN nodes n ON n.id = l.node_id
       WHERE l.id > ${W_LEDGER:-0} AND n.name IN ($POS_SQL) AND l.op = 'released_overpack' ORDER BY l.id"
}
# min_cart: the lowest count a cart at the five positions reached in the window, as Core's ledger
# records each draw, "<count> <node> <bin>"; negatives allowed (a stroke charges the cart standing
# there into the negative, decision 55).
min_cart() {
  sql "SELECT l.after_uop, n.name, l.bin_id FROM bin_uop_ledger l JOIN nodes n ON n.id = l.node_id
       WHERE l.id > ${W_LEDGER:-0} AND n.name IN ($POS_SQL) AND l.after_uop IS NOT NULL
       ORDER BY l.after_uop, l.id LIMIT 1" | tr '|' ' '
}
mark_read() {
  local lj
  lj="$(line_json)"
  W_LOADS="$(field "$lj" loads)"; W_FLIGHT="$(field "$lj" in_flight)"
  W_DATES="$(days_around "$(sim_date)")"
  W_SINCE=$(date +%s)
  # shellcheck disable=SC2086
  W_EDGE="$(edge_total $W_DATES)"
  IFS='|' read -r W_LEDGER W_PILES W_PARTS < <(sql "SELECT (SELECT COALESCE(max(id), 0) FROM bin_uop_ledger),
    ($PILES_SQL), (SELECT COALESCE(max(id), 0) FROM cell_part_events)")
}
# mark: the window opens at rest (at_rest, rest_read), then the line loads again and refills, so
# the shape starts from a full line as before. A line the run holds paused (S9's) stays paused.
mark() {
  local was=$PAUSED
  at_rest "the window opens at rest"
  rest_read mark_read
  BANK_LOADS=0
  BANK_COUNTED=0
  WRITTEN_OFF=0
  [ "$was" = 1 ] && return 0
  pause_loading false || die "the resume was refused"
  wait_for $(( $(wall "$DRAIN_S") + LAND_WALL )) "the line refilled" line_full \
    || say "LOOK: the line did not refill: $(line_json)"
}
line_full() { [ "$(line_field in_flight)" -ge "$LINE_DEPTH" ] 2>/dev/null; }

# bank: the PLC's line restarts empty with the Edge, so the window's loads so far are kept here.
# It returns with the clocks at 1x, so no stroke falls between its read and the restart; the
# restart puts the run's speed back.
bank() {
  local lj loads flight; settle; lj="$(line_json)"
  loads="$(field "$lj" loads)"; flight="$(field "$lj" in_flight)"
  BANK_LOADS=$(( BANK_LOADS + ${loads:-0} - ${W_LOADS:-0} ))
  BANK_COUNTED=$(( BANK_COUNTED + ${loads:-0} - ${W_LOADS:-0} - (${flight:-0} - ${W_FLIGHT:-0}) ))
  W_LOADS=0
  W_FLIGHT=0
}

# at_rest <why>: loading paused at the run's speed and the line drained, so no stroke moves
# anything while a window's edge is read. A machine with a position waiting for its cart does not
# cycle, so the drain waits up to LAND_WALL for the carts too. The run's exit resumes the loading
# (cleanup).
at_rest() {
  say "$1: loading paused, the line drains"
  pause_loading true || die "the pause was refused"
  wait_for $(( $(wall "$DRAIN_S") + LAND_WALL )) "the line drained" line_idle \
    || say "LOOK: the line did not drain, so this edge is not read at rest: $(line_json)"
}
# settle_rest: on the idle line, at the run's speed, the line, what Core has applied (core_sig),
# the Edge's hourly store and its backlog stand still for REST_QUIET_S wall seconds with nothing
# waiting to go, within REST_BUDGET_S. At the stack's 10x that is 50 sim-s, against the Edge's
# 5 sim-s delta flush; at 1x a stroke's draws were seen reaching Core 16 s after it, past the next
# stroke. SETTLED_LINE is the line it settled on.
REST_QUIET_S=5
REST_BUDGET_S=180
settle_rest() {
  local deadline since last sig ln dates backlog
  SETTLED_LINE=""
  resume
  dates="$(days_around "$(sim_date)")"
  deadline=$(( $(date +%s) + REST_BUDGET_S )) since=$(date +%s) last=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    ln="$(line_json)"; backlog="$(edge_backlog)"
    # shellcheck disable=SC2086 # dates, one word each
    sig="$ln $(core_sig) $(edge_total $dates) $backlog"
    [ "$sig" = "$last" ] || { last=$sig; since=$(date +%s); }
    if [ "$backlog" = "outbox 0, ticks pending 0" ] && [ $(( $(date +%s) - since )) -ge "$REST_QUIET_S" ]; then
      SETTLED_LINE=$ln
      say "the edge is read at rest: the line, Core and the Edge's hourly store stood still ${REST_QUIET_S} s"
      return 0
    fi
    nap 0.5
  done
  say "NOT SETTLED at rest in ${REST_BUDGET_S} s: this edge is read as it stands"
  return 1
}
# rest_read <reader>: the reader's reads once settled at rest; again if the line moved meanwhile.
rest_read() {
  local try
  for (( try = 1; try <= 3; try++ )); do
    settle_rest || { "$1"; return 1; }
    "$1"
    [ "$(line_json)" = "$SETTLED_LINE" ] && return 0
    say "the line moved while the edge was read at rest: read again"
  done
  return 1
}

ledger_read() {
  local lj loads flight nodes p dates
  lj="$(line_json)"
  loads="$(field "$lj" loads)"; flight="$(field "$lj" in_flight)"
  L_DL=$(( BANK_LOADS + ${loads:-0} - W_LOADS ))
  L_COUNTED=$(( BANK_COUNTED + ${loads:-0} - W_LOADS - (${flight:-0} - W_FLIGHT) ))
  nodes="$(edge_get /api/process-nodes | tr '{' '\n' | grep "\"process_id\":$PID,")"
  L_NS=0 L_NC=0
  for p in $POSITIONS; do
    case "$(printf '%s\n' "$nodes" | grep "\"core_node_name\":\"$p\"")" in
      *'"line_counter_id":'*) L_NS=$(( L_NS + 1 )) ;;
      *) L_NC=$(( L_NC + 1 )) ;;
    esac
  done
  # shellcheck disable=SC2046,SC2086 # dates, one word each
  dates="$(printf '%s\n' $W_DATES $(days_around "$(sim_date)") | sort -u)"
  # shellcheck disable=SC2086
  L_T=$(( $(edge_total $dates) - W_EDGE ))
  IFS='|' read -r L_DRAWN L_PILES < <(sql "SELECT (SELECT COALESCE(sum(COALESCE(l.before_uop, l.after_uop) - l.after_uop), 0)
    FROM bin_uop_ledger l JOIN nodes n ON n.id = l.node_id
    WHERE l.op = 'bin_uop_delta' AND l.id > $W_LEDGER
      AND (n.name IN ($POS_SQL) OR (n.name = '_TRANSIT' AND l.payload_code IN ($PARTS_SQL)))), ($PILES_SQL)")
  L_TRANSIT="$(sql "SELECT COALESCE(sum(COALESCE(l.before_uop, l.after_uop) - l.after_uop), 0)
    FROM bin_uop_ledger l JOIN nodes n ON n.id = l.node_id
    WHERE l.op = 'bin_uop_delta' AND l.id > $W_LEDGER AND n.name = '_TRANSIT' AND l.payload_code IN ($PARTS_SQL)")"
  L_LOST="$(lost_at_the_lift)"
  read -r L_EDGE_FENCED L_EDGE_UNFENCED L_CORE_FENCED L_CORE_UNFENCED < <(release_lines)
  L_REFUSED="$(refused "$W_LEDGER")"
  L_HOLDS="$(holds_list)"
  L_MIN="$(min_cart)"
  L_STRIP="$(strip_logged)"
  L_OVERPACK="$(overpacks)"
  L_CORE="$(record "${W_PARTS:-0}")"
}

# lost_at_the_lift: the draws the cell's carts lost at a release in the window, as Core's ledger
# records them: its stale drops (stale_epoch_dropped, the dropped delta in the row's metadata) of
# the cell's parts on a bin released in the window, and a partial release at a position that
# raised the cart's count above the one Core had (the draws the release's count undid; an empty
# release of a cart drawn below 0 sets it to 0, an overpack, and is not a loss). Never a term of
# the sum.
lost_at_the_lift() {
  sql "SELECT COALESCE((SELECT -sum((l.metadata->>'delta')::int) FROM bin_uop_ledger l
      WHERE l.op = 'stale_epoch_dropped' AND l.id > $W_LEDGER AND l.payload_code IN ($PARTS_SQL)
        AND EXISTS (SELECT 1 FROM bin_uop_ledger r WHERE r.bin_id = l.bin_id AND r.op LIKE 'released%' AND r.id > $W_LEDGER)), 0)
    + COALESCE((SELECT sum(l.after_uop - l.before_uop) FROM bin_uop_ledger l JOIN nodes n ON n.id = l.node_id
      WHERE l.id > $W_LEDGER AND n.name IN ($POS_SQL) AND l.op LIKE 'released_partial%'
        AND l.after_uop > COALESCE(l.before_uop, l.after_uop)), 0)"
}
# release_lines: the window's partial releases (a count above 0) at the cell, as both sides log
# them: the Edge's lines at the five positions, Core's for the bins released at them (its ledger's
# released rows in the window), "<edge fenced> <edge unfenced> <core fenced> <core unfenced>". The
# rest of the sim plant releases too, and is not this ledger's.
release_lines() {
  local since=$(( $(date +%s) - ${W_SINCE:-$START_S} + 2 )) edge core="" bins posre
  posre="$(printf '%s' "$POSITIONS" | tr -s ' ' '|')"
  bins="$(sql "SELECT DISTINCT l.bin_id FROM bin_uop_ledger l JOIN nodes n ON n.id = l.node_id
    WHERE l.id > $W_LEDGER AND n.name IN ($POS_SQL) AND l.op LIKE 'released%'" | tr '\n' '|' | sed 's/|$//')"
  edge="$(dc logs --no-log-prefix --since "${since}s" edge 2>/dev/null | grep -a 'release: ' \
    | grep -aE " node ($posre) declared " | grep -avE ' declared 0 ')"
  [ -n "$bins" ] && core="$(dc logs --no-log-prefix --since "${since}s" core 2>/dev/null \
    | grep -aE "bin_manifest: release of bin ($bins) " | grep -avE ': declared 0,')"
  echo "$(count_in "$edge" '— fenced') $(count_in "$edge" '— unfenced') $(count_in "$core" ') fenced at') $(count_in "$core" ') unfenced at')"
}
# count_in <text> <fixed string>: how many of text's lines carry the string.
count_in() { printf '%s\n' "$1" | grep -acF -- "$2"; }

# forensics: what a final ledger that is not exact is made of, read on the stack it stands on:
# Core's draws in the window by position and by op at the positions, draws Core booked at a node
# that is not a position, the positions' piles by state, the cart counts at the Edge and at Core,
# and the Edge's lines on captures, stranded piles and refused or clamped draws.
forensics() {
  say "FORENSICS draws in the window by position (node deltas parts): $(sql "SELECT n.name, count(*), sum(COALESCE(l.before_uop, l.after_uop) - l.after_uop)
    FROM bin_uop_ledger l JOIN nodes n ON n.id = l.node_id WHERE l.op = 'bin_uop_delta' AND l.id > ${W_LEDGER:-0} AND n.name IN ($POS_SQL)
    GROUP BY n.name ORDER BY n.name" | tr '\n' ';')"
  say "FORENSICS ops at the positions in the window (op rows parts): $(sql "SELECT l.op, count(*), sum(COALESCE(l.before_uop, l.after_uop) - l.after_uop)
    FROM bin_uop_ledger l JOIN nodes n ON n.id = l.node_id WHERE l.id > ${W_LEDGER:-0} AND n.name IN ($POS_SQL)
    GROUP BY l.op ORDER BY l.op" | tr '\n' ';')"
  say "FORENSICS draws booked away from the positions (node deltas parts): $(sql "SELECT COALESCE(n.name, '(none)'), count(*), sum(COALESCE(l.before_uop, l.after_uop) - l.after_uop)
    FROM bin_uop_ledger l LEFT JOIN nodes n ON n.id = l.node_id WHERE l.op = 'bin_uop_delta' AND l.id > ${W_LEDGER:-0}
    AND (n.name IS NULL OR n.name NOT IN ($POS_SQL)) GROUP BY n.name ORDER BY n.name" | tr '\n' ';')"
  say "FORENSICS draws that ended at 0 (id bin node before after detail): $(sql "SELECT l.id, l.bin_id, n.name, l.before_uop, l.after_uop, COALESCE(l.detail::text, '')
    FROM bin_uop_ledger l JOIN nodes n ON n.id = l.node_id WHERE l.op = 'bin_uop_delta' AND l.id > ${W_LEDGER:-0} AND l.after_uop <= 0
    ORDER BY l.id LIMIT 12" | tr '\n' ';' | cut -c1-900)"
  say "FORENSICS releases that raised a cart's count, draws the release's count undid (node bin before after op): $(sql "SELECT n.name, l.bin_id, l.before_uop, l.after_uop, l.op
    FROM bin_uop_ledger l JOIN nodes n ON n.id = l.node_id WHERE l.id > ${W_LEDGER:-0} AND n.name IN ($POS_SQL)
    AND l.op LIKE 'released%' AND l.after_uop > COALESCE(l.before_uop, l.after_uop) ORDER BY l.id" | tr '\n' ';')"
  say "FORENSICS Core's stale drops, a draw that reached Core after the cart's arrival (last 8): $(dc logs --no-log-prefix core 2>/dev/null | grep -a 'stale epoch DROPPED' | tail -8 | cut -c1-160 | tr '\n' ';')"
  say "FORENSICS the positions' piles (node part state qty): $(sql "SELECT core_node_name, payload_code, state, qty FROM lineside_buckets
    WHERE core_node_name IN ($POS_SQL) ORDER BY core_node_name, state" | tr '\n' ';')"
  say "FORENSICS Core's carts at the positions (node bin part count): $(sql "SELECT n.name, b.id, b.payload_code, b.uop_remaining FROM bins b JOIN nodes n ON n.id = b.node_id
    WHERE n.name IN ($POS_SQL) ORDER BY n.name" | tr '\n' ';')"
  say "FORENSICS the Edge's lines (captures, strands, clamps, refusals): $(dc logs --no-log-prefix edge 2>/dev/null | grep -aiE 'capture|stranded|count anomaly|clamp|payload mismatch|below zero|negative' | tail -12 | cut -c1-220 | tr '\n' ';')"
  say "FORENSICS the line (the readiness gate's answer is \"ready\"): $(line_raw)"
  say "FORENSICS the gate: $(gate_json | cut -c1-600)"
  local since=$(( $(date +%s) - ${W_SINCE:-${START_S:-$(date +%s)}} + 2 ))
  dc logs --no-log-prefix --since "${since}s" edge > "$OUT/forensics-edge.log" 2>&1
  dc logs --no-log-prefix --since "${since}s" core > "$OUT/forensics-core.log" 2>&1
  say "FORENSICS the Edge's and Core's logs for the window (${since}s): $OUT/forensics-edge.log, $OUT/forensics-core.log"
}

# ledger [mid]: closes the window. The final ledger (no argument) is read at rest and checked:
# carts + logged + written off = loads, the record = the parts the line counted out, Core received
# = the Edge's, nothing lost at the lift, and Core refused no draw as the wrong part. A mid-run
# ledger is read on the running line and says so.
ledger() {
  local kind="${1:-final}" carts expect n q a b other got miss="" line refused_n
  if [ "$kind" = final ]; then
    at_rest "the final ledger is read at rest"
    rest_read ledger_read
  else
    quiet_read ledger_read
  fi
  expect=$(( L_NS * L_DL + L_NC * L_COUNTED ))
  carts=$(( ${L_DRAWN:-0} - (${L_PILES:-0} - ${W_PILES:-0}) ))
  n="$(printf '%s\n' "$L_HOLDS" | grep -c .)"
  q="$(printf '%s\n' "$L_HOLDS" | awk -v nc="$L_NC" 'NF { if ($3 == 0) s += $2 * nc; else s += $2 } END {print s + 0}')"
  a="$(printf '%s\n' "$L_CORE" | awk -v id="$RUN_ID" '$1 == id {print $2}')"
  b="$(printf '%s\n' "$L_CORE" | awk -v id="$ALT_ID" '$1 == id {print $2}')"
  other="$(printf '%s\n' "$L_CORE" | awk -v r="$RUN_ID" -v x="$ALT_ID" 'NF && $1 != r && $1 != x {s += $2} END {print s + 0}')"
  got=$(( ${a:-0} + ${b:-0} + other ))
  say "draws at the five positions: carts $carts (Core's deltas $L_DRAWN, ${L_TRANSIT:-0} of them booked at _TRANSIT, piles +$(( ${L_PILES:-0} - ${W_PILES:-0} ))); expected $expect = $L_NS station-counted x $L_DL loads + $L_NC cell-counted x $L_COUNTED counted out; $n rows open, $q logged at the positions, $WRITTEN_OFF written off"
  say "partial releases in the window: the Edge's $L_EDGE_FENCED fenced, $L_EDGE_UNFENCED unfenced; Core's $L_CORE_FENCED fenced, $L_CORE_UNFENCED unfenced"
  say "Core refused as the wrong part in the window (deltas parts): $L_REFUSED"
  [ "$other" -eq 0 ] || say "Core received $other more under neither style"
  say "the record: $got parts against the $L_COUNTED the line counted out"
  line="carts Σ $carts + logged $q + written off $WRITTEN_OFF = loads Σ $expect; rows open $n; record RUN ${a:-0} / ALT ${b:-0}; Core received ${a:-0}+${b:-0} of the Edge's $L_T"
  if [ "$kind" = mid ]; then
    echo "$line (mid-run: the line runs, a read may carry one part in flight)" | tee -a "$OUT/log.txt"
    echo "lost at the lift ${L_LOST:-0}" | tee -a "$OUT/log.txt"
  else
    echo "$line" | tee -a "$OUT/log.txt"
    echo "lost at the lift ${L_LOST:-0}" | tee -a "$OUT/log.txt"
    echo "_TRANSIT ${L_TRANSIT:-0}" | tee -a "$OUT/log.txt"
    echo "minimum cart ${L_MIN:-none}" | tee -a "$OUT/log.txt"
    if [ -n "$L_STRIP" ]; then
      printf '%s\n' "$L_STRIP" | sed 's/^/strip: /' | tee -a "$OUT/log.txt"
    fi
    if [ -n "$L_OVERPACK" ]; then
      printf '%s\n' "$L_OVERPACK" | sed 's/^/overpack: /' | tee -a "$OUT/log.txt"
    fi
    [ "${L_LOST:-0}" -eq 0 ] || miss="$miss; lost at the lift ${L_LOST}"
    [ "${L_TRANSIT:-0}" -eq 0 ] || miss="$miss; ${L_TRANSIT} of the cell's draws booked at _TRANSIT (a late draw is booked at the node its cart was lifted from)"
    refused_n="${L_REFUSED%% *}"
    [ "${refused_n:-0}" -eq 0 ] || miss="$miss; Core refused $refused_n draw(s) as the wrong part"
    [ $(( ${L_EDGE_UNFENCED:-0} + ${L_CORE_UNFENCED:-0} )) -eq 0 ] || miss="$miss; unfenced partial releases (the Edge's ${L_EDGE_UNFENCED}, Core's ${L_CORE_UNFENCED})"
    [ $(( carts + q + WRITTEN_OFF )) -eq "$expect" ] || miss="$miss; carts + logged + written off $(( carts + q + WRITTEN_OFF )) against loads $expect"
    [ "$got" -eq "$L_COUNTED" ] || miss="$miss; the record $got against the $L_COUNTED counted out"
    [ "$got" -eq "$L_T" ] || miss="$miss; Core received $got against the Edge's $L_T"
    if [ -z "$miss" ]; then say "at rest: EXACT"; else say "at rest: NOT EXACT$miss"; LEDGER_EXACT=0; forensics; fi
  fi
  resume
}

# ── THE STACK ────────────────────────────────────────────────────────────────────────────
bring_up() {
  if [ -n "$(dc ps -q 2>/dev/null)" ] && [ "${SIM_4X2_REPLACE:-0}" != 1 ]; then
    echo "$PROJECT has containers running, and this run starts with down -v." >&2
    echo "Set SIM_4X2_REPLACE=1 if that stack is yours to drop." >&2
    exit 3
  fi
  bg dc --profile tools down -v --remove-orphans >"$OUT/down.log" 2>&1
  bash scripts/sim-anchor.sh mint >>"$OUT/log.txt"
  if [ "${SIM_4X2_NO_BUILD:-0}" != 1 ]; then
    say "build core edge seed"
    bg dc --profile tools build core edge seed >"$OUT/build.log" 2>&1 || die "build failed, see build.log"
  fi
  say "up postgres kafka core edge"
  SHINGO_BIND="${SHINGO_BIND:-127.0.0.1}" bg dc up -d postgres kafka core edge >"$OUT/up.log" 2>&1 || die "up failed, see up.log"
  # Core stopped across the seed: Edge migrates its database at boot and the seeder writes into
  # a migrated schema. docs/dev-env/sim.md, "Seed with Core STOPPED".
  wait_for 120 "Edge up" "curl -s -m 3 $EDGE/status >/dev/null" || die "Edge did not come up"
  bg dc stop core >>"$OUT/up.log" 2>&1
  # S9 seeds twice, the second time rewinding the mirror seats to the plant's counts. The Edge
  # stands stopped across both: a stroke it counted between them would reach Core from its outbox
  # after the second seed had put the seat's count back, and the seat would read one high.
  [ "$SHAPE" = S9 ] && bg dc stop edge >>"$OUT/up.log" 2>&1
  say "seed $PLANT"
  bg dc run --no-deps --rm seed >"$OUT/seed.log" 2>&1 || die "seed failed, see seed.log"
  [ "$SHAPE" = S9 ] && seed_unknown_mirrors
  bg dc start core >>"$OUT/up.log" 2>&1
  bg dc restart edge >>"$OUT/up.log" 2>&1
  wait_for 120 "Core up" "curl -s -m 3 $CORE/login >/dev/null" || die "Core did not come up"
  wait_for 120 "Edge up" "curl -s -m 3 $EDGE/status >/dev/null" || die "Edge did not come up"
}

# seed_unknown_mirrors (S9): the seed again, from the same spec with the mirror positions' carts
# naming no part, so their seats are written again as seats that cannot say (the seeder's rule for
# a bin with no payload), while Core keeps the MIRROR-L carts the first seed made (a re-seed leaves
# an existing bin's part alone). That is the plant's shape: Core knows the cart, the station was
# never told. No verb makes it on a running stack: Core's landing names the bin's part, a person's
# Load Payload names it, and a count names a part only to a seat with no cart bound. The Edge is
# stopped across it (the first seed leaves it polling, and its writes lock the database against
# the seeder's); bring_up's restart starts it.
seed_unknown_mirrors() {
  local re
  # shellcheck disable=SC2086 # MIRRORS is a list of positions
  re="$(printf '%s|' $MIRRORS)"; re="${re%|}"
  say "S9: the seed again with the mirror positions' carts naming no part (Core keeps its $MIRROR_RUN carts)"
  bg dc stop edge >>"$OUT/up.log" 2>&1
  bg dc run --no-deps --rm seed sh -c "sed -E '/slot: ($re),/s/ payload: [^,]+,//' $PLANT > /tmp/s9.yaml \
    && grep -E 'slot: ($re),' /tmp/s9.yaml && ! grep -qE 'slot: ($re),.*payload:' /tmp/s9.yaml \
    && seeddev --config /etc/shingo/shingocore.dev.yaml --edge-db /data/shingoedge.db --plant /tmp/s9.yaml --yes" \
    >>"$OUT/seed.log" 2>&1 || die "the S9 seed failed, see seed.log"
}

# restart_edge <catid>: docker compose restart of WELD-6's Edge mid-run. The fake PLC lives in
# it and boots at CATID 0 with an empty line, loading, so the PLC's program is set back once it
# is up.
restart_edge() {
  bank
  say "restart the Edge. Look at: the changeover, its legs and the holds when it is back"
  bg dc restart edge >>"$OUT/up.log" 2>&1 || die "edge restart failed, see up.log"
  wait_for 120 "Edge back" "curl -s -m 3 $EDGE/status >/dev/null" || die "Edge did not come back"
  edge_login
  wait_for 60 "WELD-6's line on the fake PLC" "[ -n \"\$(line_json)\" ]" || die "no WELD-6 line after the restart"
  set_catid "$1"
  resume
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
  OPS_ID="$(edge_get /api/process-nodes | tr '{' '\n' | grep "\"core_node_name\":\"${POSITIONS%% *}\"" \
    | grep -oE '"operator_station_id":[0-9]+' | head -1 | sed 's/.*://')"
  [ -n "$OPS_ID" ] || die "no operator station for ${POSITIONS%% *}"
  POS_RE="${POSITIONS// /|}"
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
# await_on_alt <sim s before it can act> <what>: the changeover (or the switch) lands on ALT.
await_on_alt() { wait_for $(( $(wall $(( $1 + 6 * TICK_S ))) + 30 )) "$2" closed_on "$ALT_ID"; }

# start_held: START to ALT with Core refusing creates, so its legs stay unlanded and its gate red.
# A swap with the fleet at a mirror position refuses START, and each position keeps one staged
# that the operator releases only once its cart is done; Core holding creates holds every cart's
# next swap too. So Core's creates are held in a gap between the mirrors' swaps on the running
# line (their next swaps park at Core, and START cancels those), the loading pauses at once so
# the carts keep their parts while nothing is delivered, the line drains, and START goes in.
# The line stays paused; the caller resumes it.
start_held() {
  local try
  for (( try = 1; ; try++ )); do
    wait_for "$LAND_WALL" "no swap with the fleet at the mirror positions" mirrors_quiet || die "the mirror positions never had a gap between swaps"
    legs_held on
    mirrors_quiet && break
    [ "$try" -lt 3 ] || die "a mirror position's swap reached the fleet as Core's creates were held, $try times"
    say "a mirror position's swap reached the fleet as Core's creates were held; Core takes them again until the next gap"
    legs_held off
  done
  pause_loading true || die "the pause was refused"
  wait_for $(( $(wall "$DRAIN_S") + 10 )) "the line drained" line_idle \
    || say "LOOK: the line stands with $(line_field in_flight) in flight: a position ran dry while Core holds creates"
  start_changeover "$ALT_ID"
  wait_for 60 "ALT's legs held back, the gate red" held_red || die "the gate did not read red"
}

# ── THE SHAPES ───────────────────────────────────────────────────────────────────────────
shape_S1() {
  say "S1 steady RUN, the five positions on the station counter, $RUN_TICKS ticks. Look at: each cart drops one a load, no hold, every count under RUN"
  run_ticks "$RUN_TICKS"
  ledger mid
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
    pause_loading false || die "the resume was refused"
  else
    start_changeover "$ALT_ID"
    say "Look at: the legs to the mirror positions; the sim operator releases them, nobody cuts over; the common positions are asked for and supplied as they run down"
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
    say "Look at: the held legs land as ordinary deliveries; the mirror positions' strokes before they landed stay logged, ALT's carts land fresh"
    status
  fi
}

shape_S3() {
  # From the START until the PLC reports ALT's part, the common positions are asked for and
  # supplied as they run down: the request guards refuse only where the part differs (the mirror
  # positions), so the line runs, and drains for the program change, on the running line.
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
    nap 2
  done
  say "held $(style_name "$1") for $2 s ($3), $most holds at most"
}

# mirror_bins: the carts standing at the mirror positions, as Core places them (bin ids).
mirror_bins() {
  local p in=""
  for p in $MIRRORS; do in="$in${in:+,}'$p'"; done
  sql "SELECT b.id FROM bins b JOIN nodes n ON n.id = b.node_id WHERE n.name IN ($in) ORDER BY b.id" | tr '\n' ' '
}
# mirrors_landed: a cart stands at each mirror position and none is one of SEEDED_MIRRORS.
mirrors_landed() {
  local b n=0
  for b in $(mirror_bins); do
    case " $SEEDED_MIRRORS " in *" $b "*) return 1 ;; esac
    n=$(( n + 1 ))
  done
  [ "$n" -eq "$(wc -w <<<"$MIRRORS")" ]
}
shape_S4() {
  # The scenario's carts all arrive by delivery, so each seat knows its cart's part. The stack's
  # first carts are seeded, and a seat learns its cart's part at a landing: S4 starts once each
  # mirror position has had one.
  SEEDED_MIRRORS="$(mirror_bins)"
  say "S4 waits for a landed cart at each mirror position (seeded: bins $SEEDED_MIRRORS)"
  wait_for "$LAND_WALL" "a landed cart at each mirror position" mirrors_landed \
    || die "a mirror position still holds its seeded cart"
  say "S4 no changeover, RUN active, both styles pinned: the FORCED program change to $CATID_ALT"
  jump
  say "Look at: the loads in flight count out as $CATID_ALT; the mirror positions' draws are held (their carts are MIRROR-L)"
  await_holds 1 "holds open"
  status
  asks_and_moves_nothing
  if [ "$VARIANT" = tap ]; then tap_and_land; return; fi
  say "S4 untapped, 10 ticks. Look at: the style stays RUN, the mirror holds grow every stroke, the common positions draw, nothing is ordered at the mirror positions, the item stays red"
  run_ticks 10
  untapped_read
}

# jump: the FORCED program change to ALT's part with no changeover open, Core's last order id
# noted first (orders_since).
JUMP_ORDER=0
jump() {
  JUMP_ORDER="$(sql "SELECT COALESCE(max(id), 0) FROM orders")"
  program_change "$CATID_ALT" forced
}
# mirror_orders_since [part]: Core's orders made since the jump that deliver to a mirror position,
# of that part when one is named, "<id> <node> <part> <status>" a line.
mirror_orders_since() {
  sql "SELECT id || ' ' || delivery_node || ' ' || payload_code || ' ' || status FROM orders
       WHERE id > ${JUMP_ORDER:-0} AND delivery_node IN ($MIRROR_SQL) ${1:+AND payload_code = '$1'} ORDER BY id" | tr '\n' ';'
}
# item_action: the wrong-part item's action, off the strip's raw JSON (slot_item splits on {"kind":,
# which the action object begins with too); empty when the item offers none.
item_action() { edge_get "/api/processes/$PID/catid-health" | grep -oE '"action":\{"kind":"start_changeover"[^}]*\}' | head -1; }
# item_line: the strip's wrong-part item, its severity, headline, detail and action.
item_line() {
  local it act; it="$(slot_item wrong_part)"; act="$(item_action)"
  echo "[$(jstr "$it" severity)] $(jstr "$it" headline) ($(jstr "$it" detail)); ${act:-no action}"
}
item_action_style() { field "$(item_action)" to_style_id; }
# asks_and_moves_nothing: waits out the declaration window the automatic switch once acted after,
# then reads that nothing moved (RUN active, no changeover open, no order for ALT's part at a
# mirror position since the jump: RUN's own resupply goes on, and a sweep in the seconds before the
# monitor confirms the new part may ask one) and that the strip's red item offers the changeover.
asks_and_moves_nothing() {
  say "Look at, $(wall $(( DECLARE_S + 2 * TICK_S ))) s: nothing moves (no switch, no changeover, no order for ALT's part); the strip's item turns red and offers the changeover"
  watch_stays "$RUN_ID" "$(wall $(( DECLARE_S + 2 * TICK_S )))" "the PLC on $CATID_ALT, no changeover" || die "the style moved with no changeover"
  co_open && die "a changeover opened with nobody tapping"
  [ -z "$(mirror_orders_since "$MIRROR_ALT")" ] || die "orders for $MIRROR_ALT at the mirror positions since the jump: $(mirror_orders_since "$MIRROR_ALT")"
  say "orders at the mirror positions since the jump (RUN's own resupply): $(mirror_orders_since)"
  say "the item: $(item_line)"
  [ "$(item_action_style)" = "$ALT_ID" ] || die "the wrong-part item offers no changeover to ALT: $(slot_item wrong_part | cut -c1-400)"
  status
}
# untapped_read: the untapped half's end: nothing has moved still.
untapped_read() {
  [ "$(active)" = "$RUN_ID" ] || die "the style moved, nobody tapping"
  co_open && die "a changeover opened, nobody tapping"
  say "the mirror holds open (id qty node): $(holds_list | tr '\n' ';')"
  say "orders at the mirror positions since the jump (RUN's own resupply): $(mirror_orders_since)"
  [ -z "$(mirror_orders_since "$MIRROR_ALT")" ] || die "orders for $MIRROR_ALT at the mirror positions since the jump: $(mirror_orders_since "$MIRROR_ALT")"
  say "the item: $(item_line)"
  status
}
# tap_and_land [hook]: the strip's one tap on the item's action (the start verb, as the page calls
# it), the hook (what a person does at the station before the legs can land), then the legs land,
# the monitor completes the changeover, and the completion lands each hold that fits its cart. A
# share bigger than the cart it would land on stays open for a person (its line names both
# numbers); the assertion is that no cart at the positions went below 0.
tap_and_land() {
  local to
  to="$(item_action_style)"
  say "TAP the item's Start changeover to $(style_name "$to")"
  start_changeover "$to" engineer
  [ -n "${1:-}" ] && "$1"
  # The monitor may complete it while the hook runs: landed, or already closed on ALT.
  wait_for "$LAND_WALL" "ALT's carts landed (the gate green, or the changeover already completed)" "landed || closed_on $ALT_ID"     || die "the carts did not land"
  await_on_alt "$DECLARE_S" "the monitor completes the changeover onto ALT"
  say "the monitor: $(dc logs --since 30m edge 2>/dev/null | grep 'predicate held' | grep -- '— cut over' | tail -1 | sed 's/^[^|]*| //' | cut -c1-300)"
  say "the rows stay for a person (id qty node): $(holds_list | tr '\n' ';'); the mirror carts (node bin part count): $(mirror_carts)"
  [ -z "$(carts_below_zero)" ] || say "carts at the positions below 0 (node bin count), shown, not refused: $(carts_below_zero | tr '\n' ';')"
  status
}
# carts_below_zero: Core's carts at the positions that read below 0, "node|bin|count" a line
# (decision 55: a stroke charges the cart standing there into the negative; shown, never refused).
carts_below_zero() {
  sql "SELECT n.name, b.id, b.uop_remaining FROM bins b JOIN nodes n ON n.id = b.node_id
       WHERE n.name IN ($POS_SQL) AND b.uop_remaining < 0"
}

shape_S5() {
  local first second preview code
  say "S5 the forced program change to $CATID_NONE, which no style pins"
  program_change "$CATID_NONE" forced
  await_holds 2 "two holds open on $CATID_NONE" || die "fewer than two holds opened"
  status
  say "holds open (id qty): $(holds_list | tr '\n' ';')"
  first="$(holds_list | awk '$3 != 0' | sed -n 1p | cut -d' ' -f1)"
  second="$(holds_list | awk '$3 != 0' | sed -n 2p | cut -d' ' -f1)"
  local wq
  wq="$(holds_list | awk -v id="$second" '$1 == id {print $2}')"
  code="$(hold_write_off "$second")"; say "Write off hold $second ($wq): $code $(last)"
  http_ok "$code" && WRITTEN_OFF=$(( WRITTEN_OFF + ${wq:-0} ))
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
  say "Look at: the charged hold's quantity off RUN's carts; the written-off one charges nothing"
  status
}

shape_S6() {
  local p
  for p in $MIRRORS; do counted_at "$p" null; done
  mark
  say "S6 positions 1-3 on the station counter, 4-5 on the cell counter: changeover RUN->ALT on the running line"
  start_changeover "$ALT_ID"
  await_landing
  program_change "$CATID_ALT" drain
  await_plc "$CATID_ALT"
  await_on_alt "$DECLARE_S" "the declaration completes the changeover onto ALT"
  say "Look at: positions 1-3 draw at the load station, 4-5 as parts count out; the line refills"
  run_ticks $(( LINE_DEPTH + 4 ))
}

shape_S7() {
  say "S7 the forced change to $CATID_NONE, which no style pins: its strokes' draws are held; then back to $CATID_RUN"
  program_change "$CATID_NONE" forced
  await_holds 1 "holds on $CATID_NONE" || die "no hold opened"
  program_change "$CATID_RUN" forced
  say "the monitor confirms $CATID_RUN again"
  sim_s $(( DECLARE_S + 2 * TICK_S ))
  say "Core holds every create from a gap between the mirror positions' swaps; the line pauses its loading and drains; START to ALT with its legs held; then the Edge restarts mid-changeover"
  start_held
  restart_kept "$CATID_RUN"
  say "the fake PLC booted with the Edge, loading; the pause is lifted all the same"
  pause_loading false || die "the resume was refused"
  status
  program_change "$CATID_ALT" forced
  say "Look at: ALT parts count; with the changeover open to ALT the mirror positions' MIRROR-L carts are charged, not held; the holds on $CATID_NONE stand"
  run_ticks 4
  restart_kept "$CATID_ALT"
  legs_held off
  say "Look at: Core takes the held legs and they land; the changeover completes onto ALT"
  wait_for $(( LAND_WALL + $(wall $(( DECLARE_S + 6 * TICK_S ))) )) "the changeover onto ALT completed" closed_on "$ALT_ID"
  say "Look at: the holds on $CATID_NONE stay open (no style runs it: a person's Charge or Write off); nothing is charged twice (the ledger's carts against its loads)"
  run_ticks 4
}

# restart_kept <catid>: restart_edge, and the holds open before it are the ones open after it.
restart_kept() {
  local before after id lost=""
  before="$(holds_list | tr '\n' ';')"
  restart_edge "$1"
  after="$(holds_list | tr '\n' ';')"
  for id in $(printf '%s' "$before" | tr ';' '\n' | cut -d' ' -f1); do
    case ";$after" in *";$id "*) ;; *) lost="$lost $id" ;; esac
  done
  if [ -z "$lost" ]; then say "the holds survived the restart (id qty): [$before] now [$after]"; else say "LOOK: holds$lost gone after the restart (id qty): [$before] now [$after]"; fi
}

shape_S8() {
  local t0 info parts code
  say "S8 the PLC moves its count from Prod_Counter_01 to Prod_Counter_02 of MES_WELD_6: _02 ticks from here and CATID_02 reads $CATID_RUN; Prod_Counter_01 and CATID_01 stand"
  # A sim ticker arms each cycle at the speed of the moment, so the slot watch's pass armed while
  # the window's edge was read at 1x comes up to SLOT_WATCH_S wall seconds later. The move waits
  # it out; from here the watch passes at the run's speed.
  say "the slot watch's pass armed during the 1x read takes up to $SLOT_WATCH_S wall-s; the move waits it out"
  nap "$SLOT_WATCH_S"
  counting_slot 2
  t0=$(date +%s)
  say "Look at: WELD-6's counter stops. The Edge's slot watch compares two reads $SLOT_WATCH_S sim-s ($(wall "$SLOT_WATCH_S") wall-s) apart, so the pass that straddles the move can say Prod_Counter_02 rises WITH _01 (info, one pass); the next is red: the PLC counts on Prod_Counter_02"
  wait_for $(( 2 * $(wall "$SLOT_WATCH_S") + 10 )) "the strip's red counter_slot_idle item" '[ -n "$(slot_item counter_slot_idle)" ]' \
    || die "no counter_slot_idle item in two slot-watch passes"
  info="$(slot_item counter_slot_idle | grep -oE '"headline":"[^"]*"')"
  say "the item, $(( $(date +%s) - t0 )) wall-s after the move: $info"
  parts="$(slot_counter 2)"
  code="$(edge_send POST "/api/processes/$PID/counter" "{\"tag_name\":\"$COUNTER_TAG2\"}")"
  say "the click, count $COUNTER_TAG2: $code $(last)"
  http_ok "$code" || die "the re-point was refused"
  [ "$(counter_tag)" = "$COUNTER_TAG2" ] || die "$PROC counts $(counter_tag) after the click, not $COUNTER_TAG2"
  say "the click came $(( $(date +%s) - t0 )) wall-s after the move, with about ${parts:-?} parts on Prod_Counter_02: the re-point counts from _02's count before it rose, so the record keeps them; the carts, at the load station, lack none"
  say "Look at: the item gone; the record and Core's heartbeat resume under RUN at the next WarLink scan; the first part on _02 carries CATID_02's $CATID_RUN (no HTTP read: S8 in integration/scenarios asserts it); the ledger's record equals the parts the line counted out"
  run_ticks 10
  status
}

# ── S9: THE HAND-PLACED CART ──────────────────────────────────────────────────────────────
# jstr <json> <name> / jbool <json> <name>: the first string / boolean under that name.
jstr() { printf '%s' "$1" | grep -oE "\"$2\":\"[^\"]*\"" | head -1 | sed 's/^"[^"]*":"//; s/"$//'; }
jbool() { printf '%s' "$1" | grep -oE "\"$2\":(true|false)" | head -1 | sed 's/.*://'; }
# seat <position>: the Edge's seat as the operator station's board reads it: the cart bound, the
# part the seat records, known or not and who said so, and its count.
OPS_ID=""
seat() {
  local t
  t="$(edge_get "/api/operator-stations/$OPS_ID/view" | sed 's/{"node":{/\n{"node":{/g' \
    | grep -E "^\{\"node\":\{[^}]*\"core_node_name\":\"$1\"" | head -1)"
  echo "$1: bin $(field "$t" active_bin_id) part $(jstr "$t" lineside_payload_code) known $(jbool "$t" lineside_payload_known) source $(jstr "$t" lineside_source) count $(field "$t" remaining_uop_cached)"
}
mirror_carts() {
  sql "SELECT n.name, b.id, b.payload_code, b.uop_remaining FROM bins b JOIN nodes n ON n.id = b.node_id
       WHERE n.name IN ($MIRROR_SQL) ORDER BY n.name, b.id" | tr '|' ' ' | tr '\n' ';'
}
MIRROR_NODE_RE=""
mirror_holds() { holds_list | awk -v re="^($MIRROR_NODE_RE)\$" '$3 ~ re' | tr '\n' ';'; }
s9_read() {
  local p
  say "S9 $1:"
  for p in $MIRRORS; do say "  seat $(seat "$p")"; done
  say "  the mirror holds open (id qty node): $(mirror_holds)"
  say "  Core's mirror carts (node bin part count): $(mirror_carts)"
  say "  Core refused as the wrong part since the window opened (deltas parts): $(refused "$W_LEDGER")"
}
# orders_at <position>: the Edge's active orders there, "<id> <status> <part>" a line.
orders_at() {
  edge_get /api/orders/active | sed 's/},{/}\n{/g' | grep "\"process_node_id\":${NODE_ID[$1]}," \
    | while IFS= read -r o; do echo "$(field "$o" id) $(jstr "$o" status) $(jstr "$o" payload_code)"; done \
    | awk '$2 !~ /^(confirmed|cancelled|failed|completed)$/'
}
staged_at() { [ "$(orders_at "$1" | grep -c ' staged ')" -gt 0 ]; }
# request_at <position>: the station's REQUEST, unless an order is live there already.
request_at() {
  local live code
  live="$(orders_at "$1")"
  if [ -n "$live" ]; then say "$1 has orders live (id status part): $(printf '%s\n' "$live" | tr '\n' ';')"; return 0; fi
  code="$(edge_send POST "/api/process-nodes/${NODE_ID[$1]}/request" '{}')"
  say "REQUEST at $1: $code $(last)"
}
# release_at <position>: the station's RELEASE of the staged swap, the standing cart sent back
# with the screen's count left as it was (send partial back, no partial_count): the release
# declares the seat's count in the fence's instant, as for a person who leaves the number. Pressed
# again every 5 s while it is refused (a leg still waiting for Core), until the standing cart has
# gone or is one of ALT's part.
release_at() {
  local part count code deadline=$(( $(date +%s) + LAND_WALL ))
  while :; do
    IFS='|' read -r part count < <(sql "SELECT b.payload_code, b.uop_remaining FROM bins b JOIN nodes n ON n.id = b.node_id
      WHERE n.name = '$1' ORDER BY b.id LIMIT 1")
    case "$part" in "" | "$MIRROR_ALT") say "the cart at $1 has gone or is $MIRROR_ALT: nothing to release"; return 0 ;; esac
    code="$(edge_send POST "/api/process-nodes/${NODE_ID[$1]}/release-staged" \
      "{\"disposition\":\"send_partial_back\",\"called_by\":\"sim-4x2\"}")"
    say "RELEASE at $1, the standing $part cart (Core's count $count) sent back with the screen's count: $code $(last)"
    http_ok "$code" && return 0
    [ "$(date +%s)" -lt "$deadline" ] || { say "LOOK: RELEASE at $1 refused for ${LAND_WALL} s"; return 1; }
    nap 5
  done
}
# cancel_at <position>: the person cancels each order live there (the order's abort door), START's
# remedy for a swap of the part the changeover replaces ("Cancel it, then start the changeover").
cancel_at() {
  local id code
  for id in $(orders_at "$1" | awk '{print $1}'); do
    code="$(edge_send POST "/api/orders/$id/abort" '{}')"
    say "CANCEL order $id at $1: $code $(last)"
  done
}
# release_hand_placed: the person at the station sends each hand-placed cart back once the
# changeover's swap there is staged (the sim operator waits for a cart that is done or recorded as
# another part, and nobody named these).
release_hand_placed() {
  local p
  say "S9 the hand-placed carts are the person's: RELEASE at each mirror position once the changeover's swap is staged"
  for p in $MIRRORS; do
    wait_for "$LAND_WALL" "the changeover's swap at $p staged" "staged_at $p" && release_at "$p"
  done
}
mirrors_on_alt() {
  [ "$(sql "SELECT count(*) FROM bins b JOIN nodes n ON n.id = b.node_id
            WHERE n.name IN ($MIRROR_SQL) AND b.payload_code = '$MIRROR_ALT'")" -eq "$(wc -w <<<"$MIRRORS")" ]
}
shape_S9() {
  local p
  for p in $MIRRORS; do MIRROR_NODE_RE="${MIRROR_NODE_RE:+$MIRROR_NODE_RE|}${NODE_ID[$p]}"; done
  say "S9 the hand-placed cart: Core's carts at the mirror positions are $MIRROR_RUN, and their seats cannot say what they are (seeded with no part)"
  s9_read "at the start"
  for p in $MIRRORS; do
    case "$(seat "$p")" in *" known false "*) ;; *) die "the seat at $p knows its cart: $(seat "$p")" ;; esac
  done
  say "S9 the line loads again (it stood paused from boot so the seeded carts stay), RUN draws $S9_RUN_TICKS ticks: the mirror carts stay above their reorder point, so no swap is asked for under RUN"
  pause_loading false || die "the resume was refused"
  sim_s $(( S9_RUN_TICKS * TICK_S ))
  s9_read "under RUN"
  for p in $MIRRORS; do
    case "$(seat "$p")" in *" known false "*) ;; *) say "LOOK: the seat at $p knows its cart before the switch (a swap under RUN): $(seat "$p")" ;; esac
  done
  say "S9 no changeover, RUN active, both styles pinned: the FORCED program change to $CATID_ALT"
  jump
  say "Look at: the strokes count as $CATID_ALT under RUN; the mirror positions' draws are held"
  await_holds 1 "holds open"
  status
  asks_and_moves_nothing
  s9_read "past the window"
  if [ "$VARIANT" = tap ]; then
    # A swap of RUN's part staged at a mirror position (RUN's own resupply, asked before the
    # monitor confirmed 1002) refuses START, and START's refusal names Cancel for a swap of the
    # part the changeover replaces: released, it would land RUN's part for the changeover's leg to
    # take away again. The person cancels it before the tap.
    for p in $MIRRORS; do
      [ -n "$(orders_at "$p")" ] || continue
      say "S9 RUN's swap at $p is live: the person cancels it once staged, before the tap, as START says"
      wait_for "$LAND_WALL" "RUN's swap at $p staged" "staged_at $p" && cancel_at "$p"
    done
    tap_and_land release_hand_placed
    wait_for "$LAND_WALL" "a $MIRROR_ALT cart at each mirror position" mirrors_on_alt \
      || say "LOOK: Core's mirror carts (node bin part count): $(mirror_carts)"
    s9_read "after the changeover"
    return
  fi
  say "S9 untapped, 10 ticks. Look at: what each mirror seat records (nothing flips), the mirror holds growing, the mirror carts' counts in Core standing, the deltas Core refused"
  run_ticks 10
  for p in $MIRRORS; do
    case "$(seat "$p")" in *" known false "*) ;; *) say "LOOK: the seat at $p knows its cart, nobody tapping: $(seat "$p")" ;; esac
  done
  untapped_read
  s9_read "after 10 ALT ticks, untapped"
}

# ── THE RUN ──────────────────────────────────────────────────────────────────────────────
. scripts/gate-lock.sh
gate_lock_acquire "sim-4x2 $SHAPE $VARIANT" || { echo "no gate lock" >&2; exit 3; }
cleanup() {
  stop_children
  [ "$LEGS_HELD" = 1 ] && legs_held off
  [ "$PAUSED" = 1 ] && pause_loading false
  rm -f "$EDGE_COOKIES" "$OUT/.last"
  gate_lock_release
}
# on_signal <name> <status>: the run stops at INT, TERM or HUP (a person's Ctrl-C, a driver
# stopping it, a closed terminal). Its jobs are killed and exit runs cleanup (Core's creates and
# the line's loading back on); the stack stays up. A Windows TerminateProcess runs no trap.
on_signal() {
  trap '' INT TERM HUP
  say "SIG$1: the run stops; its children are killed and the gate lock released"
  stop_children
  exit "$2"
}
trap cleanup EXIT
trap 'on_signal INT 130' INT
trap 'on_signal TERM 143' TERM
trap 'on_signal HUP 129' HUP

bring_up
edge_login
# S9's seeded mirror carts are its hand-placed carts, and the line draws a cart's 40 parts in under
# a minute at 10x: it stands paused from the fake PLC's first answer until the shape starts.
if [ "$SHAPE" = S9 ]; then
  wait_for 60 "WELD-6's line on the fake PLC" "[ -n \"\$(line_json)\" ]" || die "no WELD-6 line"
  pause_loading true || die "the pause was refused"
fi
resolve
if [ "$SHAPE" != S9 ]; then
  wait_for 120 "WELD-6's line loading" line_live 0 || die "WELD-6's line never loaded"
fi
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
[ "$LEDGER_EXACT" = 1 ] || exit 5
