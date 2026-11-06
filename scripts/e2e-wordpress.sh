#!/usr/bin/env bash
# Live public WordPress -> Connect -> local GoFakeS3 -> integrity + resume check.
# The argument picks the directory: wordpress (plugins, default) or wordpress-themes.
set -euo pipefail
cd "$(dirname "$0")/.."
registry="${1:-wordpress}"
case "$registry" in
  wordpress|wordpress-themes) ;;
  *) echo "Usage: $0 [wordpress|wordpress-themes]" >&2; exit 2 ;;
esac
run_dir="${DUG_E2E_DIR:-.local/e2e-$registry}"
mkdir -p "$run_dir" bin
run_dir="$(cd "$run_dir" && pwd)"
export AWS_ACCESS_KEY_ID=dug-local
export AWS_SECRET_ACCESS_KEY=dug-local-secret
export AWS_SESSION_TOKEN=
export AWS_EC2_METADATA_DISABLED=true
export DUG_BUCKET=dug-e2e
export DUG_S3_ENDPOINT=http://127.0.0.1:19000
export DUG_PLUGIN_URL=http://127.0.0.1:17881
unset DUG_PLUGIN_TOKEN
go build -o bin/dug ./cmd/dug
go build -o "bin/dug-$registry" "./sources/wordpress/cmd/dug-$registry"
go build -o bin/dug-s3local ./integration/cmd/dug-s3local
./bin/dug-s3local --listen 127.0.0.1:19000 --data "$run_dir/s3.db" >"$run_dir/s3.log" 2>&1 &
s3_pid=$!
"./bin/dug-$registry" --listen 127.0.0.1:17881 >"$run_dir/$registry.log" 2>&1 &
plugin_pid=$!
trap 'kill "$plugin_pid" "$s3_pid" 2>/dev/null || true; wait "$plugin_pid" "$s3_pid" 2>/dev/null || true' EXIT
python3 - <<'PY'
import socket,time
for port in [19000,17881]:
    for attempt in range(100):
        try:
            with socket.create_connection(('127.0.0.1',port),timeout=.2): break
        except OSError: time.sleep(.1)
    else: raise RuntimeError(f'server did not start on {port}')
PY
kill -0 "$s3_pid" "$plugin_pid"
# Full listing is required to rank globally by total downloads. No list limit.
./bin/dug list "$registry" --format summary --option page_size=250 --version-depth 1 >"$run_dir/list.json" 2>"$run_dir/list.log"
inventory_key="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["inventory_key"])' "$run_dir/list.json")"
./bin/dug download "$registry" --inventory "$inventory_key" --order=-downloads.total \
  --where 'downloads > 20 AND rating >= 0' --limit 100 \
  --concurrency 4 --downloads 4 --upload-concurrency 2 \
  >"$run_dir/download.json" 2>"$run_dir/download.log"
./bin/dug download "$registry" --inventory "$inventory_key" --order=-downloads.total \
  --where 'downloads > 20 AND rating >= 0' --limit 100 \
  --concurrency 4 --downloads 4 >"$run_dir/resume.json" 2>"$run_dir/resume.log"
python3 scripts/verify-e2e.py "$run_dir"
