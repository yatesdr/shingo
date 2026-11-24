# shellcheck shell=bash
#
# gate-mirror.sh — the gate's Linux side. A gate started from a Windows
# worktree (Git Bash) runs in a mirror of that worktree on WSL's own ext4
# filesystem, where the same tree's gate measured push 348.9 -> 127.1 s, cert
# 353.5 -> 177.4 s and dev about 110 -> 40 s (R31 and the round 2 close-out,
# work/loc-census-2026-11-20/REPORT-gate-fast.md; ruling R37). Sessions keep
# editing the Windows worktree; the mirror is a copy the gate owns.
#
# TWO HALVES IN ONE FILE. scripts/gate.sh sources it for the caller's half
# (mirror_route, after the gate lock is taken). The Linux side runs it as a
# script with the worktree as its working directory,
#     bash scripts/gate-mirror.sh linux-run .gate/mirror/spec
# reading the worktree's own copy through /mnt/c: one wsl.exe crossing a gate.
#
# WHERE A GATE RUNS (mirror_route). On Windows, a heavy mode (every mode but
# fmt, scope, order, cachescope, identity and lock-status) runs on the Linux
# side when `scripts/gate-linux-setup.sh --check` says it is set up, and on the
# Windows side, as before, when it does not; the sentence says which, and why.
#   GATE_SIDE=windows   the Windows side, as before this file existed
#   GATE_SIDE=linux     the Linux side, or FAILED (no fallback)
# The light modes stay on the caller's side: fmt is the pre-push hook's and
# must never wait, and a sync needs the gate lock; scope, order, cachescope,
# identity and lock-status only read, and describe the side they run on.
# Off Windows (CI, the houseserver, a shell inside WSL) nothing here applies.
#
# THE MIRROR IS THE WORKING TREE. Its content is the tree `git add -A` would
# commit from the worktree (gate_tree_id): committed, staged, unstaged and
# untracked files, ignore rules honoured, deletions gone. By construction it
# carries nothing a commit would not: no ignored file, no .git pointer, no
# .gate, no build output. The ignore rules are the caller's own (the clone's
# info/exclude and the user's global excludes file are copied into the mirror),
# so both sides read one set. Line endings are compared as git compares them
# (after .gitattributes normalisation), which is the one sense in which a file
# in the mirror can differ in bytes from the worktree's: a CRLF file there is
# LF here (the repo is eol=lf throughout).
#
# THE TREE ID IS THE MANIFEST. The caller names the worktree's tree; the
# mirror's gate computes its own the same way before any step runs, and a
# mismatch is a red with nothing run (mirror_inner). The sentence names it.
#
# A FILE CHANGED IN THE MIRROR BY HAND IS REFUSED. Before a sync the Linux side
# computes the mirror's tree and compares it with the tree it last synced; a
# difference is a red naming the files, and nothing is overwritten or run.
#
# MTIMES. The working tree moves from the last synced tree to the new one with
# `git read-tree -m -u` through an index of the synced tree: files that did not
# change are not touched, so Go's build and test caches still hit.
#
# GIT STATE. The mirror is a git repository of its own (not a worktree: it does
# not depend on the worktree's .git pointer). It carries HEAD's commit on its
# branch with the same upstream, every refs/remotes/* and refs/tags/* ref (the
# schemadump tests read release tags by name), the BASE of `full BASE`, and an
# index equal to the worktree's (so staged and unstaged read as they do there).
# The objects travel as a git bundle the caller builds natively from a private
# object store (.gate/mirror/objects.git, alternates to the clone's objects):
# nothing is written into the shared clone, no ref and no object. The first
# sync sends the history once (about 80 MB); later ones send what is new.
#
# ONE MIRROR PER WORKTREE: ~/gate-mirrors/<worktree dir name> (GATE_MIRROR_ROOT
# names another root). It records its source path in .git/gate-mirror, and a
# second worktree with the same name is refused rather than sharing it.
#
# ONE GATE LOCK ACROSS BOTH SIDES. The caller takes the lock in the Windows
# clone's git common dir as before; the mirror's gate is its nested run
# (GATE_LOCK_HELD, GATE_LOCK_DIR through /mnt/c). The mirror records the lock
# and timing flag paths in its config, so a gate started by hand inside it
# takes the same lock, never a private one (gate.sh reads them at its top).
#
# THE TOOLS. The Linux side's gate sources ~/gate-tools/env.sh (lane wsl's
# setup writes it: the pinned go, golangci-lint and node, GOTOOLCHAIN=local).
#
# Knobs, mostly for scripts/check-gate-mirror.sh:
#   GATE_SIDE            windows | linux (above)
#   GATE_MIRROR_ROOT     where mirrors live on the Linux side (~/gate-mirrors)
#   GATE_LINUX_ENV       the Linux side's env file (~/gate-tools/env.sh)
#   GATE_LINUX_SETUP     the setup script to --check (scripts/gate-linux-setup.sh)
#   GATE_LINUX_EXEC=local  run the "Linux side" as a local bash, not through
#                        wsl.exe: the pin's way of exercising the whole sync
#                        on any OS, CI included
#   GATE_WSL_DISTRO      the WSL distro (Ubuntu, as step_race and step_sim)

# ── Both halves ──────────────────────────────────────────────────────────

# gate_tree_id TMP_INDEX [REAL_INDEX] — the tree id of the checkout in the
# working directory, set as GATE_TREE_WORK, with the index's own tree as
# GATE_TREE_INDEX. The real index (REAL_INDEX, else git's) is copied to
# TMP_INDEX, so tracked files under an ignore rule and the executable bits come
# along as they would with `git add -A` (an empty index loses the exec bit on
# Windows, core.filemode=false); the copy is written as the index tree, then
# `git add -A` and written again. Run the same way on both sides. Fails when the
# index has unmerged paths. TMP_INDEX is the caller's to name and remove
# (guards run nested gates in one tree at once).
GATE_TREE_INDEX="" GATE_TREE_WORK=""
gate_tree_id() {
  local idx="$1" real="${2:-}"
  GATE_TREE_INDEX="" GATE_TREE_WORK=""
  [ -n "$real" ] || real="$(git rev-parse --git-path index 2>/dev/null)" || return 1
  if [ -e "$real" ]; then cp "$real" "$idx" || return 1; else rm -f "$idx"; fi
  GATE_TREE_INDEX="$(GIT_INDEX_FILE="$idx" git write-tree 2>/dev/null)" \
    || { echo "the index has unmerged paths" >&2; return 1; }
  GIT_INDEX_FILE="$idx" git add -A || return 1
  GATE_TREE_WORK="$(GIT_INDEX_FILE="$idx" git write-tree)" || return 1
}

# _mirror_lpath P — P as the Linux side reads it: a Windows path (C:/...) through
# wslpath, anything else as it is (the pin's local mode, an empty value).
_mirror_lpath() {
  case "$1" in
    [A-Za-z]:/*|[A-Za-z]:\\*)
      if command -v wslpath >/dev/null 2>&1; then wslpath -a "$1"; else printf '%s' "$1"; fi ;;
    *) printf '%s' "$1" ;;
  esac
}

# ── The caller's half (sourced by scripts/gate.sh) ─────────────────────────
#
# FEW PROCESSES. On a loaded Windows host every process, and every $(...) fork,
# costs 0.1-0.7 s (measured 2026-11-24), so this half answers with builtins
# where it can and asks git once for what one call can answer: a no-change sync
# is about twelve processes on this side.

MIRROR_RC=""        # set when this gate is decided here: gate.sh exits with it
MIRROR_INNER_PID="" # the Linux side's job while it runs (mirror_on_exit)
gate_side_note=""   # the sentence's clause: which side, why, which tree

# Git Bash's bash says msys.
_mirror_on_windows() {
  case "${OSTYPE:-}" in msys*|cygwin*) return 0 ;; esac
  return 1
}

# _mirror_wpath VAR P — sets VAR to P as both git.exe and the Linux side's
# wslpath read it (C:/...): a drive path rewritten in place, anything else
# through cygpath; P itself off Windows.
_mirror_wpath() {
  local -n _mw_out="$1"
  local _mw_p="$2" _mw_d
  if ! _mirror_on_windows; then _mw_out="$_mw_p"; return; fi
  case "$_mw_p" in
    /[A-Za-z]/*|/[A-Za-z]) _mw_d="${_mw_p:1:1}"; _mw_out="${_mw_d^^}:${_mw_p:2}"; [ -n "${_mw_p:2}" ] || _mw_out="$_mw_out/" ;;
    [A-Za-z]:*) _mw_d="${_mw_p:0:1}"; _mw_out="${_mw_d^^}${_mw_p:1}" ;;
    *) if command -v cygpath >/dev/null 2>&1; then _mw_out="$(cygpath -m "$_mw_p")"; else _mw_out="$_mw_p"; fi ;;
  esac
}

# _mirror_copy SRC DST — a small text file copied with builtins (DST empty when
# SRC cannot be read).
_mirror_copy() {
  local lines=()
  if [ -r "$1" ]; then mapfile -t lines <"$1"; fi
  if [ "${#lines[@]}" -gt 0 ]; then printf '%s\n' "${lines[@]}" >"$2"; else : >"$2"; fi
}

# _mirror_linux ARGS — `bash scripts/gate-mirror.sh ARGS` on the Linux side,
# in this worktree; in the background, so an interrupt reaches gate.sh's trap
# (bash runs a trap only once a foreground command has finished).
_mirror_linux() {
  local w bs='\'
  if [ "${GATE_LINUX_EXEC:-}" = local ]; then
    ( cd "$ROOT" && exec bash scripts/gate-mirror.sh "$@" ) &
  else
    _mirror_wpath w "$ROOT"
    MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' wsl.exe -d "${GATE_WSL_DISTRO:-Ubuntu}" \
      --cd "${w//\//$bs}" -e bash scripts/gate-mirror.sh "$@" &
  fi
  MIRROR_INNER_PID=$!
  wait "$MIRROR_INNER_PID"
  local rc=$?
  MIRROR_INNER_PID=""
  return "$rc"
}

# mirror_on_exit — from gate.sh's EXIT trap: an interrupted caller stops the
# mirror's gate too, rather than leaving it running without the lock.
mirror_on_exit() {
  [ -n "$MIRROR_INNER_PID" ] || return 0
  local pid=""
  read -r pid <"$MIRROR_W/inner.pid" 2>/dev/null
  if [ -n "$pid" ]; then
    if [ "${GATE_LINUX_EXEC:-}" = local ]; then kill -TERM "$pid" 2>/dev/null
    else MSYS_NO_PATHCONV=1 wsl.exe -d "${GATE_WSL_DISTRO:-Ubuntu}" -e kill -TERM "$pid" >/dev/null 2>&1; fi
  fi
  kill "$MIRROR_INNER_PID" 2>/dev/null
  MIRROR_INNER_PID=""
}

# mirror_route ARGS — where this gate runs, called by gate.sh once the gate lock
# is held. Sets gate_side_note, and MIRROR_RC when the gate was decided here
# (the Linux side ran it, or it failed before any step).
mirror_route() {
  MIRROR_RC="" gate_side_note=""
  if [ -n "${GATE_IN_MIRROR:-}" ]; then mirror_inner; return; fi
  { _mirror_on_windows || [ "${GATE_LINUX_EXEC:-}" = local ]; } || return 0
  MIRROR_W="$ROOT/.gate/mirror"
  MIRROR_MODE="${1:-all}" MIRROR_BASE="${2:-}"
  case "${GATE_SIDE:-}" in
    ""|linux|windows) ;;
    *) echo "gate: FAILED — GATE_SIDE='$GATE_SIDE'; it takes windows or linux"; MIRROR_RC=2; return ;;
  esac
  local r=0
  mirror_tree || r=$?
  if [ "$r" = 3 ]; then
    # A tree with no commit to mirror (an exported copy, a fresh init) runs here.
    [ "${GATE_SIDE:-}" = linux ] && { echo "gate: FAILED — GATE_SIDE=linux, and $ROOT has no git commit to mirror"; MIRROR_RC=1; return; }
    gate_side_note="on the Windows side (no git commit here to mirror)"
    export GATE_SIDE=windows
    return
  fi
  if [ "$r" != 0 ]; then
    echo "gate: FAILED — could not take the worktree's tree id (above); nothing ran"
    MIRROR_RC=1
    return
  fi
  if [ "${GATE_SIDE:-}" = windows ]; then
    gate_side_note="on the Windows side (GATE_SIDE=windows); tree ${MS_TREE:0:12}"
    # A gate a guard starts inside this one stays on this side too.
    export GATE_SIDE GATE_TREE="$MS_TREE"
    return
  fi
  if [ "${GATE_LINUX_EXEC:-}" != local ] && ! command -v wsl.exe >/dev/null 2>&1; then
    mirror_fallback "missing: wsl.exe"
    return
  fi
  if ! mirror_carry; then
    echo "gate: FAILED — could not gather the git state the mirror carries (above); nothing ran"
    MIRROR_RC=1
    return
  fi
  local try status rc
  MS_BUNDLE=""
  for try in 1 2; do
    if [ "$try" = 1 ]; then mirror_bundle incr; else mirror_bundle full; fi \
      || { echo "gate: FAILED — could not bundle the worktree's objects for the mirror"; MIRROR_RC=1; return; }
    mirror_write_spec "$@" || { echo "gate: FAILED — could not write $MIRROR_W/spec"; MIRROR_RC=1; return; }
    echo pending >"$MIRROR_W/status"
    : >"$MIRROR_W/inner.pid"
    _mirror_linux linux-run .gate/mirror/spec; rc=$?
    status=""
    read -r status <"$MIRROR_W/status" 2>/dev/null
    case "$status" in
      running)
        printf '%s\n' "${MS_REFSHAS[@]}" >"$MIRROR_W/sent"
        MIRROR_RC=$rc
        return ;;
      need-full)
        [ "$try" = 1 ] && continue
        echo "gate: FAILED — the mirror still lacks objects after a full bundle; nothing ran"
        MIRROR_RC=1
        return ;;
      missing:*)
        mirror_fallback "$status"
        return ;;
      refused)
        MIRROR_RC=1
        return ;;
      pending|"")
        mirror_fallback "missing: the Linux side did not answer (exit $rc)"
        return ;;
      *)
        echo "gate: FAILED — the sync to the Linux side stopped at '$status' (above); nothing ran"
        MIRROR_RC=1
        return ;;
    esac
  done
}

# mirror_fallback "missing: ..." — the Linux side is not set up: the Windows
# side runs the gate, and the sentence says why. GATE_SIDE=linux refuses.
mirror_fallback() {
  if [ "${GATE_SIDE:-}" = linux ]; then
    echo "gate: FAILED — GATE_SIDE=linux and the Linux side is not set up ($1); nothing ran"
    MIRROR_RC=1
    return
  fi
  echo "gate: on the Windows side — the Linux side is not set up ($1)" >&2
  gate_side_note="on the Windows side, as the Linux side is not set up ($1); tree ${MS_TREE:0:12}"
  # A gate a guard starts inside this one stays on this side, without asking.
  export GATE_SIDE=windows GATE_TREE="$MS_TREE"
}

# mirror_tree — the worktree's tree id, its objects in the private store:
# MS_INDEX (the index's tree), MS_TREE (the working tree's, gate_tree_id), and
# from the same git call MS_HEAD and MS_BRANCH. 3 when HEAD is no commit.
mirror_tree() {
  local g=()
  [ -d "$MIRROR_W" ] || mkdir -p "$MIRROR_W" || return 1
  # The clone's common dir, this worktree's index, HEAD and the ref it names.
  mapfile -t g < <(git rev-parse --path-format=absolute --git-common-dir --git-path index \
                     HEAD --symbolic-full-name HEAD 2>/dev/null)
  # Not a checkout, or no commit yet: nothing to mirror (3).
  [[ "${g[2]:-}" =~ ^[0-9a-f]{40}$ ]] || return 3
  _mirror_wpath MIRROR_COMMONW "${g[0]}"
  MS_HEAD="${g[2]}" MS_BRANCH=""
  case "${g[3]:-}" in refs/heads/*) MS_BRANCH="${g[3]#refs/heads/}" ;; esac
  # The sync's own files (the bundle among them) live in .gate/mirror; were
  # .gate/ not ignored, each snapshot would carry the last one's bundle.
  git check-ignore -q .gate/mirror/spec || { echo "  .gate/ is not ignored (.gitignore)" >&2; return 1; }
  MIRROR_OBJ="$MIRROR_W/objects.git"
  if [ ! -d "$MIRROR_OBJ/objects" ]; then
    git init -q --bare "$MIRROR_OBJ" >/dev/null || return 1
  fi
  _mirror_wpath MIRROR_OBJW "$MIRROR_OBJ/objects"
  echo "$MIRROR_COMMONW/objects" >"$MIRROR_OBJ/objects/info/alternates"
  local -x GIT_OBJECT_DIRECTORY="$MIRROR_OBJW" GIT_ALTERNATE_OBJECT_DIRECTORIES="$MIRROR_COMMONW/objects"
  gate_tree_id "$MIRROR_W/tree.index" "${g[1]}" || return 1
  MS_INDEX="$GATE_TREE_INDEX" MS_TREE="$GATE_TREE_WORK"
}

# mirror_carry — after mirror_tree, what the Linux side needs beside it:
# MS_CONTENT (a commit of MS_TREE whose parents are HEAD and a commit of
# MS_INDEX, fixed dates: the same state gives the same sha), the branch's
# upstream, the refs the mirror carries ($MIRROR_W/refs, `refname sha` a line,
# also the private store's refs; MS_REFNAMES, MS_REFSHAS) and the caller's
# ignore rules.
mirror_carry() {
  local -x GIT_OBJECT_DIRECTORY="$MIRROR_OBJW" GIT_ALTERNATE_OBJECT_DIRECTORIES="$MIRROR_COMMONW/objects"
  local -x GIT_AUTHOR_NAME=gate-mirror GIT_AUTHOR_EMAIL=gate-mirror@localhost GIT_AUTHOR_DATE='@0 +0000'
  local -x GIT_COMMITTER_NAME=gate-mirror GIT_COMMITTER_EMAIL=gate-mirror@localhost GIT_COMMITTER_DATE='@0 +0000'
  local ic k v ex="" name sha pats=() base="" full
  local -A seen=()
  ic="$(git commit-tree "$MS_INDEX" -p "$MS_HEAD" -m 'gate mirror: the index' </dev/null)" || return 1
  MS_CONTENT="$(git commit-tree "$MS_TREE" -p "$MS_HEAD" -p "$ic" -m 'gate mirror: the working tree' </dev/null)" || return 1
  # The branch's upstream, origin's URL and the global excludes file: one call.
  MS_UP_REMOTE="" MS_UP_MERGE="" MS_ORIGIN_URL=""
  while read -r k v; do
    case "$k" in
      "branch.$MS_BRANCH.remote") MS_UP_REMOTE="$v" ;;
      "branch.$MS_BRANCH.merge") MS_UP_MERGE="$v" ;;
      remote.origin.url) MS_ORIGIN_URL="$v" ;;
      core.excludesfile) ex="$v" ;;
    esac
  done < <(git config --get-regexp '^(branch\..*\.(remote|merge)|remote\.origin\.url|core\.excludesfile)$' 2>/dev/null)
  # The remote's URL without any credentials in it; the mirror never pushes.
  [[ "$MS_ORIGIN_URL" =~ ^([a-z+]+://)[^/@]*@(.*)$ ]] && MS_ORIGIN_URL="${BASH_REMATCH[1]}${BASH_REMATCH[2]}"
  # The refs: every remote-tracking ref and tag, the branch, a local upstream,
  # and the BASE of `full BASE` when the mirror would not have it by that name.
  pats=(refs/remotes refs/tags)
  [ "$MS_UP_REMOTE" = . ] && [ -n "$MS_UP_MERGE" ] && pats+=("$MS_UP_MERGE")
  if [ "${MIRROR_MODE:-}" = full ] && [ -n "${MIRROR_BASE:-}" ] && [ "$MIRROR_BASE" != --unscoped ]; then
    full="$(git rev-parse -q --symbolic-full-name "$MIRROR_BASE" 2>/dev/null)"
    case "$full" in
      refs/remotes/*|refs/tags/*) ;;
      refs/*) pats+=("$full") ;;
      *) base="$(git rev-parse -q --verify "$MIRROR_BASE^{commit}" 2>/dev/null)" ;;
    esac
  fi
  MS_REFNAMES=(refs/gate-mirror/content) MS_REFSHAS=("$MS_CONTENT")
  [ -n "$MS_BRANCH" ] && MS_REFNAMES+=("refs/heads/$MS_BRANCH") MS_REFSHAS+=("$MS_HEAD")
  [ -n "$base" ] && MS_REFNAMES+=(refs/gate-mirror/base) MS_REFSHAS+=("$base")
  for name in "${MS_REFNAMES[@]}"; do seen[$name]=1; done
  while read -r name sha; do
    [ -n "$name" ] && [ -z "${seen[$name]:-}" ] || continue
    seen[$name]=1
    MS_REFNAMES+=("$name") MS_REFSHAS+=("$sha")
  done < <(git for-each-ref --format='%(if)%(symref)%(then)%(else)%(refname) %(objectname)%(end)' "${pats[@]}")
  local i lines=() upd=()
  for i in "${!MS_REFNAMES[@]}"; do
    lines+=("${MS_REFNAMES[$i]} ${MS_REFSHAS[$i]}")
    upd+=("update ${MS_REFNAMES[$i]} ${MS_REFSHAS[$i]}")
  done
  printf '%s\n' "${lines[@]}" >"$MIRROR_W/refs"
  # The private store's refs: what the bundle is cut from. A stale one there is
  # never bundled, since the bundle names its refs.
  printf '%s\n' "${upd[@]}" >"$MIRROR_W/refs.update"
  git --git-dir="$MIRROR_OBJ" update-ref --stdin <"$MIRROR_W/refs.update" || return 1
  # The caller's ignore rules, for the mirror to use as its own.
  _mirror_copy "$MIRROR_COMMONW/info/exclude" "$MIRROR_W/exclude"
  case "$ex" in "~/"*) ex="$HOME/${ex#"~/"}" ;; "") ex="${XDG_CONFIG_HOME:-$HOME/.config}/git/ignore" ;; esac
  _mirror_copy "$ex" "$MIRROR_W/global-excludes"
}

# mirror_bundle incr|full — $MIRROR_W/sync.bundle of the carried refs: incr
# leaves out what the mirror was last sent ($MIRROR_W/sent), full sends all.
# MS_BUNDLE is its path, or empty when there is nothing to send (incr with
# nothing new, or with no record of what the mirror has: the Linux side then
# answers need-full, or says first that it is not set up). An incr that git
# refuses (a sent object since collected) is sent full.
mirror_bundle() {
  local b="$MIRROR_W/sync.bundle" args=("${MS_REFNAMES[@]}") sent=() err=()
  MS_BUNDLE=""
  if [ "$1" = incr ]; then
    [ -s "$MIRROR_W/sent" ] || return 0
    mapfile -t sent <"$MIRROR_W/sent"
    args+=("${sent[@]/#/^}")
  fi
  if git --git-dir="$MIRROR_OBJ" bundle create -q "$b" "${args[@]}" 2>"$MIRROR_W/bundle.err"; then
    MS_BUNDLE=".gate/mirror/sync.bundle"
    return 0
  fi
  mapfile -t err <"$MIRROR_W/bundle.err"
  [[ "${err[*]}" == *"Refusing to create empty bundle"* ]] && return 0
  [ "$1" = incr ] && { mirror_bundle full; return; }
  printf '  %s\n' "${err[@]}"
  return 1
}

# mirror_write_spec ARGS — what the Linux side needs, as a file it sources:
# the snapshot, the bundle, the lock and timing flag paths (where gate_lock_dir
# and gate_timing_dir put them: GATE_LOCK_DIR / GATE_TIMING_DIR, else the
# clone's common dir), the gate's arguments, and the caller's exported GATE_*
# and SHINGO_* settings (paths that name a file on this side in C:/ form, for
# wslpath there).
mirror_write_spec() {
  # An empty array among the GATE_* names (GATE_EXEC_ARGS) reads as unset to
  # ${ref@a} under set -u; the option is this function's alone.
  local -
  set +u
  local v val src lock timing
  _mirror_wpath src "$ROOT"
  _mirror_wpath lock "${GATE_LOCK_DIR:-$MIRROR_COMMONW/gate.lock}"
  _mirror_wpath timing "${GATE_TIMING_DIR:-$MIRROR_COMMONW/gate.timing}"
  {
    printf 'MS_SOURCE=%q\nMS_NAME=%q\n' "$src" "${ROOT##*/}"
    printf 'MS_HEAD=%q\nMS_BRANCH=%q\nMS_TREE=%q\nMS_INDEX=%q\nMS_CONTENT=%q\n' \
      "$MS_HEAD" "$MS_BRANCH" "$MS_TREE" "$MS_INDEX" "$MS_CONTENT"
    printf 'MS_UP_REMOTE=%q\nMS_UP_MERGE=%q\nMS_ORIGIN_URL=%q\n' "$MS_UP_REMOTE" "$MS_UP_MERGE" "$MS_ORIGIN_URL"
    printf 'MS_LOCKDIR=%q\nMS_TIMINGDIR=%q\nMS_BUNDLE=%q\n' "$lock" "$timing" "$MS_BUNDLE"
    printf 'MS_ARGS=('; [ "$#" -gt 0 ] && printf '%q ' "$@"; printf ')\n'
    # The Linux side's test-set records land beside this side's (gate-testsets.sh).
    if [ "${GATE_TESTSET:-}" = on ] && [ -z "${GATE_TESTSET_DIR:-}" ]; then
      _mirror_wpath val "$ROOT/.gate/testsets"
      printf 'export GATE_TESTSET_DIR=%q\n' "$val"
    fi
    for v in ${!GATE_@} ${!SHINGO_@}; do
      declare -n _mw_ref="$v"
      [[ "${_mw_ref@a}" == *x* ]] || continue
      case "$v" in
        GATE_SIDE|GATE_LINUX_EXEC|GATE_IN_MIRROR|GATE_MIRROR_TREE|GATE_MIRROR_SOURCE|GATE_LOCK_DIR|GATE_TIMING_DIR) continue ;;
      esac
      val="$_mw_ref"
      case "$v" in
        GATE_STEPS_LOG|GATE_DOCKER_ORDER_LOG|GATE_MANIFEST_DIR|GATE_TEST_LOG_DIR|GATE_TESTSET_DIR)
          [ -n "$val" ] && _mirror_wpath val "$val" ;;
      esac
      printf 'export %s=%q\n' "$v" "$val"
    done
    unset -n _mw_ref
  } >"$MIRROR_W/spec"
}

# mirror_prelock — before a heavy mode takes the lock: in a mirror, the lock
# and the timing flag must be the Windows clone's (read from the mirror's
# config at gate.sh's top) and the Linux side's tools sourced; a mirror that
# records no lock would take a private one, which is the one thing it must not.
mirror_prelock() {
  [ -n "${GATE_IN_MIRROR:-}" ] || return 0
  if [ -z "${GATE_LOCK_DIR:-}" ]; then
    echo "gate: FAILED — this mirror records no gate lock (git config gate.lockdir); a private lock would let it run beside the Windows side's gates. Run the gate from the Windows worktree once to sync it."
    return 1
  fi
  if [ -n "${GATE_LINUX_ENV_MISSING:-}" ]; then
    echo "gate: FAILED — the Linux side is not set up: no $GATE_LINUX_ENV_MISSING (bash scripts/gate-linux-setup.sh)"
    return 1
  fi
}

# mirror_inner — in the mirror's own gate (GATE_IN_MIRROR, set by gate.sh when
# its tree is a mirror): before any step, its tree must be the tree the caller
# named. A gate started here by hand names its tree and says it synced nothing.
mirror_inner() {
  local want="${GATE_MIRROR_TREE:-}" got src
  # A gate a guard starts inside this one is a hand-run, not this one.
  unset GATE_MIRROR_TREE
  src=""
  read -r src <"$ROOT/.git/gate-mirror" 2>/dev/null
  got=""
  gate_tree_id "$ROOT/.git/gate-tree-index.$$" && got="$GATE_TREE_WORK"
  rm -f "$ROOT/.git/gate-tree-index.$$"
  if [ -z "$want" ]; then
    gate_side_note="on the Linux side, started by hand in the mirror of ${src:-?} (synced by no caller this run); tree ${got:0:12}"
    GATE_TREE="$got"
    return
  fi
  GATE_TREE="$want"
  if [ "$got" != "$want" ]; then
    echo "gate: FAILED — the mirror's tree is ${got:-unreadable}, the worktree's is $want: the mirror is not the working tree; nothing ran"
    [ -n "$got" ] && git diff-tree -r --name-status "$want" "$got" 2>/dev/null | head -20 | sed 's/^/    /'
    MIRROR_RC=1
    return
  fi
  gate_side_note="on the Linux side (the WSL mirror $ROOT of ${GATE_MIRROR_SOURCE:-$src}); tree ${want:0:12}"
}

# ── The Linux side's half (run as a script) ────────────────────────────────

# mirror_linux_run SPEC — in the worktree (cwd): check the setup, then sync the
# mirror and exec its gate. Writes the outcome to .gate/mirror/status for the
# caller: missing: <items> | need-full | refused | syncing (stopped mid-way)
# | running (the gate is exec'd; the exit code is the gate's).
mirror_linux_run() {
  local spec="$1" wt st out rc root m synced now wtix
  wt="$(pwd)"
  st="$wt/.gate/mirror/status"
  # shellcheck source=/dev/null
  . "$spec" || { echo "gate: cannot read $spec"; return 1; }
  # 1. The setup (lane wsl's scripts/gate-linux-setup.sh --check).
  local setup="${GATE_LINUX_SETUP:-scripts/gate-linux-setup.sh}"
  if [ -f "$setup" ]; then
    out="$(bash "$setup" --check 2>&1)"; rc=$?
  else
    out="missing: $setup (not in this tree)"; rc=1
  fi
  if [ "$rc" -ne 0 ]; then
    out="$(printf '%s\n' "$out" | tr -d '\r' | sed -n 's/^missing:[[:space:]]*//p' | tail -1)"
    printf 'missing: %s\n' "${out:-$setup --check exited $rc and named nothing}" >"$st"
    return 0
  fi
  # 2. The mirror, and whose it is.
  root="${GATE_MIRROR_ROOT:-$HOME/gate-mirrors}"
  m="$root/$MS_NAME"
  if [ -f "$m/.git/gate-mirror" ]; then
    out="$(cat "$m/.git/gate-mirror")"
    if [ "$out" != "$MS_SOURCE" ]; then
      echo "gate: REFUSED — the mirror $m belongs to $out, not to $MS_SOURCE: two worktrees never share one (GATE_MIRROR_ROOT names another root); nothing ran"
      echo refused >"$st"
      return 0
    fi
  elif [ -e "$m" ]; then
    echo "gate: REFUSED — $m exists and is not a gate mirror; nothing was changed and nothing ran"
    echo refused >"$st"
    return 0
  else
    if [ -z "$MS_BUNDLE" ]; then echo need-full >"$st"; return 0; fi
    echo "gate: making the mirror $m (the first sync carries the history once)" >&2
    mkdir -p "$m" && git -C "$m" -c init.defaultBranch=gate-mirror init -q \
      && printf '%s\n' "$MS_SOURCE" >"$m/.git/gate-mirror" || { echo "gate: cannot make $m"; echo syncing >"$st"; return 0; }
  fi
  echo syncing >"$st"
  cd "$m" || return 0
  # 3. Hand edits since the last sync: refused, never overwritten. The tree
  # the mirror holds is the last synced one, or the target of a sync that
  # stopped after it wrote the working tree (refs/gate-mirror/syncing, set
  # before step 6): that one is carried on from, not taken for a hand edit.
  local base="" target=""
  synced="$(git rev-parse -q --verify 'refs/gate-mirror/synced^{tree}')" || synced=""
  target="$(git rev-parse -q --verify 'refs/gate-mirror/syncing^{tree}')" || target=""
  if [ -n "$synced" ] || [ -n "$target" ]; then
    now=""
    gate_tree_id "$m/.git/gate-tree-index.$$" && now="$GATE_TREE_WORK"
    rm -f "$m/.git/gate-tree-index.$$"
    if [ -n "$now" ] && [ "$now" = "$synced" ]; then base="$synced"
    elif [ -n "$now" ] && [ "$now" = "$target" ]; then base="$target"
    else
      echo "gate: REFUSED — the mirror $m was changed by hand since its last sync (or a sync stopped part-way through writing it); nothing was overwritten and nothing ran:"
      if [ -n "$now" ]; then git diff-tree -r --name-status "${synced:-$target}" "$now" | head -40 | sed 's/^/    /'
      else echo "    (its tree could not be read)"; fi
      echo "  Keep what you need from it, then delete the mirror ($m); the next gate makes it again."
      echo refused >"$st"
      return 0
    fi
  fi
  # 4. The objects.
  if [ -n "$MS_BUNDLE" ]; then
    if ! git bundle verify -q "$wt/$MS_BUNDLE" >/dev/null 2>&1; then echo need-full >"$st"; return 0; fi
    git fetch -q --no-tags --no-write-fetch-head "$wt/$MS_BUNDLE" '+refs/*:refs/gate-mirror/incoming/*' \
      || { echo "gate: the mirror could not fetch $MS_BUNDLE"; return 0; }
  fi
  if cut -d' ' -f2 "$wt/.gate/mirror/refs" | git cat-file --batch-check | grep -q ' missing$'; then
    echo need-full >"$st"
    return 0
  fi
  # 5. The refs: exactly the carried set, then HEAD and its upstream as there.
  { git for-each-ref --format='%(refname)' refs/heads refs/remotes refs/tags refs/gate-mirror \
      | awk 'NR == FNR { keep[$1] = 1; next } !keep[$1] && $1 != "refs/gate-mirror/synced" && $1 != "refs/gate-mirror/syncing" { print "delete " $1 }' \
          "$wt/.gate/mirror/refs" -
    awk '{ print "update " $1 " " $2 }' "$wt/.gate/mirror/refs"
  } | git update-ref --stdin || { echo "gate: the mirror's refs could not be set"; return 0; }
  if [ -n "$MS_BRANCH" ]; then
    git symbolic-ref HEAD "refs/heads/$MS_BRANCH" || return 0
    if [ -n "$MS_UP_REMOTE" ]; then
      git config "branch.$MS_BRANCH.remote" "$MS_UP_REMOTE" && git config "branch.$MS_BRANCH.merge" "$MS_UP_MERGE"
    else
      git config --unset "branch.$MS_BRANCH.remote"; git config --unset "branch.$MS_BRANCH.merge"
    fi
  else
    git update-ref --no-deref HEAD "$MS_HEAD" || return 0
  fi
  if [ -n "$MS_ORIGIN_URL" ]; then
    git config remote.origin.url "$MS_ORIGIN_URL"
    git config remote.origin.fetch '+refs/heads/*:refs/remotes/origin/*'
    git config remote.origin.pushurl 'the-gate-mirror-never-pushes'
  fi
  # 6. The working tree, from the tree the mirror holds (step 3) to the new one
  # through an index of it: unchanged files are not touched (their mtimes stay).
  # The target is recorded first, so a sync that stops after this step is
  # carried on from by the next (step 3).
  git update-ref refs/gate-mirror/syncing "$MS_CONTENT" || return 0
  wtix="$m/.git/gate-wt-index"
  if [ -n "$base" ] && [ -e "$wtix" ]; then
    GIT_INDEX_FILE="$wtix" git update-index -q --refresh >/dev/null 2>&1
    GIT_INDEX_FILE="$wtix" git read-tree -m -u "$base" "$MS_TREE" \
      || { echo "gate: the mirror's working tree could not be moved to $MS_TREE"; return 0; }
  else
    rm -f "$wtix"
    GIT_INDEX_FILE="$wtix" git read-tree -m -u "$MS_TREE" \
      || { echo "gate: the mirror's working tree could not be written (delete $m and run again)"; return 0; }
  fi
  # 7. The index, as the worktree's (keeps the stat data of unchanged entries).
  # -i: the working tree is already right (step 6), so an entry whose file had
  # unstaged edits must not stop the index moving (a plain -m refused it).
  git read-tree -m -i "$MS_INDEX" || { echo "gate: the mirror's index could not be set"; return 0; }
  git update-index -q --refresh >/dev/null 2>&1
  # 8. The caller's ignore rules, lock and timing flag; what was synced.
  cp "$wt/.gate/mirror/exclude" .git/info/exclude 2>/dev/null || : >.git/info/exclude
  cp "$wt/.gate/mirror/global-excludes" .git/gate-global-excludes 2>/dev/null || : >.git/gate-global-excludes
  git config core.excludesFile "$m/.git/gate-global-excludes"
  git config gate.lockdir "$(_mirror_lpath "$MS_LOCKDIR")"
  git config gate.timingdir "$(_mirror_lpath "$MS_TIMINGDIR")"
  git update-ref refs/gate-mirror/synced "$MS_CONTENT" || return 0
  git update-ref -d refs/gate-mirror/syncing
  # 9. The gate.
  local v
  for v in GATE_STEPS_LOG GATE_DOCKER_ORDER_LOG GATE_MANIFEST_DIR GATE_TEST_LOG_DIR GATE_TESTSET_DIR; do
    [ -n "${!v:-}" ] && export "$v=$(_mirror_lpath "${!v}")"
  done
  export GATE_MIRROR_TREE="$MS_TREE" GATE_MIRROR_SOURCE="$MS_SOURCE"
  GATE_LOCK_DIR="$(_mirror_lpath "$MS_LOCKDIR")"
  GATE_TIMING_DIR="$(_mirror_lpath "$MS_TIMINGDIR")"
  export GATE_LOCK_DIR GATE_TIMING_DIR
  unset GATE_LINUX_EXEC GATE_SIDE
  echo running >"$st"
  echo "$$" >"$wt/.gate/mirror/inner.pid"
  exec bash "$m/scripts/gate.sh" "${MS_ARGS[@]}"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  case "${1:-}" in
    linux-run) shift; mirror_linux_run "$@"; exit $? ;;
    *) echo "usage: bash scripts/gate-mirror.sh linux-run SPEC   (the Linux side's half; gate.sh runs it)" >&2; exit 2 ;;
  esac
fi
