#!/bin/sh
# Check each module without go.work or committed local replace directives.
set -eu
cd "$(dirname "$0")/.."
repo_root=$(pwd)
check_dir=$(mktemp -d)
trap 'rm -rf "$check_dir"' EXIT
export GOWORK=off
for module in . sdk sources/wordpress sources/chrome sources/firefox sources/github sources/n8n sources/ansible-galaxy sources/obsidian sources/helm; do
  module_dir="$repo_root/$module"
  cp "$module_dir/go.mod" "$check_dir/check.mod"
  cp "$module_dir/go.sum" "$check_dir/check.sum"
  if [ "$module" != sdk ]; then
    go mod edit -modfile "$check_dir/check.mod" -replace "github.com/rezen/dug/sdk=$repo_root/sdk"
  fi
  go -C "$module_dir" build -modfile "$check_dir/check.mod" ./...
  go -C "$module_dir" list -modfile "$check_dir/check.mod" -deps ./... > "$check_dir/deps"
  python3 - "$module" "$check_dir/deps" <<'PY'
import sys
module = sys.argv[1]
dependencies = open(sys.argv[2]).read().splitlines()
prefix = 'github.com/rezen/dug/'
if module == '.':
    forbidden = [name for name in dependencies if name.startswith(prefix + 'sources/')]
else:
    allowed = [prefix + 'sdk/']
    if module != 'sdk':
        allowed += [prefix + module, prefix + module + '/']
    forbidden = [name for name in dependencies if name.startswith(prefix) and not any(name == p.rstrip('/') or name.startswith(p if p.endswith('/') else p + '/') for p in allowed)]
if forbidden:
    raise SystemExit(f'{module} crossed its module boundary: {forbidden}')
print(f'{module}: standalone build and dependency boundary passed')
PY
done
