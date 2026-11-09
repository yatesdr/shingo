#!/usr/bin/env bash
# docs-verify.sh — restamp every doc the citation readers pass.
#
# The write half of scripts/check-doc-citations.sh. It runs the three readers
# over the in-scope docs (testcite, deadident and repolint, each with -docs),
# collects the docs any of them found fault with, and sets "Verified against:
# <HEAD's short sha>" under the title of every other in-scope doc
# (shared/scripts/docstamp -write). A doc with a finding keeps the stamp it
# had: it is not verified against HEAD, and saying so would be the lie the
# stamp exists to prevent.
#
# Run once by the lead at the end of a stream, after `make docs-gen` and
# before the stream's last commit, so the stamps land in the stream's own
# history. Never by CI: the gate only reads stamps (docstamp -check). HEAD is
# the commit the docs were read against, so commit the restamp on top of it;
# a history rewrite after this voids every stamp that names a rewritten
# commit (the gate then says which).
#
# Exit 0 = every in-scope doc passed and is stamped; exit 1 = some doc had a
# finding (printed above the stamp summary) and was skipped, or a reader
# could not run, in which case nothing is written.

set -uo pipefail

cd "$(dirname "$0")/../shared" || exit 1

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

readers="testcite deadident repolint"
# shellcheck disable=SC2086 # one package per reader
if ! go build -o "$tmp/" $(for r in $readers docstamp; do printf './scripts/%s ' "$r"; done) 2>"$tmp/build.err"; then
  cat "$tmp/build.err"
  echo "FAIL docs-verify (the readers did not build; nothing stamped)"
  exit 1
fi

pids=""
for r in $readers; do
  ( "$tmp/$r" -root .. -docs >"$tmp/$r.out" 2>&1; echo $? >"$tmp/$r.rc" ) &
  pids="$pids $!"
done
# shellcheck disable=SC2086 # these jobs by pid, never a bare wait
wait $pids

for r in $readers; do
  rc="$(cat "$tmp/$r.rc" 2>/dev/null)"
  case "$rc" in
    0) ;;
    1) cat "$tmp/$r.out" ;;
    *) cat "$tmp/$r.out"
       echo "FAIL docs-verify ($r could not read the docs; nothing stamped)"
       exit 1 ;;
  esac
done

# The doc each finding is in: the first path.md of the line, as testcite and
# deadident print "doc.md:N: ..." and repolint "FAIL rule  doc.md:N  ...".
for r in $readers; do
  sed -nE 's/^(FAIL [a-z-]+  )?([^ :]+\.md)(:[0-9]+)?:? .*/\2/p' "$tmp/$r.out"
done | sort -u >"$tmp/failing"

"$tmp/docstamp" -root .. -write -failing "$tmp/failing" || exit 1

if [ -s "$tmp/failing" ]; then
  echo "FAIL docs-verify ($(wc -l <"$tmp/failing" | tr -d ' ') doc(s) with findings kept their stamp)"
  exit 1
fi
echo "ok   docs-verify (every in-scope doc passed and is stamped at HEAD)"
