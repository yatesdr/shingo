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
#  12. the mirror keeps each run's logs, named by run, the newest 10; a red
#      run's last line names the kept log of the step that failed, and the
#      next run does not overwrite it (R48)
#  13. a SIGKILLed caller's mirror gate holds the lock to its end: a second
#      gate queues past the stale window, lock-status names the mirror's gate,
#      the lock frees as a dead holder's once it ends, its verdict is kept;
#      INT and TERM stop the mirror's gate and free the lock at once (R47)
#  14. the caller's private object store, past its size, loses what nothing
#      reaches (an hour old), keeps the tips the mirror was sent (after a
#      refused sync too), and the next bundle is still cut; below the size or
#      within the hour of its last look it is not touched (R50)
#  15. the Linux side removes a mirror whose worktree is gone, never the one
#      being synced, never when that worktree's parent cannot be read (R50)
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
      GATE_MODULES GATE_TEST_LOG_DIR GATE_DOCKER_ORDER_LOG GATE_MANIFEST_DIR GATE_RUN_KEPT
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

# ── 12. each run's logs, kept (R48) ───────────────────────────────────────
# Two red runs, then a green one, over ten planted older runs: each red run's
# log is kept under its own name and the next run leaves it as it was; the
# last line names the failing step's kept log; the newest 10 runs stay.
if command -v go >/dev/null 2>&1 && command -v node >/dev/null 2>&1; then
  kept_runs() { find "$M/.gate/runs" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | LC_ALL=C sort; }
  # last_log OUT — the kept log a red run's last line names, as a path.
  last_log() {
    printf '%s\n' "$1" | tail -1 \
      | sed -n 's/^gate: FAILED — unit: \(test-protocol\.log\)\. Logs of this run: \([^ ]*\).*/\2\/\1/p'
  }
  rm -rf "$M/.gate/runs"
  for i in 01 02 03 04 05 06 07 08 09 10; do mkdir -p "$M/.gate/runs/20000101T0000${i}Z-dev-1"; done
  sed -i 's/!= 42/!= 41/' "$W/protocol/p_test.go"
  out1="$(gate test protocol)"; rc1=$?
  log1="$(last_log "$out1")"
  { [ "$rc1" -ne 0 ] && [ -n "$log1" ] && grep -q -- '--- FAIL: TestAnswer' "$log1" 2>/dev/null; } \
    || fail "a red run's last line does not name its kept failing log (exit $rc1): $(printf '%s\n' "$out1" | tail -1) [log: ${log1:-none}]"
  case "$log1" in "$M"/.gate/runs/*) ;; *) fail "the kept log is not under the mirror's .gate/runs: ${log1:-none}" ;; esac
  sum1=""; [ -n "$log1" ] && sum1="$(cksum <"$log1" 2>/dev/null)"
  sed -i 's/!= 41/!= 40/' "$W/protocol/p_test.go"
  out2="$(gate test protocol)"; rc2=$?
  log2="$(last_log "$out2")"
  { [ "$rc2" -ne 0 ] && [ -n "$log2" ] && [ "$log2" != "$log1" ] && grep -q -- '--- FAIL: TestAnswer' "$log2" 2>/dev/null; } \
    || fail "a second red run did not keep its own log beside the first (exit $rc2): $(printf '%s\n' "$out2" | tail -1)"
  if [ -n "$sum1" ] && [ "$(cksum <"$log1" 2>/dev/null)" != "$sum1" ]; then
    fail "the next run overwrote the first red run's kept log ($log1)"
  fi
  sed -i 's/!= 40/!= 42/' "$W/protocol/p_test.go"
  out3="$(gate test protocol)"; rc3=$?
  [ "$rc3" -eq 0 ] || fail "the green run after the red ones went red (exit $rc3): $(printf '%s\n' "$out3" | tail -3)"
  runs="$(kept_runs)"
  [ "$(printf '%s\n' "$runs" | grep -c .)" -eq 10 ] || fail "not the newest 10 runs kept: $(printf '%s\n' "$runs" | sed 's#.*/##' | tr '\n' ' ')"
  printf '%s\n' "$runs" | grep -q '20000101T000001Z\|20000101T000002Z\|20000101T000003Z' \
    && fail "the oldest planted runs were not pruned: $(printf '%s\n' "$runs" | sed 's#.*/##' | tr '\n' ' ')"
  for l in "$log1" "$log2"; do
    case "$runs" in *"$(dirname "$l")"*) ;; *) fail "a red run's kept logs were pruned before older runs: $l" ;; esac
  done
  [ -e "$(printf '%s\n' "$runs" | tail -1)/test-protocol.log" ] || fail "the green run kept no log: $(printf '%s\n' "$runs" | tail -1)"
fi

# ── 13. a SIGKILLed caller: its mirror's gate holds the lock to its end (R47) ─
# A SIGKILL runs no EXIT trap: the caller's heartbeat stops with it and the
# mirror's gate runs on. While it runs, a second gate from the clone must queue
# (here: give up under GATE_LOCK_TIMEOUT, past the stale window) and lock-status
# must name the mirror's gate; once it ends, the lock frees as a dead holder's
# does (stale), and its verdict is in its kept logs. INT and TERM are as before:
# the caller's trap TERMs the mirror's gate and releases the lock at once.
# The mirror's gate is held open by a test that waits on a file
# (GATE_FIXTURE_WAIT names its directory; the spec carries GATE_* exports).
if command -v go >/dev/null 2>&1 && command -v node >/dev/null 2>&1; then
  cat >"$W/protocol/wait_test.go" <<'EOF'
package protocol

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWaitForTheFixture holds the run open while GATE_FIXTURE_WAIT names a
// directory: it marks itself started there, then waits for a release file, the
// directory's removal, or a minute.
func TestWaitForTheFixture(t *testing.T) {
	dir := os.Getenv("GATE_FIXTURE_WAIT")
	if dir == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "started"), nil, 0o644)
	for end := time.Now().Add(time.Minute); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
			return
		}
		if _, err := os.Stat(dir); err != nil {
			return
		}
	}
}
EOF
  # alive PID — running, not a zombie (an orphan waits for init to reap it).
  alive() {
    local st=""
    kill -0 "$1" 2>/dev/null || return 1
    [ -r "/proc/$1/stat" ] || return 0
    read -r st <"/proc/$1/stat" 2>/dev/null
    st="${st##*) }"
    [ "${st%% *}" != Z ]
  }
  # wait_until SECONDS CMD... — poll CMD every 0.2 s.
  wait_until() {
    local n=$(( $1 * 5 )) i=0
    shift
    until "$@"; do i=$((i + 1)); [ "$i" -ge "$n" ] && return 1; sleep 0.2; done
  }
  # start_caller DIR [ENV-OPTION...] — the caller's gate.sh in the background,
  # its pid in CALLER (exec: the process IS gate.sh), its output in DIR.out;
  # returns once the mirror's waiting test has started, INNER its gate's pid.
  start_caller() {
    local d="$1"
    shift
    mkdir -p "$d"
    rm -f "$W/.gate/mirror/inner.pid"
    ( cd "$W" && GATE_FIXTURE_WAIT="$d" exec env "$@" -u GITHUB_ACTIONS -u GATE_REUSE -u GATETIME_REUSE \
        bash scripts/gate.sh test protocol ) >"$d.out" 2>&1 &
    CALLER=$!
    INNER=""
    wait_until 60 test -e "$d/started" || { fail "case 13: the mirror's waiting test never started: $(tail -5 "$d.out")"; return 1; }
    read -r INNER <"$W/.gate/mirror/inner.pid" 2>/dev/null
    { [ -n "$INNER" ] && alive "$INNER"; } || { fail "case 13: no live mirror gate (inner.pid '${INNER:-}')"; return 1; }
  }
  # end_orphan DIR — releases the waiting test and waits for what ran it: the
  # mirror's gate (INNER), and the module job that writes its rc (after a TERM
  # it outlives the gate that started it).
  end_orphan() {
    rm -f "$M/.gate/test-protocol.rc"
    touch "$1/release"
    [ -z "${INNER:-}" ] || wait_until 30 eval '! alive "$INNER"' || fail "case 13: the mirror's gate did not end after its test was released"
    wait_until 30 test -e "$M/.gate/test-protocol.rc" || fail "case 13: the released test's module job never wrote its rc"
  }
  dsig=""; env --default-signal=INT true 2>/dev/null && dsig=--default-signal=INT
  # 13a. SIGKILL. The second gate's timeout is past the stale window (4 s)
  # with margin: under the old rule it took the lock about 3-5 s after the kill.
  if start_caller "$TMP/c13k"; then
    kill -9 "$CALLER"; wait "$CALLER" 2>/dev/null
    out="$(export GATE_SIDE=windows GATE_LOCK_TIMEOUT=7; gate simvet)"; rc=$?
    { [ "$rc" -ne 0 ] && echo "$out" | grep -q 'gate lock not acquired'; } \
      || fail "a second gate took the lock while a SIGKILLed caller's mirror gate still ran (exit $rc): $(printf '%s\n' "$out" | grep -E 'gate lock|^Gate' | head -5)"
    alive "$INNER" || fail "case 13: the mirror's gate ended before the second gate gave up: the pin did not hold it open"
    out="$(gate lock-status)"
    { echo "$out" | grep -q "the mirror's gate" && echo "$out" | grep -qE "pid $INNER([^0-9]|\$)"; } \
      || fail "lock-status does not name the mirror's gate (pid $INNER) while it holds the lock: $out"
    end_orphan "$TMP/c13k"
    out="$(export GATE_SIDE=windows; gate simvet)"; rc=$?
    { [ "$rc" -eq 0 ] && echo "$out" | grep -q 'taking the lock from'; } \
      || fail "after the orphaned mirror gate ended, its lock was not taken as stale (exit $rc): $(printf '%s\n' "$out" | grep -E 'gate lock|^Gate' | head -5)"
    kept="$(find "$M/.gate/runs" -mindepth 1 -maxdepth 1 -type d -name "*-test-$INNER" 2>/dev/null | head -1)"
    grep -q '^ok' "$kept/test-protocol.log" 2>/dev/null || fail "the orphaned mirror gate's verdict is not in its kept logs (${kept:-no run dir for pid $INNER})"
  fi
  # 13b, 13c. TERM and INT. INT is ignored by a job a non-interactive shell
  # backgrounds, and by everything a guard step starts (gate.sh runs each
  # guard as one), so the caller gets INT back by env --default-signal.
  if [ -n "$dsig" ]; then sigs="TERM:143 INT:130"; else
    sigs="TERM:143"; fail "case 13 needs env --default-signal (GNU coreutils 8.31+) to send INT to a background gate"
  fi
  for s in $sigs; do
    if start_caller "$TMP/c13-${s%:*}" ${dsig:+"$dsig"}; then
      kill "-${s%:*}" "$CALLER"; wait "$CALLER"; rc=$?
      [ "$rc" -eq "${s#*:}" ] || fail "a caller sent $s exited $rc, not ${s#*:}"
      wait_until 10 eval '! alive "$INNER"' || fail "a caller sent ${s%:*} left its mirror's gate running (pid $INNER)"
      [ -e "$GATE_LOCK_DIR" ] && fail "a caller sent ${s%:*} left the lock behind: $(cat "$GATE_LOCK_DIR/owner" 2>/dev/null | tr '\n' ' ')"
      out="$(export GATE_SIDE=windows GATE_LOCK_TIMEOUT=2; gate simvet)"; rc=$?
      [ "$rc" -eq 0 ] || fail "after a caller sent ${s%:*}, the next gate did not get the lock at once (exit $rc): $(printf '%s\n' "$out" | grep -E 'gate lock' | head -3)"
      INNER="" end_orphan "$TMP/c13-${s%:*}"
    fi
  done
  rm -f "$W/protocol/wait_test.go"
else
  fail "case 13 needs go and node on PATH"
fi

# ── 14. the caller's private object store is pruned past its size (R50) ──
# Past GATE_MIRROR_STORE_MB (1 here) it loses what its refs and the tips the
# mirror was last sent do not reach, but nothing under an hour old; below the
# size, or within an hour of the last look, nothing goes; and the bundle after
# a prune is still cut against what the mirror has (its prerequisites).
S="$W/.gate/mirror/objects.git"
sgit() { git --git-dir="$S" "$@"; }
# plant AGE — an unreachable 2 MiB blob in the store, its file AGE seconds old.
plant() {
  local id
  id="$(head -c 2097152 /dev/urandom | sgit hash-object -w --stdin)" || return 1
  [ "$1" -gt 0 ] && touch -d "@$(( $(date +%s) - $1 ))" "$S/objects/${id:0:2}/${id:2}"
  printf '%s' "$id"
}
# backdate — every loose object in the store two hours old, so only
# reachability can keep one.
backdate() { find "$S/objects" -type f -path '*/objects/??/*' -exec touch -d "@$(( $(date +%s) - 7200 ))" {} +; }
# cut SINCE — the last sync wrote a bundle after SINCE, and it has prerequisites.
cut() {
  [ "$W/.gate/mirror/sync.bundle" -nt "$1" ] \
    && [ "$(sed -n '2,/^$/p' "$W/.gate/mirror/sync.bundle" | grep -c '^-')" -gt 0 ]
}
a="$(plant 7200)" b="$(plant 0)"
rm -f "$W/.gate/mirror/store-checked"
printf '// 14 a\n' >>"$W/protocol/p.go"
out="$(gate simvet)"; rc=$?
{ [ "$rc" -eq 0 ] && sgit cat-file -e "$a" 2>/dev/null; } || fail "a store under the default 64 MB was pruned, or its sync went red (exit $rc): $out"
printf '// 14 b\n' >>"$W/protocol/p.go"
out="$(export GATE_MIRROR_STORE_MB=1; gate simvet)"; rc=$?
{ [ "$rc" -eq 0 ] && sgit cat-file -e "$a" 2>/dev/null; } || fail "the store's size was read again within the hour of its last look (exit $rc): $out"
rm -f "$W/.gate/mirror/store-checked"; backdate; touch -d "@$(date +%s)" "$S/objects/${b:0:2}/${b:2}"
touch "$TMP/c14.mark"; sleep 1
printf '// 14 c\n' >>"$W/protocol/p.go"
out="$(export GATE_MIRROR_STORE_MB=1; gate simvet)"; rc=$?
[ "$rc" -eq 0 ] || fail "the sync that pruned the store went red (exit $rc): $out"
echo "$out" | grep -q "object store .* pruned" || fail "a prune past the size was not said: $out"
sgit cat-file -e "$a" 2>/dev/null && fail "an unreachable object an hour old survived a prune past the size"
sgit cat-file -e "$b" 2>/dev/null || fail "a prune took an object under an hour old"
cut "$TMP/c14.mark" || fail "the sync that pruned sent a full bundle, not one cut against what the mirror had"
# After a refused sync the store's refs have moved and the mirror's tips have
# not: those tips must outlive a prune too.
printf '// 14 d\n' >>"$W/protocol/p.go"
printf '// by hand\n' >>"$M/docs/new.md"
out="$(gate simvet)"; rc=$?
{ [ "$rc" -ne 0 ] && echo "$out" | grep -q 'REFUSED'; } || fail "case 14: the hand edit was not refused (exit $rc): $out"
git -C "$M" show 'refs/gate-mirror/synced:docs/new.md' >"$M/docs/new.md"
a="$(plant 7200)"; rm -f "$W/.gate/mirror/store-checked"; backdate
touch "$TMP/c14.mark2"; sleep 1
out="$(export GATE_MIRROR_STORE_MB=1; gate simvet)"; rc=$?
{ [ "$rc" -eq 0 ] && cut "$TMP/c14.mark2"; } \
  || fail "a prune after a refused sync lost the tips the mirror was last sent: the bundle was not cut (exit $rc): $out"
sgit cat-file -e "$a" 2>/dev/null && fail "the prune after a refused sync left an unreachable object an hour old"

# ── 15. a mirror whose worktree is gone is removed (R50) ──────────────────
# By the Linux side, as it syncs: a mirror under the root whose recorded
# worktree no longer exists goes; never the one being synced; and only when
# that worktree's parent directory can be read, since a /mnt/c that is not
# mounted reads as every worktree gone.
R="$GATE_MIRROR_ROOT"
mk_mirror() { mkdir -p "$R/$1/.git" && printf '%s\n' "$2" >"$R/$1/.git/gate-mirror"; }
mkdir -p "$TMP/gone-parent" "$TMP/locked" "$R/plain"
mk_mirror gone "$TMP/gone-parent/gone"
mk_mirror alive "$TMP/clone"
mk_mirror nomount "$TMP/not-mounted/x"
mk_mirror locked "$TMP/locked/x"
chmod 000 "$TMP/locked"
printf '// 15\n' >>"$W/protocol/p.go"
out="$(gate simvet)"; rc=$?
chmod 755 "$TMP/locked"
[ "$rc" -eq 0 ] || fail "the sync that removes orphan mirrors went red (exit $rc): $out"
[ -e "$R/gone" ] && fail "a mirror whose worktree is gone was kept: $out"
echo "$out" | grep -q "removed the mirror $R/gone" || fail "the orphan's removal was not said: $out"
for k in alive nomount plain; do [ -d "$R/$k" ] || fail "the mirror $R/$k was removed (its worktree is there, or cannot be read, or it is no mirror)"; done
if [ "$(id -u)" != 0 ]; then [ -d "$R/locked" ] || fail "a mirror whose worktree's parent cannot be read was removed"; fi
[ -d "$M/.git" ] || fail "the mirror being synced was removed"
# Never the one being synced, even with its recorded worktree gone: a worktree
# of the gone one's name is refused as case 10's is, and the mirror stays.
mk_mirror fixwt3 "$TMP/gone-parent/fixwt3"
g worktree add -q -b topic3 "$TMP/o3/fixwt3" main
out="$(cd "$TMP/o3/fixwt3" && bash scripts/gate.sh simvet 2>&1)"; rc=$?
{ [ "$rc" -ne 0 ] && echo "$out" | grep -q 'belongs to .*gone-parent/fixwt3'; } \
  || fail "a worktree whose mirror records a gone worktree was not refused (exit $rc): $out"
[ -d "$R/fixwt3/.git" ] || fail "the mirror being synced was removed because its recorded worktree is gone"

if [ "$fails" -eq 0 ]; then
  echo "ok   gate-mirror (the working tree arrives whole, mtimes kept, hand edits refused, tree checked, one lock, both sides)"
  exit 0
fi
exit 1
