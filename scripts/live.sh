#!/bin/sh
# Run the live upstream contract tests (build tag "live") for every source, or one.
# They hit the real providers, so they are not part of `just check`.
set -eu
cd "$(dirname "$0")/.."
if [ -n "${1:-}" ]; then
  set -- "$1"
else
  set -- sources/*
fi
failed=""
for module do
  [ -f "$module/go.mod" ] || continue
  grep -lq '^//go:build live' "$module"/*_test.go 2>/dev/null || continue
  echo "== $module"
  go -C "$module" test -tags live -run '^TestLive' -count=1 -v ./... || failed="$failed $module"
done
if [ -n "$failed" ]; then
  echo "live contract drift or failure in:$failed" >&2
  exit 1
fi
