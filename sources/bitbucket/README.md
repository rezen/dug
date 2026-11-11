# Bitbucket registry plugin

Independent Go module archiving Bitbucket Cloud repositories: one `.tar.gz` per
tagged commit and a snapshot of the main branch, for the workspaces (tenants)
or repositories you select. Its only Dug dependency is `github.com/rezen/dug/sdk`;
it does not link the host or the S3 engine.

```sh
go test ./...
go build -o dug-bitbucket ./cmd/dug-bitbucket
BITBUCKET_TOKEN=... ./dug-bitbucket --listen 127.0.0.1:7793
```

The host connects with registry identity `bitbucket` and that plugin URL. At
least one of `workspaces` or `repos` is required:

| Option | Meaning |
| --- | --- |
| `workspaces` | Comma-separated workspace slugs; every git repository the workspace lists |
| `repos` | Comma-separated `workspace/slug`; a repository that is missing is an error |
| `forks` | Include forks when walking a workspace; default `false` |
| `snapshot` | `fallback` (default), `always`, `only`, or `never` |
| `version_depth` | Positive integer, inclusive YYYY-MM-DD cutoff, or unlimited; default 1 |

Bitbucket Cloud has no public catalog: `GET /2.0/repositories` without a
workspace returns 404 anonymously (checked 2026-11-11). A workspace walk uses
`/2.0/repositories/WORKSPACE?sort=created_on&pagelen=100`, ordered by the
immutable creation time so paging is stable, follows `next` links only on the
API origin, rejects cycles, and warns when fewer repositories arrive than the
API's `size`. Without a token it sees public repositories only. A repository
that disappears or becomes inaccessible during a walk is skipped with a warning;
an explicit `repos` entry that fails is an error. Mercurial repositories are
skipped, and empty repositories (no main branch) are skipped with a warning.

Versions are tags, ordered by the commit date they point at, newest first
(`sort=-target.date`). That is not always version order: a backported 10.2.7
can come after 11.0.2. Integer depth takes the first N tags; a date cutoff keeps
tags whose commit is on or after it. A snapshot has version = the main branch name:
`fallback` snapshots only repositories with no tags, `always` adds one to every
repository, `only` emits only snapshots, `never` leaves tagless repositories out
(an error for explicit `repos`, a warning during a walk). Snapshots ignore
`version_depth`.

Discovery pins the full commit SHA and `kind` (`tag` or `snapshot`) in
attributes, and the SHA is the revision, so a moved tag produces a new artifact.
Resolution derives `https://bitbucket.org/WORKSPACE/SLUG/get/COMMIT.tar.gz`
from the validated workspace, slug and commit and makes no request; no URL is
persisted. Bitbucket publishes no digest or size, so the blob has neither and
the host hashes it (the archive is deterministic: two fetches gave the same
SHA-256). Blob names are `SLUG-COMMIT12.tar.gz`.

Facts: `updated_at` (repository `updated_on`, repeated on every version),
`released_at` (the commit date), and `repository.url` with the archived commit
and source `registry`. Bitbucket publishes no download counts, install base,
ratings, topics or stars, so those stay absent and `--order downloads` or
`popular` sorts every reference last. The author is the workspace (slug, name,
UUID, URL). Metadata: `kind`, `language`, `repo_size` (bytes), `private`, `fork`,
`default_branch`, `created_at`, `repo_uuid`, `version_rank` and
`release_date_source` (`git-commit-date`). Repository-wide values are
listing-time snapshots attached to every version. For example,
`--order=-date:updated_at --where 'metadata:kind = tag'`.

Rate limits (Atlassian's API request limits): anonymous requests are 60 per
hour per IP; authenticated 1,000 to 10,000 per hour depending on plan, and 5,000
archive downloads per hour. Each repository costs one to three metadata requests
plus one per archive, so set `BITBUCKET_TOKEN` for anything beyond a few
repositories. 429 responses are retried with `Retry-After` or a growing wait. The
token is a repository, project or workspace access token, sent as a bearer token to
`api.bitbucket.org` and `bitbucket.org` only, over HTTPS or loopback, and never
follows a redirect to another host or reaches a reference, inventory or manifest.
The plugin was verified anonymously against public repositories; the token path
is covered by tests against a fake server, not a live private workspace, so confirm
that archive downloads of a private repository accept your token type.

Not covered: the Downloads tab (uploaded files, served from a second host),
Mercurial, wikis and issues, pull request refs, and Bitbucket Data Center
(a different API).

Credentials and S3 configuration belong to the host. `DUG_PLUGIN_TOKEN`
optionally protects the Connect service.

Local development uses the workspace's unpublished SDK. To extract this source,
publish and pin an SDK version, move this directory, and update its module path
and command self-import. No host changes are needed.

API reference: [Bitbucket Cloud REST API](https://developer.atlassian.com/cloud/bitbucket/rest/).
