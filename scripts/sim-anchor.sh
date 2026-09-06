#!/usr/bin/env bash
# Mint (or keep) the dev stack's shared sim anchor.
#
# The sim clock computes sim_now = epoch + speed x (wallNow - anchor). Every
# process in a run must use the SAME anchor or their clocks drift apart by
# boot-skew x speed, which silently expires cross-process coordination messages
# (the 2026-07-12 loop degradation: 400+ expired-message drops). That is why the
# anchor is shared. It is minted HERE, rather than written into the config
# files, because a hardcoded origin's distance from wall time grows by
# (speed-1) per unit of real time forever -- the rig's clock had drifted eight
# weeks into the future at 2x, and every elapsed duration in the UI, all
# computed against it, rendered 0 s.
#
# THE ANCHOR'S LIFETIME IS THE DATABASE'S LIFETIME. Rows already in Postgres and
# the Edge SQLite carry simulated stamps in the CURRENT anchor's frame. Minting a
# new anchor over surviving volumes puts sim-now before rows that already exist,
# which is exactly the backwards jump the 10x->2x speed edit caused on
# 2026-09-05 (~439 days; six orders left stamped in the future, frozen, and read
# as a rig defect). So:
#
#   ensure  - create .env's anchor only if absent. Safe on every bring-up, and
#             a no-op for a stack that is merely being restarted.
#   mint    - force a new anchor. Correct ONLY alongside `down -v`.
#
# Usage: scripts/sim-anchor.sh [ensure|mint|show]
set -euo pipefail

ENV_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/.env"
KEY="SHINGO_SIM_ANCHOR"
MODE="${1:-ensure}"

current() {
    [ -f "$ENV_FILE" ] || return 0
    sed -n "s/^${KEY}=//p" "$ENV_FILE" | tail -1
}

write() {
    local now
    # RFC3339 UTC to the second. clock.ResolveAnchor parses exactly this.
    now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if [ -f "$ENV_FILE" ]; then
        grep -v "^${KEY}=" "$ENV_FILE" > "${ENV_FILE}.tmp" || true
        mv "${ENV_FILE}.tmp" "$ENV_FILE"
    fi
    printf '%s=%s\n' "$KEY" "$now" >> "$ENV_FILE"
    echo "$now"
}

case "$MODE" in
    show)
        c="$(current)"
        if [ -n "$c" ]; then echo "$c"; else echo "(none)"; fi
        ;;
    ensure)
        c="$(current)"
        if [ -n "$c" ]; then
            echo "[sim-anchor] keeping $KEY=$c (restarting a stack must not re-anchor it)"
        else
            echo "[sim-anchor] minted $KEY=$(write)"
        fi
        ;;
    mint)
        echo "[sim-anchor] minted $KEY=$(write) (fresh volumes -- simulated time restarts at today)"
        ;;
    *)
        echo "usage: $0 [ensure|mint|show]" >&2
        exit 2
        ;;
esac
