#!/usr/bin/env bash
set -euo pipefail

# install-core.sh — interactive installer for ShinGo Core under systemd.
#
# Idempotent: safe to re-run after a partial install.
# Interactive: prompts before destructive steps (stop running process,
# replace systemd unit).
#
# Core does NOT have a DB migration step — PostgreSQL is its own
# systemd-managed service and its data is untouched by this script.
#
# Detects three branches:
#   FRESH     — no prior install detected; placeholder config written
#   MIGRATION — existing core process running from /home/pi/shingo
#   REINSTALL — already on FHS layout; rebuild + restart only

if [[ $EUID -ne 0 ]]; then
    echo "Must run as sudo/root"
    exit 1
fi

cd "$(dirname "$0")"
REPO_ROOT="$(pwd)"

# Ensure the dedicated 'shingo' system user exists. The service always runs
# as this user — same on every Pi, every Proxmox, every plant.
if ! id -u shingo >/dev/null 2>&1; then
    echo "==> Creating 'shingo' system user..."
    useradd --system --no-create-home --shell /usr/sbin/nologin shingo
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
    read -r -p "$prompt [y/N] " ans
    case "$ans" in
        y|Y|yes|YES) return 0 ;;
        *) return 1 ;;
    esac
}

echo "==> Pulling latest changes..."
git pull

# Discovery
echo "==> Detecting current state..."

CORE_PID=""
if ps -ef | grep -E "(shingocore|go run.*shingocore)" | grep -v grep > /dev/null; then
    CORE_PID=$(ps -ef | grep -E "(shingocore|go run.*shingocore)" | grep -v grep | awk '{print $2}' | head -1)
    echo "    core process running (pid=$CORE_PID)"
else
    echo "    no core process running"
fi

OLD_YAML=""
for cand in \
    /home/pi/shingo/shingo-core/shingocore.yaml \
    /home/pi/shingo/shingocore.yaml \
    /home/pi/shingocore.yaml; do
    if [ -f "$cand" ]; then
        OLD_YAML="$cand"
        echo "    legacy config found: $OLD_YAML"
        break
    fi
done

FHS_BINARY_EXISTS=no
if [ -f /opt/shingo/shingocore ]; then
    FHS_BINARY_EXISTS=yes
    echo "    FHS binary already installed: /opt/shingo/shingocore"
fi

UNIT_EXISTS=no
if [ -f /etc/systemd/system/shingo-core.service ]; then
    UNIT_EXISTS=yes
    echo "    systemd unit already installed: /etc/systemd/system/shingo-core.service"
fi

# Decide branch
if [ "$FHS_BINARY_EXISTS" = "yes" ] || [ "$UNIT_EXISTS" = "yes" ]; then
    MODE=REINSTALL
elif [ -n "$CORE_PID" ] || [ -n "$OLD_YAML" ]; then
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
        echo "    Existing core install detected. Binary will be moved to FHS"
        echo "    layout; the existing config will be copied to /etc/shingo/."
        ;;
    REINSTALL)
        echo "    Core already on FHS layout. Binary will be rebuilt and"
        echo "    service restarted. Config left in place."
        ;;
esac
echo ""

confirm "Proceed?" || { echo "Aborted."; exit 0; }

# Backup
BACKUP_TS=$(date +%Y%m%d-%H%M%S)
BACKUP_PATH="/tmp/shingo-pre-install-${BACKUP_TS}.tar.gz"
echo "==> Creating backup at ${BACKUP_PATH}"

BACKUP_FILES=()
[ -f "$OLD_YAML" ]                          && BACKUP_FILES+=("$OLD_YAML")
[ -f /etc/shingo/shingocore.yaml ]          && BACKUP_FILES+=("/etc/shingo/shingocore.yaml")

if [ ${#BACKUP_FILES[@]} -gt 0 ]; then
    tar czf "$BACKUP_PATH" "${BACKUP_FILES[@]}" 2>/dev/null || true
    echo "    backup contains: ${BACKUP_FILES[*]}"
else
    echo "    nothing to back up (fresh install)"
fi

# Build
echo "==> Building binary..."
(cd "$REPO_ROOT/shingo-core" && "$GO_BIN" build -o /tmp/shingocore ./cmd/shingocore)
echo "==> Build succeeded"

# FHS directories
echo "==> Ensuring FHS directories exist..."
mkdir -p /opt/shingo /etc/shingo
chown shingo:shingo /opt/shingo
chmod 755 /opt/shingo /etc/shingo

# Stop existing core
if [ -n "$CORE_PID" ]; then
    if confirm "Stop running core (pid=$CORE_PID)?"; then
        echo "==> Sending SIGTERM to pid=$CORE_PID..."
        kill "$CORE_PID" || true
        for i in $(seq 1 10); do
            if ! kill -0 "$CORE_PID" 2>/dev/null; then
                break
            fi
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

# Also stop the systemd unit if it's currently running (REINSTALL case)
if [ "$UNIT_EXISTS" = "yes" ]; then
    if systemctl is-active --quiet shingo-core; then
        if confirm "Stop running shingo-core.service?"; then
            systemctl stop shingo-core
            echo "    service stopped"
        else
            echo "Aborted."
            exit 0
        fi
    fi
fi

# Install binary
echo "==> Installing binary to /opt/shingo/shingocore..."
mv /tmp/shingocore /opt/shingo/shingocore
chown shingo:shingo /opt/shingo/shingocore
chmod 755 /opt/shingo/shingocore

# Config
if [ ! -f /etc/shingo/shingocore.yaml ]; then
    if [ "$MODE" = "MIGRATION" ] && [ -n "$OLD_YAML" ]; then
        echo "==> Copying config from $OLD_YAML to /etc/shingo/shingocore.yaml..."
        cp "$OLD_YAML" /etc/shingo/shingocore.yaml
    else
        echo "==> Writing placeholder /etc/shingo/shingocore.yaml..."
        cat > /etc/shingo/shingocore.yaml <<'YAML'
# ShinGo Core configuration.
#
# REQUIRED: configure PostgreSQL connection before starting the service.
# The exact field names depend on your build; consult shingocore/config
# for the current YAML schema. Example:
#
# postgres:
#   host:     localhost
#   port:     5432
#   user:     shingo
#   password: <fill in>
#   database: shingo
YAML
    fi
    chown shingo:shingo /etc/shingo/shingocore.yaml
    chmod 644 /etc/shingo/shingocore.yaml
else
    echo "==> /etc/shingo/shingocore.yaml already exists; leaving in place"
fi

# Install systemd unit
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

# Start service
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

# Enable on boot
echo "==> Enabling shingo-core on boot..."
systemctl enable shingo-core

# Done
echo ""
echo "============================================================"
echo " ShinGo Core install complete"
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
