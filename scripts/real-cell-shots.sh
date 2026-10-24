#!/usr/bin/env bash
#
# real-cell-shots.sh — photograph the cell picture, with the overlap check
# measured on every shot, so a picture change can be reviewed before it merges.
# Two sets, one script:
#
#   SPOTS_CELLS="spr=<path>;hk=<path>" bash scripts/real-cell-shots.sh OUTDIR
#   bash scripts/real-cell-shots.sh --fixtures [--no-u4] OUTDIR
#
# THE REAL CELLS (SPOTS_CELLS set). The data is named, never committed:
# SPOTS_CELLS points at the plant pulls, each a JSON file whose `cells` is keyed
# by station id, every entry a CellPicture as the Edge's
# GET /api/operator-stations/{id}/cell answered it. TestRealCellShots
# (shingo-edge/www/real_cells_shots_test.go) seeds a fresh Edge per station
# and serves the real app. For every station of every pull:
#
#   <plant>-<station>-relation-1280.png   the relation picture, nothing selected, HMI frame
#   <plant>-<station>-relation-1084.png   the same, the desktop's frame
#   <plant>-<station>-today-1280.png      today's picture, where the scope gate keeps it
#
# and for the review set (Springfield 18, Hopkinsville 9, Springfield 6):
#
#   <plant>-<station>-spots-1280.png          the HMI Spots step, nothing selected
#   <plant>-<station>-spots-<node>-1280.png   each place of the recipe card selected
#   <plant>-<station>-flow-1280.png           the board's flow panel
#   <plant>-<station>-draft-1440.png          the desktop recipe draft (Press 6)
#
# THE FIXTURES (--fixtures, or SPOTS_CELLS unset). TestFixtureShots
# (shingo-edge/www/fixture_shots_test.go) shoots synthetic cells committed in
# the test — the Press 6 shape with the grouped recipe, the Hopkinsville press
# shape under two recipes, a weld line, the fallback — the states beside the
# mock: the Spots step, Which recipe, Go, the flow panel and the desktop
# draft's states. A state another lane has not built yet is shot as
# <state>-TODO.png. And, unless --no-u4, plant A's board flow panel before and
# after: this script runs scripts/composer-shots.sh here and in a throwaway
# worktree at SPOTS_U4_BASE_REF (default 8096a742), and the test compares their
# u4- PNGs pixel for pixel.
#
# OUTDIR (or SPOTS_SHOTS_OUT) gets the PNGs and an INDEX.md. The pulls and
# OUTDIR must be OUTSIDE this repo: the PNGs carry cells on screen, and this
# script refuses a path inside it.
#
# Not CI, and not to be run beside another Chrome or `go test ./www` run:
# Chrome's load trips the suite's stall pins. Set COMPOSER_SHOTS_CHROME to a
# Chrome/Chromium binary if the default is not found.

set -uo pipefail
CALLER="$(pwd)"
cd "$(dirname "$0")/.."
ROOT="$(pwd)"

usage() {
  echo "usage: SPOTS_CELLS=\"spr=<path>;hk=<path>\" bash scripts/real-cell-shots.sh OUTDIR" >&2
  echo "       bash scripts/real-cell-shots.sh --fixtures [--no-u4] OUTDIR" >&2
  exit 2
}

cells="${SPOTS_CELLS:-}"
mode=cells
[ -n "$cells" ] || mode=fixtures
u4=1
args=()
for a in "$@"; do
  case "$a" in
    --fixtures) mode=fixtures ;;
    --no-u4) u4=0 ;;
    -*) usage ;;
    *) args+=("$a") ;;
  esac
done
out="${args[0]:-${SPOTS_SHOTS_OUT:-}}"
[ -n "$out" ] || usage

# Git Bash hands POSIX paths around and Go's exec wants native ones. Every
# path is made absolute against the CALLER's directory (this script has moved
# to the repo root by now) and compared in POSIX form, BEFORE anything is
# created: a refused folder must not be left behind inside the repo.
posix() { if command -v cygpath >/dev/null 2>&1; then cygpath -u "$1"; else printf '%s\n' "$1"; fi; }
native() { if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s\n' "$1"; fi; }
absolute() { (cd "$CALLER" && realpath -m -- "$(posix "$1")"); }
inside_repo() {
  case "$1" in "$ROOT"|"$ROOT"/*) return 0 ;; esac
  return 1
}

out="$(absolute "$out")"
if inside_repo "$out"; then
  echo "real-cell-shots: refusing $out — it is inside the repo, and the PNGs carry cells" >&2
  exit 2
fi
mkdir -p "$out" || { echo "real-cell-shots: cannot create $out" >&2; exit 1; }

spec=""
if [ "$mode" = cells ]; then
  IFS=';' read -r -a parts <<< "$cells"
  for part in "${parts[@]}"; do
    [ -n "$part" ] || continue
    key="${part%%=*}"
    path="${part#*=}"
    if [ "$key" = "$part" ] || [ -z "$path" ]; then
      echo "real-cell-shots: SPOTS_CELLS entry '$part' is not plant=path" >&2
      exit 2
    fi
    path="$(absolute "$path")"
    if [ ! -f "$path" ]; then
      echo "real-cell-shots: $key: no file at $path" >&2
      exit 2
    fi
    if inside_repo "$path"; then
      echo "real-cell-shots: refusing $path — a plant pull never lives inside the repo" >&2
      exit 2
    fi
    spec="${spec:+$spec;}$key=$(native "$path")"
  done
fi

# WHICH PICTURE: the commit, and the last commit that changed the code the
# picture is drawn by (a harness-only commit on top draws the same picture).
code=(shingo-edge shared protocol ':(exclude)*_test.go' ':(exclude)*.test.js')
tree="$(git rev-parse --short=8 HEAD 2>/dev/null || echo unknown)"
drawn="$(git log -1 --format=%h --abbrev=8 -- "${code[@]}" 2>/dev/null)"
[ -n "$drawn" ] && [ "$drawn" != "$tree" ] && tree="$tree (the picture's code as of $drawn)"
if [ -n "$(git status --porcelain -- "${code[@]}" 2>/dev/null)" ]; then
  tree="$tree, with uncommitted changes"
fi

tmp="$(mktemp -d)"
env_extra=()
if [ "$mode" = cells ]; then
  test_name=TestRealCellShots
  env_extra+=("SPOTS_CELLS=$spec" "SPOTS_SHOTS_OUT=$(native "$out")")
else
  test_name=TestFixtureShots
  env_extra+=("SPOTS_FIXTURES_OUT=$(native "$out")")
  if [ "$u4" = 1 ]; then
    # PLANT A, BEFORE AND AFTER: the composer harness's own run, twice. Its
    # exit code is its own checks' (two fail at 8096a742 already); what is used
    # here is the u4- PNGs it writes, and the test says if one is missing.
    base_ref="${SPOTS_U4_BASE_REF:-8096a742}"
    base_sha="$(git rev-parse --short=8 "$base_ref^{commit}" 2>/dev/null)" || { echo "real-cell-shots: no commit $base_ref" >&2; exit 2; }
    echo "real-cell-shots: plant A's u4 shots at this tree (composer-shots.sh, log $tmp/u4-tip.log)"
    bash "$ROOT/scripts/composer-shots.sh" "$tmp/u4-tip" > "$tmp/u4-tip.log" 2>&1 || echo "  composer-shots exited non-zero here; its u4 PNGs are still used"
    echo "real-cell-shots: plant A's u4 shots at $base_sha (a throwaway worktree, log $tmp/u4-base.log)"
    if git worktree add --detach "$tmp/base-tree" "$base_sha" > "$tmp/worktree.log" 2>&1; then
      bash "$tmp/base-tree/scripts/composer-shots.sh" "$tmp/u4-base" > "$tmp/u4-base.log" 2>&1 || echo "  composer-shots exited non-zero at the base; its u4 PNGs are still used"
      git worktree remove --force "$tmp/base-tree" >/dev/null 2>&1 || echo "  could not remove the worktree $tmp/base-tree" >&2
    else
      echo "real-cell-shots: could not make a worktree at $base_sha (see $tmp/worktree.log)" >&2
    fi
    env_extra+=("SPOTS_U4_BASE=$(native "$tmp/u4-base")" "SPOTS_U4_TIP=$(native "$tmp/u4-tip")" "SPOTS_U4_BASE_TREE=$base_sha")
  fi
fi

echo "real-cell-shots: $test_name at $tree into $out"
started="$(date +%s)"
log="$tmp/test.log"
# ONE LINE, no continuation (composer-shots.sh says why).
( cd "$ROOT/shingo-edge" && env "${env_extra[@]}" SPOTS_SHOTS_TREE="$tree" go test -tags shots -count=1 -timeout 60m -run "^$test_name\$" -v ./www/ ) 2>&1 | tee "$log"
rc=${PIPESTATUS[0]}

# A misspelled tag or -run pattern exits 0 with "no tests to run"; a skipped
# test exits 0 too. Neither is a run.
if ! grep -q "RUN.*$test_name" "$log" || grep -q -- "--- SKIP: $test_name" "$log"; then
  echo "real-cell-shots: FAILED — $test_name never ran (build tag, -run pattern or environment?)" >&2
  grep -E 'no tests to run|no test files|build constraints|SKIP' "$log" >&2
  exit 1
fi

index="$out/INDEX.md"
if [ ! -f "$index" ] || [ "$(date -r "$index" +%s)" -lt "$started" ]; then
  echo "real-cell-shots: FAILED — no fresh INDEX.md in $out (logs in $tmp)" >&2
  exit 1
fi
shots=$(ls -1 "$out"/*.png 2>/dev/null | wc -l)
echo "real-cell-shots: $shots PNG(s) and INDEX.md in $out"
grep -m1 -E '^Written by' "$index"
if [ "$rc" -ne 0 ]; then
  echo "real-cell-shots: FAILED (exit $rc) — see the test output above; logs in $tmp" >&2
  exit "$rc"
fi
if [ -f "$tmp/u4-tip.log" ]; then
  # composer-shots' own verdicts, which this run does not judge.
  echo "real-cell-shots: composer-shots' logs kept in $tmp (u4-tip.log, u4-base.log)"
  rm -rf "$tmp/u4-tip" "$tmp/u4-base"
else
  rm -rf "$tmp"
fi
echo "real-cell-shots: done"
