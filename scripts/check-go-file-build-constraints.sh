#!/usr/bin/env bash
# check-go-file-build-constraints.sh — a Go file whose NAME excludes it from
# the build.
#
# Go treats a trailing _<GOOS> or _<GOARCH> in a filename as an implicit build
# constraint. So `changeover_staged_arm_test.go` is a GOARCH=arm file and
# `loader_funnel_windows_test.go` is a GOOS=windows one: on any other platform
# they are not compiled, their tests do not exist, and NOTHING SAYS SO. gofmt
# formats them, `go vet ./...` passes, `go test ./...` prints `ok`, and the
# only tell is `-run` reporting "no tests to run" — which reads as a typo in
# the -run pattern rather than a file that has left the build.
#
# Both were live in this repo on 2026-08-23. The arm one was written that day;
# the windows one had been running on developer laptops and silently skipped in
# Linux CI for as long as it had existed.
#
# This is the "a killed run and a passing run produce identical evidence"
# family from AGENTS.md, at the file level: an excluded test and a passing one
# both produce no failure.
#
# NOT ONLY TESTS. A non-test file fails the other way round: the platform that
# drops it stops compiling, but only that platform. A planner file named
# `plan/loader_windows.go` (2026-09-27) built on every Windows step, fast gate
# and docker suites included, and failed every Linux build (WSL race, CI, the
# sim images, a Pi) on `undefined: LoaderWindowsDB`. So every .go file is read.
#
# Exit 0 = clean, exit 1 = at least one Go file is name-excluded.

set -euo pipefail

cd "$(dirname "$0")/../shared"

# The rules and their pins live in shared/scripts/buildconstraints.
exec go run ./scripts/buildconstraints -root ..
