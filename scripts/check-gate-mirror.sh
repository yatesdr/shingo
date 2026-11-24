#!/usr/bin/env bash
# tier: dev
# inputs: scripts/gate*.sh scripts/golangci-run.sh scripts/check-gate-mirror.sh
# check-gate-mirror.sh — pins scripts/gate-mirror.sh, the gate's Linux side:
# a worktree's gate runs in a mirror that IS its working tree, under one lock.
#
# A fixture clone with a worktree, and its own copy of the gate scripts, runs
# real gates (simvet, which runs no go on a tree without sim files; test once).
# The "Linux side" is a local bash (GATE_LINUX_EXEC=local), so the whole sync
# runs on any OS, CI included; the wsl.exe crossing is the one thing it does
# not run. The setup check and ~/gate-tools/env.sh are stubs in the fixture.
#
#   1. a first sync: committed, staged, unstaged, untracked and deleted files
#      all arrive, the index reads as the worktree's (git status equal), no
#      ignored file is copied, a tracked-but-ignored one and the exec bit are,
#      HEAD, its branch, upstream, origin/main and tags are the worktree's;
#      the sentence names the Linux side and the worktree's tree id; the gate
#      ran nested under the caller's lock, with the Linux side's env sourced
#   2. a no-change sync and a one-file change keep the other files' mtimes
#      and the mirror's own .gate (steps log, reuse store), and a deletion
#      propagates
#   3. a file changed in the mirror by hand is refused, named, kept; nothing ran
#   4. a mirror whose tree is not the one named is red before any step
#   5. an uncommitted failing assertion reaches the mirror and runs red (G11)
#   6. GATE_SIDE=windows runs here and says so; the mirror is not touched
#   7. a Linux side that is not set up falls back, the sentence naming why;
#      GATE_SIDE=linux refuses instead
#   8. fmt (the pre-push hook's), scope and lock-status never sync
#   9. a gate started by hand in the mirror takes the caller's lock and its
#      timing flag, and without a recorded lock refuses to start
#  10. a second worktree with the same name does not share the mirror
#  11. a mirror deleted behind the caller's back is made again
#
# A process costs 0.1-0.4 s on the loaded Windows laptop, so this pin takes
# minutes there and seconds on the Linux side, where the gate now runs it.
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

TMP="$(mktemp -d 2>/dev/null || mktemp -d -t gatemirror)"
trap 'rm -f "$TMP/stop"; touch "$TMP/stop"; sleep 0.3; rm -rf "$TMP"' EXIT

fails=0
fail() { echo "  FAIL gate-mirror: $*"; fails=$((fails + 1)); }

# Nothing of a gate this pin may be running inside reaches the fixture's.
unset GATE_SIDE GATE_LOCK_HELD GATE_NO_LOCK GATE_IN_MIRROR GATE_MIRROR_TREE GATE_MIRROR_SOURCE \
      GATE_TIMING_TOKEN GATE_LINUX_SETUP GATETIME_BIN GATETIME_MANIFESTS GATETIME_IDENTITY \
      GATE_MODULES GATE_TEST_LOG_DIR GATE_DOCKER_ORDER_LOG GATE_MANIFEST_DIR
export GATE_LINUX_EXEC=local
export GATE_MIRROR_ROOT="$TMP/mirrors"
export GATE_LINUX_ENV="$TMP/env.sh"
export GATE_LOCK_DIR="$TMP/gate.lock" GATE_TIMING_DIR="$TMP/gate.timing-free"
export GATE_STEPS_LOG= GATE_EXEC=off
export GATE_LOCK_HEARTBEAT=1 GATE_LOCK_STALE=4 GATE_LOCK_POLL=0.3 GATE_LOCK_REPORT=1 GATE_LOCK_TIMEOUT=20
export GIT_AUTHOR_NAME=pin GIT_AUTHOR_EMAIL=pin@example.invalid GIT_COMMITTER_NAME=pin GIT_COMMITTER_EMAIL=pin@example.invalid
printf 'touch %q\n' "$TMP/env-sourced" >"$GATE_LINUX_ENV"

# ── The fixture ──────────────────────────────────────────────────────────
C="$TMP/clone" W="$TMP/wt/fixwt" M="$TMP/mirrors/fixwt"
g() { git -C "$C" -c core.autocrlf=false "$@"; }
w() { git -C "$W" -c core.autocrlf=false "$@"; }
mkdir -p "$C/scripts" "$C/protocol" "$C/shingo-core" "$C/docs" "$C/tools" "$TMP/wt"
# Every gate script, so a file gate.sh comes to source is in the fixture too;
# the setup check is the fixture's own stand-in, written after.
cp "$ROOT"/scripts/gate*.sh "$ROOT"/scripts/gate-os-tests.txt "$ROOT"/scripts/golangci-run.sh "$C/scripts/"
cat >"$C/scripts/gate-linux-setup.sh" <<'EOF'
#!/usr/bin/env bash
# The fixture's stand-in for lane wsl's setup check.
[ "${1:-}" = --check ] || exit 2
if [ -n "${FIXTURE_SETUP_MISSING:-}" ]; then echo "missing: $FIXTURE_SETUP_MISSING"; exit 1; fi
exit 0
EOF
printf 'module fixture/protocol\n\ngo 1.25.0\n' >"$C/protocol/go.mod"
printf 'package protocol\n\n// Answer is the answer.\nfunc Answer() int { return 42 }\n' >"$C/protocol/p.go"
printf 'package protocol\n\nimport "testing"\n\nfunc TestAnswer(t *testing.T) {\n\tif Answer() != 42 {\n\t\tt.Fatal("wrong answer")\n\t}\n}\n' >"$C/protocol/p_test.go"
printf 'module fixture/core\n\ngo 1.25.0\n' >"$C/shingo-core/go.mod"
printf '.gate/\n*.secret\n' >"$C/.gitignore"
printf '* text=auto eol=lf\n' >"$C/.gitattributes"
printf 'a\n' >"$C/docs/a.md"; printf 'b\n' >"$C/docs/b.md"; printf 'kept\n' >"$C/keep.secret"
printf '#!/bin/sh\necho run\n' >"$C/tools/run.sh"
g -c init.defaultBranch=main init -q
g add -A && g add -f keep.secret && g update-index --chmod=+x tools/run.sh scripts/gate-linux-setup.sh
g commit -qm base
g tag v0.0.1
g update-ref refs/remotes/origin/main HEAD
g config remote.origin.url "$TMP/nowhere.git"
g config remote.origin.fetch '+refs/heads/*:refs/remotes/origin/*'
g worktree add -q -b topic "$W" main
w config branch.topic.remote origin
w config branch.topic.merge refs/heads/main
printf '// one more line\n' >>"$W/protocol/p.go"; w commit -qam "topic commit"
w tag -a -m annotated v0.0.2
# The working state: staged, unstaged, untracked, deleted, ignored.
printf 'a staged\n' >"$W/docs/a.md"; w add docs/a.md
printf '// unstaged\n' >>"$W/protocol/p.go"
printf 'new\n' >"$W/docs/new.md"
rm "$W/docs/b.md"
printf 'secret\n' >"$W/docs/x.secret"

# tree_of DIR — the tree id the way gate-mirror.sh defines it, worked out here
# without it: the real index copied, `git add -A`, `git write-tree`.
tree_of() {
  local idx="$TMP/pin.index" real
  real="$(git -C "$1" rev-parse --git-path index)"
  case "$real" in /*|[A-Za-z]:*) ;; *) real="$1/$real" ;; esac
  rm -f "$idx"; cp "$real" "$idx"
  GIT_INDEX_FILE="$idx" git -C "$1" add -A && GIT_INDEX_FILE="$idx" git -C "$1" write-tree
}
# The nested gates do not inherit the caller's reuse switches: this pins the
# mirror, not reuse (scripts/check-reuse-wiring.sh does), and on GitHub
# (GITHUB_ACTIONS=true) the sentence gained "Reuse: off on GitHub." before "Ran
# on the Linux side", which the sentence checks below did not expect (red on
# GitHub at 14efcd43, green everywhere else).
gate() { (cd "$W" && env -u GITHUB_ACTIONS -u GATE_REUSE -u GATETIME_REUSE bash scripts/gate.sh "$@" 2>&1); }
mtime() { stat -c %Y "$1" 2>/dev/null || date -r "$1" +%s; }
synced() { git -C "$M" rev-parse -q --verify refs/gate-mirror/synced 2>/dev/null; }

# ── 1. a first sync ──────────────────────────────────────────────────────
want="$(tree_of "$W")"
out="$(gate simvet)"; rc=$?
{ [ "$rc" -eq 0 ] && echo "$out" | grep -q '^ok   sim compile'; } || fail "first sync: the gate did not run green in the mirror (exit $rc): $out"
echo "$out" | grep -q "^Gate: sim compile\. Ran on the Linux side (the WSL mirror .*fixwt of .*fixwt); tree ${want:0:12}\.$" \
  || fail "first sync: the sentence does not name the Linux side and the tree ${want:0:12}: $out"
[ -e "$TMP/env-sourced" ] || fail "the mirror's gate did not source the Linux side's env (GATE_LINUX_ENV)"
[ -e "$GATE_LOCK_DIR" ] && fail "the caller's lock was left behind"
[ "$(git -C "$M" rev-parse HEAD 2>/dev/null)" = "$(w rev-parse HEAD)" ] || fail "the mirror's HEAD is not the worktree's"
[ "$(git -C "$M" symbolic-ref -q --short HEAD 2>/dev/null)" = topic ] || fail "the mirror is not on the worktree's branch"
[ "$(git -C "$M" rev-parse -q --verify '@{upstream}' 2>/dev/null)" = "$(w rev-parse '@{upstream}')" ] \
  || fail "the mirror's upstream is not the worktree's"
for r in origin/main 'v0.0.1^{commit}' 'v0.0.2^{commit}'; do
  [ "$(git -C "$M" rev-parse -q --verify "$r" 2>/dev/null)" = "$(w rev-parse "$r")" ] || fail "the mirror lacks $r"
done
[ "$(git -C "$M" status --porcelain 2>/dev/null)" = "$(w status --porcelain)" ] \
  || fail "staged, unstaged, untracked and deleted do not read as in the worktree: mirror [$(git -C "$M" status --porcelain 2>&1)] worktree [$(w status --porcelain)]"
[ "$(cat "$M/docs/a.md" 2>/dev/null)" = "a staged" ] || fail "the staged edit did not arrive"
grep -q '// unstaged' "$M/protocol/p.go" 2>/dev/null || fail "the unstaged edit did not arrive"
[ -e "$M/docs/new.md" ] || fail "the untracked file did not arrive"
[ -e "$M/docs/b.md" ] && fail "a file deleted in the worktree is in the mirror"
[ -e "$M/docs/x.secret" ] && fail "an ignored file was copied into the mirror"
[ -e "$M/keep.secret" ] || fail "a tracked file that matches an ignore rule did not arrive"
git -C "$M" ls-files -s tools/run.sh 2>/dev/null | grep -q '^100755 ' || fail "the exec bit of tools/run.sh did not arrive"
[ "$(cd "$M" 2>/dev/null && tree_of "$M")" = "$want" ] || fail "the mirror's tree is not the worktree's"

# ── 2. mtimes and deletions: a no-change sync, then a change and a deletion ─
# The mirror's own .gate (steps log, reuse store) is no part of the tree and
# no sync may wipe it.
mkdir -p "$M/.gate/reuse" && echo kept >"$M/.gate/reuse/keep"
touch -d '@1000000000' "$M/docs/a.md" "$M/protocol/go.mod" 2>/dev/null
before="$(mtime "$M/docs/a.md")"
out="$(gate simvet)"; rc=$?
[ "$rc" -eq 0 ] || fail "a no-change sync went red (exit $rc): $out"
[ "$(mtime "$M/docs/a.md")" = "$before" ] || fail "a no-change sync rewrote an unchanged file (mtime $before -> $(mtime "$M/docs/a.md"))"
printf '// changed\n' >>"$W/protocol/p.go"
w rm -qf docs/a.md
out="$(gate simvet)"; rc=$?
[ "$rc" -eq 0 ] || fail "a sync of a change and a deletion went red (exit $rc): $out"
grep -q '// changed' "$M/protocol/p.go" 2>/dev/null || fail "a one-file change did not arrive"
[ "$(mtime "$M/protocol/go.mod")" = "$before" ] || fail "a one-file change rewrote an unchanged file"
[ -e "$M/docs/a.md" ] && fail "a file deleted in the worktree stayed in the mirror"
[ "$(git -C "$M" status --porcelain 2>/dev/null)" = "$(w status --porcelain)" ] || fail "after a deletion the status differs"
[ -e "$M/.gate/reuse/keep" ] || fail "a sync wiped the mirror's .gate (its steps log and reuse store live there)"
# An unstaged edit, then staged: the mirror's index entry moves under a file
# that already differed from it (found by the first real sync after a commit).
w add protocol/p.go
out="$(gate simvet)"; rc=$?
[ "$rc" -eq 0 ] || fail "a sync after staging an unstaged edit went red (exit $rc): $out"
[ "$(git -C "$M" status --porcelain 2>/dev/null)" = "$(w status --porcelain)" ] || fail "after staging an edit the status differs"
# A sync that stopped after it moved the working tree (as one did when its
# index step failed): the tree is the target, synced is the one before. Made
# by hand here; the next sync must carry on from it, not call it a hand edit.
prev="$(git -C "$M" rev-parse refs/gate-mirror/synced)"
printf '// after the stop\n' >>"$W/protocol/p.go"
out="$(gate simvet)"; rc=$?
[ "$rc" -eq 0 ] || fail "the sync before the stopped one went red (exit $rc): $out"
git -C "$M" update-ref refs/gate-mirror/syncing "$(git -C "$M" rev-parse refs/gate-mirror/synced)"
git -C "$M" update-ref refs/gate-mirror/synced "$prev"
printf '// next\n' >>"$W/protocol/p.go"
out="$(gate simvet)"; rc=$?
{ [ "$rc" -eq 0 ] && grep -q '// next' "$M/protocol/p.go"; } \
  || fail "a sync after one that stopped part-way was refused or wrong (exit $rc): $out"
[ "$(git -C "$M" status --porcelain 2>/dev/null)" = "$(w status --porcelain)" ] || fail "after a stopped sync the status differs"

# ── 3. a file changed in the mirror by hand ──────────────────────────────
printf '// by hand\n' >>"$M/protocol/p.go"
s0="$(synced)"
out="$(gate simvet)"; rc=$?
{ [ "$rc" -ne 0 ] && echo "$out" | grep -q 'REFUSED .*changed by hand' && echo "$out" | grep -q 'M[[:space:]]*protocol/p.go'; } \
  || fail "a hand edit in the mirror was not refused, naming the file (exit $rc): $out"
echo "$out" | grep -q '^ok   sim compile' && fail "a step ran over a hand-edited mirror: $out"
grep -q '// by hand' "$M/protocol/p.go" || fail "the hand edit was overwritten"
[ "$(synced)" = "$s0" ] || fail "a refused sync still moved the mirror"
# Put back as synced: case 5's sync is the proof that it is accepted again.
git -C "$M" show 'refs/gate-mirror/synced:protocol/p.go' >"$M/protocol/p.go"

# ── 4. a mirror whose tree is not the one named ──────────────────────────
other="$(git -C "$M" rev-parse 'HEAD^{tree}')"
out="$(cd "$M" && GATE_MIRROR_TREE="$other" bash scripts/gate.sh simvet 2>&1)"; rc=$?
{ [ "$rc" -ne 0 ] && echo "$out" | grep -q 'is not the working tree; nothing ran'; } \
  || fail "a mirror whose tree is not the named one was not red (exit $rc): $out"
echo "$out" | grep -q '^ok   sim compile' && fail "a step ran in a mirror whose tree is not the named one: $out"

# ── 5. an uncommitted failing assertion runs red in the mirror (G11) ───────
if command -v go >/dev/null 2>&1 && command -v node >/dev/null 2>&1; then
  sed -i 's/!= 42/!= 41/' "$W/protocol/p_test.go"
  out="$(gate test protocol)"; rc=$?
  { [ "$rc" -ne 0 ] && echo "$out" | grep -q -- '--- FAIL: TestAnswer' && echo "$out" | grep -q "mirrors/fixwt/.gate/test-protocol.log"; } \
    || fail "an uncommitted failing assertion did not run red in the mirror (exit $rc): $out"
  sed -i 's/!= 41/!= 42/' "$W/protocol/p_test.go"
else
  fail "case 5 needs go and node on PATH"
fi

# ── 6. GATE_SIDE=windows ─────────────────────────────────────────────────
printf 'side\n' >"$W/docs/side.md"
want="$(tree_of "$W")"; s0="$(synced)"
out="$(export GATE_SIDE=windows; gate simvet)"; rc=$?
{ [ "$rc" -eq 0 ] && echo "$out" | grep -q "Ran on the Windows side (GATE_SIDE=windows); tree ${want:0:12}\.$"; } \
  || fail "GATE_SIDE=windows did not run here naming the flag and the tree (exit $rc): $out"
{ [ "$(synced)" = "$s0" ] && [ ! -e "$M/docs/side.md" ]; } || fail "GATE_SIDE=windows touched the mirror"

# ── 7. a Linux side that is not set up ────────────────────────────────────
out="$(export FIXTURE_SETUP_MISSING='go1.25.0, docker'; gate simvet)"; rc=$?
{ [ "$rc" -eq 0 ] && echo "$out" | grep -q "Ran on the Windows side, as the Linux side is not set up (missing: go1.25.0, docker); tree ${want:0:12}\.$"; } \
  || fail "a missing Linux side did not fall back with the reason in the sentence (exit $rc): $out"
[ "$(synced)" = "$s0" ] || fail "a fallback touched the mirror"
out="$(export FIXTURE_SETUP_MISSING='go1.25.0' GATE_SIDE=linux; gate simvet)"; rc=$?
{ [ "$rc" -ne 0 ] && echo "$out" | grep -q 'GATE_SIDE=linux and the Linux side is not set up (missing: go1.25.0)'; } \
  || fail "GATE_SIDE=linux fell back instead of failing (exit $rc): $out"
echo "$out" | grep -q '^ok   sim compile' && fail "GATE_SIDE=linux ran a step on the Windows side: $out"

# ── 8. the light modes never sync ────────────────────────────────────────
for mode in fmt scope lock-status; do
  (export GATE_MIRROR_ROOT="$TMP/mirrors-light"; gate "$mode" >/dev/null)
  [ -e "$TMP/mirrors-light" ] && { fail "gate.sh $mode synced a mirror"; rm -rf "$TMP/mirrors-light"; }
done

# ── 9. a gate started by hand in the mirror takes the caller's lock ───────
rm -f "$TMP/stop"
bash -c '. "$1"; trap gate_lock_release EXIT; gate_lock_acquire "pin-holder" || exit 1
         while [ ! -e "$2/stop" ]; do sleep 0.2; done' _ "$ROOT/scripts/gate-lock.sh" "$TMP" >/dev/null 2>&1 &
holder=$!
i=0; until [ -e "$GATE_LOCK_DIR/heartbeat" ] || [ "$i" -ge 50 ]; do sleep 0.1; i=$((i + 1)); done
out="$(cd "$M" && env -u GATE_LOCK_DIR -u GATE_TIMING_DIR GATE_LOCK_TIMEOUT=2 bash scripts/gate.sh simvet 2>&1)"; rc=$?
{ [ "$rc" -ne 0 ] && echo "$out" | grep -q 'pin-holder' && echo "$out" | grep -q 'gate lock not acquired'; } \
  || fail "a gate started by hand in the mirror did not queue on the caller's lock (exit $rc): $out"
touch "$TMP/stop"; wait "$holder"
out="$(cd "$M" && env -u GATE_LOCK_DIR -u GATE_TIMING_DIR bash scripts/gate.sh simvet 2>&1)"; rc=$?
{ [ "$rc" -eq 0 ] && echo "$out" | grep -q 'Ran on the Linux side, started by hand in the mirror of'; } \
  || fail "a gate started by hand in the mirror did not run, or did not say so (exit $rc): $out"
# ... and the caller's timing flag: while a timed run holds it, a gate started
# by hand in the mirror refuses (the mirror records the flag, gate.timingdir).
mkdir -p "$GATE_TIMING_DIR"
printf 'holder=pin@host\npid=1\ncmd=gatetime run -- the pin\ntoken=pin-token\n' >"$GATE_TIMING_DIR/owner"
touch "$GATE_TIMING_DIR/heartbeat"
out="$(cd "$M" && env -u GATE_LOCK_DIR -u GATE_TIMING_DIR bash scripts/gate.sh simvet 2>&1)"; rc=$?
{ [ "$rc" -ne 0 ] && echo "$out" | grep -q 'REFUSED .*timed run' && echo "$out" | grep -q 'the pin'; } \
  || fail "a gate started by hand in the mirror ran beside the caller's timed run (exit $rc): $out"
rm -rf "$GATE_TIMING_DIR"
lock_was="$(git -C "$M" config --get gate.lockdir)"
git -C "$M" config --unset gate.lockdir
out="$(cd "$M" && env -u GATE_LOCK_DIR bash scripts/gate.sh simvet 2>&1)"; rc=$?
{ [ "$rc" -ne 0 ] && echo "$out" | grep -q 'records no gate lock'; } \
  || fail "a mirror that records no lock started a gate under a private one (exit $rc): $out"
git -C "$M" config gate.lockdir "$lock_was"

# ── 10. a second worktree with the same name ──────────────────────────────
g worktree add -q -b topic2 "$TMP/other/fixwt" main
out="$(cd "$TMP/other/fixwt" && bash scripts/gate.sh simvet 2>&1)"; rc=$?
{ [ "$rc" -ne 0 ] && echo "$out" | grep -q 'belongs to .*wt/fixwt'; } \
  || fail "a second worktree of the same name shared the mirror (exit $rc): $out"

# ── 11. a mirror deleted behind the caller's back ─────────────────────────
rm -rf "$M"
out="$(gate simvet)"; rc=$?
{ [ "$rc" -eq 0 ] && [ "$(synced)" != "" ]; } || fail "a deleted mirror was not made again (exit $rc): $out"

if [ "$fails" -eq 0 ]; then
  echo "ok   gate-mirror (the working tree arrives whole, mtimes kept, hand edits refused, tree checked, one lock, both sides)"
  exit 0
fi
exit 1
