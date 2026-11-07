# Joomla registry plugin

Independent Go module for TODO(joomla): what it archives, from where. Its
only Dug dependency is `github.com/rezen/dug/sdk`; it does not link the host or
the S3 engine.

```sh
go test ./...
go build -o dug-joomla ./cmd/dug-joomla
./dug-joomla --listen 127.0.0.1:7790
```

The host connects with registry identity `joomla` and that plugin URL.
Options: TODO(joomla): each option with its range and default, and
`version_depth` (positive integer, inclusive date cutoff or unlimited) or a note
that history is not published.

TODO(joomla): how the catalog is enumerated, what caps it, what it cannot see
(private, unlisted, removed items), and how version history is ordered. Say what
discovery pins (URL, digest, size, IDs) and that resolution does not re-query.

Metadata: TODO(joomla): each key with its scale and meaning (`updated_at`,
`released_at`, downloads or installs, `rating` 0–100 with the raw scale beside
it). Item-level metrics are listing-time snapshots attached to every version.

Credentials and S3 configuration belong to the host. `DUG_PLUGIN_TOKEN`
optionally protects the Connect service. TODO(joomla): which hosts serve
downloads, whether digests are upstream or host-computed, and any upstream
credential the plugin reads from the environment.

Local development uses the workspace's unpublished SDK. To extract this source,
publish and pin an SDK version, move this directory, and update its module path
and command self-import. No host changes are needed.

API reference: the Joomla Extensions Directory is developed in the open at
<https://github.com/joomla-projects/Joomla-Extension-Directory>.
