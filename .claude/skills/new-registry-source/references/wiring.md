# Workspace wiring for a source

The workspace is a set of independent modules, so nothing discovers a new
source automatically. Every list below is maintained by hand, and each one
has a distinct failure mode when missed. `scripts/scaffold.py` applies the
edits marked "script" when it finds the anchor; verify them in `git diff` and
do the rest yourself.

Find anything this list has fallen behind on with:

```sh
grep -rln firefox --exclude-dir=sources --exclude-dir=.git --exclude-dir=bin .
```

Every file it prints names an existing source somewhere; a new source should
appear in the same places.

## Module files (script creates, you fill in)

```
sources/NAME/
├── go.mod                 module github.com/rezen/dug/sources/NAME; requires only the SDK
├── go.sum                 copied from an existing source (identical dependency set)
├── NAME.go                Source, Discover, Resolve, HTTP helpers
├── plugin.go              PluginFactory: option validation
├── provider.go            Provider(): identity and catalog kind
├── NAME_test.go           offline tests against httptest
├── README.md              options, metadata, coverage, API reference
└── cmd/dug-NAME/main.go   pluginhost.Main: shared flags and lifecycle; extra flags and credentials via hooks
```

The package name is NAME without hyphens; the directory, identity and
executable keep the hyphen if there is one (`open-vsx` is awkward; prefer
`openvsx`).

## Registration

| File | Edit | Why | Done by |
| --- | --- | --- | --- |
| `go.work` | `./sources/NAME` in the `use` block, sorted | Otherwise the workspace cannot resolve the unpublished SDK for this module. Do not touch the `replace` lines; they are load-bearing. | script |
| `scripts/build.sh` | `go build -o bin/dug-NAME ./sources/NAME/cmd/dug-NAME` | `just build` and CI produce the binary | script |
| `scripts/check.sh` | add `sources/NAME` to the `set -- . sdk ...` module list, before `integration` | `just check`, `just test`, `just vet`, `just fmt` iterate this list; `./...` from the root stops at nested `go.mod` files | script |
| `scripts/check-boundaries.sh` | add `sources/NAME` to the `for module in ...` list | Builds the module with `GOWORK=off` and fails if it imports the host or another source | script |
| `justfile` | in the `plugin` recipe, add `NAME` to the `case` arm that runs `sources/$1/cmd/dug-$1` (a source served from another module's directory, like `wordpress-themes`, gets its own arm), and to the recipe comment; then `just --fmt --check` | `just plugin NAME --listen ...` | script |
| `cmd/dug/main.go` | a help block before "Run a command with --help": `TITLE: dug-NAME (use --plugin-url http://127.0.0.1:PORT)` followed by `--option` examples | `dug --help` is where users learn the options; this is documentation, not registration | script inserts a placeholder; you replace the options |
| `sources/README.md` | a table row; tick or remove the todo entry; correct the note if the API differs | The source index and the backlog | script inserts a row with TODO cells |
| `docs/monorepo.md` | a module table row, two mermaid edges, the executable count in "Local development", the tag example list | Module boundary documentation | script does the row and edges |
| `docs/plugins.md` | registry list in the first paragraph; an options table ("NAME accepts:"); the default port sentence in "Deployment"; a metadata paragraph at the end | The protocol-level reference for options and metadata | you |
| `README.md` | the bold registry list in the intro; a `## TITLE` section with runnable commands, options, metadata and caveats; the compile-time-check sentence in "Registry plugin interface" (`NAME.Source`); the ordering note when the source lacks popularity | The user-facing manual | you |
| `.github/workflows/check.yml` | nothing | It runs `just check` and `just build`, which read the lists above | nobody |

## Ports

Each executable defaults to the next `127.0.0.1:778N` address. The script picks
`max + 1` over every `sources/*/cmd/*/main.go`. The same port appears in
`main.go`, the help text, `sources/README.md`, `docs/plugins.md`, the module
README and the root README section; keep them identical.

## Help text shape

```
Firefox: dug-firefox (use --plugin-url http://127.0.0.1:7783)
  --option slugs=ublock-origin,darkreader
  --option search=TEXT
  --option page_size=50

GitHub: dug-github (use --plugin-url http://127.0.0.1:7784; set GITHUB_TOKEN)
  --option repos=OWNER/NAME,OWNER/NAME
```

One line per option with a realistic example value; mention a required
environment variable in the header line.

## Root README section shape

```markdown
## TITLE

```sh
./bin/dug-NAME --listen 127.0.0.1:PORT

# One explicit-selection example and one catalog example.
./bin/dug sync NAME --plugin-url http://127.0.0.1:PORT --option slugs=a,b --version-depth 2
./bin/dug list NAME --plugin-url http://127.0.0.1:PORT --limit 100
./bin/dug download NAME --plugin-url http://127.0.0.1:PORT \
  --inventory INVENTORY_KEY --order=-number:KEY --where '...' --limit 100
```

Prose: what the options select, how enumeration works and what it cannot
see, how version depth behaves, which metadata keys exist and what they
mean, which hosts serve bytes, whether digests are upstream or host-computed,
credentials. Link to `sources/NAME/README.md`.
```

## Module README shape

Follow `sources/firefox/README.md`: one paragraph on what it archives and its
only dependency, a build-and-run block, options, enumeration behavior and
limits, metadata with scales, security notes (download hosts, digests,
credentials), the extraction note, and the upstream API reference link.

## Optional

- `integration/`: an end-to-end test through the Connect bridge, inventory and
  engine, as `integration/chrome_test.go` does. Worth adding when the source
  has engine-visible behavior (bounded listing concurrency, multi-blob
  artifacts). Requires adding the source module to `integration/go.mod` and a
  matching `replace` in `go.work`.
- `scripts/e2e-NAME.sh` and a `just e2e-NAME` recipe: a live run against
  `bin/dug-s3local` with integrity and resume checks, following
  `scripts/e2e-firefox.sh`.

## Bucket layout

Add the registry identity to the `types` map in `internal/layout/layout.go` so its
objects land under `<registry>/<type>/` (for example `helm/charts`). An unlisted
registry works but is filed under `<registry>/packages`.
