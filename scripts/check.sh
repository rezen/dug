#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mode=${1:-check}
case "$mode" in
  check|test|vet|fmt) ;;
  *) echo "Usage: $0 [check|test|vet|fmt] [module]" >&2; exit 2 ;;
esac
# ./... stops at nested go.mod boundaries; enumerate the workspace explicitly.
if [ -n "${2:-}" ]; then
  set -- "$2"
else
  set -- . sdk sources/wordpress sources/chrome sources/firefox sources/github sources/n8n sources/ansible-galaxy sources/obsidian sources/helm sources/joomla integration
fi
for module do
  case "$mode" in
    check) go -C "$module" test -race ./...; go -C "$module" vet ./... ;;
    test) go -C "$module" test -race ./... ;;
    vet) go -C "$module" vet ./... ;;
    fmt) go -C "$module" fmt ./... ;;
  esac
done
if [ "$mode" = check ]; then
  sh scripts/check-boundaries.sh
fi
