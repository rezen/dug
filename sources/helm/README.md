# Helm charts registry plugin

Independent Go module that archives Helm charts indexed by
[Artifact Hub](https://artifacthub.io). Its only Dug dependency is
`github.com/rezen/dug/sdk` (the shared `sdk/services/artifacthub` client); it
does not link the host or the S3 engine.

```sh
go test ./...
go build -o dug-helm ./cmd/dug-helm
./dug-helm --listen 127.0.0.1:7789
```

The host connects with registry identity `helm` and that plugin URL.
Options: `charts` (comma-separated `REPOSITORY/CHART`, using Artifact Hub's
repository names), `repos` (every chart of those Artifact Hub repositories),
`search` (full-text query, up to 100 characters), and `version_depth` (positive
integer, inclusive date cutoff or unlimited; default is the current version).
`charts` cannot be combined with `repos` or `search`. Unknown options are errors.

With no selector the plugin walks every helm package Artifact Hub indexes,
deprecated ones included (18,942 on 2026-11-06), 60 per page, ordered by last
update. Artifact Hub answers a sustained search rate above about one request per
second with a bare 429, so searches are spaced two seconds apart and the index
walk takes about ten minutes; package requests are spaced 400 ms. Because the
catalog shifts while it is walked, the walk repeats up to three times if it comes
up short of the reported total, warns about a small remaining gap and fails on a
large one. An explicitly requested chart that is missing, or whose current
version cannot be archived, is an error; during a walk such versions are skipped
with a warning. Private repositories and packages Artifact Hub does not index
are not covered.

Versions come from the chart's version list in Artifact Hub's order (newest
first), each fetched from its own version document because that is the only
place an older version's content URL and digest appear: depth `N` costs `N`
package requests per chart. Discovery pins the chart's download URL, the
repository URL, the package ID and the SHA-256 digest; resolution validates them
again, makes no request, and fails if the version was deleted upstream. The
revision is the digest. Artifact Hub publishes no archive size, so blobs carry
size -1 and the host verifies the digest while streaming.

Metadata: `stars` (search results only, so explicit `charts` lack it), `repository`,
`official`, `verified_publisher`, `cncf`, `category`, `app_version`, `license`,
`prerelease`, `deprecated`, `signed`, `version_rank` and `release_date_source`
(`artifacthub-ts`, the repository's own timestamp for the version). Typed facts:
`UpdatedAt` (the current version's publish time), `ReleasedAt` and a declared
`Repository` from the chart's source link; `Downloads` and `Installs` stay nil
because Artifact Hub publishes neither. Item-level values are listing-time
snapshots attached to every version.

Security notes. Charts are served from each repository's own host, so there is no
fixed allowlist. Resolve accepts a download URL only over HTTPS on the default
port to a dotted public DNS name (no IP literals, `localhost` or internal
suffixes) that is the repository URL's host or one of `github.com`,
`codeload.github.com` and `raw.githubusercontent.com`. Inventories live in S3 and
the repository URL is part of the persisted descriptor, so an edited inventory
could still name any public host; the rule narrows that, it does not remove it.
Run the plugin without access to internal networks. Charts published to `oci://`
registries (about a tenth, including Bitnami's classic repository) are skipped and
counted in a warning; the future OCI source covers them. Versions without a
digest, on an HTTP URL or on an unaccepted host are skipped too, since nothing could
pin them. Artifact Hub needs no credentials. `DUG_PLUGIN_TOKEN` optionally protects
the Connect service.

Local development uses the workspace's unpublished SDK. To extract this source,
publish and pin an SDK version, move this directory, and update its module path
and command self-import. No host changes are needed.

API reference: https://artifacthub.io/docs/api/
