---
name: new-registry-source
description: >-
  Add a new registry source to dug: an independent Go module under sources/NAME
  that serves the Connect registry protocol (Discover + Resolve) for one upstream
  package registry or artifact catalog, with its dug-NAME executable, option
  factory, tests, README, and every piece of workspace wiring (go.work, build,
  check and boundary scripts, justfile, docs, host help text). Use this whenever
  the user wants to add, create, scaffold, implement, start, or wire up a source,
  registry, plugin, or adapter for dug, picks an entry from the sources/README.md
  todo list (npm, PyPI, OCI, Jenkins, Open VSX, Helm, dbt, F-Droid, ...), or asks
  what supporting a new registry would take, even when they do not say "source"
  or "plugin".
---

# Add a registry source to dug

A source is an independent Go module at `sources/NAME` that speaks the Connect
registry protocol for one upstream catalog. It depends only on
`github.com/rezen/dug/sdk`. The host owns S3, ordering, filtering, resume,
hashing and transfer retries; the source owns upstream
pagination, authentication, artifact resolution, and fetching bytes. The result
is an executable `dug-NAME` that the host reaches by URL and selects with
`dug list NAME`.

The corpus exists so artifacts can be security reviewed: malware triage,
diffing versions, keeping copies after upstream takedown. That is why the rules
below insist on complete enumeration, descriptors pinned at listing time,
version history, and an inventory that cannot steer the plugin anywhere.

Before starting, read `sources/README.md` (the "What a source must provide"
section and the todo entry for your registry, if there is one) and skim
`docs/plugins.md`. `sources/firefox` is the cleanest complete example:
pinned URL, digest and size, search partitioning, version history.
`sources/github` shows a token, rate limits, Link pagination, and download URLs
derived from validated IDs instead of persisted. `sources/wordpress` shows a
registry with no digests and two directories served by one module.

## Workflow

1. Research the registry with real requests and record the facts
   (`references/research.md`).
2. Make the design decisions in section 2 and state them before coding.
3. Scaffold the module and the mechanical wiring:
   `python3 .claude/skills/new-registry-source/scripts/scaffold.py NAME`.
4. Implement Discover and Resolve against the recorded facts.
5. Write tests against an `httptest` fake of the API.
6. Finish the prose wiring and the module README (`references/wiring.md`).
7. Verify: module tests, `just check`, `just build`, a live probe.
8. Report what was built and what the registry cannot provide.

Steps 1 and 2 decide whether a module should exist at all. A registry that only
indexes artifacts hosted on arbitrary developer sites, forbids bulk retrieval,
or offers no way to enumerate belongs under "Blocked or deferred" in
`sources/README.md`. The right deliverable then is a short write-up of what you
found, with the URLs, not a half-working module. Tell the user and stop.

Three shapes are not a new module:

- **Same API family as an existing source** (WordPress themes beside plugins,
  Thunderbird beside Firefox AMO): add a `Kind` and a second executable inside
  that module, the way `sources/wordpress` serves `dug-wordpress-themes` with
  identity `wordpress-themes`.
- **An index over GitHub releases or tags** (Obsidian plugins, Packagist,
  Homebrew taps, agent skill catalogs): add a seed option to `sources/github`
  instead of rewriting its client, token handling, and rate-limit waits.
- **An enumerator that points at other registries** (the MCP Registry pointing
  at npm, PyPI, and OCI): a seed list for those sources, recorded as metadata.

## 1. Research first

Probe the real API with `curl` before writing Go, and keep the raw responses in
your scratchpad; they become test fixtures. `references/research.md` lists the
questions. The ones that shape everything else:

- How is the whole catalog enumerated, and what caps it (page limits, result
  windows, a single large document)? A source that can only fetch named items
  is not useful for corpus work.
- For one item, where is the version list, and per version the download URL,
  digest (which algorithm and encoding), size, and release time?
- What metadata exists for ordering: last update, downloads or installs,
  rating?
- Which hosts serve the bytes? Do download URLs redirect to mirrors or CDNs?
- Rate limits, required headers, and terms of use for bulk access.

The todo notes in `sources/README.md` were checked on a date; APIs drift, so
verify them rather than trusting them.

## 2. Design decisions

State these in a short table in your reply before scaffolding, so the user can
object early. Each one is persisted in inventories and manifests and is
expensive to change later.

| Decision | Guidance |
| --- | --- |
| Identity | Lowercase, short, the CLI argument and `Reference.Registry`. Directory, package, executable and identity share it (`openvsx`, `dug-openvsx`). |
| Namespace | The upstream host (`addons.mozilla.org`, `github.com`), so manifests say where an artifact came from. |
| Name and Version | The registry's own stable identifiers. Names containing `/` or `.` are fine; blob file names are derived separately. |
| Attributes | Everything Resolve needs, pinned at listing: `download_url`, `sha256`, `size`, upstream IDs. Part of artifact identity. Never credentials. |
| Metadata | Mutable facts for ordering and filtering, named per `references/conventions.md` so `--order popular` and `--where downloads > N` work. Excluded from identity. |
| Revision | The content digest when the registry publishes one; otherwise an immutable upstream ID or commit SHA; the version string as a last resort. It must change when bytes change. |
| Options | At least one explicit selector (`slugs`, `ids`, `repos`) plus a full catalog walk when no selector is given. `version_depth` when history exists, an explicit rejection when it does not. Unknown keys are errors. |
| Download hosts | A fixed allowlist in `DefaultDownloadHosts`. If artifacts live on arbitrary hosts, say so and ask before building an index-only or per-reference-host mode. |
| Credentials | An environment variable read in the `pluginhost.Command` `Factory` closure in `cmd/dug-NAME`, passed into `PluginFactory`, sent only over HTTPS or loopback. Never an option or attribute. |
| Provider | `Provider()` declares the identity and a catalog `Kind` (`extensions`, `addons`, `packages`). A module serving several catalogs declares one `Definition` per identity. |

## 3. Scaffold

```sh
python3 .claude/skills/new-registry-source/scripts/scaffold.py NAME [--title "Open VSX"] [--port 7786]
```

The script creates `sources/NAME` (`go.mod`, `go.sum`, `NAME.go`, `plugin.go`,
`provider.go`, `NAME_test.go`, `README.md`, `cmd/dug-NAME/main.go` on
`pluginhost.Main`) from compiling templates
with `TODO(NAME)` markers where registry facts go, picks the next free
`127.0.0.1:778N` port, and applies the edits that have stable anchors: the
`go.work` use block, `scripts/build.sh`, the module lists in
`scripts/check.sh` and `scripts/check-boundaries.sh`, the `plugin` recipe in
the justfile, the host help text in `cmd/dug/main.go`, the table in
`sources/README.md`, and the module table and diagram in `docs/monorepo.md`.
It prints one line per edit: `edited`, `already present`, or `not found`, and
the prose edits it leaves to you. Review everything with `git status` and
`git diff`; an edit it could not anchor is yours to do from
`references/wiring.md`.

Each wiring point matters on its own. A module missing from `check.sh` has no
tests in CI, one missing from `check-boundaries.sh` can silently import the
host, one missing from `build.sh` ships no binary, and one missing from the
justfile cannot be started with `just plugin NAME`.

## 4. Implement Discover and Resolve

Keep the template's shape and replace the `TODO(NAME)` parts with the recorded
facts. The rules that matter most, and why:

- **Emit one reference per version, current first.** The host counts
  versions, not packages, and `version_depth` is applied here: an integer
  caps the count per item, a date keeps versions released on or after it,
  `unlimited` keeps everything advertised. Return the `errStop` sentinel to
  stop paging once the depth is reached.
- **Self-resolve before emitting.** Call `s.Resolve(ctx, ref)` on every
  reference before `emit`, as firefox and github do. A listing must never
  persist something stage two would reject.
- **Pin at listing, trust nothing at resolution.** Resolve re-validates every
  persisted field: registry, name, version, download URL host against the
  allowlist, digest format, size, IDs. Inventories live in S3 and could be
  edited; the plugin must not become an open proxy. Resolve performs no
  network requests when the descriptor is complete, so a deleted upstream
  version fails loudly instead of being replaced by a newer one.
- **Walk the whole catalog, never silently truncate.** When a search window is
  capped, split it by an immutable field such as creation date and fail if a
  one-second window still exceeds the cap (firefox, github). Detect
  pagination cycles, and reject next-page URLs that leave the API origin.
- **Dedupe by upstream ID**, because live catalogs shift under pagination.
- **Bound metadata reads**: a 2 minute timeout per request and a 32 MiB
  response cap. Use `httpx.Get`, which retries 429/408/5xx with `Retry-After`
  and marks other HTTP failures permanent. Wrap failures that no retry can
  fix (bad JSON, invalid descriptors) in `retry.Permanent` so the host stops
  retrying too.
- **`Blob.Open` uses its own context** and returns a fresh body at byte zero
  each time; the resolution context is cancelled before the first transfer.
- **Skip with a warning, fail with an error.** An individual catalog entry
  that is unavailable upstream may be skipped with `slog.Warn` during a full
  walk; an explicitly requested item that is missing is an error. Invalid
  metadata is always an error, never a guess.
- **Listing facts go in two places**, per `references/conventions.md`:
  typed cross-registry facts (download counts and windows, install base,
  release and update times, source repository) in `Reference.Facts` from
  `sdk/registry/facts.go`; registry-specific ones (`rating` normalized to
  0–100 with the raw scale beside it, stars, `version_rank`) in `Metadata`
  as strings. The SDK is mid-migration to `Facts`; the reference explains how
  to tell which the host reads today and what to set meanwhile. A metric that
  means something different from an existing one keeps a distinct name
  (`weekly_downloads`, not `downloads`).
- Error strings are lowercase and name the registry: `firefox page_size must
  be 1–50`. Unknown-option errors list the supported keys.

## 5. Tests

Tests run against an `httptest.Server` through `Endpoint` and
`DownloadHosts` overrides with `Attempts: 1`, so they are fast and offline.
Cover at least:

1. Option validation: unknown keys, malformed selectors, out-of-range sizes,
   mutually exclusive options, `version_depth` values.
2. Discovery per depth (`1`, `N`, a date, `unlimited`): the emitted versions in
   order, each reference's `Facts` field by field (absent fields stay nil),
   its `Metadata`, and that the fake's pagination was followed.
3. Pinned download: `Resolve` then `Open` returns the bytes without another
   metadata request, and tampering with the digest, size, or URL host in the
   reference makes `Resolve` fail. Include a link-local or loopback URL such as
   `http://169.254.169.254/` as the off-host case.
4. Pagination safety: a cycle and a cross-origin next link are rejected; a
   capped window is split (when the registry has a cap).
5. Cancellation: a cancelled context stops discovery with `context.Canceled`.

Use your saved real responses, trimmed, as the fake's payloads so the parsing
is tested against the real shape. `sources/firefox/firefox_test.go` and
`sources/github/github_test.go` are the patterns.

## 6. Wiring and docs

The scaffold handled the anchored edits. `references/wiring.md` lists every
file with the shape of each edit. The prose parts you write yourself:

- `docs/plugins.md`: the intro sentence, an options table, the default port
  in "Deployment", and the metadata summary at the end.
- `README.md`: the intro list of registries, a `## TITLE` section with
  runnable commands and a plain-language description of options and
  metadata, the compile-time-check sentence in "Registry plugin interface",
  and the ordering note if the source lacks popularity metadata.
- `sources/README.md`: fill the table row, tick or remove the todo entry, and
  record anything learned that contradicts the note.
- `sources/NAME/README.md`: options, metadata, what is and is not covered,
  and the API reference URL.
- `docs/monorepo.md`: the executable count and the tag list.
- `cmd/dug/main.go`: replace the placeholder options in the help text.

Finish with `grep -rn 'TODO(NAME)' sources/NAME cmd docs sources/README.md`;
it must print nothing. Then `grep -rln firefox --exclude-dir=sources
--exclude-dir=.git .` shows every place an existing source is named, in case a
new one appeared since this skill was written; mirror any you have not touched.

## 7. Verify

```sh
just test sources/NAME          # race tests for the new module
just check                      # every module, vet, justfile format, boundaries
just build                      # bin/dug-NAME must appear
grep -rn 'TODO(NAME)' sources/NAME cmd docs sources/README.md
```

If you added a dependency, tidy with a temporary replace, because `go mod tidy`
ignores `go.work` and the SDK is unpublished:

```sh
cd sources/NAME && go mod edit -replace github.com/rezen/dug/sdk=../../sdk && go mod tidy && go mod edit -dropreplace github.com/rezen/dug/sdk
```

Then prove it against the real registry. Write a throwaway probe in `.local/`
(gitignored) that discovers a handful of references, resolves one, opens its
blob, and checks the digest and size, and run it with `go run`:

```go
package main

import ("context";"crypto/sha256";"encoding/json";"errors";"fmt";"io";"net/http";"os";"github.com/rezen/dug/sdk/registry";"github.com/rezen/dug/sources/NAME")

func main() {
	stop := errors.New("sample complete")
	source, err := NAME.PluginFactory(http.DefaultClient)(map[string]string{"version_depth": "2"})
	if err != nil { panic(err) }
	var refs []registry.Reference
	err = source.Discover(context.Background(), func(r registry.Reference) error { refs = append(refs, r); if len(refs) == 5 { return stop }; return nil })
	if err != nil && !errors.Is(err, stop) { panic(err) }
	json.NewEncoder(os.Stdout).Encode(refs)
	artifact, err := source.Resolve(context.Background(), refs[0])
	if err != nil { panic(err) }
	body, err := artifact.Blobs[0].Open(context.Background())
	if err != nil { panic(err) }
	defer body.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, body)
	if err != nil { panic(err) }
	fmt.Printf("revision=%s bytes=%d sha256:%x digest=%s size=%d\n", artifact.Revision, n, hash.Sum(nil), artifact.Blobs[0].Digest, artifact.Blobs[0].Size)
}
```

A mismatch between the computed hash and the pinned digest means the digest
encoding or the download URL is wrong; fix that before anything else. For a
full stage-one and stage-two run against local S3, follow the shape of
`scripts/e2e-firefox.sh` with `bin/dug-s3local`.

## 8. Report

Tell the user, in plain language: the identity, port, and options; which
metadata keys exist and which built-in orders therefore work; how version
history behaves; what the registry does not provide (digests, popularity,
history, exhaustive enumeration) and how the source handles it; the live probe
result with real numbers; and anything in the todo note that turned out to be
wrong. List the files changed outside `sources/NAME`.
