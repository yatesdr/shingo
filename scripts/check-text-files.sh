#!/usr/bin/env bash
# Fail when a tracked file with the `text` attribute is indexed as non-text.
#
# WHY THIS EXISTS: gate.sh:391 once carried literal CR+NUL bytes inside a
# `tr -d '…'` argument. A NUL byte makes Git index the file `-text`, and
# makes ripgrep skip the WHOLE FILE — so every search over scripts/ silently
# missed gate.sh until someone noticed grep could not find a string that was
# visibly in the file. The bytes were introduced by an editor round-trip and
# survived because nothing looked for them.
#
# The check reads `git ls-files --eol`, whose eolinfo column is `i/<index-eol>`
# for the index copy. For a file with the text attribute the value must be
# one of lf, crlf, mixed (all textual) — `-text` means the index copy has a
# NUL byte or is otherwise binary, which for a file we declare text is always
# corruption, never intent.
#
# `mixed` is allowed: .gitattributes says these files are text, and a mixed
# index EOL is a line-ending question, not a content corruption. The byte-level
# question this guard answers is "did a NUL get in".
set -u
cd "$(dirname "$0")/.."

# textattr <path> — does the path have the text attribute set?
# `git check-attr text -- <path>` prints "<path>: text: set|unset|unspecified".
has_text_attr() {
  local v
  v="$(git check-attr text -- "$1" | sed 's/.*text:[[:space:]]*//')"
  [ "$v" = "set" ]
}

status=0
TAB="$(printf '\t')"
# `git ls-files --eol` emits "<i/eol> <w/eol> attr/<attrs>\t<path>" — exactly
# two tab-separated fields; the eol/attr columns are space-separated inside
# field 1. Read two fields, then take field 1's first space-word.
while IFS="$TAB" read -r eolinfo path; do
  i="${eolinfo%% *}"  # first space-separated column: i/<index-eol>
  i="${i#i/}"         # index-side eol: lf|crlf|mixed|-text|(none)
  [ "$i" = "-text" ] || continue
  # Only files Git itself treats as text (attribute set, or default text
  # detection — but attribute-set files are the declared ones; leave
  # unattributed binaries alone).
  if has_text_attr "$path"; then
    echo "FAIL: $path is indexed -text but has the text attribute — the index copy contains a NUL byte or is binary. Fix the file content (often an editor round-trip), then re-add."
    status=1
  fi
done < <(git ls-files --eol)

if [ "$status" -eq 0 ]; then
  echo "ok   check-text-files ($(git ls-files | wc -l | tr -d ' ') tracked files, no text-attributed file indexed -text)"
fi
exit "$status"
