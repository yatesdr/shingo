# sync-checkout.sh — sourced by install-core.sh and install-edge.sh; not run on
# its own. It replaces the bare `git pull` both installers ran.
#
# A PLANT CHECKOUT IS A BUILD INPUT, NOT A WORKSPACE. Plants run main, and what
# a box builds is what origin has. So before a build the checkout is made to
# match origin, and anything that would be thrown away is saved first:
#
#   1. Local edits (changed or untracked files) are saved to
#      ~<owner>/shingo-backups/dirty-checkout-<UTC>/ (status.txt, tracked.patch,
#      untracked.tgz, HEAD), then discarded. Ignored files stay: the installers'
#      config discovery reads them.
#   2. The branch is set to origin's copy of it. Commits origin does not have (a
#      local commit, or a main origin rewrote) are saved as local-commits.bundle
#      first.
#   3. A branch other than main is built, with a loud warning here and on the
#      install summary: plants should not, but it can happen, and a refusal
#      would strand a box mid-change. A branch origin has never seen is built
#      at its committed HEAD, as is.
#   4. The checkout goes back to its owner (setup / pi): the installers run as
#      root, and root-owned objects broke a later non-root `git fetch` at HK.
#
# Run by hand, each discard asks first ("Nothing changed" on a no); with --yes
# it goes ahead, because the copy is saved. Why: SPR 2026-11-12, a Core install
# stopped at "cannot pull with rebase: You have unstaged changes" -- 959 files
# rewritten under the checkout the day before, and the box sets pull.rebase.
# A history rewrite on origin used to need a hand `git reset --hard` too.
#
# Callers provide confirm() and ASSUME_YES and run from the checkout root.
# SYNC_BACKUP_ROOT overrides where the copies go (the guard's test uses it).

# sync_checkout_backup_dir prints (and creates) this run's backup directory.
sync_checkout_backup_dir() {
    local owner home root
    owner=$(stat -c %U . 2>/dev/null || echo root)
    home=$(getent passwd "$owner" 2>/dev/null | cut -d: -f6 || true)
    root="${SYNC_BACKUP_ROOT:-${home:-/root}/shingo-backups}"
    mkdir -p "$root/dirty-checkout-$SYNC_TS"
    echo "$root/dirty-checkout-$SYNC_TS"
}

# sync_checkout makes the checkout's current branch match origin. A detached
# HEAD is the caller's to refuse (a tag install leaves one).
sync_checkout() {
    local branch upstream dir changed untracked ahead
    branch=$(git symbolic-ref -q --short HEAD)
    SYNC_TS=$(date -u +%Y%m%dT%H%M%SZ)
    dir=""

    echo "==> Fetching origin..."
    GIT_TERMINAL_PROMPT=0 git fetch --prune origin

    if [ -n "$(git status --porcelain)" ]; then
        changed=$(git status --porcelain --untracked-files=no | wc -l | tr -d ' ')
        untracked=$(git ls-files --others --exclude-standard | wc -l | tr -d ' ')
        dir=$(sync_checkout_backup_dir)
        git status --porcelain > "$dir/status.txt"
        git diff --binary HEAD > "$dir/tracked.patch"
        git ls-files -z --others --exclude-standard | tar --null -czf "$dir/untracked.tgz" -T -
        git rev-parse HEAD > "$dir/HEAD"
        echo "==> The checkout has local edits: $changed changed file(s), $untracked untracked."
        echo "    A plant builds what origin has. Saved to $dir"
        if ! confirm "Discard the local edits and continue?"; then
            echo "Nothing changed. Move the edits off the box (or commit them), then run this again."
            exit 1
        fi
        git reset -q --hard HEAD
        git clean -fdq
    fi

    if [ "$branch" != "main" ]; then
        echo "!! ================================================================"
        echo "!! This box builds branch '$branch', not main. Plants run main."
        echo "!! To go back: git checkout main, then run this installer again."
        echo "!! ================================================================"
    fi

    upstream="origin/$branch"
    if ! git rev-parse -q --verify "refs/remotes/$upstream" >/dev/null; then
        echo "!! Branch '$branch' is not on origin: nothing to sync. Building its"
        echo "!! committed HEAD ($(git rev-parse --short HEAD)) as is."
    else
        if ! git merge-base --is-ancestor HEAD "$upstream"; then
            ahead=$(git rev-list --count "$upstream..HEAD")
            dir=${dir:-$(sync_checkout_backup_dir)}
            git log --oneline "$upstream..HEAD" > "$dir/local-commits.txt"
            git bundle create -q "$dir/local-commits.bundle" "$upstream..HEAD"
            echo "==> $branch holds $ahead commit(s) $upstream does not (a local commit,"
            echo "    or origin rewrote the branch). Saved to $dir"
            if ! confirm "Reset $branch to $upstream?"; then
                echo "Nothing changed. Push or move those commits, then run this again."
                exit 1
            fi
        fi
        echo "==> Setting $branch to $upstream ($(git rev-parse --short "$upstream"))..."
        git reset -q --hard "$upstream"
    fi

    if [ "$(id -u)" = "0" ]; then
        local owner group
        owner=$(stat -c %U .)
        group=$(stat -c %G .)
        if [ "$owner" != "root" ]; then
            chown -R "$owner:$group" .
            if [ -n "$dir" ]; then
                chown -R "$owner:$group" "$(dirname "$dir")"
            fi
        fi
    fi
    return 0
}

# sync_checkout_branch_line prints the install summary's branch line, loud when
# the box is not on main.
sync_checkout_branch_line() {
    local branch
    branch=$(git symbolic-ref -q --short HEAD 2>/dev/null || echo "detached at $(git describe --tags --always 2>/dev/null)")
    if [ "$branch" = "main" ]; then
        echo " Branch:  main"
    else
        echo " Branch:  $branch   <-- NOT main"
    fi
}
