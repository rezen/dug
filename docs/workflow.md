# Workflow, ordering, and limits

How `list`, `download`, and `sync` fit together, and how downloads are prioritized.

`list` streams a table of names, versions, providers, catalog kinds, and registry
IDs to stdout. WordPress rows explicitly distinguish provider `wordpress`, kind
`plugins` from kind `themes`. It fetches metadata only, with no artifact downloads.
While listing runs it uploads the inventory to S3 in JSONL parts
(`<registry>/<type>/inventories/<ulid>/part-NNNNNN.jsonl`, `--inventory-part-entries`
entries each, see [storage](storage.md)) and commits it with `complete.json` once
listing finishes. It also keeps the concatenated records (header and references) at
`.local/inventories/<ulid>.jsonl`, then prints the absolute local
path and S3 URI to stderr. Use `--inventory-dir DIR` to choose the local directory.
The header records the registry, provider and kind (when advertised), query
options, timestamp, and selection limit. Older plugins show `-` for unknown
provider/kind; these values are never inferred from registry names.

Use `--format jsonl` for one reference per stdout line, including explicit
`provider` and `kind` labels, or `--format summary` for a single JSON object with
`inventory_key`, `entries`, and `inventory_file` (the absolute local path).
Scripts consuming the previous summary-only output must add `--format summary`.
The local inventory contains full references and discovery URLs/metadata; it is
not a directory of downloaded archives. `download --inventory` still takes the
S3 inventory key, not a local path.

Displayed rows are provisional until listing completes. If discovery or row
output fails, there is no `complete.json`, so the inventory is not complete and
`last_run` is not updated; the temporary local file is removed. The parts already
uploaded stay in S3 and the failure log names their key. Rerunning stage one
re-enumerates from the start, or `download --inventory KEY --allow-partial` downloads
what was saved (the listing is missing everything after the last uploaded part).
For `list`, `--limit` bounds inventory entries and is recorded in its header.

`download --inventory KEY` validates each part's size, SHA-256 and entry count against
`complete.json`, plus record structure and the total count, before scheduling anything. It reuses saved query options
and never calls discovery. Run it again with the same key to resume, or choose a
different order or filter. `--option` and `--version-depth` belong to stage one.
`download` and `sync` apply `--limit` to matching versions after ordering and
filtering; zero means unlimited. `sync --list-limit N` optionally bounds its
stage-one inventory separately. Without it, `sync` lists the whole matching
catalog before choosing its top N. `sync` logs the inventory key before downloading.

Every completed stage one (`list` or `sync`) records a `<registry>/<type>/last_run/` pointer in S3 for
its query: registry, plugin options (including `--version-depth`), and list limit.
`sync` defaults to that pointer and logs `reusing last run`; it lists the catalog
only when no pointer exists. Pass `--rediscover` to list again (which replaces the
pointer) or `--inventory KEY` to pin a specific inventory. A different `--option`,
`--version-depth`, or `--list-limit` is a different query with its own pointer. This
`--rediscover` is unrelated to `--refresh`, which reprocesses completed resources.

`--where` filters downloads in `download` and `sync`; it does not filter `list`.
`list --limit N` keeps the first N entries in discovery order. To inspect filtered
entries without downloading, use `list --format jsonl` and filter its reference
facts with a tool such as `jq`.

| `--order` | Priority |
| --- | --- |
| `discovery` | Original listing order (default) |
| `name` / `-name` | Resource name ascending / descending |
| `updated` | Most recently updated first (`-updated_at`) |
| `popular` | Most active installs first (`-installs`) |
| `downloads` | Most package-wide downloads first (`-downloads.total`) |
| `rating` | Highest rating first (`-number:rating`) |
| `released` | Most recent known version date first (`-released_at`) |
| `PATH` / `-PATH` | Typed fact ascending / descending: `downloads.total`, `downloads.week`, `version_downloads.total`, `installs`, `released_at`, `updated_at`, `repository.url` and the other paths below |
| `metadata:KEY` / `-metadata:KEY` | Lexical metadata value ascending / descending |
| `number:KEY` / `-number:KEY` | Numeric metadata value ascending / descending |

Missing facts and metadata sort last; ties preserve discovery order. Malformed
numeric metadata causes an error. WordPress supplies update times and popularity;
Chrome currently supplies neither, so use discovery or name ordering for Chrome.
GitHub supplies `stars` and last-push times, so `-number:stars` or `updated` work there.
Ordering sets queue priority; concurrent workers can start or finish out of order.
Use `--concurrency 1` for sequential resource processing.

Sorting uses temporary files and bounded merge runs, with a default 16 MiB of
encoded entries per run. Go object overhead is additional. Inventory staging and
sorting need temporary disk proportional to inventory size, in addition to blobs.
Graceful cleanup removes temporary files; abrupt termination can leave them behind.
