#!/usr/bin/env bash
# tier: dev
# inputs: scripts/gate.sh scripts/check-push-skip.sh
# check-push-skip.sh — pins the push tier's skip in scripts/gate.sh: the docker
# step skips the integration module's production-window leaves (PUSH_SKIP_TESTS)
# by name in push and nowhere else, and the push sentence names each one.
#
# It reads `gate.sh skips TIER`, which prints, through the functions the docker
# step and the sentence call (docker_skip, gate_sentence), the -skip each module
# would be handed in that tier and the sentence a run of that tier would print
# with the docker suites run.
#
#   1. push: the integration module gets `^(A|B|...)$` of exactly the listed
#      names, and no other module gets a skip
#   2. cert, step (`gate.sh docker`, CI's `gate.sh scripts`), dev and no tier
#      at all: no module gets a skip (the fail-safe: a tier this file does not
#      know runs everything)
#   3. the push sentence names every listed test, and names it as skipped; no
#      other tier's sentence says anything was skipped
#   4. the list is not empty, and names only Go test names (a stray character
#      would make the regex skip what the sentence does not name)
#
# The list itself is held to the tree by the repolint rule production-window
# (each name a top-level test, each but S5 with its short-window twin).
#
# Exit 0 = pinned, 1 = a case failed.

set -uo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GATE="$ROOT/scripts/gate.sh"
export GATE_STEPS_LOG=

fails=0
fail() { echo "FAIL push-skip: $*"; fails=$((fails + 1)); }

# The list, as gate.sh declares it: every word inside PUSH_SKIP_TESTS="...".
names="$(awk '/^PUSH_SKIP_TESTS="/ { on = 1; sub(/^PUSH_SKIP_TESTS="/, "") }
              on { line = $0; done = sub(/".*$/, "", line); print line; if (done) exit }' "$GATE" | tr -s ' \t\r' '\n\n\n' | sed '/^$/d')"
n="$(printf '%s\n' "$names" | sed '/^$/d' | wc -l | tr -d ' ')"
[ "$n" -gt 0 ] || fail "PUSH_SKIP_TESTS in scripts/gate.sh is empty or unreadable: nothing to pin"
bad="$(printf '%s\n' "$names" | grep -vE '^(Test|Example|Fuzz)[A-Za-z0-9_]*$')"
[ -z "$bad" ] || fail "PUSH_SKIP_TESTS holds words that are not test names: $(echo $bad)"
want="^($(echo $names | tr ' ' '|'))\$"

for tier in push cert step dev ""; do
  out="$(bash "$GATE" skips "$tier" 2>&1)"
  rc=$?
  [ "$rc" -eq 0 ] || { fail "gate.sh skips '$tier' exited $rc: $out"; continue; }
  label="${tier:-no tier}"
  for m in protocol shared shingo-core shingo-edge integration; do
    got="$(printf '%s\n' "$out" | sed -n "s/^$m: //p")"
    printf '%s\n' "$out" | grep -q "^$m:" || { fail "$label: gate.sh skips prints no line for $m"; continue; }
    if [ "$tier" = push ] && [ "$m" = integration ]; then
      [ "$got" = "$want" ] || fail "push: the integration module's docker run skips '$got', want '$want' (PUSH_SKIP_TESTS, anchored)"
    else
      [ -z "$got" ] || fail "$label: the $m docker run skips '$got', want nothing: only push skips, and only the integration module"
    fi
  done
  sentence="$(printf '%s\n' "$out" | grep '^Gate')"
  [ -n "$sentence" ] || { fail "$label: gate.sh skips printed no sentence"; continue; }
  if [ "$tier" = push ]; then
    printf '%s\n' "$sentence" | grep -q 'Skipped in push' || fail "push: the sentence does not say what it skipped: $sentence"
    for t in $names; do
      printf '%s\n' "$sentence" | grep -qw "$t" || fail "push: the sentence does not name $t, which the docker step skips: $sentence"
    done
  else
    ! printf '%s\n' "$sentence" | grep -qi 'skipped in push' || fail "$label: the sentence says something was skipped in push: $sentence"
  fi
done

if [ "$fails" -eq 0 ]; then
  echo "ok   push-skip (push skips the $n production-window tests by name and says so; cert, step, dev and no tier skip nothing)"
  exit 0
fi
echo "     PUSH_SKIP_TESTS, docker_skip and push_skip_note in scripts/gate.sh: push skips exactly the list, nothing else does, the sentence names each."
exit 1
