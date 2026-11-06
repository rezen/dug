# Storage and resume

```text
s3://BUCKET/PREFIX/
  inventories/sha256/<hex>.jsonl # complete stage-one inventory
  blobs/sha256/<hex-digest>       # exact downloaded bytes
  manifests/<identity-hash>.json # completed resource + provenance + blob references
```

Manifests contain the source, namespace, resource name, requested version,
resolved revision, completion time, and each blob's S3 key, SHA-256, size, name,
and media type. Keys inside manifests are relative to `PREFIX`. Artifact identity
includes discovery attributes and the resolved revision. Mutable sorting metadata
is excluded from identity, so changed popularity alone does not force a download. For WordPress, the
plugin version is the revision; new versions create new completion manifests.

WordPress discovery does not supply archive hashes. ZIPs are hashed as they
download and deduplicated before upload. Identical content goes to the same blob
key. SHA-256 records the downloaded content; it does not independently verify
publisher authenticity. Chrome supplies an expected SHA-256 and size, which the engine verifies.

A manifest is committed **only after every blob upload succeeds**. Rerunning the
`download --inventory KEY` command skips completed manifests without discovery.
Rerunning `sync` creates a fresh inventory and then skips completed versions.
Stage-one listing itself restarts from the beginning after interruption. Existing completion manifests are trusted; avoid
deleting their referenced blobs. Use `--refresh` to bypass completion checks and
re-download a plugin archive and repair missing blobs.
Stored blobs are trusted when HEAD reports the expected size; an object with the
wrong size is replaced. Refresh does not rehash existing S3 objects.

Individual failures are logged as JSON to stderr and processing continues by
default. Any failure produces a nonzero exit code: 2 for usage errors and 1 for
failed runs. `--fail-fast` cancels the run on
the first failed resource. A final JSON report goes to stdout. `download` and `sync`
log stage starts and, every 10 seconds, a `listing` (entries found so far) or
`downloading` (processed and failed counts) heartbeat to stderr. `--quiet` suppresses
those and successful-resource logs; failures and the `inventory complete` key still print. Ctrl-C cancels discovery and transfers; failed multipart
uploads are aborted on a best-effort basis with a separate cleanup deadline.

