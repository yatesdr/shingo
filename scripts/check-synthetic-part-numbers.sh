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
# Part numbers only. NODE NAMES AND PLANT NAMES STAY (PLN_01, SMN_BUF_100,
# Supermarket Area, Press B4): they are shingo's own vocabulary and the plant's
# words for its own equipment, they carry nothing a customer owns, and the
# fixtures would stop being readable without them.
#
# THE SHAPE, not a list of known strings: five digits, a dash, three letters,
# two digits, a dot, two digits. That is the format both plants use and the
# format the anonymiser used to imitate. A new pull that reintroduced it, or a
# real code pasted into a test, fails here.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

SHAPE='[0-9]{5}-[A-Z]{3}[0-9]{2}\.[0-9]{2}'

hits="$(git grep -nE "$SHAPE" -- \
  ':!*.png' ':!*.jpg' ':!*.pdf' 2>/dev/null)"

if [ -n "$hits" ]; then
  echo "FAIL synthetic part numbers: a committed file carries a customer-shaped part number." >&2
  echo "     Shape: NNNNN-LLLNN.NN. Use a synthetic code (SYN-A-P007, SHOT-PART-01)." >&2
  echo "$hits" >&2
  exit 1
fi

echo "ok   synthetic part numbers (no NNNNN-LLLNN.NN in tracked files)"
