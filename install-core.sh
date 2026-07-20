#!/usr/bin/env bash
set -euo pipefail

# install-core.sh — interactive installer for shingo-core under systemd.
#
# Idempotent: safe to re-run after a partial install.
# Interactive: prompts before destructive steps (stop running process,
# replace systemd unit).
#
# shingo-core uses PostgreSQL; the DB lives wherever the operator
# pointed it (local postgres, remote host, etc.). This installer
# does not touch the DB — only the binary, config, and systemd unit.
#
# Discovers existing installs by inspecting the running process and
# scanning common install locations, then picks a branch:
#   FRESH     — no prior install detected; placeholder config written.
#               PostgreSQL connection MUST be filled in before the
#               service can do useful work.
#   MIGRATION — legacy install found outside /etc/shingo; config
#               copied to /etc/shingo/.
#   REINSTALL — already on FHS layout; rebuild + restart only.
#
# Flags:
#   --legacy-config <path>   Force MIGRATION using the given config file.
#                            Escape hatch when discovery can't find it.
#   --yes, -y                Non-interactive: assume "yes" to all confirm
#                            prompts. Intended for unattended fleet updates
#                            via Ansible / SSH fanout. Still refuses to
#                            overwrite an unknown setup (the safety check
#                            for "running process but no discoverable config"
#                            is an error exit, not a confirm).

LEGACY_CONFIG_ARG=""
ASSUME_YES=no
while [[ $# -gt 0 ]]; do
    case "$1" in
        --legacy-config)   LEGACY_CONFIG_ARG="$2"; shift 2 ;;
        --legacy-config=*) LEGACY_CONFIG_ARG="${1#*=}"; shift ;;
        --yes|-y)          ASSUME_YES=yes; shift ;;
        *)
            echo "Unknown argument: $1"
            echo "Usage: $0 [--legacy-config /path/to/shingocore.yaml] [--yes]"
            exit 1
            ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    echo "Must run as sudo/root"
    exit 1
fi

cd "$(dirname "$0")"
REPO_ROOT="$(pwd)"

if ! id -u shingo >/dev/null 2>&1; then
    echo "==> Creating 'shingo' system user..."
    useradd --system --no-create-home --shell /usr/sbin/nologin shingo
fi

# curl is required by alert-on-stop.sh (Teams crash-alert hook). A missing
# curl made the script silently log "WARN: webhook post failed" instead of
# anything actionable on Springfield 2026-05-21. Install it loudly so that
# never happens again. apt-get is the only package manager we support here;
# if the box runs something else (rpm-family, etc.), warn-and-continue —
# the service still works, only Teams alerts are degraded.
if ! command -v curl >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then
        echo "==> Installing curl (required by alert-on-stop.sh)..."
        apt-get update -qq && apt-get install -y curl
    else
        echo "WARNING: curl not installed and apt-get not available."
        echo "         Teams crash alerts won't work until curl is installed by hand."
    fi
fi

# Locate the Go toolchain. sudo's secure_path often hides /usr/local/go/bin,
# so we resolve the absolute path here and use it directly during the build.
GO_BIN=""
if command -v go >/dev/null 2>&1; then
    GO_BIN=$(command -v go)
else
    for cand in /usr/local/go/bin/go /opt/go/bin/go /usr/lib/go/bin/go /snap/bin/go; do
        if [ -x "$cand" ]; then GO_BIN="$cand"; break; fi
    done
fi
if [ -z "$GO_BIN" ]; then
    echo "ERROR: go toolchain not found on this box."
    echo "Hint: install Go from https://go.dev/dl/ - typically extracts to /usr/local/go."
    exit 1
fi
echo "==> Using go at: $GO_BIN"

confirm() {
    local prompt="$1"
    if [ "$ASSUME_YES" = "yes" ]; then
        echo "$prompt [auto-yes]"
        return 0
    fi
    read -r -p "$prompt [y/N] " ans
    case "$ans" in
        y|Y|yes|YES) return 0 ;;
        *) return 1 ;;
    esac
}

# Parse `--config <path>` or `--config=<path>` out of a cmdline string.
parse_config_flag() {
    local cmdline="$1"
    local prev="" val=""
    for tok in $cmdline; do
        case "$tok" in
            --config=*) val="${tok#--config=}"; break ;;
            --config)   prev="--config" ;;
            *)
                if [ "$prev" = "--config" ]; then val="$tok"; break; fi
                prev=""
                ;;
        esac
    done
    echo "$val"
}

echo "==> Pulling latest changes..."
git pull

# ----------------------------------------------------------------------
# Discovery
# ----------------------------------------------------------------------
echo "==> Detecting current state..."

# 1. Running core process.
CORE_PID=""
CORE_CWD=""
CORE_CMDLINE=""
for pid in $(pgrep -f 'shingocore|go run.*shingocore' 2>/dev/null || true); do
    [ -r "/proc/$pid/cmdline" ] || continue
    CORE_PID="$pid"
    CORE_CWD=$(readlink "/proc/$pid/cwd" 2>/dev/null || echo "")
    CORE_CMDLINE=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || echo "")
    break
done

if [ -n "$CORE_PID" ]; then
    echo "    core process running (pid=$CORE_PID)"
    [ -n "$CORE_CWD" ]     && echo "      cwd:     $CORE_CWD"
    [ -n "$CORE_CMDLINE" ] && echo "      cmdline: $CORE_CMDLINE"
else
    echo "    no core process running"
fi

# 2. Locate the legacy config. Priority:
#    a. --legacy-config flag.
#    b. --config arg of the running process.
#    c. <cwd>/shingocore.yaml (the binary's default search path).
#    d. Bounded scan of /home/*/, /opt/*/, /srv/*/ for shingocore.yaml.
LEGACY_CONFIG=""

if [ -n "$LEGACY_CONFIG_ARG" ]; then
    if [ ! -f "$LEGACY_CONFIG_ARG" ]; then
        echo "ERROR: --legacy-config '$LEGACY_CONFIG_ARG' not found"
        exit 1
    fi
    LEGACY_CONFIG="$LEGACY_CONFIG_ARG"
    echo "    legacy config (--legacy-config): $LEGACY_CONFIG"
elif [ -n "$CORE_PID" ]; then
    flag_cfg=$(parse_config_flag "$CORE_CMDLINE")
    if [ -n "$flag_cfg" ] && [ -f "$flag_cfg" ]; then
        LEGACY_CONFIG="$flag_cfg"
        echo "    legacy config (--config flag):   $LEGACY_CONFIG"
    elif [ -n "$CORE_CWD" ] && [ -f "$CORE_CWD/shingocore.yaml" ]; then
        LEGACY_CONFIG="$CORE_CWD/shingocore.yaml"
        echo "    legacy config (cwd default):     $LEGACY_CONFIG"
    fi
fi

if [ -z "$LEGACY_CONFIG" ]; then
    mapfile -t scan_hits < <(find /home /opt /srv -maxdepth 4 -name 'shingocore.yaml' 2>/dev/null | grep -v '^/etc/shingo/' || true)
    if [ ${#scan_hits[@]} -eq 1 ]; then
        LEGACY_CONFIG="${scan_hits[0]}"
        echo "    legacy config (scan):            $LEGACY_CONFIG"
    elif [ ${#scan_hits[@]} -gt 1 ]; then
        echo "    multiple legacy configs found:"
        for i in "${!scan_hits[@]}"; do
            echo "      [$((i+1))] ${scan_hits[$i]}"
        done
        if [ "$ASSUME_YES" = "yes" ]; then
            echo "ERROR: multiple candidate legacy configs and --yes was passed."
            echo "       Refusing to guess. Re-run with --legacy-config <path>."
            exit 1
        fi
        echo "      [0]  none of these"
        read -r -p "    pick one [0]: " pick
        pick="${pick:-0}"
        if [[ "$pick" =~ ^[0-9]+$ ]] && [ "$pick" -ge 1 ] && [ "$pick" -le "${#scan_hits[@]}" ]; then
            LEGACY_CONFIG="${scan_hits[$((pick-1))]}"
            echo "    legacy config (picked):          $LEGACY_CONFIG"
        fi
    fi
fi

# 3. FHS layout state.
FHS_BINARY_EXISTS=no
[ -f /opt/shingo/shingocore ] && FHS_BINARY_EXISTS=yes && echo "    FHS binary installed:            /opt/shingo/shingocore"

FHS_CONFIG_EXISTS=no
[ -f /etc/shingo/shingocore.yaml ] && FHS_CONFIG_EXISTS=yes && echo "    FHS config installed:            /etc/shingo/shingocore.yaml"

UNIT_EXISTS=no
[ -f /etc/systemd/system/shingo-core.service ] && UNIT_EXISTS=yes && echo "    systemd unit installed:          /etc/systemd/system/shingo-core.service"

# ----------------------------------------------------------------------
# Decide branch — refuse to silently overwrite an unknown setup.
# ----------------------------------------------------------------------
if [ -n "$CORE_PID" ] && [ -z "$LEGACY_CONFIG" ] && [ "$FHS_CONFIG_EXISTS" = "no" ]; then
    cat >&2 <<EOF

ERROR: shingo-core is running (pid=$CORE_PID) but the installer could not
       locate its config. Refusing to overwrite an unknown setup with a
       placeholder (a placeholder would crashloop against default
       localhost:5432 postgres).

       Find the config the running process is using:
         sudo readlink /proc/$CORE_PID/cwd
         sudo tr '\\0' ' ' < /proc/$CORE_PID/cmdline; echo

       Then re-run with:
         sudo bash $0 --legacy-config /path/to/shingocore.yaml
EOF
    exit 1
fi

LEGACY_IS_FHS=no
[ -n "$LEGACY_CONFIG" ] && [ "$LEGACY_CONFIG" = "/etc/shingo/shingocore.yaml" ] && LEGACY_IS_FHS=yes

if { [ "$FHS_BINARY_EXISTS" = "yes" ] || [ "$UNIT_EXISTS" = "yes" ] || [ "$FHS_CONFIG_EXISTS" = "yes" ]; } \
   && { [ -z "$LEGACY_CONFIG" ] || [ "$LEGACY_IS_FHS" = "yes" ]; }; then
    MODE=REINSTALL
elif [ -n "$LEGACY_CONFIG" ] && [ "$LEGACY_IS_FHS" = "no" ]; then
    MODE=MIGRATION
else
    MODE=FRESH
fi

echo ""
echo "==> Install mode: $MODE"
case "$MODE" in
    FRESH)
        echo "    No prior shingo-core install detected. A placeholder config"
        echo "    will be written; PostgreSQL connection MUST be filled in"
        echo "    before the service can do useful work."
        ;;
    MIGRATION)
        echo "    Legacy install detected at:"
        echo "      config: $LEGACY_CONFIG"
        echo "    The config will be copied to /etc/shingo/ as-is (it already"
        echo "    contains the working PostgreSQL connection settings)."
        ;;
    REINSTALL)
        echo "    Core already on FHS layout. Binary will be rebuilt and"
        echo "    service restarted. Config left in place."
        ;;
esac
echo ""

confirm "Proceed?" || { echo "Aborted."; exit 0; }

# ----------------------------------------------------------------------
# Backup
# ----------------------------------------------------------------------
BACKUP_TS=$(date +%Y%m%d-%H%M%S)
BACKUP_PATH="/tmp/shingo-pre-install-${BACKUP_TS}.tar.gz"
echo "==> Creating backup at ${BACKUP_PATH}"

# Prune older backups before creating the new one — keep the most
# recent 3. systemd-tmpfiles also cleans /tmp at the OS level on a
# 10-day window, but on Pi SD cards the SQLite DB makes each tarball
# meaningful (tens to hundreds of MB) so we don't want to wait. ls -t
# orders by mtime newest-first; tail -n +4 skips the first 3 and
# emits everything older. xargs -r is a no-op when input is empty
# (fresh install, no prior backups).
ls -1t /tmp/shingo-pre-install-*.tar.gz 2>/dev/null | tail -n +4 | xargs -r rm -f || true

BACKUP_FILES=()
for f in "$LEGACY_CONFIG" /etc/shingo/shingocore.yaml; do
    [ -n "$f" ] && [ -f "$f" ] && BACKUP_FILES+=("$f")
done

if [ ${#BACKUP_FILES[@]} -gt 0 ]; then
    tar czf "$BACKUP_PATH" "${BACKUP_FILES[@]}" 2>/dev/null || true
    echo "    backup contains: ${BACKUP_FILES[*]}"
else
    echo "    nothing to back up (fresh install)"
fi

# ----------------------------------------------------------------------
# Build
# ----------------------------------------------------------------------
echo "==> Building binary..."
(cd "$REPO_ROOT/shingo-core" && "$GO_BIN" build -o /tmp/shingocore ./cmd/shingocore)
echo "==> Build succeeded"

# ----------------------------------------------------------------------
# FHS directories
# ----------------------------------------------------------------------
echo "==> Ensuring FHS directories exist..."
mkdir -p /opt/shingo /etc/shingo
chown shingo:shingo /opt/shingo
chmod 755 /opt/shingo /etc/shingo

# Crash-alert support: state dir + log file owned by the shingo user
# (alert-on-stop.sh runs as that user via ExecStopPost). The
# systemd-journal group membership lets the alert script include a
# `journalctl -u <unit>` tail in the message body.
mkdir -p /var/lib/shingo
chown shingo:shingo /var/lib/shingo
chmod 755 /var/lib/shingo

touch /var/log/shingo-alert.log
chown shingo:shingo /var/log/shingo-alert.log
chmod 644 /var/log/shingo-alert.log

if getent group systemd-journal >/dev/null 2>&1; then
    if ! id -nG shingo | grep -qw systemd-journal; then
        echo "==> Adding 'shingo' user to systemd-journal group (for alert journal tail)..."
        usermod -aG systemd-journal shingo
    fi
fi

# Deploy marker — alert-on-stop.sh checks this file and suppresses
# crash alerts while the install is restarting the service.
DEPLOY_MARKER=/run/shingo-deploy-in-progress
touch "$DEPLOY_MARKER"
trap 'rm -f "$DEPLOY_MARKER"' EXIT

# ----------------------------------------------------------------------
# Stop existing core
# ----------------------------------------------------------------------
# Stop through systemd FIRST when the unit owns the process.
#
# The unit is Restart=always/RestartSec=5s. A raw `kill` is an UNEXPECTED exit
# as far as systemd is concerned, so the restart policy fires ~5s later and
# relaunches /opt/shingo/shingocore — which at that point is still the OLD
# binary, because the swap happens further down this script. The relaunched
# old process then satisfies the `systemctl start` and `is-active` checks at
# the end, and the install reports success while the previous build runs.
#
# This is the Springfield 2026-07-20 failure. It hit the EDGE installer (which
# shares this logic) because edge is slow to exit and takes the SIGKILL path;
# core survived only because it exits promptly on SIGTERM, which left the unit
# still `is-active` so the stop below actually ran. That is luck, not design —
# a slow core exit reproduces it here. See
# INCIDENT-springfield-stale-binary-deploy.
#
# `systemctl stop` is an INTENTIONAL exit, so the restart policy does not fire.
if [ "$UNIT_EXISTS" = "yes" ]; then
    if systemctl is-active --quiet shingo-core || [ -n "$CORE_PID" ]; then
        if confirm "Stop shingo-core.service?"; then
            echo "==> Stopping shingo-core.service..."
            systemctl stop shingo-core || true
            for i in $(seq 1 45); do
                systemctl is-active --quiet shingo-core || break
                sleep 1
            done
            if systemctl is-active --quiet shingo-core; then
                echo "ERROR: shingo-core.service still active after 45s; aborting"
                echo "       before swapping the binary (a swap now would race the"
                echo "       restart policy and leave the old build running)."
                exit 1
            fi
            echo "    service stopped"
        else
            echo "Aborted; core still running."
            exit 0
        fi
    fi
fi

# Re-detect after the unit stop: anything still alive is NOT managed by the
# unit (a stray or legacy foreground launch), so systemd will not restart it
# and a raw kill is safe.
CORE_PID=""
for pid in $(pgrep -f 'shingocore|go run.*shingocore' 2>/dev/null || true); do
    [ -r "/proc/$pid/cmdline" ] || continue
    CORE_PID="$pid"
    break
done

if [ -n "$CORE_PID" ]; then
    if confirm "Stop stray (non-unit) core process pid=$CORE_PID?"; then
        echo "==> Sending SIGTERM to pid=$CORE_PID..."
        kill "$CORE_PID" || true
        for i in $(seq 1 10); do
            kill -0 "$CORE_PID" 2>/dev/null || break
            sleep 1
        done
        if kill -0 "$CORE_PID" 2>/dev/null; then
            echo "==> Process still alive after 10s - sending SIGKILL"
            kill -9 "$CORE_PID" || true
            sleep 1
        fi
        echo "    core stopped"
    else
        echo "Aborted; core still running."
        exit 0
    fi
fi

# ----------------------------------------------------------------------
# Install binary
# ----------------------------------------------------------------------

# One-slot rollback: save the live binary as shingocore.previous before
# overwriting. Operator rollback recipe:
#   sudo systemctl stop shingo-core
#   sudo mv /opt/shingo/shingocore.previous /opt/shingo/shingocore
#   sudo systemctl start shingo-core
# .previous always reflects the binary that was running just before this
# install — overwritten on every successful run, so a second install
# replaces the snapshot. For multi-version recovery use the
# timestamped /tmp/shingo-pre-install-*.tar.gz (config + DB only) plus
# git checkout + reinstall.
if [ -f /opt/shingo/shingocore ]; then
    if cp -p /opt/shingo/shingocore /opt/shingo/shingocore.previous; then
        echo "==> Saved previous binary to /opt/shingo/shingocore.previous"
    else
        echo "    WARNING: failed to snapshot previous binary; install will continue"
    fi
fi

echo "==> Installing binary to /opt/shingo/shingocore..."
mv /tmp/shingocore /opt/shingo/shingocore
chown shingo:shingo /opt/shingo/shingocore
chmod 755 /opt/shingo/shingocore

echo "==> Installing alert-on-stop.sh to /opt/shingo/..."
cp "$REPO_ROOT/scripts/alert-on-stop.sh" /opt/shingo/alert-on-stop.sh
chown shingo:shingo /opt/shingo/alert-on-stop.sh
chmod 755 /opt/shingo/alert-on-stop.sh

# ----------------------------------------------------------------------
# Config
# ----------------------------------------------------------------------
if [ ! -f /etc/shingo/shingocore.yaml ]; then
    if [ "$MODE" = "MIGRATION" ] && [ -n "$LEGACY_CONFIG" ]; then
        echo "==> Copying config from $LEGACY_CONFIG to /etc/shingo/shingocore.yaml..."
        cp "$LEGACY_CONFIG" /etc/shingo/shingocore.yaml
    else
        echo "==> Writing placeholder /etc/shingo/shingocore.yaml..."
        cat > /etc/shingo/shingocore.yaml <<'YAML'
# shingo-core configuration.
#
# REQUIRED: configure the PostgreSQL connection before starting the service.
# The exact field names depend on your build; consult shingo-core/config
# for the current YAML schema. Example:
#
# database:
#   postgres:
#     host:     localhost
#     port:     5432
#     user:     shingocore
#     password: <fill in>
#     database: shingocore
#     sslmode:  disable
YAML
    fi
    chown shingo:shingo /etc/shingo/shingocore.yaml
    chmod 644 /etc/shingo/shingocore.yaml
else
    echo "==> /etc/shingo/shingocore.yaml already exists; leaving in place"
fi

# ----------------------------------------------------------------------
# Crash-alert config (/etc/shingo/alert.env)
# Prompt the operator for the Teams webhook URL the first time alerts
# are configured on this box. Re-runs leave the file alone. --yes drops
# the template (alerts stay disabled until the operator edits the file).
# ----------------------------------------------------------------------
if [ ! -f /etc/shingo/alert.env ]; then
    cp "$REPO_ROOT/scripts/alert.env.template" /etc/shingo/alert.env
    if [ "$ASSUME_YES" = "yes" ]; then
        echo "==> Installing alert config template at /etc/shingo/alert.env"
        echo "    (TEAMS_WEBHOOK_URL empty; alerts disabled until you fill it in)"
    else
        echo "==> Configure crash alerts (Teams webhook)"
        echo "    Paste the Teams webhook URL, or press Enter to skip"
        echo "    (alerts disabled; configure later via /etc/shingo/alert.env)."
        read -r -p "    URL: " webhook_url
        plant_default=$(hostname -s)
        read -r -p "    Plant name [default: $plant_default]: " plant_id
        [ -z "$plant_id" ] && plant_id="$plant_default"
        # Delete-and-append avoids sed escape issues with URL special chars
        # (& is bash background-op when unquoted, \ trips sed's replacement
        # parser). The script writes the final values at the end; bash
        # sources top-to-bottom so the template's empty placeholders never
        # win even if not deleted, but we strip them for tidiness.
        sed -i '/^TEAMS_WEBHOOK_URL=/d; /^PLANT_ID=/d' /etc/shingo/alert.env
        if [ -n "$webhook_url" ]; then
            printf 'TEAMS_WEBHOOK_URL="%s"\n' "$webhook_url" >> /etc/shingo/alert.env
            echo "    alerts enabled"
        else
            printf 'TEAMS_WEBHOOK_URL=""\n' >> /etc/shingo/alert.env
            echo "    alerts left disabled"
        fi
        printf 'PLANT_ID="%s"\n' "$plant_id" >> /etc/shingo/alert.env
        echo "    plant identified as: $plant_id"
    fi
    chown root:shingo /etc/shingo/alert.env
    chmod 640 /etc/shingo/alert.env
else
    echo "==> /etc/shingo/alert.env already exists; leaving in place"
fi

# ----------------------------------------------------------------------
# Install systemd unit
# ----------------------------------------------------------------------
NEED_UNIT_INSTALL=yes
if [ "$UNIT_EXISTS" = "yes" ]; then
    if cmp -s "$REPO_ROOT/shingo-core/deploy/shingo-core.service" /etc/systemd/system/shingo-core.service; then
        echo "==> Existing shingo-core.service is identical to repo copy; not replacing"
        NEED_UNIT_INSTALL=no
    else
        if confirm "Replace existing shingo-core.service unit?"; then
            NEED_UNIT_INSTALL=yes
        else
            echo "Aborted; existing unit kept."
            NEED_UNIT_INSTALL=no
        fi
    fi
fi

if [ "$NEED_UNIT_INSTALL" = "yes" ]; then
    echo "==> Installing systemd unit..."
    cp "$REPO_ROOT/shingo-core/deploy/shingo-core.service" /etc/systemd/system/shingo-core.service
    systemctl daemon-reload
fi

# ----------------------------------------------------------------------
# Start service
# ----------------------------------------------------------------------
echo "==> Starting shingo-core..."
systemctl start shingo-core

echo "==> Waiting up to 30s for service to become active..."
ACTIVE=no
for i in $(seq 1 30); do
    if systemctl is-active --quiet shingo-core; then
        ACTIVE=yes
        break
    fi
    sleep 1
done

if [ "$ACTIVE" != "yes" ]; then
    echo "ERROR: shingo-core did not become active within 30s."
    echo "Recent journal:"
    journalctl -u shingo-core -n 50 --no-pager || true
    exit 1
fi

# "active" is not the same as "running the binary we just installed". A process
# relaunched from the pre-swap inode is still active and reports healthy on
# every other signal while executing the previous build. Verify what is ACTUALLY
# executing via /proc/<pid>/exe: after the binary is replaced, a stale process's
# exe link resolves to the unlinked inode and Linux marks it "(deleted)".
echo "==> Verifying the running process is the binary we installed..."
RUN_PID=$(systemctl show shingo-core -p MainPID --value 2>/dev/null || echo "")
if [ -z "$RUN_PID" ] || [ "$RUN_PID" = "0" ]; then
    echo "ERROR: could not determine shingo-core MainPID; cannot verify the build."
    exit 1
fi
RUN_EXE=$(readlink "/proc/$RUN_PID/exe" 2>/dev/null || echo "")
case "$RUN_EXE" in
    *"(deleted)"*)
        echo "ERROR: shingo-core (pid=$RUN_PID) is running a DELETED binary:"
        echo "       $RUN_EXE"
        echo "       The service was relaunched before the binary swap — this is the"
        echo "       stale-binary deploy failure. Run: systemctl restart shingo-core"
        exit 1
        ;;
    /opt/shingo/shingocore) : ;;
    "")
        echo "ERROR: could not read /proc/$RUN_PID/exe; cannot verify the build."
        exit 1
        ;;
    *)
        echo "ERROR: shingo-core (pid=$RUN_PID) is running an unexpected binary:"
        echo "       $RUN_EXE (expected /opt/shingo/shingocore)"
        exit 1
        ;;
esac
# Same inode, not just the same path — catches a swap that happened between the
# start and this check.
if ! [ "/proc/$RUN_PID/exe" -ef /opt/shingo/shingocore ]; then
    echo "ERROR: shingo-core (pid=$RUN_PID) exe is not the installed binary"
    echo "       (path matches but inode differs — stale process)."
    echo "       Run: systemctl restart shingo-core"
    exit 1
fi
echo "    verified: pid=$RUN_PID running /opt/shingo/shingocore"

# Service is back up — clear the deploy marker so crash alerts resume.
rm -f "$DEPLOY_MARKER"
trap - EXIT

echo "==> Enabling shingo-core on boot..."
systemctl enable shingo-core

echo ""
echo "============================================================"
echo " shingo-core install complete"
echo "============================================================"
echo " Service status: active"
echo " Binary:  /opt/shingo/shingocore"
echo " Config:  /etc/shingo/shingocore.yaml"
echo " Backup:  $BACKUP_PATH"
echo " Logs:    sudo journalctl -u shingo-core -f"
if [ "$MODE" = "FRESH" ]; then
    echo ""
    echo " IMPORTANT: edit /etc/shingo/shingocore.yaml to set the"
    echo " PostgreSQL connection, then run: systemctl restart shingo-core"
fi
echo "============================================================"
