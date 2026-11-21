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
#   (neither of the next)    Install the newest release: fetch origin, check
#                            its tag out (a detached HEAD), build it, and stamp
#                            the binary's version with the tag.
#   --tag <vX.Y.Z>, vX.Y.Z   Install that release instead: to pin a plant to
#                            one, or roll it back.
#   --main                   Build origin's main instead: a change merged and
#                            not yet released. Either way the checkout is set
#                            to it first, whatever it was on
#                            (deploy/sync-checkout.sh: local edits and commits
#                            are saved, then discarded), and the Deploy: line
#                            of every release it brings or takes off is shown
#                            before the "Proceed?".

# Remember the invocation before the arg loop consumes it and before the `cd`
# below, so the re-exec after `git pull` is faithful.
#
# ORIG_ARGS especially: the loop shifts, so "$@" is EMPTY by the time the pull
# runs. A re-exec using "$@" there would silently drop --yes and turn an
# unattended fleet update into a confirm prompt that hangs forever.
SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
ORIG_ARGS=("$@")

LEGACY_CONFIG_ARG=""
ASSUME_YES=no
# Seed timezone for a FRESH install's placeholder yaml — same contract as
# install-edge.sh: OS zone as the suggested default, --timezone to override
# for unattended installs that know the plant clock disagrees with the box.
CORE_TIMEZONE=$(timedatectl show -p Timezone --value 2>/dev/null || echo "America/Chicago")
# Distinguishes a zone somebody TYPED from one that merely defaulted to the OS
# zone above. --yes must refuse to guess, but it must not refuse an explicit
# instruction: without this there is no way to set the zone in an unattended
# install, which is the plant-rollout case.
CORE_TIMEZONE_EXPLICIT=no
# What to build: INSTALL_TAG, or main when INSTALL_MAIN; neither = the newest
# release, which the checkout step names into INSTALL_TAG.
INSTALL_TAG=""
INSTALL_MAIN=no
while [[ $# -gt 0 ]]; do
    case "$1" in
        --legacy-config)   LEGACY_CONFIG_ARG="$2"; shift 2 ;;
        --legacy-config=*) LEGACY_CONFIG_ARG="${1#*=}"; shift ;;
        --yes|-y)          ASSUME_YES=yes; shift ;;
        --timezone)        CORE_TIMEZONE="$2"; CORE_TIMEZONE_EXPLICIT=yes; shift 2 ;;
        --timezone=*)      CORE_TIMEZONE="${1#*=}"; CORE_TIMEZONE_EXPLICIT=yes; shift ;;
        --tag)             INSTALL_TAG="$2"; shift 2 ;;
        --tag=*)           INSTALL_TAG="${1#*=}"; shift ;;
        v[0-9]*)           INSTALL_TAG="$1"; shift ;;
        --main)            INSTALL_MAIN=yes; shift ;;
        *)
            echo "Unknown argument: $1"
            echo "Usage: $0 [--legacy-config /path/to/shingocore.yaml] [--yes] [--timezone IANA_ZONE] [--tag vX.Y.Z | vX.Y.Z | --main]"
            echo "With no --tag or --main it installs the newest release (one is cut on each merge to main)."
            exit 1
            ;;
    esac
done
if [ -n "$INSTALL_TAG" ] && ! [[ "$INSTALL_TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "ERROR: '$INSTALL_TAG' is not a release tag (vMAJOR.MINOR.PATCH, e.g. v0.1.0)."
    exit 1
fi
if [ -n "$INSTALL_TAG" ] && [ "$INSTALL_MAIN" = "yes" ]; then
    echo "ERROR: --tag and --main name different builds; give one."
    exit 1
fi

if [[ $EUID -ne 0 ]]; then
    echo "Must run as sudo/root"
    exit 1
fi

cd "$(dirname "$0")"
REPO_ROOT="$(pwd)"

# The checkout sync both installers share: the checkout is made to match origin,
# local edits and commits saved first (deploy/sync-checkout.sh).
# shellcheck source=deploy/sync-checkout.sh
. "$REPO_ROOT/deploy/sync-checkout.sh"

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

# Parse `--config <path>` out of a systemd unit's ExecStart.
#
# THE DURABLE SIGNAL, AND THE ONE DISCOVERY WAS MISSING. The running process's
# cmdline is the best evidence of which config is live -- but it exists only
# while the service is UP, and stopping the service is the most natural thing an
# operator does before reinstalling. That is precisely when discovery fell
# through to scanning the filesystem.
#
# Measured at Hopkinsville 2026-08-07: with the service stopped, the scan found
# three stray shingocore.yaml files -- a copy inside the checkout, a `shingo-test`
# tree pointing at localhost, and a pre-FHS relic under /opt -- so `--yes`
# correctly refused to guess and the install aborted with the service already
# down. Springfield had survived the same code path only because it happened to
# carry exactly one stray file.
#
# The unit says what the operator DECLARED. It is present whether the process is
# or not, it survives a stop, and it is what the service will actually run with.
# `systemctl show` is preferred over reading the file so drop-in overrides are
# honoured; the file is the fallback for a box where systemctl cannot answer.
#
# This yields the FHS path on an already-migrated box, which is not "legacy" at
# all -- and that is fine: LEGACY_IS_FHS below turns it into REINSTALL, so the
# config is left in place and nothing is copied over itself. It is the same
# value the running-process branch already produces on a live migrated box. A
# unit still pointing at a pre-FHS path correctly selects MIGRATION instead.
config_from_unit() {
    local unit="$1" line=""
    line=$(systemctl show "$unit" -p ExecStart --value 2>/dev/null || true)
    if [ -z "$line" ]; then
        line=$(grep -h '^ExecStart=' "/etc/systemd/system/$unit" 2>/dev/null | tail -1 || true)
    fi
    [ -z "$line" ] && return 0
    parse_config_flag "$line"
}

# Set the checkout, then re-exec if this script itself changed.
#
# Without this the installer pulls a new version of ITSELF and then finishes
# the run as the OLD one. git replaces a tracked file by writing a temp and
# renaming over it -- measured, a fresh inode every pull -- so the running bash
# keeps reading the original content through its open fd. That is safe (there
# is no torn read or byte-offset hazard here; renaming is what makes it safe),
# but it means every fix to this script lands one run late. The config
# discovery fix above is exactly that case: pull it at a plant, and the run
# that pulled it still discovers the old way.
#
# So: hash this file before and after. Unchanged, carry on in-process.
# Changed, exec a fresh bash on the new file so the run uses the logic it just
# fetched. The sentinel keeps the second run from pulling again, so it cannot
# loop. Verified at 72 KB with a mid-run pull: args preserved, one pass, the
# post-pull logic is what executes.
#
# A RELEASE IS CHECKED OUT, NOT PULLED, and the same rule holds: the tag's own
# installer finishes the run, so it is the one that matches the tag's unit,
# deploy files and build. That installer is re-executed with these arguments
# and the tag named by --tag (the newest-release default is resolved once,
# here), so it must take --tag: a tag whose installer does not (any tag before
# the installers took one) is refused before anything has changed, rather than
# half-run. The re-executed run is told where the checkout was, for the deploy
# notes.
if [ -z "${SHINGO_INSTALL_REEXEC:-}" ]; then
    self_before=$(sha256sum "$SELF" | cut -d' ' -f1)
    sync_checkout_pick install-core.sh "$INSTALL_TAG" "$INSTALL_MAIN"
    sync_checkout "$SYNC_TARGET"
    if [ "$SYNC_TARGET" != "main" ]; then
        INSTALL_TAG="$SYNC_TARGET"
    fi
    self_after=$(sha256sum "$SELF" | cut -d' ' -f1)
    if [ "$self_before" != "$self_after" ]; then
        echo "==> That checkout changed the installer -- restarting with its version."
        export SHINGO_INSTALL_REEXEC=1 SHINGO_INSTALL_FROM="$SYNC_FROM"
        if [ -n "$INSTALL_TAG" ]; then
            exec bash "$SELF" ${ORIG_ARGS[@]+"${ORIG_ARGS[@]}"} --tag "$INSTALL_TAG"
        fi
        exec bash "$SELF" ${ORIG_ARGS[@]+"${ORIG_ARGS[@]}"}
    fi
fi
# Where the checkout was before this run: SYNC_FROM in the run that set it,
# SHINGO_INSTALL_FROM in the one it re-executed, neither when an older
# installer set the checkout and re-executed this one.
SYNC_FROM="${SYNC_FROM:-${SHINGO_INSTALL_FROM:-}}"
# Whichever run gets here builds what HEAD is, so a tag install proves HEAD is
# the tag before it stamps the binary with the tag's name.
if [ -n "$INSTALL_TAG" ] && \
   [ "$(git rev-parse HEAD)" != "$(git rev-parse -q --verify "refs/tags/${INSTALL_TAG}^{commit}" || true)" ]; then
    echo "ERROR: HEAD is not $INSTALL_TAG; refusing to build it under that name."
    exit 1
fi

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
#    d. --config on the systemd unit's ExecStart. Works with the service
#       STOPPED, which is when the scan below used to have to guess.
#    e. Bounded scan of /home/*/, /opt/*/, /srv/*/ for shingocore.yaml.
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
    unit_cfg=$(config_from_unit "shingo-core.service")
    if [ -n "$unit_cfg" ] && [ -f "$unit_cfg" ]; then
        LEGACY_CONFIG="$unit_cfg"
        echo "    legacy config (systemd unit):    $LEGACY_CONFIG"
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

sync_checkout_notes "$SYNC_FROM"
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
# Stamp the build. This build had no -ldflags at all until 2026-07-25, so the
# plant binary reported "shingocore dev" and the boot markers in the journal
# had no join key to a commit.
#
# THE DUBIOUS-OWNERSHIP EXPLANATION FOR THIS IS REFUTED — recorded here so it
# is not re-derived. Springfield's core did report version=dev commit=unknown
# on 2026-07-29, and "the installer runs under sudo against a repo owned by
# the login user, so git refuses it" is the obvious reading. It is wrong:
# measured on that box, `sudo git -C /home/setup/shingo describe` returns
# v0.0.14-941-g7c21ef3f with no complaint. Every other link checks out too —
# main.Version/main.Commit exist for -X to target, and the stamping code was
# already in the deployed commit. The 01:33 build simply did not come through
# this path; deploy night had manual intervention. Confirmed by the next
# install through it, which stamped v0.0.14-946-g6ff16bdc correctly.
#
# `git describe --always` CANNOT return empty, so "dev" requires git to fail
# outright. That is what makes this a refutation rather than a maybe: there is
# no partial-failure path that yields the observed value.
#
# safe.directory is kept as hardening — scoped to THIS repo for THIS
# invocation, so it is a no-op where git already works and covers a box
# configured differently. It is NOT the fix for anything observed here. Not
# `git config --global`, which would mutate root's config on a plant box as a
# side effect of an install, and not `--system`, same reason.
GIT_STAMP=(git -c "safe.directory=$REPO_ROOT" -C "$REPO_ROOT")
BUILD_VERSION=$("${GIT_STAMP[@]}" describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_COMMIT=$("${GIT_STAMP[@]}" rev-parse --short HEAD 2>/dev/null || echo unknown)
# A tag install is stamped with the tag itself (HEAD was proved to be it
# above), not with describe's pick among the tags on that commit; -dirty, as
# describe would add, when the checkout carries local edits.
if [ -n "$INSTALL_TAG" ]; then
    BUILD_VERSION="$INSTALL_TAG"
    "${GIT_STAMP[@]}" diff --quiet HEAD -- 2>/dev/null || BUILD_VERSION="${INSTALL_TAG}-dirty"
fi
echo "    stamping version=${BUILD_VERSION} commit=${BUILD_COMMIT}"
(cd "$REPO_ROOT/shingo-core" && "$GO_BIN" build \
    -ldflags "-s -w -X main.Version=${BUILD_VERSION} -X main.Commit=${BUILD_COMMIT}" \
    -o /tmp/shingocore ./cmd/shingocore)
# The check below runs it as the shingo user: a root umask of 077 would leave
# it 0700 and the check would fail to start for a reason that is not the
# database's.
chmod 755 /tmp/shingocore
echo "==> Build succeeded"

# ----------------------------------------------------------------------
# Can the new build start on this database? Asked before anything stops.
# ----------------------------------------------------------------------
# The new binary's own `migrate check` reads the database and answers with the
# decision its boot would make, writing nothing. A database this build will not
# start on (one below the release its baseline retires, a partial restore) is
# refused here, by name, while the old Core still serves, instead of by a crash
# loop after the stop. Exit 1 is that refusal; exit 2 is a database the check
# could not read, which the new Core could not start on either. On exit 0 it
# prints `database v<cur> -> v<target>`, which the rollback recipe at the end is
# built from (pinned by the migrate check test in cmd/shingocore).
DB_FROM=""
DB_TO=""
CHECK_CONFIG=""
if [ -f /etc/shingo/shingocore.yaml ]; then
    CHECK_CONFIG=/etc/shingo/shingocore.yaml
elif [ -n "$LEGACY_CONFIG" ] && [ -f "$LEGACY_CONFIG" ]; then
    CHECK_CONFIG="$LEGACY_CONFIG"
fi
if [ "$MODE" = "FRESH" ] || [ -z "$CHECK_CONFIG" ]; then
    echo "==> No Core config yet: no database to check (Core checks it when it starts)"
else
    echo "==> Checking the database with the new build ($CHECK_CONFIG; writes nothing)..."
    check_rc=0
    check_out=$(cd /tmp && runuser -u shingo -- /tmp/shingocore migrate -config "$CHECK_CONFIG" check 2>&1) || check_rc=$?
    printf '%s\n' "$check_out" | sed 's/^/    /'
    if [ "$check_rc" -ne 0 ]; then
        case "$check_rc" in
            1) echo "ERROR: the new build would not start on this database (migrate check refused it)."
               echo "       Do what the sentence above says (it names the release to install first), then rerun." ;;
            2) echo "ERROR: migrate check could not read the database or its config (exit 2; the reason is above)."
               echo "       Fix that (connection, credentials, $CHECK_CONFIG readable by the shingo user), then rerun." ;;
            *) echo "ERROR: migrate check did not run (exit $check_rc): the reason is above, not the database."
               echo "       (126: /tmp/shingocore could not be executed, e.g. a noexec /tmp; 127: runuser missing.)" ;;
        esac
        echo "       Nothing was stopped or changed: Core is still running the previous build."
        rm -f /tmp/shingocore
        exit 1
    fi
    DB_FROM=$(printf '%s\n' "$check_out" | sed -nE 's/^database v([0-9]+) -> v([0-9]+)$/\1/p' | tail -1)
    DB_TO=$(printf '%s\n' "$check_out" | sed -nE 's/^database v([0-9]+) -> v([0-9]+)$/\2/p' | tail -1)
    if [ -z "$DB_FROM" ] || [ -z "$DB_TO" ]; then
        echo "ERROR: migrate check passed but did not say which versions it would take the database from and to."
        echo "       Nothing was stopped or changed. Without them this run cannot print its rollback."
        rm -f /tmp/shingocore
        exit 1
    fi
fi

# print_rollback prints the way back to the previous build, from what this run
# does to the database (DB_FROM -> DB_TO, from migrate check). It runs at the end
# of a good install, and from the EXIT trap when the install fails after the
# binary swap, which is when the operator needs it most. Its argument says
# whether Core has been started on the new build: before that the database is
# still at DB_FROM and there is no down step to run.
SWAPPED=no
STARTED=no
print_rollback() {
    local started="$1"
    if [ ! -f /opt/shingo/shingocore.previous ]; then
        echo " Rollback: there is no /opt/shingo/shingocore.previous to go back to."
        return
    fi
    if [ "$DB_FROM" = "0" ]; then
        echo " Rollback: this run created the database (born at v${DB_TO}); the previous build has none to return to."
        return
    fi
    if [ "$started" = yes ] && [ -n "$DB_FROM" ] && [ "$DB_TO" -gt "$DB_FROM" ]; then
        echo " Rollback (this run takes the database from v${DB_FROM} to v${DB_TO}):"
    else
        echo " Rollback (the database was not migrated by this run):"
    fi
    echo "   sudo systemctl stop shingo-core"
    if [ "$started" = yes ] && [ -n "$DB_FROM" ]; then
        local v=$DB_TO
        while [ "$v" -gt "$DB_FROM" ]; do
            echo "   sudo -u shingo /opt/shingo/shingocore.next migrate -config /etc/shingo/shingocore.yaml down $v"
            v=$((v - 1))
        done
    fi
    echo "   sudo mv /opt/shingo/shingocore.previous /opt/shingo/shingocore"
    echo "   sudo systemctl start shingo-core"
    if [ "$started" = yes ] && [ -n "$DB_FROM" ] && [ "$DB_TO" -gt "$DB_FROM" ]; then
        echo "   Run the down step(s) before the mv: they are in shingocore.next only. If Core stopped before it"
        echo "   finished migrating, \`shingocore.next migrate -config /etc/shingo/shingocore.yaml check\` says where the"
        echo "   database stands; a down step for a version it has not reached refuses and changes nothing."
    fi
}

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
# The trap also prints the way back when the install fails after the binary
# swap (print_rollback above): re-running the installer then would copy the new
# build over .previous and lose the previous one.
trap 'rc=$?; rm -f "$DEPLOY_MARKER"; if [ "$rc" -ne 0 ] && [ "$SWAPPED" = yes ]; then echo ""; echo "The install stopped after the new binary went in (exit $rc). To go back to the previous build:"; print_rollback "$STARTED"; fi' EXIT

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
# overwriting, and keep the new one as shingocore.next. The recipe, printed by
# print_rollback with this run's versions (at the end, or from the EXIT trap
# when the install fails after the swap):
#   sudo systemctl stop shingo-core
#   sudo -u shingo /opt/shingo/shingocore.next migrate -config /etc/shingo/shingocore.yaml down <N>
#     (once per version this run's migrations added, newest first; none when it added none)
#   sudo mv /opt/shingo/shingocore.previous /opt/shingo/shingocore
#   sudo systemctl start shingo-core
# The down runs with the NEW binary, before the mv: only it carries the down step
# for its own migrations, and the mv overwrites /opt/shingo/shingocore. Nothing
# stops the previous binary from starting on a database a newer migration has
# changed (a build accepts a database newer than itself), so the order matters.
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
SWAPPED=yes
chown shingo:shingo /opt/shingo/shingocore
chmod 755 /opt/shingo/shingocore
# A hard link, not a copy: it costs no space, so it cannot fail on a full disk
# between the stop and the start (the next install's mv replaces the name, and
# the link keeps this binary). The copy is only the fallback, and failing both
# is a warning, not an exit: the rollback then needs this release rebuilt.
if ln -f /opt/shingo/shingocore /opt/shingo/shingocore.next 2>/dev/null \
    || cp -p /opt/shingo/shingocore /opt/shingo/shingocore.next; then
    echo "==> Kept the new binary as /opt/shingo/shingocore.next (its migrate down is the rollback)"
else
    echo "    WARNING: could not keep /opt/shingo/shingocore.next; a rollback's down step needs this release's"
    echo "             binary (install-core.sh --tag of this release rebuilds it, but also starts it)"
fi

echo "==> Installing alert-on-stop.sh to /opt/shingo/..."
cp "$REPO_ROOT/scripts/alert-on-stop.sh" /opt/shingo/alert-on-stop.sh
chown shingo:shingo /opt/shingo/alert-on-stop.sh
chmod 755 /opt/shingo/alert-on-stop.sh

# Rotation for the per-run debug file (--log-debug writes it; truncated on
# open, so only a months-long run needs the rotation). Idempotent: skipped
# when the installed copy already matches the repo copy.
if ! command -v logrotate >/dev/null 2>&1; then
    echo "WARNING: logrotate not found on PATH; /etc/logrotate.d/shingo-debug will be inert until it is installed"
fi
if [ -f /etc/logrotate.d/shingo-debug ] && cmp -s "$REPO_ROOT/deploy/shingo-debug.logrotate" /etc/logrotate.d/shingo-debug; then
    echo "==> /etc/logrotate.d/shingo-debug already current; leaving in place"
else
    echo "==> Installing /etc/logrotate.d/shingo-debug..."
    cp "$REPO_ROOT/deploy/shingo-debug.logrotate" /etc/logrotate.d/shingo-debug
fi

# ----------------------------------------------------------------------
# Config
# ----------------------------------------------------------------------
if [ ! -f /etc/shingo/shingocore.yaml ]; then
    if [ "$MODE" = "MIGRATION" ] && [ -n "$LEGACY_CONFIG" ]; then
        echo "==> Copying config from $LEGACY_CONFIG to /etc/shingo/shingocore.yaml..."
        cp "$LEGACY_CONFIG" /etc/shingo/shingocore.yaml
    else
        # NEVER INVENT A TIMEZONE, on a fresh install either. Seeding the box's
        # OS zone here is the same defect as seeding it into an existing config,
        # with a WIDER blast radius: core is the plant's authority for the zone,
        # and once edges inherit it, one wrong guess here is wrong on every box
        # at the site. Hopkinsville's core VM reports America/New_York while the
        # plant clock is Central — that guess would have been wrong.
        #
        # Empty is the honest answer and a diagnosable one: an unset zone shows
        # as unconfigured rather than as a plausible wrong number.
        SEED_TZ=""
        if [ "$CORE_TIMEZONE_EXPLICIT" = "yes" ]; then
            SEED_TZ="$CORE_TIMEZONE"
        elif [ "$ASSUME_YES" != "yes" ]; then
            echo "    Plant timezone is UNSET. The OS zone is $CORE_TIMEZONE,"
            echo "    which is a suggestion, not the plant clock — confirm it."
            if confirm "Record timezone: $CORE_TIMEZONE?"; then
                SEED_TZ="$CORE_TIMEZONE"
            fi
        else
            echo "    --yes: leaving timezone unset; pass --timezone IANA_ZONE to set it"
        fi
        echo "==> Writing placeholder /etc/shingo/shingocore.yaml..."
        cat > /etc/shingo/shingocore.yaml <<YAML
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

# IANA zone for plant-local display. Storage and the wire stay UTC; this is
# the rendering clock only. Empty means UNCONFIGURED, and is left that way on
# purpose: the installer never guesses a zone, because the box's OS zone is not
# the plant's clock (Hopkinsville's core VM reports America/New_York while the
# plant runs Central). Set it here, in the web UI, or with --timezone.
# PLANT_TIMEZONE env still overrides this key.
timezone: "${SEED_TZ}"
YAML
    fi
    chown shingo:shingo /etc/shingo/shingocore.yaml
    chmod 644 /etc/shingo/shingocore.yaml
else
    echo "==> /etc/shingo/shingocore.yaml already exists"
    # Same narrow exception as edge: absent-or-empty timezone: means the
    # display clock is running on the code default (America/Chicago) with
    # nothing in the file recording that decision. Both live plants are in
    # this state today — right by default, not by record.
    #
    # READ THE VALUE, do not pattern-match "looks set". The previous test was
    # `grep -qE 'timezone:[[:space:]]*[^[:space:]#]'`, and the quote character
    # in `timezone: ""` satisfies [^[:space:]#] — so the exact state both
    # plants ship in read as CONFIGURED and this block never ran. The comment
    # above claimed to handle empty; the regex only ever caught absent.
    # Verified against the live boxes 2026-09-08: core (key absent) prompted,
    # edge (key present, empty) said nothing at all.
    current_tz=$(sed -nE 's/^[[:space:]]*timezone[[:space:]]*:[[:space:]]*(.*)$/\1/p' \
                 /etc/shingo/shingocore.yaml | head -1 | tr -d "\"' ")
    if [ -z "$current_tz" ]; then
        suggested_tz="$CORE_TIMEZONE"
        [ -z "$suggested_tz" ] && suggested_tz="America/Chicago"
        echo "    timezone: is unset in the existing config"
        echo "    OS zone suggests: $suggested_tz (core is likely running the"
        echo "    America/Chicago code default — confirm that is the plant clock)"
        write_tz=""
        if [ "$CORE_TIMEZONE_EXPLICIT" = "yes" ]; then
            # Typed, not guessed. Honour it with or without --yes.
            write_tz="$CORE_TIMEZONE"
        elif [ "$ASSUME_YES" = "yes" ]; then
            echo "    --yes: leaving timezone unset (code default applies)"
            echo "    pass --timezone IANA_ZONE to set it unattended"
        elif confirm "Record timezone: $suggested_tz in the config?"; then
            write_tz="$suggested_tz"
        fi
        if [ -n "$write_tz" ]; then
            # REPLACE an existing key, only append when there is none. Core
            # appended unconditionally before, which was harmless only while
            # the empty case was unreachable: with detection fixed, appending
            # to a file that already carries `timezone: ""` yields a DUPLICATE
            # key, and gopkg.in/yaml.v3 rejects those outright ("mapping key
            # already defined") — the config parses to nothing and core does
            # not start. Verified against yaml.v3 v3.0.1.
            if grep -qE '^[[:space:]]*timezone[[:space:]]*:' /etc/shingo/shingocore.yaml; then
                sed -i -E "s|^[[:space:]]*timezone[[:space:]]*:.*|timezone: $write_tz|" /etc/shingo/shingocore.yaml
            else
                echo "timezone: $write_tz" >> /etc/shingo/shingocore.yaml
            fi
            echo "    timezone recorded: $write_tz"
        fi
    fi
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
STARTED=yes
systemctl start shingo-core
# The restart counter is read AFTER the start: a manual start resets NRestarts
# to 0 (systemd's service_start), so a value read before it, on a box whose Core
# once restarted itself, would read as a restart on the first pass. Only a rise
# from here is one.
RESTARTS_BEFORE=$(systemctl show shingo-core -p NRestarts --value 2>/dev/null || echo 0)

# WAIT FOR CORE TO ANSWER, NOT FOR is-active. The unit is Type=simple, so it
# reads active the moment the process exists, while Core is still migrating the
# database (the column-model migration takes minutes on a large plant database)
# or already crash-looping on a refused one (Restart=always brings it back every
# 5 s, active each time). Any HTTP answer counts, not only a healthy one: the
# web server starts after migrate (cmd/shingocore/main.go: store.Open, then
# startHTTPServer), so an answer means the migration is done.
#
# THE PROBE IS GET /login, NOT /api/health. /login answers as soon as the web
# server does: it renders the sign-in page (www handleLoginPage) and makes no
# fleet call. /api/health pings the fleet first
# (www apiHealthCheck: Fleet().Ping()), so with the RDS unreachable every call
# takes the RDS client's timeout (rds.timeout, 10 s by default) and this 5 s
# probe never sees an answer: measured 2026-11-21 on a systemd box with the RDS
# address black-holed, /api/health answered in 10.0 s, and the install of a Core
# that was serving waited out the 900 s and failed with the rollback text (the
# data-model-attack round's closing step 5f). A dark RDS is a fleet problem, not
# this install's.
# The wait is a wall-clock deadline. A restart counter that rises while waiting
# is a crash loop: stop there and show why.
WEB_PORT=8083
WEB_HOST=127.0.0.1
if [ -f /etc/shingo/shingocore.yaml ]; then
    # The web block's port and host, CRLF-tolerant. A host bound to every
    # address (or none named) is reached on loopback; a named one, on itself.
    cfg_port=$(tr -d '\r' < /etc/shingo/shingocore.yaml \
               | awk '/^web:/{w=1; next} /^[^[:space:]#]/{w=0} w && $1=="port:" {print $2; exit}' | tr -d "\"' ")
    case "$cfg_port" in ''|*[!0-9]*) ;; *) WEB_PORT="$cfg_port" ;; esac
    cfg_host=$(tr -d '\r' < /etc/shingo/shingocore.yaml \
               | awk '/^web:/{w=1; next} /^[^[:space:]#]/{w=0} w && $1=="host:" {print $2; exit}' | tr -d "\"' ")
    case "$cfg_host" in ''|0.0.0.0|::|'[::]'|localhost) ;; *) WEB_HOST="$cfg_host" ;; esac
fi
WEB_URL="http://${WEB_HOST}:${WEB_PORT}/login"
HEALTH_WAIT_S=900
if [ "$MODE" = "FRESH" ]; then
    echo "==> Waiting for shingo-core to be active (a fresh box has no database configured, so Core cannot answer yet)"
else
    echo "==> Waiting up to ${HEALTH_WAIT_S}s for Core to answer ${WEB_URL}"
    echo "    (a release that migrates the database starts serving only when the migration is done)"
fi
HEALTHY=no
HEALTH_CODE=""
wait_start=$SECONDS
next_note=$((SECONDS + 30))
while [ $((SECONDS - wait_start)) -lt "$HEALTH_WAIT_S" ]; do
    if [ "$MODE" = "FRESH" ]; then
        # A fresh box's config is a placeholder with no database: Core cannot
        # serve until the operator sets one (see IMPORTANT below). Active is all
        # there is to wait for.
        if systemctl is-active --quiet shingo-core; then HEALTHY=active; break; fi
    else
        HEALTH_CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$WEB_URL" 2>/dev/null || true)
        if [ -n "$HEALTH_CODE" ] && [ "$HEALTH_CODE" != "000" ]; then
            HEALTHY=yes
            break
        fi
    fi
    restarts_now=$(systemctl show shingo-core -p NRestarts --value 2>/dev/null || echo 0)
    if [ "${restarts_now:-0}" -gt "${RESTARTS_BEFORE:-0}" ]; then
        echo "ERROR: shingo-core restarted while starting (NRestarts ${RESTARTS_BEFORE} -> ${restarts_now}): it is crash-looping."
        echo "Recent journal:"
        journalctl -u shingo-core -n 50 --no-pager || true
        exit 1
    fi
    if [ "$SECONDS" -ge "$next_note" ]; then
        echo "    still starting after $((SECONDS - wait_start))s: $(journalctl -u shingo-core -n 1 --no-pager -o cat 2>/dev/null || true)"
        next_note=$((SECONDS + 30))
    fi
    sleep 1
done

if [ "$HEALTHY" = "no" ]; then
    echo "ERROR: shingo-core did not answer ${WEB_URL} within ${HEALTH_WAIT_S}s."
    echo "Recent journal:"
    journalctl -u shingo-core -n 50 --no-pager || true
    exit 1
fi

# "active" is not the same as "running the binary we just installed". A process
# relaunched from the pre-swap inode is still active and reports healthy on
# every other signal while executing the previous build. Verify what is ACTUALLY
# executing via /proc/<pid>/exe: after the binary is replaced, a stale process's
# exe link resolves to the unlinked inode and Linux marks it "(deleted)".
#
# ON systemd 255 AND LATER, MainPID IS systemd-executor FOR A MOMENT. systemd
# starts a service through systemd-executor, which then execs the binary, and a
# Type=simple unit reads active before that exec. A fresh install waits only for
# active, so it read the executor here and stopped with "running an unexpected
# binary" (measured 2026-11-21 on systemd 259, Ubuntu 26.04 under WSL: v0.17.3's
# installer and this one's, each on a fresh box). A fresh Core, with no database
# yet, also restarts every 5 s and has no MainPID in between. So the check looks
# again, for up to 15 s, while MainPID is missing or is the executor; a deleted
# or foreign binary still fails at once below. Do not drop the look-again for a
# single read: that is the version that failed. Expiry: when no box this installs
# on runs a systemd that starts services through an executor.
echo "==> Verifying the running process is the binary we installed..."
verify_until=$((SECONDS + 15))
while :; do
    RUN_PID=$(systemctl show shingo-core -p MainPID --value 2>/dev/null || echo "")
    RUN_EXE=""
    if [ -n "$RUN_PID" ] && [ "$RUN_PID" != "0" ]; then
        RUN_EXE=$(readlink "/proc/$RUN_PID/exe" 2>/dev/null || echo "")
    fi
    case "$RUN_EXE" in
        ""|*/systemd-executor) ;;
        *) break ;;
    esac
    [ "$SECONDS" -ge "$verify_until" ] && break
    sleep 0.5
done
if [ -z "$RUN_PID" ] || [ "$RUN_PID" = "0" ]; then
    echo "ERROR: could not determine shingo-core MainPID; cannot verify the build."
    exit 1
fi
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
if [ "$HEALTHY" = "yes" ]; then
    echo " Service status: serving (Core answered HTTP ${HEALTH_CODE})"
else
    echo " Service status: active (no database configured yet)"
fi
echo " Binary:  /opt/shingo/shingocore (also kept as shingocore.next)"
echo " Config:  /etc/shingo/shingocore.yaml"
echo " Backup:  $BACKUP_PATH"
echo " Version: ${BUILD_VERSION} (${BUILD_COMMIT})"
sync_checkout_source_line
echo " Logs:    sudo journalctl -u shingo-core -f"
if [ -n "$DB_FROM" ]; then
    print_rollback yes
fi
# Journal retention is host-wide (journald has no per-unit retention), but it
# ships with the product: unconfigured retention is how Springfield ended up
# holding ~15 days of journal — less than the age of the incident it was being
# read for. Idempotent: skipped when the installed copy already matches.
if [ -f /etc/systemd/journald.conf.d/10-shingo.conf ] && \
   cmp -s "$REPO_ROOT/deploy/journald-shingo.conf" /etc/systemd/journald.conf.d/10-shingo.conf; then
    echo "==> /etc/systemd/journald.conf.d/10-shingo.conf already current; leaving in place"
else
    echo "==> Installing /etc/systemd/journald.conf.d/10-shingo.conf..."
    mkdir -p /etc/systemd/journald.conf.d
    cp "$REPO_ROOT/deploy/journald-shingo.conf" /etc/systemd/journald.conf.d/10-shingo.conf
    systemctl restart systemd-journald
    echo "==> journald restarted: $(systemctl is-active systemd-journald)"
fi
if [ "$MODE" = "FRESH" ]; then
    echo ""
    echo " IMPORTANT: edit /etc/shingo/shingocore.yaml to set the"
    echo " PostgreSQL connection, then run: systemctl restart shingo-core"
fi
echo "============================================================"
