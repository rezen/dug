"""Read back every completed ZIP from the local S3 API and verify provenance."""
import hashlib
import io
import json
import os
from pathlib import Path
import sys
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
import zipfile

root = Path(sys.argv[1])
endpoint = os.environ.get("DUG_S3_ENDPOINT", "http://127.0.0.1:19000")
bucket = os.environ.get("DUG_BUCKET", "dug-e2e")


def get(key):
    with urllib.request.urlopen(endpoint + "/" + bucket + "/" + urllib.parse.quote(key, safe="/"), timeout=120) as response:
        return response.read()


listing = json.loads((root / "list.json").read_text())
download = json.loads((root / "download.json").read_text())
resume = json.loads((root / "resume.json").read_text())
inventory = get("dug/" + listing["inventory_key"])
assert hashlib.sha256(inventory).hexdigest() == Path(listing["inventory_key"]).stem
records = [json.loads(line) for line in inventory.splitlines()]
entries = [record["reference"] for record in records if record["type"] == "entry"]
assert records[-1]["count"] == len(entries) == listing["entries"]
def total_downloads(ref):
    return ref.get("facts", {}).get("downloads", {}).get("total")


eligible = [ref for ref in entries if (total_downloads(ref) or 0) > 20 and float(ref["metadata"].get("rating", -1)) >= 0]
expected = sorted(eligible, key=lambda ref: -total_downloads(ref))[:100]
assert len(expected) == 100
assert download["scheduled"] == 100 and download["failed"] == 0, download
assert resume["scheduled"] == 100 and resume["skipped"] == 100 and resume["failed"] == 0, resume
keys = []
token = None
while True:
    query = {"list-type": "2", "prefix": "dug/manifests/"}
    if token:
        query["continuation-token"] = token
    with urllib.request.urlopen(endpoint + "/" + bucket + "?" + urllib.parse.urlencode(query), timeout=30) as response:
        xml = ET.fromstring(response.read())
    keys.extend(element.text for element in xml.findall("{*}Contents/{*}Key"))
    token = xml.findtext("{*}NextContinuationToken")
    if xml.findtext("{*}IsTruncated") != "true":
        break
    assert token
manifests = [json.loads(get(key)) for key in keys]
by_identity = {(m["reference"]["name"], m["reference"]["version"]): m for m in manifests}
total = 0
selected = []
for ref in expected:
    manifest = by_identity[(ref["name"], ref["version"])]
    for field in ["rating", "ratings", "num_ratings"]:
        assert field in manifest["reference"]["metadata"], (ref["name"], field)
    assert total_downloads(manifest["reference"]) is not None, (ref["name"], "downloads.total")
    for blob in manifest["blobs"]:
        data = get("dug/" + blob["key"])
        assert len(data) == blob["size"]
        assert "sha256:" + hashlib.sha256(data).hexdigest() == blob["digest"]
        with zipfile.ZipFile(io.BytesIO(data)) as archive:
            assert archive.testzip() is None
        total += len(data)
    selected.append({"name": ref["name"], "version": ref["version"], "downloads": total_downloads(ref), **ref["metadata"]})
(root / "top100.json").write_text(json.dumps(selected, indent=2) + "\n")
report = {"inventory_key": listing["inventory_key"], "listed_entries": len(entries), "verified_resources": len(selected), "verified_zip_bytes": total, "resume_skipped": resume["skipped"], "ranking": "total downloads descending", "backend": "GoFakeS3 v1.2.0 / Bolt"}
(root / "verification.json").write_text(json.dumps(report, indent=2) + "\n")
print(json.dumps(report, indent=2))
