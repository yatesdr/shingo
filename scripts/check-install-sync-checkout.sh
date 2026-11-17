#!/usr/bin/env bash
# tier: dev
# inputs: deploy/sync-checkout.sh install-core.sh install-edge.sh scripts/check-install-sync-checkout.sh
# check-install-sync-checkout.sh — both installers pick what a box builds (the
# newest release, --tag's, or --main's) and set the plant checkout to it,
# saving what they discard and printing the Deploy: lines it crosses
# (deploy/sync-checkout.sh).
#
# Runs the shared code against a throwaway origin and a "plant" clone, one case
# per shape a box has been found in: behind origin; left detached at a release
# by an earlier tag install (SPR 2026-11-17); local edits (SPR 2026-11-12, 959
# files rewritten under the checkout); a local commit on main; a main origin
# rewrote; a branch only the box has; a rollback; a tag that predates tag
# installs; a hand-run "no". And both installers call it instead of pulling.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SYNC="$ROOT/deploy/sync-checkout.sh"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
fails=0
cases=0
fail() { echo "FAIL: $*"; fails=$((fails + 1)); }

g() { git -c user.name=gate -c user.email=gate@example.invalid -c init.defaultBranch=main -c advice.detachedHead=false "$@"; }

# changelog SECTION... — a CHANGELOG.md whose sections are SECTION (a version
# or Unreleased), newest first, each with a Deploy: line naming it.
changelog() {
    printf '# Changelog\n'
    local s
    for s in "$@"; do
        printf '\n## %s — 2026-11-17\n\nDeploy: notes of %s.\n\n- a change\n' "$s" "$s"
    done
}

# fresh CASE — a bare origin whose main runs v0.2.0 (an installer that takes no
# --tag), v0.9.0, v0.10.0, then one commit past it with an Unreleased section;
# the plant box is cloned when v0.9.0 was the tip, so the later tags reach it
# only by the fetch. v0.10.0 after v0.9.0 is the version sort a plain `sort`
# gets wrong.
fresh() {
    local c="$T/$1"
    mkdir -p "$c/work"
    g init -q --bare "$c/origin.git"
    (cd "$c/work" && g init -q && printf 'a\n' > f.txt && printf '*.db\n' > .gitignore &&
        printf 'old installer\n' > install-core.sh && changelog v0.2.0 > CHANGELOG.md &&
        g add . && g commit -qm one && g tag v0.2.0 &&
        printf 'case "$1" in --tag) ;; esac\n' > install-core.sh && changelog v0.9.0 v0.2.0 > CHANGELOG.md &&
        printf 'b\n' >> f.txt && g commit -qam two && g tag v0.9.0 &&
        g remote add origin "$c/origin.git" && g push -q origin main --tags)
    g clone -q "$c/origin.git" "$c/box"
    (cd "$c/work" && printf 'c\n' >> f.txt && changelog v0.10.0 v0.9.0 v0.2.0 > CHANGELOG.md &&
        g commit -qam three && g tag v0.10.0 &&
        printf 'd\n' >> f.txt && changelog Unreleased v0.10.0 v0.9.0 v0.2.0 > CHANGELOG.md &&
        g commit -qam four && g push -q origin main --tags)
    echo "$c"
}

# run CASE ASSUME_YES TAG MAIN [INPUT] — the installers' calls, in a subshell, as
# they make them: set -euo pipefail, confirm() and ASSUME_YES theirs.
run() {
    local c="$1"
    cases=$((cases + 1))
    (
        cd "$c/box" || exit 99
        export SYNC_BACKUP_ROOT="$c/backups"
        ASSUME_YES="$2"
        confirm() {
            if [ "$ASSUME_YES" = "yes" ]; then echo "$1 [auto-yes]"; return 0; fi
            read -r -p "$1 [y/N] " ans
            case "$ans" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
        }
        set -euo pipefail
        # shellcheck source=deploy/sync-checkout.sh
        . "$SYNC"
        sync_checkout_pick install-core.sh "$3" "$4"
        sync_checkout "$SYNC_TARGET"
        sync_checkout_notes "$SYNC_FROM"
        sync_checkout_source_line
    ) <<< "${5:-}" > "$c/out.txt" 2>&1
}

at() { git -C "$1/box" rev-parse HEAD; }
ref() { git -C "$1/box" rev-parse "$2^{commit}"; }
detached() { ! git -C "$1/box" symbolic-ref -q HEAD >/dev/null; }
clean() { [ -z "$(git -C "$1/box" status --porcelain)" ]; }
saved() { ls "$1"/backups/dirty-checkout-*/"$2" >/dev/null 2>&1; }
says() { grep -qE -- "$2" "$1/out.txt"; }

# 1. Behind origin, plain run: the newest release by version (v0.10.0, not
#    v0.9.0), fetched, detached; nothing saved; the notes bring v0.10.0 only.
c=$(fresh behind)
run "$c" yes "" no || fail "behind: exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$(ref "$c" v0.10.0)" ] || fail "behind: HEAD is not v0.10.0"
detached "$c" || fail "behind: HEAD is on a branch, not the release"
[ ! -d "$c/backups" ] || fail "behind: saved a copy with nothing to save"
says "$c" 'Installing v0\.10\.0 \(the newest release\)' || fail "behind: does not say it installs the newest release"
says "$c" 'brings +v0\.10\.0 +Deploy: notes of v0\.10\.0' || fail "behind: no v0.10.0 Deploy line"
says "$c" 'v0\.9\.0 +Deploy' && fail "behind: printed the notes of v0.9.0, which the box had"
says "$c" 'unreleased' && fail "behind: printed main's unreleased notes on a release build"
says "$c" '^ Source:  release v0\.10\.0$' || fail "behind: summary is not 'Source:  release v0.10.0'"

# 2. Left detached at a release by an earlier tag install (SPR's Core at
#    v0.7.0): a plain run goes on to the newest release instead of stopping.
c=$(fresh detached)
(cd "$c/box" && g checkout -q --detach v0.2.0)
run "$c" yes "" no || fail "detached: exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$(ref "$c" v0.10.0)" ] || fail "detached: HEAD is not v0.10.0"
says "$c" 'brings +v0\.9\.0 .*' || fail "detached: no v0.9.0 Deploy line"
grep -n 'brings' "$c/out.txt" | head -1 | grep -q 'v0\.9\.0' || fail "detached: notes are not oldest first"

# 3. Local edits (the SPR shape): saved, discarded, at the release; an ignored
#    file stays.
c=$(fresh dirty)
(cd "$c/box" && printf 'edit\n' >> f.txt && printf 'x\n' > stray.txt && printf 'db\n' > live.db)
run "$c" yes "" no || fail "dirty: exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$(ref "$c" v0.10.0)" ] || fail "dirty: HEAD is not v0.10.0"
clean "$c" || fail "dirty: the checkout is not clean"
saved "$c" tracked.patch && grep -q '^+edit' "$c"/backups/dirty-checkout-*/tracked.patch ||
    fail "dirty: tracked.patch does not hold the edit"
saved "$c" untracked.tgz && tar -tzf "$c"/backups/dirty-checkout-*/untracked.tgz | grep -qx stray.txt ||
    fail "dirty: untracked.tgz does not hold stray.txt"
[ -f "$c/box/live.db" ] || fail "dirty: the ignored live.db was removed"

# 4. --main from a release: on main at origin's, with main's unreleased notes.
c=$(fresh main)
(cd "$c/box" && g checkout -q --detach v0.9.0)
run "$c" yes "" yes || fail "main: exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$(ref "$c" origin/main)" ] || fail "main: HEAD is not origin/main"
[ "$(git -C "$c/box" symbolic-ref -q --short HEAD)" = "main" ] || fail "main: not on branch main"
says "$c" 'brings +unreleased +Deploy: notes of Unreleased' || fail "main: no unreleased Deploy line"
says "$c" '^ Source:  main at v0\.10\.0-1-g[0-9a-f]+   <-- not a release$' || fail "main: summary does not say not a release"

# 5. --main with a local commit on main: saved as a bundle, main set to origin's.
c=$(fresh ahead)
(cd "$c/box" && printf 'local\n' > l.txt && g add l.txt && g commit -qm local)
run "$c" yes "" yes || fail "ahead: exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$(ref "$c" origin/main)" ] || fail "ahead: HEAD is not origin/main"
saved "$c" local-commits.bundle || fail "ahead: the local commit was not bundled"
says "$c" 'The checkout had 1 commit\(s\) this build does not' || fail "ahead: does not count the commit it drops"

# 6. Origin rewrote main: the box's main is not an ancestor; --main bundles it
#    and takes the new one.
c=$(fresh rewritten)
(cd "$c/box" && g fetch -q origin && g reset -q --hard origin/main)
(cd "$c/work" && g reset -q --hard HEAD~1 && printf 'e\n' >> f.txt && g commit -qam rewritten &&
    g push -qf origin main)
run "$c" yes "" yes || fail "rewritten: exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$(ref "$c" origin/main)" ] || fail "rewritten: HEAD is not the rewritten origin/main"
saved "$c" local-commits.bundle || fail "rewritten: the old commit was not bundled"

# 7. A branch only the box has: a plain run takes it to the release, the edit
#    saved; the branch keeps its commit.
c=$(fresh boxonly)
(cd "$c/box" && g checkout -qb hotfix && printf 'h\n' > h.txt && g add h.txt && g commit -qm hotfix &&
    printf 'wip\n' >> h.txt)
hotfix=$(git -C "$c/box" rev-parse hotfix)
run "$c" yes "" no || fail "boxonly: exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$(ref "$c" v0.10.0)" ] || fail "boxonly: HEAD is not v0.10.0"
[ "$(git -C "$c/box" rev-parse hotfix)" = "$hotfix" ] || fail "boxonly: the hotfix branch lost its commit"
saved "$c" tracked.patch || fail "boxonly: the uncommitted edit was not saved"

# 8. --tag back to an older release: the notes take off the newer one.
c=$(fresh rollback)
(cd "$c/box" && g fetch -q --tags origin && g checkout -q --detach v0.10.0)
run "$c" yes v0.9.0 no || fail "rollback: exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$(ref "$c" v0.9.0)" ] || fail "rollback: HEAD is not v0.9.0"
says "$c" 'Installing v0\.9\.0 \(asked for\)' || fail "rollback: does not say the tag was asked for"
says "$c" 'takes off +v0\.10\.0 +Deploy: notes of v0\.10\.0' || fail "rollback: no takes-off line for v0.10.0"

# 9. A tag whose installer takes no --tag, and a tag origin lacks: refused,
#    nothing moved.
c=$(fresh refused)
head_before=$(at "$c")
if run "$c" yes v0.2.0 no; then fail "refused: installed v0.2.0, whose installer takes no --tag"; fi
says "$c" 'predates' || fail "refused: does not say v0.2.0 predates tag installs"
if run "$c" yes v9.9.9 no; then fail "refused: installed v9.9.9, which origin lacks"; fi
says "$c" 'no tag v9\.9\.9 on origin' || fail "refused: does not say origin lacks v9.9.9"
[ "$(at "$c")" = "$head_before" ] || fail "refused: HEAD moved"

# 10. Run by hand and told no: exits non-zero and changes nothing.
c=$(fresh declined)
(cd "$c/box" && printf 'keep\n' >> f.txt)
head_before=$(at "$c")
if run "$c" no "" no n; then fail "declined: went ahead after a no"; fi
[ "$(at "$c")" = "$head_before" ] || fail "declined: HEAD moved"
grep -q '^keep$' "$c/box/f.txt" || fail "declined: the edit was discarded"

# 11. Both installers call the shared code, no longer pull or stop on a detached
#     HEAD, take --main, show the notes before "Proceed?" and the source line.
for f in install-core.sh install-edge.sh; do
    grep -q '^\. "\$REPO_ROOT/deploy/sync-checkout.sh"$' "$ROOT/$f" || fail "$f does not source deploy/sync-checkout.sh"
    grep -qE "^[[:space:]]+sync_checkout_pick $f \"\\\$INSTALL_TAG\" \"\\\$INSTALL_MAIN\"$" "$ROOT/$f" ||
        fail "$f does not pick with its own name"
    grep -qE '^[[:space:]]+sync_checkout "\$SYNC_TARGET"$' "$ROOT/$f" || fail "$f does not call sync_checkout"
    grep -qE '^[[:space:]]*git pull' "$ROOT/$f" && fail "$f still runs git pull"
    grep -q 'no branch to pull' "$ROOT/$f" && fail "$f still stops on a detached HEAD"
    grep -qE '^[[:space:]]+--main\)' "$ROOT/$f" || fail "$f does not take --main"
    grep -A1 '^sync_checkout_notes "\$SYNC_FROM"$' "$ROOT/$f" | grep -q 'confirm "Proceed?"' ||
        fail "$f does not show the notes right before Proceed?"
    grep -q '^sync_checkout_source_line$' "$ROOT/$f" || fail "$f's summary has no source line"
done

if [ "$fails" -gt 0 ]; then
    echo "check-install-sync-checkout: $fails failure(s)"
    exit 1
fi
echo "check-install-sync-checkout: ok ($cases runs, 11 cases)"
