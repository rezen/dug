# OCI registries registry plugin

Independent Go module that archives container images and other OCI artifacts
(Helm charts, Sigstore signatures, SBOMs) from distribution registries: Docker
Hub, Quay, GHCR and ECR Public. Its only Dug dependency is
`github.com/rezen/dug/sdk`; it does not link the host or the S3 engine. The
distribution client (token exchange, manifests, blobs, tag lists) and the
Docker Hub and Quay catalog APIs live in `sdk/services/oci`, so another source
can reuse them.

```sh
go test ./...
go build -o dug-oci ./cmd/dug-oci
./dug-oci --listen 127.0.0.1:7791
```

The host connects with registry identity `oci` and that plugin URL.

| Option | Meaning |
| --- | --- |
| `repos` | Comma-separated `HOST/REPOSITORY`, for example `docker.io/alpine`, `quay.io/coreos/etcd`, `ghcr.io/jqlang/jq`. `docker.io/NAME` means `docker.io/library/NAME` |
| `namespaces` | Comma-separated `HOST/NAMESPACE`; every public repository in each. Docker Hub and Quay only |
| `registries` | Registries to walk when no selector is given. Docker Hub and Quay only; default `docker.io,quay.io` |
| `platform` | `os/arch[/variant]` or `all`; default `linux/amd64` |
| `max_layer_size` | Omit layers larger than this many bytes (a warning names each one); default no limit |
| `max_tags` | Registries without a catalog: fail when a repository has more tags than this; 1–100000, default 1000 |
| `version_depth` | Positive integer, inclusive YYYY-MM-DD cutoff, or unlimited; default 1 |

`registries` cannot be combined with `repos` or `namespaces`. Unknown options
are errors.

## What is enumerated

- **Docker Hub**: `namespaces=docker.io/library` lists the official images
  (about 180). Hub has no way to list every namespace, so with no selector
  discovery walks `library` only; community repositories come from `repos` or
  `namespaces`. Tags come from Hub's tag API, newest first, each with the
  digest it pointed at.
- **Quay**: with no selector it walks every public repository through
  `/api/v1/repository` and its `next_page` token. Tags come from the tag API,
  newest first. Quay lists per-architecture tags (`v3.7.3-s390x`) beside the
  index tag, and each counts as a version.
- **GHCR and ECR Public** have no usable catalog (`/v2/_catalog` is denied), so
  name repositories with `repos`. Their tag list is lexical, so recency comes
  from each image's creation time, which costs one or two requests per tag. A
  repository with more than `max_tags` tags fails rather than being truncated.
  Tags that carry no creation time sort last, and a date `version_depth` fails
  on them. Seed lists from the GitHub packages API or the MCP Registry can feed
  `repos`.

Several tags often point at one digest (`latest`, `3.24`, `3.24.4`). Each is a
version and `version_depth` counts tags, so depth 3 can be one image; the host
stores identical blobs once.

## What a reference pins

Listing pins the manifest or index **digest** the tag pointed at. Resolution
fetches that digest, verifies the served bytes hash to it, selects the
platform, and lists these blobs: the index (when there is one), each selected
image manifest, its config, and its layers. `Revision` is the pinned digest, so
a tag that was moved upstream still resolves to the old content, and one that
was deleted fails instead of being replaced. Listing does not fetch manifests
(Docker Hub counts them against its hourly pull allowance); resolution does:
one request for the pinned manifest plus one per selected platform.

- `platform=linux/arm64` matches any variant; `linux/arm64/v8` pins one. A
  single-manifest image ignores `platform`.
- A tag with no image for the selected platform (a Windows-only repository under
  the default `linux/amd64`) fails resolution with a permanent error that lists
  the platforms available; use `platform=all` or a matching platform for those.
- `platform=all` archives every entry of an index, BuildKit attestations
  (SBOM and provenance) included.
- An index whose entries carry no platform (cosign signature or referrer
  indexes, published under `sha256-<digest>` tags) is archived whole.
- Layers shared between platforms are one blob. Blob names are
  `index-`, `manifest-`, `config-` and `layer-` plus 16 hex digits of the digest.

`Name` is `HOST/REPOSITORY` (`docker.io/library/alpine`), `Namespace` is the host
and `Version` is the tag. The host is in the name because the storage layout keys
artifacts by name and version alone, so `docker.io/grafana/grafana` and
`ghcr.io/grafana/grafana` must differ; artifacts land under
`oci/images/artifacts/docker.io/library/alpine/latest/`.

No URL is persisted: requests are built from the validated registry,
repository and digest. Resolution refuses unknown registries, malformed
repositories, tags and digests (`sha256:` plus 64 lowercase hex only), and bad
option values.

## Metadata

Docker Hub supplies `Downloads.Total` (the repository's lifetime pull count,
repeated on every tag), `UpdatedAt`, `ReleasedAt` (the tag's last push, so a
re-pushed tag moves it) and metadata `stars`. Quay supplies `ReleasedAt` (the
tag's start time) only. GHCR and ECR supply `ReleasedAt` from the image's
creation time. Unpublished facts stay nil, so a registry without download counts
sorts last under `--order downloads`. Metadata keeps `version_rank` and
`release_date_source` (`dockerhub-tag-last-updated`, `quay-tag-start`,
`image-created`). Repository-level facts are listing-time snapshots.

## Access and limits

Anonymous access works everywhere. Docker Hub allows 100 manifest pulls per
hour per IP anonymously; a 429 is retried with backoff, but a large run needs a
login. Credentials stay in the plugin process: `DOCKER_HUB_USERNAME` and
`DOCKER_HUB_TOKEN`, `QUAY_USERNAME` and `QUAY_TOKEN`, `GHCR_USERNAME` and
`GHCR_TOKEN`. They are sent only to the registry's own token service, never to
blob redirect targets.

Bytes come from `registry-1.docker.io`, `quay.io`, `ghcr.io` and
`public.ecr.aws`, which redirect blobs to CDNs. A redirect must stay on the
registry's scheme and go to a public DNS name; IP literals, `localhost` and
internal names are refused. Token realms must be the registry's own hosts. The
host verifies every blob's SHA-256 and size against the pinned descriptor.

Not covered: private repositories, Docker Hub namespaces beyond `library`
without naming them, GHCR and ECR enumeration, referrers API discovery, and
Docker schema 1 manifests.

Credentials and S3 configuration belong to the host. `DUG_PLUGIN_TOKEN`
optionally protects the Connect service.

Local development uses the workspace's unpublished SDK. To extract this source,
publish and pin an SDK version, move this directory, and update its module path
and command self-import. No host changes are needed.

API reference: <https://distribution.github.io/distribution/spec/api/>,
<https://github.com/opencontainers/distribution-spec>, Docker Hub
<https://docs.docker.com/reference/api/hub/latest/>, Quay
<https://docs.quay.io/api/>.
