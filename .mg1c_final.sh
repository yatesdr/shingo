#!/usr/bin/env bash
ROOT=/mnt/c/Users/stephen.brown/GitHub/shingo
LINT=$HOME/go/bin/golangci-lint
rc=0
echo "########## gofmt + lint + plain tests, five modules ##########"
for m in shingo-core shingo-edge protocol shared integration; do
  cd "$ROOT/$m" || exit 1
  f=$(gofmt -l . | grep -v '^vendor/')
  [ -n "$f" ] && { echo "=== $m GOFMT ==="; echo "$f"; rc=1; }
  l=$("$LINT" run --build-tags docker --config="$ROOT/.golangci.yml" ./... 2>&1)
  echo "$l" | grep -q '^0 issues' && echo "  $m lint: 0 issues" || { echo "=== $m LINT ==="; echo "$l" | tail -12; rc=1; }
  o=$(go test ./... 2>&1); e=$?
  echo "  $m plain test: exit=$e"
  [ $e -ne 0 ] && { echo "$o" | grep -E "^(FAIL|--- FAIL)" | head -10; rc=1; }
done

echo "########## race + docker ##########"
cd "$ROOT/shingo-core" && go test -race -count=1 -tags docker ./dispatch/... >/tmp/f-race.log 2>&1
e=$?; echo "  core dispatch race+docker: exit=$e"
[ $e -ne 0 ] && { grep -E "^(FAIL|--- FAIL)" /tmp/f-race.log | head -10; rc=1; }

for m in shingo-core shingo-edge integration; do
  cd "$ROOT/$m" || exit 1
  o=$(go test -count=1 -p 1 -tags docker ./... 2>&1); e=$?
  echo "  $m docker: exit=$e"
  [ $e -ne 0 ] && { echo "$o" | grep -E "^(FAIL|--- FAIL)" | head -15; rc=1; }
done
echo "FINAL_GATE_RC=$rc"
