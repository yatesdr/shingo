# sync-checkout.sh — sourced by install-core.sh and install-edge.sh; not run on
# its own. It picks what a box builds and sets the checkout to it.
#
# WHAT A BOX BUILDS. The newest release by default; `--tag vX.Y.Z` for one
# release (to pin a plant or roll it back); `--main` for origin's main (a change
# merged and not yet released). A release is cut from main only once CI's gates
# pass on it, so the newest release is the newest main that passed them: main
# runs ahead only by a push with no changelog lines, a release still being cut,
# or a gate that failed. Whatever the box is on, a branch or the tag an earlier
# install left, it goes to what is picked. Why: SPR 2026-11-17, a Core left
# detached at v0.7.0 by a tag install refused every plain run ("no branch to
# pull") until someone named a tag.
#
# A PLANT CHECKOUT IS A BUILD INPUT, NOT A WORKSPACE. What a box builds is what
# origin has, so before a build the checkout is set to it, and anything that
# would be thrown away is saved first:
#
#   1. Local edits (changed or untracked files) are saved to
#      ~<owner>/shingo-backups/dirty-checkout-<UTC>/ (status.txt, tracked.patch,
#      untracked.tgz, HEAD), then discarded. Ignored files stay: the installers'
#      config discovery reads them.
#   2. A release is checked out detached, so the branch the box was on keeps
#      its commits. For main, the local main is set to origin's; commits origin
#      does not have (a local commit, or a main origin rewrote) are saved as
#      local-commits.bundle first.
#   3. The checkout goes back to its owner (setup / pi): the installers run as
#      root, and root-owned objects broke a later non-root `git fetch` at HK.
#
# Run by hand, each discard asks first ("Nothing changed" on a no); with --yes
# it goes ahead, because the copy is saved. Why: SPR 2026-11-12, a Core install
# stopped at "cannot pull with rebase: You have unstaged changes" -- 959 files
# rewritten under the checkout the day before, and the box sets pull.rebase.
#
# Before the installer asks to go on, sync_checkout_notes prints the Deploy:
# line of every release the install brings or takes off: a jump across several
# releases is where one that wants the Edges first hides behind one that wants
# Core first (v0.7.0 -> v0.11.0 crosses both).
#
# Callers provide confirm() and ASSUME_YES and run from the checkout root.
# SYNC_BACKUP_ROOT overrides where the copies go (the guard's test uses it).

SYNC_RELEASE_RE='^v[0-9]+\.[0-9]+\.[0-9]+$'

# sync_checkout_describe COMMIT names a commit the way the build stamps it.
sync_checkout_describe() {
    git describe --tags --always "$1" 2>/dev/null || git rev-parse --short "$1"
}

# sync_checkout_latest_release prints the newest release tag, by version.
sync_checkout_latest_release() {
    local tags
    tags=$(git tag -l --sort=-v:refname)
    grep -E -m1 "$SYNC_RELEASE_RE" <<< "$tags" || true
}

# sync_checkout_pick INSTALLER TAG MAIN fetches origin and sets SYNC_TARGET to
# what this run builds: "main" when MAIN is yes, else TAG, else the newest
# release. SYNC_FROM is the commit the checkout is at before the run. A tag
# whose INSTALLER takes no --tag predates tag installs: its installer could not
# finish the run it is re-executed into, so it is refused before anything moves.
sync_checkout_pick() {
    local installer="$1" tag="$2" main="$3" why="asked for" body
    SYNC_FROM=$(git rev-parse HEAD)
    echo "==> Fetching origin..."
    GIT_TERMINAL_PROMPT=0 git fetch --prune --tags origin
    if [ "$main" = "yes" ]; then
        SYNC_TARGET=main
        echo "==> Building main at $(sync_checkout_describe origin/main), not a release;" \
            "the checkout is at $(sync_checkout_describe "$SYNC_FROM")"
        return 0
    fi
    if [ -z "$tag" ]; then
        tag=$(sync_checkout_latest_release)
        why="the newest release"
        if [ -z "$tag" ]; then
            echo "ERROR: origin has no release tag (vX.Y.Z). Build main with --main."
            exit 1
        fi
    fi
    if ! git rev-parse -q --verify "refs/tags/${tag}^{commit}" >/dev/null; then
        echo "ERROR: no tag $tag on origin."
        exit 1
    fi
    body=$(git show "refs/tags/${tag}:${installer}" 2>/dev/null || true)
    case "$body" in
        *'--tag)'*) ;;
        *)
            echo "ERROR: $tag has no $installer that takes --tag (it predates"
            echo "       tag installs), so it cannot install itself this way. Nothing has changed."
            exit 1
            ;;
    esac
    SYNC_TARGET="$tag"
    echo "==> Installing $tag ($why); the checkout is at $(sync_checkout_describe "$SYNC_FROM")"
    return 0
}

# sync_checkout_backup_dir prints (and creates) this run's backup directory.
sync_checkout_backup_dir() {
    local owner home root
    owner=$(stat -c %U . 2>/dev/null || echo root)
    home=$(getent passwd "$owner" 2>/dev/null | cut -d: -f6 || true)
    root="${SYNC_BACKUP_ROOT:-${home:-/root}/shingo-backups}"
    mkdir -p "$root/dirty-checkout-$SYNC_TS"
    echo "$root/dirty-checkout-$SYNC_TS"
}

# sync_checkout TARGET sets the checkout to TARGET: "main" (origin's) or a
# release tag, which sync_checkout_pick has fetched.
sync_checkout() {
    local target="$1" dir="" changed untracked ahead
    SYNC_TS=$(date -u +%Y%m%dT%H%M%SZ)

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

    if [ "$target" = "main" ]; then
        if git rev-parse -q --verify refs/heads/main >/dev/null &&
            ! git merge-base --is-ancestor refs/heads/main origin/main; then
            ahead=$(git rev-list --count origin/main..refs/heads/main)
            dir=${dir:-$(sync_checkout_backup_dir)}
            git log --oneline origin/main..refs/heads/main > "$dir/local-commits.txt"
            git bundle create -q "$dir/local-commits.bundle" origin/main..refs/heads/main
            echo "==> main holds $ahead commit(s) origin/main does not (a local commit,"
            echo "    or origin rewrote main). Saved to $dir"
            if ! confirm "Reset main to origin/main?"; then
                echo "Nothing changed. Push or move those commits, then run this again."
                exit 1
            fi
        fi
        echo "==> Setting main to origin/main ($(git rev-parse --short origin/main))..."
        git checkout -q -B main origin/main
    else
        echo "==> Checking out $target..."
        git checkout -q --detach "refs/tags/$target"
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

# sync_checkout_releases IN NOT_IN prints the release tags IN reaches and NOT_IN
# does not, oldest first.
sync_checkout_releases() {
    local a b
    a=$(git tag -l --merged "$1" | grep -E "$SYNC_RELEASE_RE" | LC_ALL=C sort || true)
    b=$(git tag -l --merged "$2" | grep -E "$SYNC_RELEASE_RE" | LC_ALL=C sort || true)
    LC_ALL=C comm -23 <(printf '%s\n' "$a") <(printf '%s\n' "$b") | grep -v '^$' | sort -V || true
}

# sync_checkout_deploy_line CHANGELOG SECTION prints the Deploy: line of the
# `## SECTION` heading (a version, or Unreleased) in CHANGELOG's text.
sync_checkout_deploy_line() {
    awk -v s="$2" '
        /^## / { in_s = ($2 == s); next }
        in_s && /^Deploy:/ && !done { print; done = 1 }
    ' <<< "$1"
}

# sync_checkout_notes FROM prints, against the commit the checkout was at before
# the run, the Deploy: line of every release this build brings (oldest first),
# of main's unreleased changes when it builds them, and of every release it takes
# off (newest first). FROM is empty when an older installer set the checkout
# and re-executed this one: that run has nothing to compare with.
sync_checkout_notes() {
    local from="$1" to log_to log_from tag line dropped
    to=$(git rev-parse HEAD)
    if [ -z "$from" ]; then
        echo "==> Building $(sync_checkout_describe "$to"), which an older installer checked out; no deploy notes."
        return 0
    fi
    if [ "$from" = "$to" ]; then
        return 0
    fi
    echo "==> Deploy notes, $(sync_checkout_describe "$from") -> $(sync_checkout_describe "$to"):"
    log_to=$(git show "$to:CHANGELOG.md" 2>/dev/null || true)
    log_from=$(git show "$from:CHANGELOG.md" 2>/dev/null || true)
    for tag in $(sync_checkout_releases "$to" "$from"); do
        line=$(sync_checkout_deploy_line "$log_to" "$tag")
        printf '    brings    %-10s %s\n' "$tag" "${line:-(no Deploy: line)}"
    done
    line=$(sync_checkout_deploy_line "$log_to" Unreleased)
    if [ -n "$line" ]; then
        printf '    brings    %-10s %s\n' unreleased "$line"
    fi
    for tag in $(sync_checkout_releases "$from" "$to" | sort -rV); do
        line=$(sync_checkout_deploy_line "$log_from" "$tag")
        printf '    takes off %-10s %s\n' "$tag" "${line:-(no Deploy: line)}"
    done
    dropped=$(git rev-list --count "$to..$from")
    if [ "$dropped" -gt 0 ]; then
        echo "    The checkout had $dropped commit(s) this build does not."
    fi
    echo "    A note that puts Core or the Edges first: that side goes first, at every box of the plant."
    return 0
}

# sync_checkout_source_line prints the install summary's source line, loud when
# the build is not a release.
sync_checkout_source_line() {
    local tag
    tag=$(git describe --tags --exact-match HEAD 2>/dev/null || true)
    if git symbolic-ref -q HEAD >/dev/null; then
        if [[ "$tag" =~ $SYNC_RELEASE_RE ]]; then
            echo " Source:  $(git symbolic-ref --short HEAD), at release $tag"
        else
            echo " Source:  $(git symbolic-ref --short HEAD) at $(sync_checkout_describe HEAD)   <-- not a release"
        fi
    elif [[ "$tag" =~ $SYNC_RELEASE_RE ]]; then
        echo " Source:  release $tag"
    else
        echo " Source:  $(sync_checkout_describe HEAD)   <-- not a release"
    fi
}
