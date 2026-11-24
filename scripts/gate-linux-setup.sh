#!/usr/bin/env bash
# gate-linux-setup.sh — the Linux side's pinned gate tools, in one command (D24).
#
#   bash scripts/gate-linux-setup.sh           install what is missing, then say
#                                              what is still missing
#   bash scripts/gate-linux-setup.sh --check   change nothing: exit 0 when ready;
#                                              else exit 1 and ONE line
#                                              "missing: <items>"
#   bash scripts/gate-linux-setup.sh --pins    print each pin, its file and its
#                                              sha256 (check-gate-toolchain.sh
#                                              reads it; runs on any OS)
#
# Run it on the Linux side (WSL's Ubuntu, any linux/amd64). Everything it
# writes is under ~/gate-tools, one directory per pinned version, so a setup
# never swaps a tree under a gate that is running from it:
#   go<V>/                           the go tarball. Pin: shingo-core/go.mod's
#                                    go line, read as gate.sh reads GATE_GO
#                                    (check-gate-toolchain.sh holds every
#                                    go.mod and CI's go-version to it)
#   golangci-lint-<V>/golangci-lint  the release binary CI's
#                                    golangci-lint-action downloads. Pin:
#                                    .github/workflows/lint.yml's version
#   node-v<V>/                       the node tarball. Pin: NODE_PIN below; the
#                                    repo had no node pin (CI uses
#                                    ubuntu-latest's node), so this is its home
#   dl/                              the downloads, each checked against SHA256
#                                    below before it is used
#   env.sh                           exports PATH for those three directories
#                                    and GOTOOLCHAIN=local, nothing else; the
#                                    Linux-side gate sources it
# Anything else in ~/gate-tools (an older go/, node/, bin/) is left alone and
# named. Ready means: env.sh is what this script writes, each tool it names
# answers its pinned version, and Docker's server answers both for the CLI the
# gate's pg_up uses and on the socket testcontainers uses. No credentials, no
# plant data: three public tarballs and one generated file.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

GO_PIN="$(sed -n 's/^go[[:space:]]*//p' "$ROOT/shingo-core/go.mod" | tr -d '\r')"
LINT_PIN="$(sed -n 's/^[[:space:]]*version:[[:space:]]*v\{0,1\}\([0-9][0-9.]*\).*/\1/p' "$ROOT/.github/workflows/lint.yml" | tr -d '\r' | head -1)"
NODE_PIN=26.13.0

GO_FILE="go$GO_PIN.linux-amd64.tar.gz"
LINT_FILE="golangci-lint-$LINT_PIN-linux-amd64.tar.gz"
NODE_FILE="node-v$NODE_PIN-linux-x64.tar.xz"

# SHA256 <file> — the sha256 a download must have, from the vendor's own list:
# go.dev/dl/?mode=json, the release's golangci-lint-<V>-checksums.txt,
# nodejs.org/dist/v<V>/SHASUMS256.txt. A pin with no row here is refused:
# add its row (from that list) in the commit that bumps the pin.
SHA256() {
  case "$1" in
    go1.25.0.linux-amd64.tar.gz) echo 2852af0cb20a13139b3448992e69b868e50ed0f8a1e5940ee1de9e19a123b613 ;;
    golangci-lint-2.11.4-linux-amd64.tar.gz) echo 200c5b7503f67b59a6743ccf32133026c174e272b930ee79aa2aa6f37aca7ef1 ;;
    node-v26.13.0-linux-x64.tar.xz) echo d28a96af6c38596f6d9d97c0ee531c13d238d757742387d1928de822d959e997 ;;
  esac
}

# pins_row <tool> <pin> <file> — one --pins line: tool, pin, file, sha256 ("none"
# for an empty pin or a file with no SHA256 row).
pins_row() {
  local sha
  sha="$(SHA256 "$3")"
  printf '%s %s %s %s\n' "$1" "${2:-none}" "$3" "${sha:-none}"
}

if [ "${1:-}" = --pins ]; then
  pins_row go "$GO_PIN" "$GO_FILE"
  pins_row golangci-lint "$LINT_PIN" "$LINT_FILE"
  pins_row node "$NODE_PIN" "$NODE_FILE"
  exit 0
fi

MODE=setup
case "${1:-}" in
  "") ;;
  --check) MODE=check ;;
  *) echo "usage: bash scripts/gate-linux-setup.sh [--check|--pins]" >&2; exit 2 ;;
esac

T="$HOME/gate-tools"
GO_DIR="$T/go$GO_PIN"
LINT_DIR="$T/golangci-lint-$LINT_PIN"
NODE_DIR="$T/node-v$NODE_PIN"
ENV_FILE="$T/env.sh"

# env_body — the one env.sh this version of the script writes.
env_body() {
  echo "# Written by scripts/gate-linux-setup.sh (the Linux-side gate sources it); setup rewrites it."
  echo "export PATH=\"$GO_DIR/bin:$LINT_DIR:$NODE_DIR/bin:\$PATH\""
  echo "export GOTOOLCHAIN=local"
}

# asked <cmd...> — a tool's version answer, run so that it writes nothing: from /
# (no go.mod, so no toolchain switch), GOTOOLCHAIN=local, and the user's config
# and cache dirs pointed at /dev/null. Without that, `go version` writes its
# telemetry counters under ~/.config/go and `golangci-lint version` makes
# ~/.cache/goimports (both seen on an empty HOME, 2026-11-24), and --check
# would change the machine it reads.
asked() {
  (cd / && XDG_CONFIG_HOME=/dev/null XDG_CACHE_HOME=/dev/null GOTOOLCHAIN=local GOFLAGS= "$@" 2>&1 | head -1)
}

# answers <tool> — 0 when the tool in its pinned directory reports its pin.
answers() {
  local out
  case "$1" in
    go) out="$(asked "$GO_DIR/bin/go" version)"
        [ "${out#go version go$GO_PIN }" != "$out" ] ;;
    golangci-lint) out="$(asked "$LINT_DIR/golangci-lint" version)"
        case "$out" in *"has version $LINT_PIN "*) true ;; *) false ;; esac ;;
    node) out="$(asked "$NODE_DIR/bin/node" --version)"
        [ "$out" = "v$NODE_PIN" ] ;;
  esac
}

# version_of <tool> — what the tool says, for the setup's account.
version_of() {
  case "$1" in
    go) asked "$GO_DIR/bin/go" version ;;
    golangci-lint) asked "$LINT_DIR/golangci-lint" version ;;
    node) asked "$NODE_DIR/bin/node" --version ;;
  esac
}

# docker_answers — 0 when Docker's server answers the CLI (pg_up's docker run)
# and, unless DOCKER_HOST names another, on /var/run/docker.sock, the socket
# testcontainers falls back to. Prints the server version, or why not.
docker_answers() {
  local v w
  command -v docker >/dev/null 2>&1 || { echo "no docker CLI on PATH"; return 1; }
  v="$(timeout 20 docker version --format '{{.Server.Version}}' 2>/dev/null | tr -d '\r')"
  [ -n "$v" ] || { echo "the CLI's server does not answer (docker version)"; return 1; }
  if [ -z "${DOCKER_HOST:-}" ]; then
    w="$(timeout 20 docker -H unix:///var/run/docker.sock version --format '{{.Server.Version}}' 2>/dev/null | tr -d '\r')"
    [ -n "$w" ] || { echo "no server on /var/run/docker.sock (testcontainers' default; set DOCKER_HOST or fix the socket)"; return 1; }
  fi
  echo "server $v answers"
}

host_ok() {
  [ "$(uname -s)" = Linux ] && [ "$(uname -m)" = x86_64 ]
}

# ── --check: read only ──────────────────────────────────────────────────────
missing_items() {
  local m=""
  if ! host_ok; then echo "linux-amd64"; return; fi
  answers go || m="$m go$GO_PIN"
  answers golangci-lint || m="$m golangci-lint-$LINT_PIN"
  answers node || m="$m node-v$NODE_PIN"
  [ -f "$ENV_FILE" ] && [ "$(cat "$ENV_FILE")" = "$(env_body)" ] || m="$m env.sh"
  docker_answers >/dev/null || m="$m docker"
  echo "${m# }"
}

if [ "$MODE" = check ]; then
  m="$(missing_items)"
  [ -z "$m" ] && exit 0
  echo "missing: $m"
  exit 1
fi

# ── setup ───────────────────────────────────────────────────────────────────
if ! host_ok; then
  echo "gate-linux-setup: this is $(uname -s)/$(uname -m); the pins carry linux-amd64 checksums only"
  echo "missing: linux-amd64"
  exit 1
fi
for p in "go:$GO_PIN:$GO_FILE" "golangci-lint:$LINT_PIN:$LINT_FILE" "node:$NODE_PIN:$NODE_FILE"; do
  IFS=: read -r name pin file <<<"$p"
  if [ -z "$pin" ]; then
    echo "gate-linux-setup: FAILED — no $name pin read (see the header for where it lives)"; exit 1
  fi
  if [ -z "$(SHA256 "$file")" ]; then
    echo "gate-linux-setup: FAILED — no sha256 for $file in scripts/gate-linux-setup.sh's SHA256:"
    echo "  add its row from the vendor's checksum list (the header says which) with the pin bump"
    exit 1
  fi
done
for c in tar xz sha256sum mktemp; do
  command -v "$c" >/dev/null 2>&1 || { echo "gate-linux-setup: FAILED — $c is not on PATH (setup needs tar, xz, sha256sum, mktemp, and curl or wget)"; exit 1; }
done
mkdir -p "$T/dl" || exit 1
TMP="$(mktemp -d "$T/.tmp.XXXXXX")" || exit 1
trap 'rm -rf "$TMP"' EXIT
changed=()

echo "gate-linux-setup: $T (go$GO_PIN from shingo-core/go.mod, golangci-lint $LINT_PIN from .github/workflows/lint.yml, node v$NODE_PIN from this script)"

# fetch <file> <url> — dl/<file> with its pinned sha256, downloading it when the
# copy there is absent or wrong. A download that does not match is deleted.
fetch() {
  local f="$1" url="$2" want got
  want="$(SHA256 "$f")"
  if [ -f "$T/dl/$f" ] && [ "$(sha256sum <"$T/dl/$f" | cut -d' ' -f1)" = "$want" ]; then
    echo "    dl/$f: sha256 ok (cached)"
    return 0
  fi
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 2 -o "$TMP/$f" "$url" || { echo "    FAILED download $url"; return 1; }
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$TMP/$f" "$url" || { echo "    FAILED download $url"; return 1; }
  else
    echo "    FAILED — neither curl nor wget to download $url"; return 1
  fi
  got="$(sha256sum <"$TMP/$f" | cut -d' ' -f1)"
  if [ "$got" != "$want" ]; then
    echo "    FAILED $url: sha256 $got, want $want (deleted)"
    rm -f "$TMP/$f"; return 1
  fi
  mv -f "$TMP/$f" "$T/dl/$f" || return 1
  echo "    dl/$f: downloaded, sha256 ok"
}

# install_tool <tool> <file> <url> <top dir in the archive> <target dir>
install_tool() {
  local tool="$1" f="$2" url="$3" top="$4" dst="$5" x="$TMP/x-$1"
  if answers "$tool"; then
    echo "  $tool: present ($(version_of "$tool"))"
    return 0
  fi
  echo "  $tool: installing into $dst"
  fetch "$f" "$url" || return 1
  mkdir -p "$x" && tar -xf "$T/dl/$f" -C "$x" || { echo "    FAILED to unpack dl/$f"; return 1; }
  if [ -e "$dst" ]; then
    # It is there but does not answer its pin: broken, so nothing runs from it.
    mv -T "$dst" "$TMP/broken-$tool" && echo "    moved aside a $dst that did not answer its version"
  fi
  if ! mv -T "$x/$top" "$dst"; then
    answers "$tool" && { echo "    another setup installed it meanwhile: $(version_of "$tool")"; return 0; }
    echo "    FAILED to move the unpacked tree to $dst"; return 1
  fi
  if ! answers "$tool"; then
    echo "    FAILED — $dst does not answer $tool's pin: $(version_of "$tool")"; return 1
  fi
  echo "    $(version_of "$tool")"
  changed+=("installed $tool")
}

install_tool go "$GO_FILE" "https://go.dev/dl/$GO_FILE" go "$GO_DIR"
install_tool golangci-lint "$LINT_FILE" "https://github.com/golangci/golangci-lint/releases/download/v$LINT_PIN/$LINT_FILE" \
  "golangci-lint-$LINT_PIN-linux-amd64" "$LINT_DIR"
install_tool node "$NODE_FILE" "https://nodejs.org/dist/v$NODE_PIN/$NODE_FILE" "node-v$NODE_PIN-linux-x64" "$NODE_DIR"

if [ -f "$ENV_FILE" ] && [ "$(cat "$ENV_FILE")" = "$(env_body)" ]; then
  echo "  env.sh: current"
else
  env_body >"$TMP/env.sh" && mv -f "$TMP/env.sh" "$ENV_FILE" && echo "  env.sh: written" && changed+=("wrote env.sh")
fi

if d="$(docker_answers)"; then echo "  docker: $d"; else echo "  docker: NOT reachable — $d"; fi

# What is in ~/gate-tools that this script did not make: named, left alone.
others=""
for e in "$T"/* "$T"/.[!.]*; do
  [ -e "$e" ] || continue
  case "$e" in
    "$GO_DIR"|"$LINT_DIR"|"$NODE_DIR"|"$ENV_FILE"|"$T/dl"|"$TMP") ;;
    *) others="$others $(basename "$e")" ;;
  esac
done
[ -n "$others" ] && echo "  left alone (not this script's, not on env.sh's PATH):$others"

m="$(missing_items)"
if [ "${#changed[@]}" -eq 0 ]; then
  echo "gate-linux-setup: nothing changed"
else
  echo "gate-linux-setup: changed: $(IFS=,; echo "${changed[*]}" | sed 's/,/, /g')"
fi
if [ -z "$m" ]; then
  echo "ready: . $ENV_FILE before the Linux-side gate"
  exit 0
fi
echo "missing: $m"
exit 1
