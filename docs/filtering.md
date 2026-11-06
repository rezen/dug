# Filters and version history

```sh
# Save the full current catalog so global ranking is meaningful.
./bin/dug list wordpress --option page_size=250

# WordPress `downloads` means total downloads across all package versions.
# `installs` is the separate rounded active-install count.
# Filter and download at most 100 matching versions from that inventory.
./bin/dug download wordpress --inventory INVENTORY_KEY \
  --where 'downloads >= 100000 AND installs >= 10000 AND rating >= 80' \
  --order=-downloads.total --limit 100

# `version_downloads.total` counts downloads for a specific version, when known.
./bin/dug download wordpress --inventory INVENTORY_KEY \
  --where 'version_downloads.total >= 1000' --order=-version_downloads.total --limit 20

# Inspect matching current references without downloading archives.
./bin/dug list wordpress --option page_size=250 --format jsonl |
  jq 'select((.facts.downloads.total // 0) >= 100000)'

# Current release plus up to two other versions per plugin.
./bin/dug list wordpress --option slugs=akismet,wordpress-seo --version-depth 3

# Inclusive history cutoff, based on original SVN tag creation dates.
./bin/dug list wordpress --option slugs=akismet --version-depth 2025-01-01

# Every version currently advertised by WordPress, excluding moving trunk.
./bin/dug list wordpress --option slugs=akismet --version-depth unlimited
```

`--where` is a conjunction of comparisons joined by `AND` (case insensitive).
Operators: `>`, `>=`, `<`, `<=`, `=`, `==`, `!=`. It does not support OR,
parentheses, or strings containing ` AND `. Repeatable filters are not needed;
put the expression in one quoted argument. Supported fields:

| Field | Meaning |
| --- | --- |
| `downloads` / `downloads.total` | Package-wide downloads since publication (WordPress, Ansible Galaxy) |
| `downloads.day`, `.week`, `.month`, `.quarter`, `.year` | Package-wide downloads in the trailing 1, 7, 30, 90 or 365 days, where the registry publishes that window (Firefox: `week`; n8n: `month`, so order n8n with `--order=-downloads.month`) |
| `version_downloads.total` (and the same windows) | Downloads of that version alone (GitHub release assets) |
| `installs` / `active_installs` | Install base: WordPress active installs, Firefox average daily users |
| `rating` | WordPress or Firefox rating on a **0–100** scale |
| `ratings` / `num_ratings` | Total number of ratings |
| `updated` / `updated_at` | Package update timestamp |
| `released` / `released_at` | Version publication timestamp, when available |
| `repository` / `repository.url`, `repository.source`, `repository.commit`, `repository.directory` | Source repository: canonical URL, how it is known (`registry`, `declared`, `attested`), pinned commit, monorepo subdirectory |
| `author` / `author.username`, `author.name`, `author.id`, `author.url`, `author.email` | Publishing account: WordPress.org profile, AMO account, GitHub owner, Galaxy namespace, npm publisher of that version. `author` is the username |
| `tag` | Tag membership with `=` or `!=` only: WordPress tags, Firefox tags and categories, GitHub topics, Galaxy collection tags, npm keywords, all lowercase |
| `name`, `version`, `registry`, `namespace` | Lexical identity fields |
| `number:KEY`, `date:KEY`, `metadata:KEY` | Numeric, date, or lexical registry-specific metadata |

Dates accept `YYYY-MM-DD` (midnight UTC) or RFC3339 timestamps. Missing fields do
not match, including `!=`; malformed numbers/dates produce errors. Filters use
the inventory's captured facts, not live data. `--limit` counts selected artifact
versions, including already-completed entries on resume; it is not a success quota
and does not backfill failures. Ordering determines priority before the limit.

The typed facts (`facts` in a saved reference) mean the same thing in every
registry and are nullable: a registry that does not publish a window or metric
leaves it absent rather than zero, and absent never matches or sorts first.
Registry-specific metrics stay in string metadata: WordPress keeps `rating`,
`num_ratings`, `ratings` (a JSON-encoded star-count map), and `ratings_1` through
`ratings_5` there. Download and rating facts are package-wide current metrics,
also attached to historical versions; only `version_downloads` counts one
version. For example, `number:ratings_5 >= 100` filters by five-star ratings. The
saved inventory keeps fresh facts even if a completion manifest is reused from an
earlier run.

Version depth defaults to **1**, the current published release. An integer N
includes the current release, then remaining advertised tags in descending
semantic-version order, capped at N per plugin. Nonstandard tags fall after
parseable versions in descending lexical order; prerelease tags are included if
advertised. `unlimited` includes all advertised version URLs except `trunk`.
Deleted versions are not reconstructed, and availability can change after listing.

A date depth includes only versions whose SVN tag was originally created on or
after the UTC cutoff. The adapter checks creation history rather than treating a
later edit as a new release. This is a tag-creation date, not a guaranteed public
publication timestamp. Missing tag/history data fails the listing instead of
guessing. Date selection adds up to eight concurrent SVN metadata requests per
plugin; integer/unlimited modes use the API's version catalog without those date
lookups. `released_at` is populated by date-mode discovery. To change depth, create
a new inventory; the download stage never discovers additional versions.

The host forwards `--version-depth` as the plugin option `version_depth`.
WordPress, Firefox, n8n, and Ansible Galaxy implement it; plugins without history support reject it explicitly.
