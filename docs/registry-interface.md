# Registry plugin interface

Implement `registry.Registry`:

```go
type Registry interface {
    Discover(context.Context, func(registry.Reference) error) error
    Resolve(context.Context, registry.Reference) (registry.Artifact, error)
}
```

`Discover` streams references serially and must respect backpressure, cancellation,
and callback errors. The host completes discovery before stage two. `Resolve`
can run concurrently with other resolution calls and returns a stable revision plus blob
descriptors. Each blob has a repeatable, context-aware `Open` function, optional
SHA-256, and size (`-1` if unknown). `Open` must use its supplied transfer context,
not retain the earlier resolution context, which is cancelled after resolution.

Expose the implementation with `plugin.NewHandler` in a standalone HTTP server.
The host wraps its URL using `plugin.NewClient` and calls
`inventory.Create` for stage one, then `inventory.Open` and
`engine.Download(ctx, snapshot.Entries, client, store, engine.DefaultOptions())`
for stage two. `Registry` embeds separate `Lister` and `Resolver` interfaces;
the download engine accepts only a `Resolver` and saved `Entries`. The engine owns
concurrency, retries, integrity checking, staging, and completion.
Implement `storage.ReadableStore` (`Stat`, `Put`, `Open`) to add another object backend. Public packages are in
`sdk/registry/`, `sdk/plugin/`, `engine/`, `sources/`, `sdk/services/`, and `services/storage/`; the host CLI
lives in `cmd/dug/`. Each source owns its executable under `sources/NAME/cmd/dug-NAME/`.

`wordpress.Source`, `chrome.Source`, `firefox.Source`, `github.Source`, `n8n.Source`, `ansiblegalaxy.Source`, `obsidian.Source`, `helm.Source`, and `oci.Source` have compile-time interface checks. A future adapter implements the two methods above and is served through
the Connect bridge; no host, engine, or S3 changes are required. Its `Blob.Open` functions handle the registry's download protocol
and authentication. Keep credentials out of persisted reference attributes.
See [the executable adapter example](../sdk/registry/example_test.go) and
[plugin design](plugins.md) for implementation details. `registry.Source`
remains an alias for existing callers.
