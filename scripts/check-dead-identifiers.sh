#!/usr/bin/env bash
# check-dead-identifiers.sh — an identifier a Go comment cites must still exist.
#
# The half check-comment-references.sh leaves out on purpose ("arbitrary
# identifiers. Too noisy to be read"), done narrowly enough to be read: only
# code-shaped citations (`Foo(`, `x.Foo`, backticked), at least 8 characters and
# mixed case, plus a short list of retired names read even as bare words.
# Gravestone lines ("removed", "renamed", "used to", ...) are exempt. The rules
# and their pins live in shared/scripts/deadident.
#
# Exit 0 = clean, exit 1 = a comment cites a name nothing declares.

set -uo pipefail

cd "$(dirname "$0")/../shared"

if go run ./scripts/deadident -root ..; then
  echo "ok   dead identifiers (every code-shaped comment citation resolves)"
  exit 0
fi
exit 1
