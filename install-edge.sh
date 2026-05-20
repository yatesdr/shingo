#!/usr/bin/env bash
set -euo pipefail

# install-edge.sh - interactive installer for ShinGo Edge under systemd.
#
# Idempotent: safe to re-run after a partial install.
# Interactive: prompts before destructive steps (stop running process,
# DB migration, replace systemd unit).
#
# Detects three branches:
#   FRESH     - no prior install detected; placeholder config written
#   MIGRATION - existing edge running from /home/pi/shingo with old DB
#   REINSTALL - already on FHS layout; rebuild + restart only

if [[ $EUID -ne 0 ]]; then
    echo "Must run as sudo/root"
    exit 1
fi

cd "$(dirname "$0")"
REPO_ROOT="$(pwd)"

# Ensure the dedicated 'shingo' system user exists. The service always runs
# as this user - same on every Pi, every Proxmox, every plant.
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

EDGE_PID=""
if ps -ef | grep -E "(shingoedge|go run.*shingoedge)" | grep -v grep > /dev/null; then
    EDGE_PID=$(ps -ef | grep -E "(shingoedge|go run.*shingoedge)" | grep -v grep | awk '{print $2}' | head -1)
    echo "    edge process running (pid=$EDGE_PID)"
else
    echo "    no edge process running"
fi

OLD_DB=""
if [ -f /home/pi/shingo/shingo-edge/shingoedge.db ]; then
    OLD_DB=/home/pi/shingo/shingo-edge/shingoedge.db
    echo "    legacy DB found: $OLD_DB"
fi

OLD_YAML=""
if [ -f /home/pi/shingo/shingo-edge/shingoedge.yaml ]; then
    OLD_YAML=/home/pi/shingo/shingo-edge/shingoedge.yaml
    echo "    legacy config found: $OLD_YAML"
fi

FHS_BINARY_EXISTS=no
if [ -f /opt/shingo/shingoedge ]; then
    FHS_BINARY_EXISTS=yes
    echo "    FHS binary already installed: /opt/shingo/shingoedge"
fi

FHS_DB_EXISTS=no
if [ -f /var/lib/shingo-edge/shingoedge.db ]; then
    FHS_DB_EXISTS=yes
    echo "    FHS DB already installed: /var/lib/shingo-edge/shingoedge.db"
fi

UNIT_EXISTS=no
if [ -f /etc/systemd/system/shingo-edge.service ]; then
    UNIT_EXISTS=yes
    echo "    systemd unit already installed: /etc/systemd/system/shingo-edge.service"
fi

# Decide branch
if [ "$FHS_DB_EXISTS" = "yes" ] || [ "$UNIT_EXISTS" = "yes" ]; then
    MODE=REINSTALL
elif [ -n "$OLD_DB" ]; then
    MODE=MIGRATION
else
    MODE=FRESH
fi

echo ""
echo "==> Install mode: $MODE"
case "$MODE" in
    FRESH)
        echo "    No prior shingo-edge install detected. A placeholder config"
        echo "    will be written; configure connections via the web UI after boot."
        ;;
    MIGRATION)
        echo "    Existing edge install at /home/pi/shingo detected."
        echo "    The legacy DB will be migrated to /var/lib/shingo-edge/."
        ;;
    REINSTALL)
        echo "    Edge already on FHS layout. Binary will be rebuilt and"
        echo "    service restarted. DB and config left in place."
        ;;
esac
echo ""

confirm "Proceed?" || { echo "Aborted."; exit 0; }

# Backup
BACKUP_TS=$(date +%Y%m%d-%H%M%S)
BACKUP_PATH="/tmp/shingo-pre-install-${BACKUP_TS}.tar.gz"
echo "==> Creating backup at ${BACKUP_PATH}"

BACKUP_FILES=()
[ -f "$OLD_DB" ]                                      && BACKUP_FILES+=("$OLD_DB")
[ -f "${OLD_DB}-wal" ]                                && BACKUP_FILES+=("${OLD_DB}-wal")
[ -f "${OLD_DB}-shm" ]                                && BACKUP_FILES+=("${OLD_DB}-shm")
[ -f "$OLD_YAML" ]                                    && BACKUP_FILES+=("$OLD_YAML")
[ -f /var/lib/shingo-edge/shingoedge.db ]             && BACKUP_FILES+=("/var/lib/shingo-edge/shingoedge.db")
[ -f /var/lib/shingo-edge/shingoedge.db-wal ]         && BACKUP_FILES+=("/var/lib/shingo-edge/shingoedge.db-wal")
[ -f /var/lib/shingo-edge/shingoedge.db-shm ]         && BACKUP_FILES+=("/var/lib/shingo-edge/shingoedge.db-shm")
[ -f /etc/shingo/shingoedge.yaml ]                    && BACKUP_FILES+=("/etc/shingo/shingoedge.yaml")
[ -f /home/pi/shingo/start-edge.sh ]                  && BACKUP_FILES+=("/home/pi/shingo/start-edge.sh")

if [ ${#BACKUP_FILES[@]} -gt 0 ]; then
    tar czf "$BACKUP_PATH" "${BACKUP_FILES[@]}" 2>/dev/null || true
    echo "    backup contains: ${BACKUP_FILES[*]}"
else
    echo "    nothing to back up (fresh install)"
fi

# Build
echo "==> Building binary..."
(cd "$REPO_ROOT/shingo-edge" && "$GO_BIN" build -o /tmp/shingoedge ./cmd/shingoedge)
echo "==> Build succeeded"

# FHS directories
echo "==> Ensuring FHS directories exist..."
mkdir -p /opt/shingo /etc/shingo /var/lib/shingo-edge
chown shingo:shingo /opt/shingo /var/lib/shingo-edge
chmod 755 /opt/shingo /etc/shingo /var/lib/shingo-edge

# Stop existing edge
if [ -n "$EDGE_PID" ]; then
    if confirm "Stop running edge (pid=$EDGE_PID)?"; then
        echo "==> Sending SIGTERM to pid=$EDGE_PID..."
        kill "$EDGE_PID" || true
        for i in $(seq 1 10); do
            if ! kill -0 "$EDGE_PID" 2>/dev/null; then
                break
            fi
            sleep 1
        done
        if kill -0 "$EDGE_PID" 2>/dev/null; then
            echo "==> Process still alive after 10s - sending SIGKILL"
            kill -9 "$EDGE_PID" || true
            sleep 1
        fi
        echo "    edge stopped"
    else
        echo "Aborted; edge still running."
        exit 0
    fi
fi

# Also stop the systemd unit if it's currently running (REINSTALL case)
if [ "$UNIT_EXISTS" = "yes" ]; then
    if systemctl is-active --quiet shingo-edge; then
        if confirm "Stop running shingo-edge.service?"; then
            systemctl stop shingo-edge
            echo "    service stopped"
        else
            echo "Aborted."
            exit 0
        fi
    fi
fi

# DB migration
if [ "$MODE" = "MIGRATION" ]; then
    if confirm "Migrate DB from $OLD_DB to /var/lib/shingo-edge/?"; then
        echo "==> Running DB migration..."
        bash "$REPO_ROOT/shingo-edge/deploy/db-migration.sh"
        echo "==> DB migration complete"
    else
        echo "Aborted; install not completed."
        exit 0
    fi
fi

# Install binary
echo "==> Installing binary to /opt/shingo/shingoedge..."
mv /tmp/shingoedge /opt/shingo/shingoedge
chown shingo:shingo /opt/shingo/shingoedge
chmod 755 /opt/shingo/shingoedge

# Config
if [ ! -f /etc/shingo/shingoedge.yaml ]; then
    if [ "$MODE" = "MIGRATION" ] && [ -n "$OLD_YAML" ]; then
        echo "==> Copying config from $OLD_YAML to /etc/shingo/shingoedge.yaml..."
        cp "$OLD_YAML" /etc/shingo/shingoedge.yaml
        if grep -q '^database_path:' /etc/shingo/shingoedge.yaml; then
            sed -i 's|^database_path:.*|database_path: /var/lib/shingo-edge/shingoedge.db|' /etc/shingo/shingoedge.yaml
        else
            echo "database_path: /var/lib/shingo-edge/shingoedge.db" >> /etc/shingo/shingoedge.yaml
        fi
    else
        echo "==> Writing placeholder /etc/shingo/shingoedge.yaml..."
        cat > /etc/shingo/shingoedge.yaml <<'YAML'
# ShinGo Edge configuration. Configure other settings via the web UI
# after first boot (http://<host>:<port>/system-config).
database_path: /var/lib/shingo-edge/shingoedge.db
YAML
    fi
    chown shingo:shingo /etc/shingo/shingoedge.yaml
    chmod 644 /etc/shingo/shingoedge.yaml
else
    echo "==> /etc/shingo/shingoedge.yaml already exists; leaving in place"
fi

# Install systemd unit
NEED_UNIT_INSTALL=yes
if [ "$UNIT_EXISTS" = "yes" ]; then
    if cmp -s "$REPO_ROOT/shingo-edge/deploy/shingo-edge.service" /etc/systemd/system/shingo-edge.service; then
        echo "==> Existing shingo-edge.service is identical to repo copy; not replacing"
        NEED_UNIT_INSTALL=no
    else
        if confirm "Replace existing shingo-edge.service unit?"; then
            NEED_UNIT_INSTALL=yes
        else
            echo "Aborted; existing unit kept."
            NEED_UNIT_INSTALL=no
        fi
    fi
fi

if [ "$NEED_UNIT_INSTALL" = "yes" ]; then
    echo "==> Installing systemd unit..."
    cp "$REPO_ROOT/shingo-edge/deploy/shingo-edge.service" /etc/systemd/system/shingo-edge.service
    systemctl daemon-reload
fi

# Start service
echo "==> Starting shingo-edge..."
systemctl start shingo-edge

echo "==> Waiting up to 30s for service to become active..."
ACTIVE=no
for i in $(seq 1 30); do
    if systemctl is-active --quiet shingo-edge; then
        ACTIVE=yes
        break
    fi
    sleep 1
done

if [ "$ACTIVE" != "yes" ]; then
    echo "ERROR: shingo-edge did not become active within 30s."
    echo "Recent journal:"
    journalctl -u shingo-edge -n 50 --no-pager || true
    exit 1
fi

# Enable on boot
echo "==> Enabling shingo-edge on boot..."
systemctl enable shingo-edge

# Done
echo ""
echo "============================================================"
echo " ShinGo Edge install complete"
echo "============================================================"
echo " Service status: active"
echo " Binary:  /opt/shingo/shingoedge"
echo " Config:  /etc/shingo/shingoedge.yaml"
echo " DB:      /var/lib/shingo-edge/shingoedge.db"
echo " Backup:  $BACKUP_PATH"
echo " Logs:    sudo journalctl -u shingo-edge -f"
if [ "$MODE" = "FRESH" ]; then
    echo ""
    echo " Open http://<host-ip>:8081 to configure connections via the web UI."
fi
echo "============================================================"
