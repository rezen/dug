#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
run_dir="${DUG_E2E_DIR:-.local/e2e-jenkins}"
mkdir -p "$run_dir" bin
run_dir="$(cd "$run_dir" && pwd)"
export AWS_ACCESS_KEY_ID=dug-local AWS_SECRET_ACCESS_KEY=dug-local-secret AWS_SESSION_TOKEN=
export AWS_EC2_METADATA_DISABLED=true DUG_BUCKET=dug-e2e
export DUG_S3_ENDPOINT=http://127.0.0.1:19003 DUG_PLUGIN_URL=http://127.0.0.1:17892
unset DUG_PLUGIN_TOKEN
go build -o bin/dug ./cmd/dug
go build -o bin/dug-s3local ./integration/cmd/dug-s3local
go build -o bin/dug-jenkins ./sources/jenkins/cmd/dug-jenkins
./bin/dug-s3local --listen 127.0.0.1:19003 --data "$run_dir/s3.db" >"$run_dir/s3.log" 2>&1 &
s3_pid=$!
./bin/dug-jenkins --listen 127.0.0.1:17892 >"$run_dir/jenkins.log" 2>&1 &
plugin_pid=$!
trap 'kill "$plugin_pid" "$s3_pid" 2>/dev/null || true; wait "$plugin_pid" "$s3_pid" 2>/dev/null || true' EXIT
python3 - <<'PY'
import socket,time
for port in [19003,17892]:
    for attempt in range(100):
        try:
            with socket.create_connection(('127.0.0.1',port),timeout=.2): break
        except OSError: time.sleep(.1)
    else: raise RuntimeError(f'server did not start on {port}')
PY
kill -0 "$plugin_pid" "$s3_pid"
./bin/dug list jenkins --format summary --option plugins=git,credentials --version-depth 2 >"$run_dir/list.json"
inventory_key="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["inventory_key"])' "$run_dir/list.json")"
for pass in download resume; do
  ./bin/dug download jenkins --inventory "$inventory_key" --order name --concurrency 4 \
    >"$run_dir/$pass.json" 2>"$run_dir/$pass.log"
done
python3 - "$run_dir" <<'PY'
import hashlib,io,json,os,sys,urllib.request,urllib.parse,xml.etree.ElementTree as ET,zipfile
from pathlib import Path
root=Path(sys.argv[1]); output=root/'hpis'; output.mkdir(exist_ok=True)
base=os.environ['DUG_S3_ENDPOINT']+'/dug-e2e/'
def get(key):
    with urllib.request.urlopen(base+urllib.parse.quote(key,safe='/'),timeout=60) as r:return r.read()
resume=json.loads((root/'resume.json').read_text())
assert resume['scheduled']==4 and resume['skipped']==4 and resume['failed']==0,resume
with urllib.request.urlopen(base+'?list-type=2&prefix=dug/jenkins/plugins/artifacts/',timeout=60) as r:doc=ET.fromstring(r.read())
assert doc.findtext('{*}IsTruncated')!='true'
results=[]
for entry in doc.findall('{*}Contents/{*}Key'):
    if not entry.text.endswith('/manifest.json'): continue
    m=json.loads(get(entry.text))
    for blob in m['blobs']:
        data=get('dug/'+blob['key'])
        assert len(data)==blob['size'] and 'sha256:'+hashlib.sha256(data).hexdigest()==blob['digest']
        with zipfile.ZipFile(io.BytesIO(data)) as archive:assert archive.testzip() is None
        name=blob['name'];assert Path(name).name==name
        (output/name).write_bytes(data)
        results.append({'name':m['reference']['name'],'version':m['reference']['version'],'bytes':len(data),'file':name})
assert len(results)>=4
report={'verified_hpis':len(results),'bytes':sum(r['bytes'] for r in results),'resume_skipped':resume['skipped'],'files':results}
(root/'verification.json').write_text(json.dumps(report,indent=2)+'\n')
print(json.dumps(report,indent=2))
PY
