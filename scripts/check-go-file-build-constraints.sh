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

cd "$(dirname "$0")/.."

exec python3 - "$PWD" <<'PY'
import os
import re
import sys

# Every GOOS and GOARCH Go recognises as a filename suffix.
SUFFIXES = set("""
aix android darwin dragonfly freebsd hurd illumos ios js linux nacl netbsd
openbsd plan9 solaris wasip1 windows zos
386 amd64 amd64p32 arm arm64 arm64be armbe loong64 mips mips64 mips64le
mips64p32 mips64p32le mipsle ppc ppc64 ppc64le riscv riscv64 s390 s390x
sparc sparc64 wasm
""".split())

SKIP_DIRS = {'.git', 'node_modules', '.gate', 'vendor', 'testdata'}
root = sys.argv[1]
fail = False

for dirpath, dirnames, filenames in os.walk(root):
    dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
    for name in filenames:
        if not name.endswith('.go'):
            continue
        test = name.endswith('_test.go')
        stem = name[:-len('_test.go')] if test else name[:-len('.go')]
        # Go also honours _<GOOS>_<GOARCH>; checking the last segment catches
        # both, because the GOARCH half is what trails.
        last = stem.rsplit('_', 1)[-1] if '_' in stem else ''
        if last not in SUFFIXES:
            continue
        path = os.path.join(dirpath, name)
        # A DELIBERATE platform test is fine — it carries a //go:build line
        # naming that platform. Only an accident is reported.
        with open(path, encoding='utf-8', errors='replace') as fh:
            head = fh.read(2048)
        if re.search(r'^//go:build .*\b%s\b' % re.escape(last), head, re.M):
            continue
        rel = os.path.relpath(path, root).replace(os.sep, '/')
        print('FAIL %s' % rel)
        print('     ends in _%s, which Go reads as a GOOS/GOARCH build constraint, so this file is' % last)
        if test:
            print('     NOT COMPILED on other platforms and its tests silently do not exist there.')
            print('     Rename it (%s_test.go -> %s_gate_test.go), or add a //go:build line naming' % (stem, stem))
        else:
            print('     NOT COMPILED on other platforms: what it declares is missing there.')
            print('     Rename it (%s.go -> %s_choice.go, say), or add a //go:build line naming' % (stem, stem))
        print('     %s if the constraint is intended.' % last)
        fail = True

if not fail:
    print('ok   Go filenames (no accidental GOOS/GOARCH build constraints)')
sys.exit(1 if fail else 0)
PY
