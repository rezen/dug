#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mode=${1:-check}
case "$mode" in
  check|test|vet|fmt|lint|vuln) ;;
  *) echo "Usage: $0 [check|test|vet|fmt|lint|vuln] [module]" >&2; exit 2 ;;
esac
# ./... stops at nested go.mod boundaries; enumerate the workspace explicitly.
if [ -n "${2:-}" ]; then
  set -- "$2"
else
  set -- . sdk sources/wordpress sources/chrome sources/firefox sources/github sources/n8n sources/ansible-galaxy sources/obsidian sources/helm sources/joomla sources/oci sources/jenkins sources/bitbucket integration
fi
for module do
  case "$mode" in
    check) go -C "$module" test -race ./...; go -C "$module" vet ./... ;;
    test) go -C "$module" test -race ./... ;;
    vet) go -C "$module" vet ./... ;;
    fmt) go -C "$module" fmt ./... ;;
    lint) go -C "$module" run honnef.co/go/tools/cmd/staticcheck@latest ./... ;;
    vuln) go -C "$module" run golang.org/x/vuln/cmd/govulncheck@latest ./... ;;
  esac
done
if [ "$mode" = check ]; then
  sh scripts/check-boundaries.sh
fi
