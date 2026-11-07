# GitHub registry plugin

Independent Go module archiving GitHub repositories through the REST API:
release source archives, release assets, and default-branch or named-ref
snapshots. Its only Dug dependency is the public SDK; it does not link the host
or S3 engine.

From this directory in the workspace:

```sh
go test ./...
go build -o dug-github ./cmd/dug-github
GITHUB_TOKEN=ghp_... ./dug-github --listen 127.0.0.1:7784
```

The host connects with registry identity `github` and that plugin URL. Exactly
one of `repos`, `owner`, or `search` selects repositories:

| Option | Meaning |
| --- | --- |
| `repos` | Comma-separated `owner/name` list |
| `owner` | User or organization login; enumerates the repositories it owns |
| `search` | Repository search query; discovery adds `created:` ranges itself |
| `forks` | Include forks when enumerating an owner; default `false` |
| `assets` | Archive release assets alongside the source archive; default `true` |
| `snapshot` | `fallback` (default), `always`, `only`, or `never` |
| `refs` | Comma-separated extra branches or tags to snapshot for every repository |
| `tags` | Archive every tag not already listed as a release or ref; default `false` |
| `version_depth` | Positive integer, inclusive YYYY-MM-DD cutoff, or unlimited; default 1 |
| `listing_concurrency` | Repositories looked up at once, 1–16; default 4. Search and owner paging stay serial, and references are emitted in walk order |

Versions are published (non-draft) releases in GitHub's API order, newest first.
Integer depth takes the first N; date depth keeps releases published on or after
the cutoff. Listing resolves each release tag to its commit, peeling annotated
tags, and pins that SHA, the release ID, and the uploaded assets (ID, name, size,
SHA-256 digest when GitHub recorded one, content type) in reference attributes.
Resolution derives every download URL from those validated fields; no URL is
persisted, so a tampered inventory cannot steer the token-bearing client to
other API routes or hosts. Prereleases are included and marked `prerelease`.

A snapshot is the current head of the default branch: version = branch name,
revision = commit SHA, so an unchanged head is skipped on the next run and a
moved head produces a new manifest. `fallback` snapshots only repositories with
no published releases, `always` adds one to every repository, `only` skips
releases, and `never` leaves release-less repositories out (an error for explicit
`repos`, a warning during enumeration). `refs` snapshots additional names, such
as a GitHub Action's moving `v4` tag; missing refs fail for explicit `repos` and
are skipped during enumeration. Empty repositories are treated the same way.

`tags=true` archives every remaining tag in GitHub's API order with `kind`
`tag`, revision = the commit the tag points at (the tags route reports peeled
commits, so this costs no extra requests). Integer depth bounds tags
separately from releases; a date cutoff looks up each tag's commit date, one
request per tag, and records it as `released_at`. Re-listing a GitHub Action
with `tags=true` shows whether a moving major tag such as `v4` was retargeted,
because the pinned commit changes the artifact identity.

Blobs: `NAME-SHA.zip` is the source archive (size unknown, hashed by the host);
each asset keeps its own file name, expected size, and digest. Assets still
uploading are skipped with a warning. The `download` attribute selects the
route: `direct` for public github.com repositories fetches
`codeload.github.com/OWNER/NAME/zip/SHA` and
`github.com/OWNER/NAME/releases/download/TAG/NAME` with no token and no API
quota; `api` for private and Enterprise repositories fetches the API zipball and
asset routes with the token, and GitHub redirects to a download host. Go drops
the `Authorization` header on that cross-host redirect.

Facts: `updated_at` (last push), `released_at` (release publication or commit
date), `repository.url` with the archived commit as `repository.commit` and
source `registry`, and for releases `version_downloads.total`, the asset
downloads of that one release. There is no repository-wide count, so
`downloads` stays absent. Metadata: `stars`, `forks`, `open_issues`, `language`,
`license` (SPDX), `fork`, `archived`, `private`, `default_branch`, `repo_id`,
`created_at`, `owner_type` (`User` or `Organization`), `kind` (`release`,
`snapshot`, or `tag`), `prerelease`, `release_date_source`, `asset_count`, and
`version_rank`. Reference tags are the repository topics; the author is the
owner account (login, numeric ID and profile URL). For example,
`--order=-number:stars --where 'metadata:kind = release AND metadata:prerelease = false'`.

Rate limits: every API request, including each `api`-mode blob download, counts
toward GitHub's primary limit (60 per hour unauthenticated, 5,000 per hour with a
token; search is 30 per minute). The plugin waits out primary and secondary
limits using `X-RateLimit-Reset` and `Retry-After`, up to an hour per wait, and
treats other 403 and 404 responses as permanent. Set `GITHUB_TOKEN` in the
plugin environment; it is sent only to the API origin, refused over plaintext to
non-loopback hosts, and never written to references, inventories, or manifests.
Private repositories need a token with access; `owner` enumerates an
organization's private repositories but not a user's own private repositories.

`--api https://HOST/api/v3` targets GitHub Enterprise Server; references then use
that host as their namespace instead of `github.com`. Search windows split by
creation date when a window exceeds 1,000 results; a one-second window that
still exceeds the cap fails rather than silently omitting entries, and pages
flagged `incomplete_results` are retried. Searching all of GitHub is not
practical; scope queries with qualifiers such as `org:`, `topic:`, or `stars:`.

Local development uses the workspace's unpublished SDK. To extract this source,
publish/pin an SDK version, move this directory, and update its module path and
command self-import. No host changes are needed.

API reference: [GitHub REST API](https://docs.github.com/en/rest).
