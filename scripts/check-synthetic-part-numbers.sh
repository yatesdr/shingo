#!/usr/bin/env bash
# check-synthetic-part-numbers.sh — no committed file carries a string shaped
# like a customer part number.
#
# WHY THIS IS A GUARD AND NOT A REVIEW NOTE. The anonymiser used to invent part
# codes in the real ones' shape — the one spelled out under SHAPE below — on
# the reasoning that a fixture should exercise the shapes a plant uses. (No
# example here: this file is scanned like every other, and the first run of it
# failed on its own documentation.) The codes were invented, but
# nothing about them said so: 503 of them sat in two committed fixtures, a
# dozen more were copied out of those fixtures into test literals by hand, and
# every handoff shot of the composer photographed a part chip that read like a
# customer's drawing number. Anyone auditing the repo had to take it on trust
# that the strings were made up, and a real one pasted in during debugging
# would have looked exactly like its neighbours.
#
# AND THE OTHER 137 WERE NOT INVENTED AT ALL. The first version of this guard
# caught one family — five digits, three letters, two digits — and the sweep
# that came with it read as if the job were done. It was not: a second family
# with an alphanumeric middle block was spread over 52 files in every module,
# in test literals, in fixtures, and in the incident narratives that comments
# cite as evidence. Those came off a plant. One pattern covers both now.
#
# Part numbers only. NODE NAMES AND PLANT NAMES STAY (PLN_01, SMN_BUF_100,
# Supermarket Area, Press B4): they are shingo's own vocabulary and the plant's
# words for its own equipment, they carry nothing a customer owns, and the
# fixtures would stop being readable without them.
#
# THE SHAPE, not a list of known strings: five digits, a dash, three to six
# alphanumerics, a dot, two digits. That covers both formats the plants use and
# the one the anonymiser imitated. A new pull that reintroduced either, or a
# real code pasted into a test, fails here.
#
# THE DOT SUFFIX IS WHAT MAKES IT A PART NUMBER and not a timestamp: Go's
# reference time formats as "20060102-150405", which the leading digits and the
# dash match and the dot does not. The dot may be BACKSLASH-ESCAPED, because a
# JS regex literal spells it that way and a code hidden inside one is still a
# code — `[\]?` and not `\\?`, which this file had for one revision and which
# matches NOTHING in ERE, so the guard passed on a tree that still held 137.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

SHAPE='[0-9]{5}-[0-9A-Z]{3,6}[\]?\.[0-9]{2}'

# LIVENESS FIRST, for the same reason gate.sh asserts that its shot test RAN: a
# pattern that matches nothing prints the same "ok" as a clean tree, and this
# one did. The probes are ASSEMBLED rather than written out, so the file stays
# clean under its own scan.
probe_bad="$(printf '%s-%s.%s' 74577 6SA0A 06) $(printf '%s-%s.%s' 40421 RVJ56 37) $(printf '%s-%s\\.%s' 76683 6TA0A 06)"
probe_ok="20060102-150405 SYN-PART07A.06 SYN-A-P007 PLN_01 Press B4"
for tok in $probe_bad; do
  if ! printf '%s\n' "$tok" | grep -qE "$SHAPE"; then
    echo "FAIL synthetic part numbers: the pattern is dead — it does not match $tok" >&2
    exit 1
  fi
done
for tok in $probe_ok; do
  if printf '%s\n' "$tok" | grep -qE "$SHAPE"; then
    echo "FAIL synthetic part numbers: the pattern is too broad — it matches $tok" >&2
    exit 1
  fi
done

hits="$(git grep -nE "$SHAPE" -- \
  ':!*.png' ':!*.jpg' ':!*.pdf' 2>/dev/null)"

if [ -n "$hits" ]; then
  echo "FAIL synthetic part numbers: a committed file carries a customer-shaped part number." >&2
  echo "     Shape: NNNNN-XXXXX.NN. Use a synthetic code (SYN-A-P007, SYN-PART07A.06)." >&2
  echo "$hits" >&2
  exit 1
fi

echo "ok   synthetic part numbers (no NNNNN-XXXXX.NN in tracked files; pattern proved live)"
