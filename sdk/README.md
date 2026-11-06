# Dug registry SDK

This independent Go module is the shared contract for the host and registry
plugins. Import paths start with `github.com/rezen/dug/sdk`:

- `registry`: `Lister`, `Resolver`, `Registry`, references, blobs, version depth.
- `plugin`: Connect client, handler, and optional standalone `Serve` helper.
- `gen/dug/registry/v1`: generated protobuf and Connect definitions.
- `httpx`, `retry`: public transport/retry utilities for source implementations.

The SDK has no host, storage, or source dependencies. Its protobuf source lives
in `proto/`; regenerate through the workspace's `just generate` recipe.

Run `go test ./...` in this directory. Local consumers currently use the workspace
because the SDK has not been released; `v0.0.0` is a placeholder. See
[module releases and extraction](../docs/monorepo.md) for publication details.
