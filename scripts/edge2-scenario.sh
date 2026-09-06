#!/usr/bin/env bash
# edge2-scenario.sh — drive and observe an EDGE 2 changeover scenario.
#
# Runs ON houseserver, beside qcore.sh / qedge2.sh.
#
# WHY THERE IS A STABILITY GATE. The first S1 attempt started a changeover while
# PRS_010 was mid-swap with no carrier bound, and everything after that was
# uninterpretable: the node was already starved, replenishment was correctly
# suppressed ("below its level but not accepting orders: changeover in progress"),
# and the node task went to error. That run proved nothing about the code and cost
# the time of a run that looked like it might. A scenario that starts from an
# unknown state produces evidence about the state, not about the change.
#
# So: sample until the plant is demonstrably ready, N times consecutively, before
# touching anything, and exit non-zero rather than starting anyway if it never is.
# What "ready" means is spelled out at cells_ok below — the first version of it
# demanded a state this plant never reaches and called a healthy loop unstable.
set -u

Q="$HOME/e2/qedge2.sh"
QC="$HOME/e2/qcore.sh"
EDGE2_URL="http://localhost:8082"
CELLS="PRS_010 PRS_011 WLD_010"

usage() { echo "usage: $0 stable [samples] | start <to_style_id> | watch <seconds> | snap <label>"; exit 2; }

# one row per cell: node, claim, bin, uop, lineside, known, source
snap_rows() {
  $Q "SELECT n.core_node_name || '|' || COALESCE(r.active_claim_id,0) || '|' || COALESCE(r.active_bin_id,0)
             || '|' || r.remaining_uop_cached || '|' || r.lineside_payload_code
             || '|' || r.lineside_payload_known || '|' || r.lineside_source
      FROM process_node_runtime_states r JOIN process_nodes n ON n.id=r.process_node_id
      ORDER BY n.core_node_name;" 2>/dev/null | grep '|'
}

# READY IS NOT "EVERY CELL BOUND AT ONCE" — that state may never exist and
# demanding it makes the gate unpassable rather than strict.
#
# A produce cell is legitimately carrier-less for a window: its full carrier
# ships out and an empty has not landed yet. An A/B pair's parked side holds a
# seeded carrier nobody ever delivered, so its identity is correctly unknown.
# The first version of this gate required bin>0 AND known=1 on all three cells
# simultaneously and reported a healthy, cycling loop as NOT STABLE for twenty
# minutes.
#
# What a scenario actually needs:
#   - the node it acts on (SCENARIO_NODE) is bound, non-negative, identity known;
#   - no cell is stuck NEGATIVE (a drained cell mid-rotation is fine at 0, not below);
#   - any cell that IS holding a carrier has an established identity — an unknown
#     identity on a bound carrier is the thing under test and must not be baseline;
#   - the plant is moving: confirmed orders increasing between samples.
SCENARIO_NODE="${SCENARIO_NODE:-PRS_010}"

confirmed_count() {
  $QC "SELECT count(*) FROM orders WHERE status='confirmed';" 2>/dev/null | sed -n '3p' | tr -d ' '
}

cells_ok() {
  local rows="$1" c line bin uop known
  line=$(printf '%s
' "$rows" | grep "^$SCENARIO_NODE|") || return 1
  bin=$(printf '%s' "$line" | cut -d'|' -f3)
  uop=$(printf '%s' "$line" | cut -d'|' -f4)
  known=$(printf '%s' "$line" | cut -d'|' -f6)
  [ "${bin:-0}" -gt 0 ] 2>/dev/null || return 1
  [ "${uop:-0}" -ge 0 ] 2>/dev/null || return 1
  [ "${known:-0}" -eq 1 ] 2>/dev/null || return 1
  for c in $CELLS; do
    line=$(printf '%s
' "$rows" | grep "^$c|") || continue
    bin=$(printf '%s' "$line" | cut -d'|' -f3)
    uop=$(printf '%s' "$line" | cut -d'|' -f4)
    known=$(printf '%s' "$line" | cut -d'|' -f6)
    [ "${uop:-0}" -ge 0 ] 2>/dev/null || return 1          # nothing stuck negative
    if [ "${bin:-0}" -gt 0 ] 2>/dev/null; then
      [ "${known:-0}" -eq 1 ] 2>/dev/null || return 1      # a bound carrier is identified
    fi
  done
  return 0
}

case "${1:-}" in
  stable)
    want="${2:-3}"; run=0; prev=$(confirmed_count)
    for i in $(seq 1 60); do
      rows="$(snap_rows)"; now_c=$(confirmed_count)
      moving=0; [ "${now_c:-0}" -gt "${prev:-0}" ] 2>/dev/null && moving=1
      if cells_ok "$rows" && [ "$moving" -eq 1 ]; then run=$((run+1)); else run=0; fi
      printf '%s  ok=%d/%d confirmed=%s(+%s) node=%s
' "$(date -u +%H:%M:%S)" "$run" "$want"         "${now_c:-?}" "$(( ${now_c:-0} - ${prev:-0} ))" "$SCENARIO_NODE"
      printf '%s
' "$rows" | sed 's/^/    /'
      prev="$now_c"
      [ "$run" -ge "$want" ] && { echo "STABLE"; exit 0; }
      sleep 20
    done
    echo "NOT STABLE after 60 samples — do not start a scenario against this." >&2
    exit 1 ;;
  start)
    [ $# -ge 2 ] || usage
    echo "T0=$(date -u +%H:%M:%S) pre-state:"; snap_rows | sed 's/^/    /'
    curl -s -m 10 -X POST "$EDGE2_URL/api/processes/1/changeover/start" \
      -H 'Content-Type: application/json' \
      -d "{\"to_style_id\":$2,\"actor\":\"scenario\",\"note\":\"scenario drive\"}"
    echo ;;
  watch)
    secs="${2:-600}"; endt=$(( $(date +%s) + secs ))
    while [ "$(date +%s)" -lt "$endt" ]; do
      printf 'T=%s\n' "$(date -u +%H:%M:%S)"; snap_rows | sed 's/^/    /'
      curl -s -m 6 "$EDGE2_URL/api/processes/1/changeover/gate-status" 2>/dev/null | sed 's/^/    gate: /'; echo
      sleep 20
    done ;;
  snap)
    lbl="${2:-snap}"
    echo "=== $lbl @ $(date -u +%H:%M:%S) ==="
    echo "-- edge2 runtime --"; snap_rows | sed 's/^/  /'
    echo "-- core bins at edge2 nodes --"
    $QC "SELECT b.id, b.label, b.payload_code, b.uop_remaining, n.name AS at_node
         FROM bins b JOIN nodes n ON n.id=b.node_id
         WHERE n.name IN ('PRS_010','PRS_010B','PRS_011','WLD_010','SLN_L2_01')
         ORDER BY n.name;"
    echo "-- lineside reports (Core read-model) --"
    $QC "SELECT station, core_node_name, payload_code, bin_count, bin_uop, bucket_qty, reported_at
         FROM edge_lineside_reports WHERE station LIKE 'edge2%' ORDER BY core_node_name, payload_code;"
    echo "-- cms transactions --"
    $QC "SELECT id, node_name, cat_id, delta, bin_label, payload_code, source_type, storeroom
         FROM cms_transactions ORDER BY id DESC LIMIT 15;" ;;
  *) usage ;;
esac
