#!/usr/bin/env bash
# tier: dev
# inputs: scripts/gate.sh scripts/check-gate-exec.sh
# side: linux
# check-gate-exec.sh — pins the host port pg_up (scripts/gate.sh) publishes the
# gate's Postgres on (docket G23). pg_up, read out of gate.sh and run against a
# docker that only records: the server is published on a host port pg_up picks
# itself, in 61000-65535, above the ports Linux hands a client; a pick docker
# cannot start the server on moves to another, the container the failed start
# left removed first; a docker that cannot create the container is asked once;
# with every pick taken it gives up. On WSL it also reads the client range.
# Once a server starts, the image id docker reports for its container is in
# the run's kept logs (gate_keep_run, read out of gate.sh too), one line
# naming the server's role and the id (docket G14).
#
# The name is history: this file pinned the record-only `go test -exec`
# wrapper and the gate run's identity, which went with the wrapper, and this
# pin was added beside them.
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1
TMP="$(mktemp -d 2>/dev/null || mktemp -d -t gateexec)"
trap 'rm -rf "$TMP"' EXIT

fails=0
fail() { echo "  FAIL gate-exec: $*"; fails=$((fails + 1)); }

# ── pg_up: the host port the server is published on (docket G23) ────────────
# Docker Desktop opens a published port inside WSL once, when the container
# starts, by listening on the same number there. A number some socket holds at
# that instant without listening on it (a docker step leaves thousands of its
# own client ports in TIME-WAIT) fails that listen, nothing retries it, and
# every dial from WSL is refused for the container's life. Left to docker, the
# number came from Windows' dynamic range, most of which Linux also hands to
# clients. So pg_up names the port, from a range Linux hands no client, and
# takes another when docker cannot start the server on its pick.
PGUP_LO=61000 PGUP_HI=65535
pgup_src="$(sed -n '/^pg_up() {$/,/^}$/p' "$ROOT/scripts/gate.sh")"
case "$pgup_src" in
  *'docker run'*) ;;
  *) fail "pg_up was not read out of scripts/gate.sh (no 'pg_up() {' block with a docker run line): the pg_up checks below checked nothing" ;;
esac
# The image the started server runs, as the fake docker reports it for the
# container: what the run's kept logs must name (docket G14). A floating tag
# such as postgres:16-alpine names whatever image was pulled last.
PGUP_IMAGE_ID="sha256:6a7e-the-started-containers-image"
keep_src="$(sed -n '/^gate_keep_run() {$/,/^}$/p' "$ROOT/scripts/gate.sh")"
case "$keep_src" in
  *'.gate/runs'*) ;;
  *) fail "gate_keep_run was not read out of scripts/gate.sh (no 'gate_keep_run() {' block naming .gate/runs): the image record was checked against no kept run" ;;
esac
# pgup CASE TAKEN [nocreate] — one pg_up, its files under $TMP/pgup/CASE: out,
# rc, ports (the -p value of each docker run) and calls (each docker verb).
# The first TAKEN runs find their port taken, as docker reports it: the start
# fails and the container is left behind, created. With nocreate no run
# creates a container at all (docker down, no image). A pg_up still running
# docker after 50 runs is stopped, and leaves no rc. Each case is a gate run
# of its own under $S/root (its ROOT), started as the docker step's server,
# and kept by gate_keep_run afterwards into $S/root/.gate/runs.
pgup() {
  local S="$TMP/pgup/$1"
  mkdir -p "$S/root/.gate"
  ( taken="$2" nocreate="${3:-}"
    ROOT="$S/root" GATE_KEPT_RUNS=10 GATE_KEEP_MARK="$S/root/.gate/.keep"
    touch -d "@$(( $(date +%s) - 60 ))" "$GATE_KEEP_MARK"
    docker() {
      local a prev="" p=""
      echo "$1" >>"$S/calls"
      case "$1" in
        run)
          for a in "$@"; do [ "$prev" = -p ] && p="$a"; prev="$a"; done
          echo "$p" >>"$S/ports"
          [ "$(wc -l <"$S/ports")" -le 50 ] || exit 3
          [ -z "$nocreate" ] || return 125
          : >"$S/container"
          [ "$(wc -l <"$S/ports")" -gt "$taken" ] || return 125 ;;
        rm) rm -f "$S/container" ;;
        # docker [container] inspect NAME: the container exists, and runs this
        container|inspect) [ -e "$S/container" ] && echo "$PGUP_IMAGE_ID" ;;
        port) echo "127.0.0.1:54321" ;;     # a port of docker's own choosing
      esac
    }
    PG_IMAGE=postgres:16-alpine
    eval "$pgup_src"
    eval "$keep_src"
    pg_up shingo-gate-exec-check docker >"$S/out" 2>/dev/null
    echo $? >"$S/rc"
    gate_keep_run )
}
pgup_rc() { cat "$TMP/pgup/$1/rc" 2>/dev/null || echo "none: stopped after 50 docker runs"; }
pgup_runs() { grep -c . "$TMP/pgup/$1/ports" 2>/dev/null; }
# pgup_ports CASE — fails on a docker run of the case that did not publish
# 127.0.0.1:<a port in PGUP_LO-PGUP_HI>:5432, or on two runs with one port.
pgup_ports() {
  local v bad=0
  while read -r v; do
    if [[ "$v" =~ ^127\.0\.0\.1:([0-9]+):5432$ ]] && [ "${BASH_REMATCH[1]}" -ge "$PGUP_LO" ] && [ "${BASH_REMATCH[1]}" -le "$PGUP_HI" ]; then
      continue
    fi
    bad=1
    fail "pg_up ($1) published the server with -p '$v': want -p 127.0.0.1:<port>:5432 with a port pg_up picks in $PGUP_LO-$PGUP_HI, where Linux hands no client port (a port left to docker is one Docker Desktop's listen inside WSL can lose to a client socket, and then every dial from WSL is refused)"
  done <"$TMP/pgup/$1/ports"
  [ -z "$(sort "$TMP/pgup/$1/ports" | uniq -d)" ] \
    || { bad=1; fail "pg_up ($1) tried one port twice: $(tr '\n' ' ' <"$TMP/pgup/$1/ports")"; }
  return $bad
}
# pgup_printed CASE — the address the case printed, against its last run's port.
pgup_printed() {
  local want
  want="$(tail -1 "$TMP/pgup/$1/ports" | sed 's/:5432$//')"
  [ "$(cat "$TMP/pgup/$1/out")" = "$want" ] \
    || fail "pg_up ($1) printed '$(cat "$TMP/pgup/$1/out")': want $want, the address of the run that started"
}
# pgup_image CASE ROLE — the case's kept run holds one line naming ROLE and the
# image id docker reports for the container pg_up started (docket G14).
pgup_image() {
  local lines n
  lines="$(grep -rhF -- "$PGUP_IMAGE_ID" "$TMP/pgup/$1/root/.gate/runs" 2>/dev/null)"
  n="$(printf '%s' "$lines" | grep -c .)"
  { [ "$n" = 1 ] && printf '%s\n' "$lines" | grep -qw -- "$2"; } \
    || fail "pg_up ($1): the run's kept logs (.gate/runs) hold $n line(s) naming the image id docker reports for the container it started ($PGUP_IMAGE_ID)${lines:+: '$lines'}; want one, naming the $2 step's server. Once the server starts, pg_up writes its container's image id (docker container inspect --format '{{.Image}}') into a .gate/*.log that gate_keep_run keeps: PG_IMAGE is a floating tag"
}
if [ -n "$pgup_src" ]; then
  # a free port: one run, on a port in the range, and that address printed
  pgup free 0
  [ "$(pgup_rc free)" = 0 ] && [ "$(pgup_runs free)" = 1 ] \
    || fail "pg_up (free): rc $(pgup_rc free) after $(pgup_runs free) docker runs, want 0 after 1"
  pgup_ports free && pgup_printed free
  pgup_image free docker
  # two taken picks: each failed start's container removed, then another port
  pgup taken 2
  [ "$(pgup_rc taken)" = 0 ] && [ "$(pgup_runs taken)" = 3 ] \
    || fail "pg_up (two picks taken): rc $(pgup_rc taken) after $(pgup_runs taken) docker runs, want 0 after 3: a start that fails on a taken port must move to another port, not end pg_up"
  [ "$(grep -xE 'run|rm' "$TMP/pgup/taken/calls" | tr '\n' ' ')" = "run rm run rm run " ] \
    || fail "pg_up (two picks taken) ran docker '$(tr '\n' ' ' <"$TMP/pgup/taken/calls")': want the container each failed start left removed before the next run (its name is the next run's)"
  pgup_ports taken && [ "$(pgup_rc taken)" = 0 ] && pgup_printed taken && pgup_image taken docker
  # a docker that cannot create the container: asked once, not once a port
  pgup nocreate 0 nocreate
  [ "$(pgup_rc nocreate)" != 0 ] && [ "$(pgup_runs nocreate)" = 1 ] && [ ! -s "$TMP/pgup/nocreate/out" ] \
    || fail "pg_up (docker creates nothing): rc $(pgup_rc nocreate) after $(pgup_runs nocreate) docker runs, printed '$(cat "$TMP/pgup/nocreate/out")'; want a failure after 1 run and no address: a run that left no container did not fail on its port"
  # every pick taken: it gives up, with nothing printed and no container left
  pgup alltaken 1000
  n="$(pgup_runs alltaken)"
  { [ "$(pgup_rc alltaken)" = 1 ] && [ "$n" -gt 1 ] && [ ! -s "$TMP/pgup/alltaken/out" ] && [ ! -e "$TMP/pgup/alltaken/container" ]; } \
    || fail "pg_up (every pick taken): rc $(pgup_rc alltaken) after $n docker runs, printed '$(cat "$TMP/pgup/alltaken/out")', container left: $([ -e "$TMP/pgup/alltaken/container" ] && echo yes || echo no); want rc 1 after more than one run, no address and no container"
  [ "$n" -gt 1 ] && pgup_ports alltaken
fi
# The premise, where it matters (the Linux side of a Windows host): Linux
# hands a client no port in pg_up's range.
if [ -n "${WSL_DISTRO_NAME:-}" ] && read -r _ pgup_client_hi 2>/dev/null </proc/sys/net/ipv4/ip_local_port_range; then
  [ "$pgup_client_hi" -lt "$PGUP_LO" ] \
    || fail "this host hands client ports up to $pgup_client_hi (/proc/sys/net/ipv4/ip_local_port_range), inside pg_up's $PGUP_LO-$PGUP_HI: a docker step's own connections can then hold the number its server is published on; move pg_up's range above it, or the sysctl back"
fi

if [ "$fails" -eq 0 ]; then
  echo "ok   gate-exec (pg_up publishes on a port of its own above Linux's client range and moves off a taken one; its server's image id is in the run's kept logs)"
  exit 0
fi
exit 1
