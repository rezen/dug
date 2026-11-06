# Storage and resume

Everything for one kind of artifact lives under `<registry>/<type>/`. The type
comes from the registry identity (`internal/layout`): `wordpress` is
`wordpress/plugins`, `wordpress-themes` is `wordpress/themes`, `chrome` is
`chrome/extensions`, `firefox` is `firefox/addons`, `github` is `github/repos`,
`ansible-galaxy` is `ansible-galaxy/collections`, `helm` is `helm/charts`, `n8n` is
`n8n/nodes`, `obsidian` is `obsidian/plugins`. A registry not listed there uses
`packages`; add new sources to that map.

```text
s3://BUCKET/PREFIX/<registry>/<type>/
  artifacts/<name>/<version>/<blob name>   # the downloaded files, as-is
  artifacts/<name>/<version>/manifest.json # completion record, written last
  inventories/<ulid>.jsonl                 # complete stage-one inventory, sorted by time
  last_run/<hash>.json                     # latest inventory per query

e.g. wordpress/plugins/artifacts/akismet/5.3/akismet.5.3.zip
```

Names, versions and blob names are percent-encoded where needed, so a `/` in a
version (a git branch) becomes `%2F` and nothing can climb out of its directory;
`.`, `..` and empty values are rejected, and `manifest.json` is reserved. A name
may contain `/` as a directory boundary (GitHub's `owner/repo`). Files are not
deduplicated across versions: each version stores its own copy.

Manifests contain the source, namespace, resource name, requested version,
resolved revision, completion time, and each blob's S3 key, SHA-256, size, name,
and media type. Keys inside manifests are relative to `PREFIX`. A version's
location is its name and version; the resolved revision decides whether a stored
version is current. Mutable sorting metadata does not affect either, so changed
popularity alone does not force a download.

WordPress discovery does not supply archive hashes. ZIPs are hashed as they
download. SHA-256 records the downloaded content; it does not independently verify
publisher authenticity. Chrome supplies an expected SHA-256 and size, which the engine verifies.

A manifest is committed **only after every blob upload succeeds**. If a version is
seen again with a different resolved revision (a moving branch, a republished
version), its files and manifest are replaced in place; the same revision is skipped. Rerunning the
`download --inventory KEY` command skips completed manifests without discovery.
Rerunning `sync` creates a fresh inventory and then skips completed versions.
Stage-one listing itself restarts from the beginning after interruption. A manifest whose revision matches is trusted, so avoid
deleting files next to it. Use `--refresh` to bypass completion checks and
download and overwrite a version's files again.

Individual failures are logged as JSON to stderr and processing continues by
default. Any failure produces a nonzero exit code: 2 for usage errors and 1 for
failed runs. `--fail-fast` cancels the run on
the first failed resource. A final JSON report goes to stdout. `download` and `sync`
log stage starts and, every 10 seconds, a `listing` (entries found so far) or
`downloading` (processed and failed counts) heartbeat to stderr. `--quiet` suppresses
those and successful-resource logs; failures and the `inventory complete` key still print. Ctrl-C cancels discovery and transfers; failed multipart
uploads are aborted on a best-effort basis with a separate cleanup deadline.

