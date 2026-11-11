<p align="center">
  <img src="docs/assets/dug-small.png" alt="dug" width="200">
</p>


# dug


Bulk-download packages from public registries into S3-compatible storage, intact
and with provenance, hashes, and completion manifests. Built to collect a corpus
for secret scanning and security review.

Supported registries: WordPress plugins and themes, Chrome extensions, Firefox
extensions, GitHub repositories, n8n community nodes, Ansible Galaxy collections,
Obsidian community plugins, Helm charts, and OCI registries (Docker Hub, Quay, GHCR,
ECR Public).

## How it works

1. **List**: enumerate a registry's metadata into a saved inventory (no downloads).
2. **Download**: fetch that inventory's archives in the order you choose, filtered
   by facts such as downloads, rating, or update time. Rerunning skips what is done.

`dug sync` does both in one command. Each registry is a separate Connect RPC
plugin process, so the host knows nothing about any particular registry.

## Quick start

Requires Go 1.27+, [just](https://github.com/casey/just), and Docker (for the
local S3 used below). Each command starts the registry plugin and a local rclone S3 server
bucket for you, then stops the plugin when it finishes.

```sh
just list wordpress --limit 20                     # inventory only, no downloads
just sync wordpress --option slugs=akismet         # list and download one plugin
just sync wordpress --limit 100 --order popular    # top 100 by downloads
just sync github --option repos=cli/cli            # any registry, same shape
```

Swap `wordpress` for any registry below. Against your own bucket, use the standard
AWS credential chain and run the host directly (plugin running separately, see
[Plugin design](docs/plugins.md)):

```sh
export DUG_BUCKET=artifact-corpus
export DUG_S3_ENDPOINT=http://localhost:9000   # omit for AWS S3

./bin/dug sync wordpress --limit 100 --order updated
```

## Inventory dashboard

`just web` serves a read-only page at <http://127.0.0.1:7790> over the local inventories
`dug list` keeps in `.local/inventories` (`--inventory-dir` to change). Each inventory shows its
entry and package counts, the download size where the registry publishes archive sizes
(Ansible Galaxy, Chrome, Firefox; WordPress publishes none, so the page says the total is
unknown rather than zero), and the top 10 packages by downloads, installs, or stars. It has no
authentication, so it listens on loopback by default.

## Registries

| Registry | Plugin binary | Default port | Example selector |
| --- | --- | ---: | --- |
| `wordpress` | `dug-wordpress` | 7781 | `--option slugs=akismet` |
| `chrome` | `dug-chrome` | 7782 | `--option ids=<extension id>` |
| `firefox` | `dug-firefox` | 7783 | `--option slugs=ublock-origin` |
| `github` | `dug-github` | 7784 | `--option repos=cli/cli` |
| `wordpress-themes` | `dug-wordpress-themes` | 7785 | `--option slugs=astra` |
| `n8n` | `dug-n8n` | 7786 | `--option packages=<npm name>` |
| `ansible-galaxy` | `dug-ansible-galaxy` | 7787 | `--option collections=community.general` |
| `obsidian` | `dug-obsidian` | 7788 | `--option ids=dataview` |
| `helm` | `dug-helm` | 7789 | `--option charts=argo/argo-cd` |
| `oci` | `dug-oci` | 7791 | `--option repos=docker.io/alpine` |

Settings can live in a `dug.yaml` in the working directory (or `--config PATH` /
`DUG_CONFIG`): keys are flag names, with per-registry overrides and `options`.
Flags override the file. See [dug.example.yaml](dug.example.yaml).

Point the host at a plugin with `--plugin-url` or `DUG_PLUGIN_URL` (default
`http://127.0.0.1:7781`). Set matching `DUG_PLUGIN_TOKEN` values in host and plugin
to enable bearer authentication. Details and options for each registry are in
[docs/sources.md](docs/sources.md).

## Documentation

| Doc | Covers |
| --- | --- |
| [Workflow](docs/workflow.md) | `list`, `download`, `sync`, inventories, ordering, limits |
| [Filters and version history](docs/filtering.md) | `--where` expressions, facts, `--version-depth` |
| [Sources](docs/sources.md) | Per-registry options, behavior, and caveats |
| [S3 and local setup](docs/s3.md) | Bucket permissions, rclone, GoFakeS3 |
| [Storage and resume](docs/storage.md) | Bucket layout, manifests, failures, exit codes |
| [Tuning](docs/tuning.md) | Concurrency, timeouts, retries, HTTP caching |
| [Plugin design](docs/plugins.md) | Protocol, lifecycle, authentication |
| [Registry interface](docs/registry-interface.md) | Writing a new registry adapter |
| [Module boundaries](docs/monorepo.md) | Workspace layout and repository extraction |
| [Development](docs/development.md) | `just` tasks, tests, live end-to-end runs |

Each source module also has its own README; see [`sources/`](sources/README.md).

## Development

```sh
just build   # all executables
just check   # race tests, vet, module boundaries
```

See [docs/development.md](docs/development.md).
