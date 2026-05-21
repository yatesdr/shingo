#!/usr/bin/env bash
# alert-on-stop.sh — Teams crash-alert hook for shingo services.
#
# Invoked by systemd ExecStopPost on every service stop. Posts an Adaptive
# Card to a Teams webhook when the stop looks unplanned. Stays silent on:
#   - planned restarts (/run/shingo-deploy-in-progress touched by install)
#   - system reboot/shutdown (systemctl is-system-running == "stopping")
#   - missing/empty TEAMS_WEBHOOK_URL (alerts disabled)
#   - exponential-backoff window (see schedule below)
#
# Invocation from the unit:
#   ExecStopPost=/opt/shingo/alert-on-stop.sh %n ${SERVICE_RESULT} ${EXIT_CODE} ${EXIT_STATUS}
#
# Config (/etc/shingo/alert.env, sourced as bash):
#   TEAMS_WEBHOOK_URL=https://...     # empty disables alerting
#   PLANT_ID=<name>                   # falls back to hostname -s
#
# Per-service state (/var/lib/shingo/alert-state-<unit>):
#   last_alert_ts=<unix-seconds>
#   backoff_level=<int>
#
# Backoff: alert fires on level 0 immediately, then suppresses until the
# interval at the current level has elapsed. After 30 min of no crashes,
# level resets to 0 (treated as a new incident).
#
#   level 0 -> 0s        (fire on first crash of incident)
#   level 1 -> 5 min
#   level 2 -> 15 min
#   level 3 -> 30 min
#   level 4+ -> 60 min   (ongoing trouble)
#
# Each fired alert is titled "CRASH (alert #N)" so escalation is visible.
# No separate "service is dead" tier — services use StartLimitIntervalSec=0
# by design, so systemd never gives up.

set -u
exec >>/var/log/shingo-alert.log 2>&1
echo "$(date -Iseconds) [alert-on-stop] unit=${1:-} result=${2:-} code=${3:-} status=${4:-}"

UNIT="${1:-unknown}"
RESULT="${2:-unknown}"
EXIT_CODE="${3:-}"
EXIT_STATUS="${4:-}"

CONFIG=/etc/shingo/alert.env
if [ ! -f "$CONFIG" ]; then
    echo "  no config at $CONFIG, exit silent"
    exit 0
fi
# shellcheck disable=SC1090
. "$CONFIG"

if [ -z "${TEAMS_WEBHOOK_URL:-}" ]; then
    echo "  TEAMS_WEBHOOK_URL empty, exit silent"
    exit 0
fi
PLANT_ID="${PLANT_ID:-$(hostname -s)}"

# --- Suppression ---

if [ -f /run/shingo-deploy-in-progress ]; then
    echo "  deploy marker present, suppress"
    exit 0
fi

SYS_STATE=$(systemctl is-system-running 2>/dev/null || echo "unknown")
if [ "$SYS_STATE" = "stopping" ]; then
    echo "  system stopping ($SYS_STATE), suppress"
    exit 0
fi

# --- Backoff state ---

STATE_DIR=/var/lib/shingo
STATE_FILE="$STATE_DIR/alert-state-$UNIT"
mkdir -p "$STATE_DIR" 2>/dev/null || true

now=$(date +%s)
last_alert_ts=0
backoff_level=0
if [ -f "$STATE_FILE" ]; then
    # shellcheck disable=SC1090
    . "$STATE_FILE"
fi

RESET_THRESHOLD=$((30 * 60))
since=$((now - last_alert_ts))
if [ "$since" -ge "$RESET_THRESHOLD" ]; then
    backoff_level=0
fi

intervals=(0 300 900 1800 3600)
idx=$backoff_level
if [ "$idx" -ge "${#intervals[@]}" ]; then idx=$((${#intervals[@]} - 1)); fi
interval=${intervals[$idx]}

if [ "$since" -lt "$interval" ]; then
    echo "  suppressed by backoff (level=$backoff_level, since=${since}s, interval=${interval}s)"
    exit 0
fi

# --- Message ---

# NOTE: the manual-stop alert below fires on every `systemctl restart`
# too (restart = stop + start, and the stop leg has RESULT=success). That
# means routine operator restarts will ping the channel. Kept for now
# because it's useful for testing — a `systemctl restart` is the easiest
# way to confirm the wiring works without crashing the service. Once the
# alert path is trusted in production, consider removing this branch and
# only alerting on RESULT != "success".
if [ "$RESULT" = "success" ]; then
    title="STOPPED (manual): $UNIT on $PLANT_ID"
    msg="$UNIT was stopped cleanly on $PLANT_ID. No install in progress and the box is not shutting down — likely a manual systemctl stop or restart. If this wasn't intentional, investigate."
else
    title="CRASH (alert #$((backoff_level + 1))): $UNIT on $PLANT_ID"
    msg="$UNIT crashed on $PLANT_ID. result=$RESULT exit=$EXIT_CODE/$EXIT_STATUS. systemd will auto-restart."
fi

journal_tail=$(journalctl -u "$UNIT" -n 5 --no-pager 2>/dev/null | tail -n 5 | head -c 800)
if [ -n "$journal_tail" ]; then
    msg="$msg

Recent log:
$journal_tail"
fi

# --- Post ---

escape_json() {
    local s="$1"
    s="${s//\\/\\\\}"
    s="${s//\"/\\\"}"
    s="${s//$'\n'/\\n}"
    s="${s//$'\r'/}"
    s="${s//$'\t'/\\t}"
    printf '%s' "$s"
}
title_j=$(escape_json "$title")
msg_j=$(escape_json "$msg")

body="{\"type\":\"message\",\"attachments\":[{\"contentType\":\"application/vnd.microsoft.card.adaptive\",\"content\":{\"type\":\"AdaptiveCard\",\"version\":\"1.4\",\"body\":[{\"type\":\"TextBlock\",\"size\":\"Medium\",\"weight\":\"Bolder\",\"text\":\"$title_j\",\"wrap\":true},{\"type\":\"TextBlock\",\"text\":\"$msg_j\",\"wrap\":true}]}}]}"

if curl -fsS -X POST -H 'Content-Type: application/json' --max-time 10 -d "$body" "$TEAMS_WEBHOOK_URL" >/dev/null 2>&1; then
    echo "  alert posted (level=$backoff_level -> $((backoff_level + 1)))"
else
    echo "  WARN: webhook post failed"
fi

# --- Persist state ---
{
    echo "last_alert_ts=$now"
    echo "backoff_level=$((backoff_level + 1))"
} > "$STATE_FILE" 2>/dev/null || true

exit 0
