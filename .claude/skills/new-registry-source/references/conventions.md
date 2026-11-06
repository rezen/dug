# Source conventions

What every dug source agrees on, and why. The host enforces some of this at
the protocol boundary (`sdk/plugin/wire.go`); the rest is convention that
keeps `--order`, `--where`, manifests, and resume behaving the same across
registries.

## Reference anatomy

```go
registry.Reference{
    Registry:   "firefox",              // the identity; must equal the plugin's
    Namespace:  "addons.mozilla.org",   // upstream host; informational
    Name:       "ublock-origin",        // upstream stable identifier
    Version:    "1.62.0",               // upstream version string
    Attributes: map[string]string{...}, // resolution inputs, part of identity
    Metadata:   map[string]string{...}, // ordering facts, excluded from identity
}
```

`Attributes` and `Metadata` are both persisted to S3 in the inventory and
restored for stage two, so neither may hold a credential. The split matters
for resume: artifact identity hashes `Attributes` plus the resolved revision,
so a popularity change in `Metadata` does not force a re-download, while a
changed download URL or digest in `Attributes` does.

## Attributes

Pin at listing time everything Resolve needs, so stage two never consults
live metadata and a deleted upstream version fails rather than being replaced:

| Key | Format | Notes |
| --- | --- | --- |
| `download_url` | `https://HOST/...` | Must be on `DefaultDownloadHosts`. Record the URL before any redirect; `httpx.Get` follows redirects, the allowlist applies to the persisted URL. |
| `sha256` | `sha256:<64 lowercase hex>` | Normalize base64 or uppercase digests during discovery. Omit the key when the registry publishes none. |
| `size` | decimal bytes | Omit when unknown; `Blob.Size` is then -1. |
| upstream IDs | decimal or opaque string | `addon_id`, `file_id`, `release_id`, `commit`. They let Resolve derive URLs instead of trusting persisted ones (github) and make blob names unique. |
| structured lists | JSON in a string | github stores release assets this way. Validate every element in Resolve. |

## Listing facts: `Facts` and `Metadata`

Two reference fields carry mutable listing facts. Both are persisted in the
inventory and excluded from artifact identity, so a popularity change never
forces a re-download.

### `Reference.Facts`: typed, the same meaning in every registry

`*registry.Facts` from `sdk/registry/facts.go`. The host orders and filters on
these by dotted path, and its built-in orders are aliases for them:

| Field | Path | Built-in order | Meaning |
| --- | --- | --- | --- |
| `Downloads.Total` | `downloads.total` | `downloads` | Package-wide downloads since publication |
| `Downloads.Day` ... `.Year` | `downloads.day`, `.week`, `.month`, `.quarter`, `.year` | | Package-wide downloads over a trailing 1, 7, 30, 90 or 365 days |
| `VersionDownloads.*` | `version_downloads.total`, ... | | Downloads of this version alone |
| `Installs` | `installs` | `popular` | Current install base or active users |
| `UpdatedAt` | `updated_at` | `updated` | When the package last changed upstream |
| `ReleasedAt` | `released_at` | `released` | When this version was published; date-mode `version_depth` |
| `Repository` | `repository.url`, `.source`, `.commit`, `.directory` | | Source repository |

How to fill them:

- Use `registry.Int64(n)` and `registry.Time(t)`; leave a field nil when the
  registry does not publish it. Never derive one window from another: AMO
  publishes weekly downloads, so firefox sets `Downloads.Week` and leaves
  `Downloads.Total` nil rather than guessing.
- Build the package-wide facts once per item, then `facts.Clone()` per
  version and set `ReleasedAt` on the clone, so emitted references share
  nothing mutable (firefox `emitVersions`).
- `Repository.URL` goes through `registry.NormalizeRepositoryURL`; `Source` is
  `registry.RepositoryDeclared` for a publisher's unverified claim,
  `RepositoryAttested` when provenance ties it to the artifact, and
  `RepositoryRegistry` when the registry hosts the repository itself.
- The plugin bridge rejects references whose facts fail `Facts.Validate()`
  (negative counts, zero timestamps, repositories without URL or source), and
  tests should assert the facts struct field by field, as
  `sources/firefox/firefox_test.go` does.

### `Reference.Metadata`: registry-specific strings

`map[string]string` for everything that is not a typed fact. The host reads it
through `metadata:KEY` (lexical), `number:KEY` and `date:KEY`; `--order rating`
is an alias for `-number:rating`.

| Key | Format | Notes |
| --- | --- | --- |
| `rating` | decimal, **0–100** | Normalize; keep the raw scale under its own key (firefox `rating_average`, 0–5) |
| `num_ratings` | decimal | Filter alias `ratings` / `num_ratings` |
| `version_rank` | 1, 2, 3, ... | 1 is the current version; lets a deep inventory select "current only" |
| `release_date_source` | short slug | Which upstream field `ReleasedAt` came from (`amo-file-created`, `github-release-published`, `git-commit-date`) |
| anything else | strings | `stars`, `kind`, `prerelease`, `license`; counts as decimal strings, times as UTC RFC3339 |

Rules behind both tables:

- Do not duplicate a typed fact in `Metadata`; firefox tests assert
  `weekly_downloads` and `released_at` are absent from its metadata.
- A metric whose meaning differs from a typed fact keeps a distinct metadata
  name instead of being forced into the struct.
- Item-wide facts are attached to every version, including old ones; they are
  a listing-time snapshot, not history. Say so in the README.
- Missing values sort last. Do not fabricate zeros.

The SDK and host evolve; if `sdk/registry/facts.go`, `inventory/order.go`
(`orderAliases`) or `inventory/filter.go` disagree with this file, they win.
Copy the most recently changed source rather than this text.

## Revision

`Artifact.Revision` must change whenever the bytes change and should stay
identical otherwise, because the host skips completed manifests by identity
plus revision.

1. The content digest, when the registry publishes one (`sha256:...`).
2. Else an immutable upstream identifier: a commit SHA, a file ID, a manifest
   digest.
3. Else the version string, only when the registry promises versions are
   immutable (most package registries do; moving tags do not).

## Blobs

- `Name` is a plain file name: nonempty, not `.` or `..`, no `/`, `\` or NUL.
  Manifest consumers write it to disk. Make it unique within the artifact and
  stable across repeated resolutions of the same revision, because the Connect
  bridge re-resolves on every download and looks the blob up by name.
  Include an ID or version in it: `ublock-origin.4321.xpi`, `cli-<sha>.zip`.
- `MediaType` is the archive type (`application/zip`,
  `application/x-xpinstall`, `application/octet-stream` when unknown).
- `Digest` is `sha256:<hex>` or empty. `Size` is bytes or -1. The host
  verifies both when present.
- `Open` takes its own context, performs the request with `httpx.Get` (or the
  registry's authenticated client), and returns `response.Body`. It must work
  after the resolution context is cancelled and must return byte zero on each
  call; the engine retries from scratch.

## Resolve

Resolve is the trust boundary. Inventories are files in S3; assume one could
have been edited.

- Check `ref.Registry == Identity`, nonempty name and version, and the name
  against the registry's identifier rules.
- `httpx.ValidateURLHost(url, s.downloadHosts())` on every persisted URL. The
  test suite's off-host case is a cloud metadata address such as
  `http://169.254.169.254/latest/meta-data/`.
- Parse digests and sizes strictly; reject uppercase hex, wrong lengths,
  zero or negative sizes.
- Prefer deriving URLs from validated IDs over trusting persisted URLs when a
  token is involved (github builds `repos/OWNER/NAME/zipball/SHA` itself), so
  the token-bearing client can never be pointed at a foreign route.
- No network requests when the descriptor is complete. If resolution must
  re-query (wordpress has no digest, so its revision is the version), keep it
  to the one request that the reference already identifies.

## HTTP

- `pluginhost.Main` builds one `http.Client` per plugin process
  (`services.NewDugHttpClient` with the `--connections` limit) and passes it to
  `PluginFactory`; every factory call shares it. Do not build your own
  transport; `httpx.Transport` is deprecated.
- `httpx.Get(ctx, client, url, attempts)`: validates the URL, sets
  `User-Agent: dug/0.1`, retries 429/408/5xx with jitter and `Retry-After`,
  and marks other HTTP errors `retry.Permanent`. Set extra headers (API
  version, `Accept`, bearer token) in your own `do` function as github does
  when the registry needs them.
- Metadata reads: `context.WithTimeout(ctx, 2*time.Minute)` and
  `io.LimitReader(body, 32<<20+1)`; exceeding the cap is a permanent error.
  Downloads have no plugin-side timeout; the host's `--timeout` covers them.
- A registry that documents rate limits (crates.io, Chocolatey) needs a
  plugin-side limiter or low default `listing_concurrency`; say which in the
  README.
- `Attempts` defaults to 4 in the factory and to 1 in tests.

## Pagination and enumeration

- Follow `next` links only after checking scheme, host and path prefix against
  the API base; a registry's own link could still be a redirect to elsewhere.
- Track seen page URLs and fail on a cycle.
- When a search window is capped (AMO 30,000, GitHub 1,000), split by an
  immutable field such as creation time, recurse, and fail if a one-second
  window still exceeds the cap. Never silently return a partial catalog.
- Compare counts when the API reports a total and fail if the walk ended
  early.
- Dedupe items by upstream ID; live catalogs shift between pages.
- Where listing concurrency is useful (per-item lookups after a cheap index,
  as chrome does), bound it with an option and keep emission order
  deterministic.
- Return `ctx.Err()` promptly; the host cancels discovery on `--limit`.

## Options

- Parsed and validated in `PluginFactory`; the factory is called once per RPC
  and may run concurrently, so it must not mutate shared state.
- Reject unknown keys with the supported list in the message.
- Reject mutually exclusive selectors together (`search` with `slugs`).
- `version_depth`: validate with `registry.ParseVersionDepth` when history is
  supported; otherwise reject with a message saying the registry publishes no
  history. The host forwards `--version-depth` under that key.
- Range-check numeric options (`page_size`, `listing_concurrency`) to the
  registry's documented limits.
- Options are persisted in inventories: selection and tuning only, never
  secrets.

## Credentials

Read from the environment in `cmd/dug-NAME/main.go` (`GITHUB_TOKEN`), passed
into the factory, stored on the `Source`, sent only to the API origin and only
over HTTPS or loopback. Warn at startup when the variable is unset if
anonymous access is degraded. Never log it, never put it in a reference.

## Logging and errors

- `slog.Warn` with structured fields when skipping an entry during a full
  walk; the host prints plugin logs on stderr.
- Errors are lowercase, start with the registry name or the item
  (`firefox page_size must be 1–50`, `acme/widget@v3: cannot peel annotated
  tag`), and wrap the cause with `%w`.
- Invalid upstream metadata is an error, not a guess; missing data in a mode
  that needs it (a date cutoff without release dates) fails the listing.

## Tests

- `httptest.NewServer` with a handler that switches on `r.URL.Path`;
  `Source{Client: server.Client(), Endpoint: server.URL + "/api/", Attempts: 1,
  DownloadHosts: []string{strings.TrimPrefix(server.URL, "http://")}}`.
- Count metadata requests with `atomic.Int32` to prove `Open` does not
  re-query.
- Table-drive version depth: `map[string][]string{"1": {...}, "2": {...},
  "unlimited": {...}, "2025-01-01": {...}, "2099-01-01": {}}`.
- Use trimmed real API responses as payloads so field names and shapes are
  the real ones.
- Keep tests offline; a live probe lives in `.local/` and is not a test.
