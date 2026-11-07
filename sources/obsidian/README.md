# Obsidian registry plugin

Independent Go module for the Obsidian community plugins: the catalog
in `obsidianmd/obsidian-releases` and each plugin's GitHub releases. Its
only Dug dependency is `github.com/rezen/dug/sdk`; it does not link the host or
the S3 engine.

```sh
go test ./...
go build -o dug-obsidian ./cmd/dug-obsidian
./dug-obsidian --listen 127.0.0.1:7788
```

The host connects with registry identity `obsidian` and that plugin URL.
Options: `ids` (comma-separated plugin ids; without it the whole catalog is
walked) and `version_depth` (positive integer, inclusive YYYY-MM-DD cutoff, or
`unlimited`; default 1, the newest non-draft release). `listing_concurrency`
(1–16, default 4) sets how many plugins' release lists are fetched at once;
references are still emitted in catalog order. Raise it carefully: GitHub's
secondary rate limits apply, and the shared client waits them out.

Discovery reads `community-plugins.json` (about 10,000 entries) and
`community-plugin-stats.json`, then lists each plugin's releases through the
GitHub API, newest first, using the shared client in `sdk/services/github`
(rate limits are waited out). A release needs `main.js` and `manifest.json`;
others are skipped with a warning, as are plugins whose repository is gone. An
explicitly requested id that is missing or has nothing to archive is an error.
Discovery pins the repository, release ID, and each asset's ID, name, size and
SHA-256 digest when GitHub records one (older assets have none; the host then
hashes the bytes). Resolution makes no request. Not covered: themes
(`community-css-themes.json`), plugins removed from the catalog, and releases
whose tag is not a plain version string. A maintainer can replace a release
asset without changing its ID, so the revision (`release:ID`) is only as
immutable as the asset digests.

Facts: `Downloads.Total` and `UpdatedAt` from the catalog stats,
`VersionDownloads.Total` per release, `ReleasedAt` (release publication), and
`Repository` (declared); `Installs` stays nil. Metadata: `plugin_name`, `repo`,
`prerelease`, `asset_count`, `version_rank`, `release_date_source`. Item-level
figures are listing-time snapshots attached to every version.

Credentials and S3 configuration belong to the host. `DUG_PLUGIN_TOKEN`
optionally protects the Connect service. Assets download from
`github.com/OWNER/NAME/releases/download/TAG/FILE` (redirected to GitHub's
CDN) without quota. `GITHUB_TOKEN` is read from the environment for release
listings; without it GitHub allows 60 requests an hour. `--api` points at a
GitHub Enterprise API.

Local development uses the workspace's unpublished SDK. To extract this source,
publish and pin an SDK version, move this directory, and update its module path
and command self-import. No host changes are needed.

API reference: https://docs.github.com/en/rest/releases/releases and the
catalog at https://github.com/obsidianmd/obsidian-releases.
