#!/usr/bin/env bash
# tier: dev
# check-changelog.sh — a change to what a binary is built from is in
# CHANGELOG.md's `## Unreleased`, with the migrations it carries.
#
# WHY. A release is cut by renaming `## Unreleased` (make release), so that
# section is the release's notes and its deploy instructions, and a change
# that never reached it ships unannounced. The one fact a deploy cannot get
# wrong is the migrations: a binary that applies one at boot changes a
# database the previous build may not run on. So once any SOURCE file differs
# from the last v* tag, `## Unreleased` must be non-empty, open with a
# `Deploy:` line, and that line must name, per binary, exactly the migration
# versions added since the tag (`Core v176`, `Edge v45–v46`), or say
# `no migration` when none was. SOURCE is what a binary is built from: under
# the five modules, non-test .go, .sql, go.mod and the embedded templates and
# static assets (.html, .js, .css, .svg, .ico); tests, testdata and docs are
# not, since they change no binary. The added migrations are Core's
# migrationList and the Edge's edgeMigrations read at the working tree and at
# the tag. `## Unreleased` must also be the file's first section: an entry
# above it, in the old dated style, is carried by no release. The tip is the
# working tree, as the gate's diff is, so an uncommitted migration counts.
# With no v* tag (a shallow clone has none; CI
# checks out with fetch-depth: 0) there is nothing to compare with: it says so
# and passes. The rules and their pins live in shared/scripts/release.
#
# Exit 0 = clean, exit 1 = `## Unreleased` is missing, empty or wrong, exit 2 =
# the tree could not be read.

set -uo pipefail

cd "$(dirname "$0")/../shared"

exec go run ./scripts/release -root .. -check-changelog
