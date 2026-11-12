#!/usr/bin/env bash
# tier: dev
# inputs: deploy/sync-checkout.sh install-core.sh install-edge.sh scripts/check-install-sync-checkout.sh
# check-install-sync-checkout.sh — both installers make the plant checkout match
# origin before they build, saving what they discard (deploy/sync-checkout.sh).
#
# Runs the shared sync against a throwaway origin and a "plant" clone, one case
# per shape a box has been found in: behind origin; local edits (SPR
# 2026-11-12, 959 files rewritten under the checkout, and pull.rebase set); a
# local commit; a main origin rewrote; a branch on origin; a branch only the box
# has; a hand-run "no". And both installers source the sync instead of pulling.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SYNC="$ROOT/deploy/sync-checkout.sh"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
fails=0
fail() { echo "FAIL: $*"; fails=$((fails + 1)); }

g() { git -c user.name=gate -c user.email=gate@example.invalid -c init.defaultBranch=main "$@"; }

# fresh CASE — a bare origin with main (two commits) and branch 'side', and a
# plant clone at main~1 with pull.rebase set, as SPR's box has it.
fresh() {
    local c="$T/$1"
    mkdir -p "$c/work"
    g init -q --bare "$c/origin.git"
    (cd "$c/work" && g init -q && printf 'a\n' > f.txt && printf '*.db\n' > .gitignore &&
        g add . && g commit -qm one && printf 'b\n' >> f.txt && g commit -qam two &&
        g remote add origin "$c/origin.git" && g push -q origin main &&
        g checkout -qb side && printf 's\n' > s.txt && g add s.txt && g commit -qm side &&
        g push -q origin side && g checkout -q main)
    g clone -q "$c/origin.git" "$c/box"
    (cd "$c/box" && g config pull.rebase true && g reset -q --hard HEAD~1)
    echo "$c"
}

# sync CASE ASSUME_YES [INPUT] — the installers' call, in a subshell, as they
# make it: set -euo pipefail, confirm() and ASSUME_YES theirs.
sync() {
    local c="$1"
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
        sync_checkout
        sync_checkout_branch_line
    ) <<< "${3:-}" > "$c/out.txt" 2>&1
}

at() { git -C "$1/box" rev-parse HEAD; }
ref() { git -C "$1/box" rev-parse "$2"; }
clean() { [ -z "$(git -C "$1/box" status --porcelain)" ]; }
saved() { ls "$1"/backups/dirty-checkout-*/"$2" >/dev/null 2>&1; }

# 1. Behind origin: brought to origin/main, nothing saved.
c=$(fresh behind)
sync "$c" yes || fail "behind: sync exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$(ref "$c" origin/main)" ] || fail "behind: HEAD is not origin/main"
[ ! -d "$c/backups" ] || fail "behind: saved a copy with nothing to save"
grep -q ' Branch:  main$' "$c/out.txt" || fail "behind: summary line is not 'Branch:  main'"

# 2. Local edits (the SPR shape): saved, discarded, at origin/main; an ignored
#    file stays.
c=$(fresh dirty)
(cd "$c/box" && printf 'edit\n' >> f.txt && printf 'x\n' > stray.txt && printf 'db\n' > live.db)
sync "$c" yes || fail "dirty: sync exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$(ref "$c" origin/main)" ] || fail "dirty: HEAD is not origin/main"
clean "$c" || fail "dirty: the checkout is not clean"
saved "$c" tracked.patch && grep -q '^+edit' "$c"/backups/dirty-checkout-*/tracked.patch ||
    fail "dirty: tracked.patch does not hold the edit"
saved "$c" untracked.tgz && tar -tzf "$c"/backups/dirty-checkout-*/untracked.tgz | grep -qx stray.txt ||
    fail "dirty: untracked.tgz does not hold stray.txt"
[ -f "$c/box/live.db" ] || fail "dirty: the ignored live.db was removed"

# 3. A local commit on main: saved as a bundle, main set to origin's.
c=$(fresh ahead)
(cd "$c/box" && g reset -q --hard origin/main && printf 'local\n' > l.txt && g add l.txt && g commit -qm local)
sync "$c" yes || fail "ahead: sync exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$(ref "$c" origin/main)" ] || fail "ahead: HEAD is not origin/main"
saved "$c" local-commits.bundle || fail "ahead: the local commit was not bundled"

# 4. Origin rewrote main: the box's main is not an ancestor; set to the new one.
c=$(fresh rewritten)
(cd "$c/box" && g reset -q --hard origin/main)
(cd "$c/work" && g checkout -q main && g reset -q --hard HEAD~1 && printf 'c\n' >> f.txt &&
    g commit -qam rewritten && g push -qf origin main)
sync "$c" yes || fail "rewritten: sync exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$(ref "$c" origin/main)" ] || fail "rewritten: HEAD is not the rewritten origin/main"
saved "$c" local-commits.bundle || fail "rewritten: the old commit was not bundled"

# 5. A branch on origin: built, with the warning and the summary saying so.
c=$(fresh side)
(cd "$c/box" && g checkout -q -b side --track origin/side && g reset -q --hard HEAD~1)
sync "$c" yes || fail "side: sync exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$(ref "$c" origin/side)" ] || fail "side: HEAD is not origin/side"
grep -q "builds branch 'side', not main" "$c/out.txt" || fail "side: no not-main warning"
grep -q 'Branch:  side   <-- NOT main' "$c/out.txt" || fail "side: summary does not say NOT main"

# 6. A branch only the box has: built at its committed HEAD, edits still saved.
c=$(fresh boxonly)
(cd "$c/box" && g checkout -qb hotfix && printf 'h\n' > h.txt && g add h.txt && g commit -qm hotfix &&
    printf 'wip\n' >> h.txt)
head_before=$(at "$c")
sync "$c" yes || fail "boxonly: sync exited non-zero: $(cat "$c/out.txt")"
[ "$(at "$c")" = "$head_before" ] || fail "boxonly: HEAD moved off the local branch's commit"
clean "$c" || fail "boxonly: the uncommitted edit was not discarded"
grep -q "is not on origin" "$c/out.txt" || fail "boxonly: no not-on-origin warning"

# 7. Run by hand and told no: exits non-zero and changes nothing.
c=$(fresh declined)
(cd "$c/box" && printf 'keep\n' >> f.txt)
head_before=$(at "$c")
if sync "$c" no n; then fail "declined: sync went ahead after a no"; fi
[ "$(at "$c")" = "$head_before" ] || fail "declined: HEAD moved"
grep -q '^keep$' "$c/box/f.txt" || fail "declined: the edit was discarded"

# 8. Both installers sync through the shared file and no longer pull.
for f in install-core.sh install-edge.sh; do
    grep -q '^\. "\$REPO_ROOT/deploy/sync-checkout.sh"$' "$ROOT/$f" || fail "$f does not source deploy/sync-checkout.sh"
    grep -qE '^[[:space:]]+sync_checkout$' "$ROOT/$f" || fail "$f does not call sync_checkout"
    grep -qE '^[[:space:]]*git pull' "$ROOT/$f" && fail "$f still runs git pull"
    grep -q '^sync_checkout_branch_line$' "$ROOT/$f" || fail "$f's summary has no branch line"
done

if [ "$fails" -gt 0 ]; then
    echo "check-install-sync-checkout: $fails failure(s)"
    exit 1
fi
echo "check-install-sync-checkout: ok (8 cases)"
