#!/usr/bin/env bash
# check-makefile-targets.sh — no Makefile recipe runs a whole-module,
# docker-tagged `go test` at default package parallelism.
#
# WHY THIS IS A GUARD AND NOT A REVIEW NOTE. The gate's docker step spends a
# comment block (scripts/gate.sh, the one ending "AD-HOC `go test
# -tags=docker ./...` IS NOT THIS, AND WILL LIE TO YOU") on one fact: `go
# test` defaults -p to GOMAXPROCS, so `go test -tags=docker ./...` by hand is
# ~20 packages' worth of connection pools, each up to GOMAXPROCS-parallel, on
# one Postgres server — the observed result was three timing-sensitive tests
# failing at 48-74s that pass in isolation at 25s. A Makefile target is a
# hand-run waiting to happen: it looks official, so nobody re-derives the -p
# the gate computes. The rule here is the gate's: a docker-tagged `./...`
# recipe must pass an explicit -p.
#
# UNTAGGED `go test ./...` IS NOT COVERED. The untagged suite has no shared
# server to thrash; this guard is about the docker-tagged one only. The
# exemption below (WHITELIST) covers shingo-core's `test-all`, which predates
# this guard — exempted 2026-09-26, "one fact, one owner" stream, rather than
# silently retuned by a guard script's first commit.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

# file:target pairs exempted, dated above. Narrow on purpose: a `test-all`
# elsewhere is a new decision, not covered by this exemption.
WHITELIST="shingo-core/Makefile:test-all"

# scan FILE — prints one "file\ttarget\trecipe" line per violation.
# Recipe lines are tab-indented under a `target:` line; backslash
# continuations are joined first so a `-p` on the next physical line counts.
scan() {
  awk -v file="$1" '
    {
      line = $0
      while (line ~ /\\[ \t]*$/ && (getline nxt) > 0) {
        sub(/\\[ \t]*$/, "", line)
        line = line " " nxt
      }
      if (line ~ /^[A-Za-z0-9_.$()\/-]+[ \t]*:([^=]|$)/) {
        target = line; sub(/[ \t]*:.*/, "", target); intarget = 1; next
      }
      if (line ~ /^\t/ && intarget) {
        body = line; sub(/^\t/, "", body)
        if (body ~ /go test/ &&
            body ~ /-tags[= ]([^ ]*,)*docker(,[^ ]*)?([ \t]|$)/ &&
            body ~ /(^|[ \t="])\.?\/\.\.\./ &&
            body !~ /(^|[ \t=])-p([ \t=]|$)/) {
          print file "\t" target "\t" body
        }
        next
      }
      if (line ~ /^[^\t#]/) intarget = 0
    }
  ' "$1"
}

# LIVENESS FIRST, for the same reason gate.sh asserts its shot test RAN: a
# pattern that matches nothing prints the same "ok" as a clean tree. The
# probes run the real scanner over a Makefile assembled on the fly, so the
# exemption path and every accept/reject arm below are the ones the real scan
# takes. Assembled with printf so this file's own text never matches the scan.
probe_dir="$(mktemp -d)"
trap 'rm -rf "$probe_dir"' EXIT
{
  printf 'good:\n'
  printf '\tgo test -tags=%s -count=1 -p 1 ./...\n' docker
  printf 'good2:\n'
  printf '\tgo test -v ./...\n'
  printf 'good3:\n'
  printf '\tgo test -tags=%s ./...\n' sim
  printf 'bad:\n'
  printf '\tgo test -v -tags=%s ./... ; rc=$$? ; echo $$rc\n' docker
  printf 'bad2:\n'
  printf '\tgo test -tags %s -count=1 \\\n' docker
  printf '\t\t./...\n'
  printf 'whitelisted:\n'
  printf '\tgo test -tags=%s ./... ; $(MAKE) -s clean\n' docker
} >"$probe_dir/Makefile"
probe_hits="$(scan "$probe_dir/Makefile" | cut -f2 | tr '\n' ' ')"
probe_want="bad bad2 whitelisted "
if [ "$probe_hits" != "$probe_want" ]; then
  echo "PROBE FAIL: scanner saw [$probe_hits], wanted [$probe_want]" >&2
  exit 1
fi

# Discovery liveness: the scan must actually reach the known Makefiles.
makefiles="$(git ls-files 2>/dev/null | grep -E '(^|/)Makefile$')"
for must in shingo-core/Makefile shingo-edge/Makefile; do
  case "$makefiles" in
    *"$must"*) ;;
    *) echo "PROBE FAIL: discovery missed $must" >&2; exit 1 ;;
  esac
done

status=0
while IFS="$(printf '\t')" read -r file target body; do
  [ -n "$file" ] || continue
  case " $WHITELIST " in
    *" $file:$target "*) continue ;;  # dated exemption above
  esac
  echo "FAIL $file: target \`$target\` runs a docker-tagged whole-module go test without -p:"
  echo "     $body"
  echo "     (the gate computes -p for exactly this reason — see the comment block"
  echo "      above docker_p in scripts/gate.sh)"
  status=1
done <<EOF
$(for f in $makefiles; do [ -f "$f" ] && scan "$f"; done)
EOF

[ "$status" -eq 0 ] || exit 1
echo "ok   makefile targets (no -tags=docker ./... recipe without -p; scanner proved live)"
