# Firefox registry plugin

Independent Go module implementing Mozilla Add-ons discovery and XPI downloads.
Its only Dug dependency is the public SDK; it does not link the host or S3 engine.

From this directory in the workspace:

```sh
go test ./...
go build -o dug-firefox ./cmd/dug-firefox
./dug-firefox --listen 127.0.0.1:7783
```

The host connects with registry identity `firefox` and that plugin URL. Options:
`slugs` (IDs, slugs or GUIDs, comma separated), `search`, `page_size` (1–50), and
`version_depth` (positive integer, inclusive date cutoff or unlimited).

Search queries public Firefox extensions and partitions creation-date ranges when
a result window exceeds 30,000 items. It still sees a live catalog: private,
deleted, disabled or unlisted releases are excluded. A one-second range that
exceeds the cap fails explicitly. History follows AMO's public version pagination;
current version comes first, then remaining API-ordered versions. Date depth uses
the file creation timestamp. Inventories pin version/file IDs, download URLs,
SHA-256 and size. Resolution does not re-query live metadata.

Facts carry AMO's trailing seven-day downloads as `downloads.week`, average
daily users as `installs`, `updated_at`, and `released_at` (file creation time).
AMO publishes no lifetime count, so `downloads.total` stays absent rather than
being guessed. Metadata keeps `num_ratings`, raw `rating_average` on a 0–5
scale, and `rating` normalized to 0–100. These add-on level metrics are
snapshot facts, including when attached to older versions. Reference tags
merge AMO's category slugs and free-form tags. The author is the first listed
AMO account (name, username, numeric ID and profile URL); further co-authors
are not recorded.

Credentials and S3 configuration belong to the host. `DUG_PLUGIN_TOKEN` optionally
protects the Connect service. XPI signatures are not independently validated;
expected SHA-256 and size are verified by the host.

Local development uses the workspace's unpublished SDK. To extract this source,
publish/pin an SDK version, move this directory, and update its module path and
command self-import. No host changes are needed.

API reference: [Mozilla Add-ons API](https://mozilla.github.io/addons-server/topics/api/addons/).
