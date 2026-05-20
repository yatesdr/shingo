#!/usr/bin/env bash
set -euo pipefail

# install-edge.sh — interactive installer for shingo-edge under systemd.
#
# Idempotent: safe to re-run after a partial install.
# Interactive: prompts before destructive steps (stop running process,
# DB migration, replace systemd unit).
#
# Discovers existing installs by inspecting the running process and
# scanning common install locations, then picks a branch:
#   FRESH     — no prior install detected; placeholder config written.
#   MIGRATION — legacy install found outside /etc/shingo; config + DB
#               migrated to the FHS layout (/etc/shingo, /var/lib/shingo-edge).
#   REINSTALL — already on FHS layout; rebuild + restart only.
#
# Flags:
#   --legacy-config <path>   Force MIGRATION using the given config file.
#                            Escape hatch when discovery can't find it.

LEGACY_CONFIG_ARG=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --legacy-config)   LEGACY_CONFIG_ARG="$2"; shift 2 ;;
        --legacy-config=*) LEGACY_CONFIG_ARG="${1#*=}"; shift ;;
        *)
            echo "Unknown argument: $1"
            echo "Usage: $0 [--legacy-config /path/to/shingoedge.yaml]"
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

# Read the top-level `database_path:` scalar from a yaml. Quoted or unquoted.
read_db_path() {
    local yaml="$1"
    [ -f "$yaml" ] || { echo ""; return; }
    local raw
    raw=$(grep -E '^[[:space:]]*database_path[[:space:]]*:' "$yaml" | head -1 || true)
    [ -z "$raw" ] && { echo ""; return; }
    raw="${raw#*:}"
    raw="${raw%%#*}"
    echo "$raw" | sed -E "s/^[[:space:]]*//; s/[[:space:]]*\$//; s/^\"//; s/\"\$//; s/^'//; s/'\$//"
}

echo "==> Pulling latest changes..."
git pull

# ----------------------------------------------------------------------
# Discovery
# ----------------------------------------------------------------------
echo "==> Detecting current state..."

# 1. Running edge process.
EDGE_PID=""
EDGE_CWD=""
EDGE_CMDLINE=""
for pid in $(pgrep -f 'shingoedge|go run.*shingoedge' 2>/dev/null || true); do
    [ -r "/proc/$pid/cmdline" ] || continue
    EDGE_PID="$pid"
    EDGE_CWD=$(readlink "/proc/$pid/cwd" 2>/dev/null || echo "")
    EDGE_CMDLINE=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || echo "")
    break
done

if [ -n "$EDGE_PID" ]; then
    echo "    edge process running (pid=$EDGE_PID)"
    [ -n "$EDGE_CWD" ]     && echo "      cwd:     $EDGE_CWD"
    [ -n "$EDGE_CMDLINE" ] && echo "      cmdline: $EDGE_CMDLINE"
else
    echo "    no edge process running"
fi

# 2. Locate the legacy config. Priority:
#    a. --legacy-config flag.
#    b. --config arg of the running process.
#    c. <cwd>/shingoedge.yaml (the binary's default search path).
#    d. Bounded scan of /home/*/, /opt/*/, /srv/*/ for shingoedge.yaml.
LEGACY_CONFIG=""

if [ -n "$LEGACY_CONFIG_ARG" ]; then
    if [ ! -f "$LEGACY_CONFIG_ARG" ]; then
        echo "ERROR: --legacy-config '$LEGACY_CONFIG_ARG' not found"
        exit 1
    fi
    LEGACY_CONFIG="$LEGACY_CONFIG_ARG"
    echo "    legacy config (--legacy-config): $LEGACY_CONFIG"
elif [ -n "$EDGE_PID" ]; then
    flag_cfg=$(parse_config_flag "$EDGE_CMDLINE")
    if [ -n "$flag_cfg" ] && [ -f "$flag_cfg" ]; then
        LEGACY_CONFIG="$flag_cfg"
        echo "    legacy config (--config flag):   $LEGACY_CONFIG"
    elif [ -n "$EDGE_CWD" ] && [ -f "$EDGE_CWD/shingoedge.yaml" ]; then
        LEGACY_CONFIG="$EDGE_CWD/shingoedge.yaml"
        echo "    legacy config (cwd default):     $LEGACY_CONFIG"
    fi
fi

if [ -z "$LEGACY_CONFIG" ]; then
    mapfile -t scan_hits < <(find /home /opt /srv -maxdepth 4 -name 'shingoedge.yaml' 2>/dev/null | grep -v '^/etc/shingo/' || true)
    if [ ${#scan_hits[@]} -eq 1 ]; then
        LEGACY_CONFIG="${scan_hits[0]}"
        echo "    legacy config (scan):            $LEGACY_CONFIG"
    elif [ ${#scan_hits[@]} -gt 1 ]; then
        echo "    multiple legacy configs found:"
        for i in "${!scan_hits[@]}"; do
            echo "      [$((i+1))] ${scan_hits[$i]}"
        done
        echo "      [0]  none of these"
        read -r -p "    pick one [0]: " pick
        pick="${pick:-0}"
        if [[ "$pick" =~ ^[0-9]+$ ]] && [ "$pick" -ge 1 ] && [ "$pick" -le "${#scan_hits[@]}" ]; then
            LEGACY_CONFIG="${scan_hits[$((pick-1))]}"
            echo "    legacy config (picked):          $LEGACY_CONFIG"
        fi
    fi
fi

# 3. Resolve the legacy DB path from the config (database_path field,
#    falling back to the binary's default of <config_dir>/shingoedge.db).
LEGACY_DB=""
if [ -n "$LEGACY_CONFIG" ]; then
    db_field=$(read_db_path "$LEGACY_CONFIG")
    config_dir=$(dirname "$LEGACY_CONFIG")
    if [ -n "$db_field" ]; then
        case "$db_field" in
            /*) LEGACY_DB="$db_field" ;;
            *)  LEGACY_DB="$config_dir/$db_field" ;;
        esac
    else
        LEGACY_DB="$config_dir/shingoedge.db"
    fi
    if [ -f "$LEGACY_DB" ]; then
        echo "    legacy DB:                       $LEGACY_DB"
    else
        echo "    legacy DB (not on disk):         $LEGACY_DB"
        LEGACY_DB=""
    fi
fi

# 4. FHS layout state.
FHS_BINARY_EXISTS=no
[ -f /opt/shingo/shingoedge ] && FHS_BINARY_EXISTS=yes && echo "    FHS binary installed:            /opt/shingo/shingoedge"

FHS_CONFIG_EXISTS=no
[ -f /etc/shingo/shingoedge.yaml ] && FHS_CONFIG_EXISTS=yes && echo "    FHS config installed:            /etc/shingo/shingoedge.yaml"

FHS_DB_EXISTS=no
[ -f /var/lib/shingo-edge/shingoedge.db ] && FHS_DB_EXISTS=yes && echo "    FHS DB installed:                /var/lib/shingo-edge/shingoedge.db"

UNIT_EXISTS=no
[ -f /etc/systemd/system/shingo-edge.service ] && UNIT_EXISTS=yes && echo "    systemd unit installed:          /etc/systemd/system/shingo-edge.service"

# ----------------------------------------------------------------------
# Decide branch — and refuse to silently overwrite an unknown setup.
# ----------------------------------------------------------------------
if [ -n "$EDGE_PID" ] && [ -z "$LEGACY_CONFIG" ] && [ "$FHS_CONFIG_EXISTS" = "no" ]; then
    cat >&2 <<EOF

ERROR: shingo-edge is running (pid=$EDGE_PID) but the installer could not
       locate its config. Refusing to overwrite an unknown setup with a
       placeholder.

       Find the config the running process is using:
         sudo readlink /proc/$EDGE_PID/cwd
         sudo tr '\\0' ' ' < /proc/$EDGE_PID/cmdline; echo

       Then re-run with:
         sudo bash $0 --legacy-config /path/to/shingoedge.yaml
EOF
    exit 1
fi

LEGACY_IS_FHS=no
[ -n "$LEGACY_CONFIG" ] && [ "$LEGACY_CONFIG" = "/etc/shingo/shingoedge.yaml" ] && LEGACY_IS_FHS=yes

if { [ "$FHS_DB_EXISTS" = "yes" ] || [ "$UNIT_EXISTS" = "yes" ] || [ "$FHS_CONFIG_EXISTS" = "yes" ]; } \
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
        echo "    No prior shingo-edge install detected. A placeholder config"
        echo "    will be written; configure connections via the web UI after boot."
        ;;
    MIGRATION)
        echo "    Legacy install detected at:"
        echo "      config: $LEGACY_CONFIG"
        [ -n "$LEGACY_DB" ] && echo "      DB:     $LEGACY_DB"
        echo "    The DB will be copied to /var/lib/shingo-edge/ and the"
        echo "    config copied to /etc/shingo/ with the DB path rewritten."
        ;;
    REINSTALL)
        echo "    Edge already on FHS layout. Binary will be rebuilt and"
        echo "    service restarted. DB and config left in place."
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

BACKUP_FILES=()
for f in "$LEGACY_DB" "${LEGACY_DB}-wal" "${LEGACY_DB}-shm" "$LEGACY_CONFIG" \
         /var/lib/shingo-edge/shingoedge.db /var/lib/shingo-edge/shingoedge.db-wal \
         /var/lib/shingo-edge/shingoedge.db-shm /etc/shingo/shingoedge.yaml; do
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
(cd "$REPO_ROOT/shingo-edge" && "$GO_BIN" build -o /tmp/shingoedge ./cmd/shingoedge)
echo "==> Build succeeded"

# ----------------------------------------------------------------------
# FHS directories
# ----------------------------------------------------------------------
echo "==> Ensuring FHS directories exist..."
mkdir -p /opt/shingo /etc/shingo /var/lib/shingo-edge
chown shingo:shingo /opt/shingo /var/lib/shingo-edge
chmod 755 /opt/shingo /etc/shingo /var/lib/shingo-edge

# ----------------------------------------------------------------------
# Stop existing edge
# ----------------------------------------------------------------------
if [ -n "$EDGE_PID" ]; then
    if confirm "Stop running edge (pid=$EDGE_PID)?"; then
        echo "==> Sending SIGTERM to pid=$EDGE_PID..."
        kill "$EDGE_PID" || true
        for i in $(seq 1 10); do
            kill -0 "$EDGE_PID" 2>/dev/null || break
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

# ----------------------------------------------------------------------
# DB migration
# ----------------------------------------------------------------------
if [ "$MODE" = "MIGRATION" ] && [ -n "$LEGACY_DB" ]; then
    if confirm "Migrate DB from $LEGACY_DB to /var/lib/shingo-edge/?"; then
        echo "==> Running DB migration..."
        bash "$REPO_ROOT/shingo-edge/deploy/db-migration.sh" "$LEGACY_DB"
        echo "==> DB migration complete"
    else
        echo "Aborted; install not completed."
        exit 0
    fi
elif [ "$MODE" = "MIGRATION" ] && [ -z "$LEGACY_DB" ]; then
    echo "==> No legacy DB file on disk; skipping DB migration (config-only migration)"
fi

# ----------------------------------------------------------------------
# Install binary
# ----------------------------------------------------------------------
echo "==> Installing binary to /opt/shingo/shingoedge..."
mv /tmp/shingoedge /opt/shingo/shingoedge
chown shingo:shingo /opt/shingo/shingoedge
chmod 755 /opt/shingo/shingoedge

# ----------------------------------------------------------------------
# Config
# ----------------------------------------------------------------------
if [ ! -f /etc/shingo/shingoedge.yaml ]; then
    if [ "$MODE" = "MIGRATION" ] && [ -n "$LEGACY_CONFIG" ]; then
        echo "==> Copying config from $LEGACY_CONFIG to /etc/shingo/shingoedge.yaml..."
        cp "$LEGACY_CONFIG" /etc/shingo/shingoedge.yaml
        if grep -qE '^[[:space:]]*database_path[[:space:]]*:' /etc/shingo/shingoedge.yaml; then
            sed -i -E 's|^[[:space:]]*database_path[[:space:]]*:.*|database_path: /var/lib/shingo-edge/shingoedge.db|' /etc/shingo/shingoedge.yaml
        else
            echo "database_path: /var/lib/shingo-edge/shingoedge.db" >> /etc/shingo/shingoedge.yaml
        fi
    else
        echo "==> Writing placeholder /etc/shingo/shingoedge.yaml..."
        cat > /etc/shingo/shingoedge.yaml <<'YAML'
# shingo-edge configuration. Configure other settings via the web UI
# after first boot (http://<host>:<port>/system-config).
database_path: /var/lib/shingo-edge/shingoedge.db
YAML
    fi
    chown shingo:shingo /etc/shingo/shingoedge.yaml
    chmod 644 /etc/shingo/shingoedge.yaml
else
    echo "==> /etc/shingo/shingoedge.yaml already exists; leaving in place"
fi

# ----------------------------------------------------------------------
# Install systemd unit
# ----------------------------------------------------------------------
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

# ----------------------------------------------------------------------
# Start service
# ----------------------------------------------------------------------
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

echo "==> Enabling shingo-edge on boot..."
systemctl enable shingo-edge

echo ""
echo "============================================================"
echo " shingo-edge install complete"
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
