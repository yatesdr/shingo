#!/usr/bin/env bash
# docs-touched.sh [BASE] — the docs a change may have made wrong.
#
# Lists every in-scope doc whose citations name a path BASE..HEAD changed or a
# Go name declared on a line it changed, one per line with the citation that
# matched (shared/scripts/docstamp -touched; its doc comment says what it
# cannot see). BASE defaults to main; `main..HEAD` is read as main. A stream is
# done when every doc listed is one the stream has read and changed or
# restamped.
#
# Exit 0 whatever it lists; non-zero = the diff could not be read (a BASE
# this clone does not have, say).

set -uo pipefail

base="${1:-main}"
base="${base%..HEAD}"
base="${base%..}"

cd "$(dirname "$0")/../shared" || exit 2

exec go run ./scripts/docstamp -root .. -touched "$base"
