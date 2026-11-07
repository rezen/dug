# n8n community nodes registry plugin

Independent Go module that archives n8n community node packages from the npm
registry. A community node is an npm package carrying the
`n8n-community-node-package` keyword; n8n loads it into its own process, where it
can read every credential the instance holds, and self-hosted instances install any
matching package without review. Its only Dug dependency is the public SDK, whose
`sdk/services/npm` package is the shared npm client (package documents, search,
download counts, tarball rules and integrity verification) that other npm-hosted
sources can reuse. It does not link the host or S3 engine.

From this directory in the workspace:

```sh
go test ./...
go build -o dug-n8n ./cmd/dug-n8n
./dug-n8n --listen 127.0.0.1:7786
```

The host connects with registry identity `n8n` and that plugin URL. Options:

| Option | Meaning |
| --- | --- |
| `packages` | Comma-separated npm package names, scoped or not. Explicit packages need not carry the keyword; a missing one is an error |
| `keyword` | npm keyword to walk; lowercase letters, digits, `.`, `_`, `-`. Default `n8n-community-node-package` |
| `enumerate` | `all` (default), `search` or `ecosystems` |
| `version_depth` | Positive integer, inclusive `YYYY-MM-DD` cutoff or `unlimited`; default 1 |
| `listing_concurrency` | Packument fetches in flight, 1–16, default 4. Download counts for the next 128 packages are fetched while the current ones emit; references are still emitted in package order |

`packages` excludes `keyword` and `enumerate`. Unknown options are errors. Other
npm-hosted plugin ecosystems fit the same shape through `keyword` (for example
`node-red`), though their facts and metadata have not been checked against it.

## Enumeration

npm offers no way to walk a keyword to the end:

- **npm search** (`/-/v1/search?text=keywords:KEYWORD`) reports the full total
  (14,244 on 2026-11-06) but serves only the first 5,000 results. Later offsets
  return the top of the ranking again instead of failing, so a naive walk would
  silently return a wrong catalog. Keyword terms combine only with AND (a comma),
  and negation does not combine, so the window cannot be split into exact
  complements. Search also answers HTTP 429 under load; `Retry-After` is honored.
- **ecosyste.ms** (`/api/v1/keywords/KEYWORD`, 1000 per page, `Link` headers) pages
  through everything it knows (13,807 npm packages), across ecosystems, but its
  sync lags npm: newer packages are missing and removed ones remain.
- **api.n8n.io/api/community-nodes** lists only n8n's verified nodes, so it cannot
  enumerate the catalog.

`enumerate=all` walks the ecosyste.ms index, then adds what npm search knows and the
index did not (449 names on 2026-11-06; the union was 14,265). Every name is then
fetched from npm. A name npm no longer serves, an unpublished package, or one that
lost the keyword is skipped with a warning; ecosyste.ms entries from other
ecosystems are ignored. At the end of a walk the plugin warns when it found fewer
live packages than npm search reports. That gap is expected to be small but is not
zero-checked: **a full walk is a best-effort subset of the catalog, not a proven
complete one.** `enumerate=search` covers only the top 5,000 and warns that it is
partial; `enumerate=ecosystems` has no npm total to compare against.
If ecosyste.ms or npm search is unavailable, the walk fails rather than returning
the other index's partial result.

## Versions and pinning

Discovery fetches each package's document once. History follows it: the current
version (the `latest` dist-tag, or the newest publish if the tag is missing) comes
first, then the remaining versions newest published first. Date depth uses the
version's publish time and fails if a version has none. Each reference pins the
tarball URL, derived from the validated name and version rather than copied from
the document, and npm's integrity hash. Resolution makes no request, rejects any
URL that is not exactly that tarball route on `registry.npmjs.org`, and fails for a
version that was unpublished rather than substituting another.

A walk costs one package document per package plus a download-count request per
scoped package (about 3,500); unscoped names are counted in batches of 128. It is
sequential and has not been timed end to end. Package documents are capped at 32
MiB (`n8n-nodes-base`, which is not a community node, is about 30 MB, close to
it); a package over the cap fails the listing.

## Facts and metadata

- `downloads.month`: npm's trailing 30 days, repeated on every version. npm has no
  lifetime total or weekly figure here, so `downloads.total` and `downloads.week`
  stay absent and the `downloads` and `popular` built-in orders have nothing to sort
  on; use `--order=-downloads.month`.
- `updated_at`: the package document's modified time. `released_at`: the version's
  publish time.
- `repository`: the package's `repository` field with source `declared`, and its
  monorepo directory; the publisher's unverified claim.
- Tags: the package's keywords, lowercase. Author: the account that published that
  version (username and email), which differs from the maintainers when a release is
  made by someone else, as after an account takeover.
- Metadata: `version_rank` (1 is current), `release_date_source` (`npm-time`),
  `license`, and, only when true, `install_script` (a preinstall, install or
  postinstall script), `deprecated` and `provenance` (a build attestation).

Package-wide values are a listing-time snapshot and are attached to older versions
too. npm publishes no ratings or install base.

## Security notes

Tarballs come only from `registry.npmjs.org` (`DefaultDownloadHosts`). npm
publishes SHA-512 (SHA-1 for versions from before 2017) and no SHA-256 or size, so
the blob has an empty digest and size -1 for the host, which computes SHA-256. The
plugin itself checks the published hash while the tarball streams, so a transfer
whose bytes differ from npm's own hash fails instead of being archived. This does
not check npm's registry signatures or provenance attestations; `provenance` only
records that one exists. No credentials are used or persisted. `DUG_PLUGIN_TOKEN`
optionally protects the Connect service.

Local development uses the workspace's unpublished SDK. To extract this source,
publish/pin an SDK version, move this directory, and update its module path and
command self-import. No host changes are needed.

API references: [npm registry](https://github.com/npm/registry/blob/main/docs/REGISTRY-API.md),
[npm downloads](https://github.com/npm/registry/blob/main/docs/download-counts.md),
[ecosyste.ms packages](https://packages.ecosyste.ms/docs/index.html).
