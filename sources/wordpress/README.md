# WordPress registry plugins

Independent Go module for the public WordPress.org plugin and theme directories
and their ZIP releases. Its only Dug dependency is `github.com/rezen/dug/sdk`.
One adapter serves both directories. Each runs as its own executable and
registry identity, because plugin and theme slugs are separate namespaces.

| Executable | Identity | Default listen | Info API | SVN history |
| --- | --- | --- | --- | --- |
| `dug-wordpress` | `wordpress` | `127.0.0.1:7781` | `plugins/info/1.2` | `plugins.svn.wordpress.org/SLUG/tags/VERSION/` |
| `dug-wordpress-themes` | `wordpress-themes` | `127.0.0.1:7785` | `themes/info/1.2` | `themes.svn.wordpress.org/SLUG/VERSION/` |

```sh
go test ./...
go build -o dug-wordpress ./cmd/dug-wordpress
go build -o dug-wordpress-themes ./cmd/dug-wordpress-themes
./dug-wordpress --listen 127.0.0.1:7781
./dug-wordpress-themes --listen 127.0.0.1:7785
```

Both accept the query options `search`, `slugs`, `page_size` (1–250), `browse`
and `version_depth` (integer, inclusive date cutoff, unlimited). `browse` takes
`updated` (default), `popular` or `new`; the theme directory also takes
`featured`. For themes, `popular` lists only themes with active installs and
`featured` is a small curated set, so `updated` or `new` walk the whole catalog.
Date depth uses the original SVN creation history of each tag or version
directory. Facts carry the all-time download count as `downloads.total`,
active installs as `installs`, `updated_at`, `released_at` when date depth
looked it up, and the item's own SVN URL as `repository.url` (source
`registry`). Metadata keeps the rating percentage, rating count and star
breakdown. Themes add `template`, the parent theme slug of a child theme.
Reference tags are the directory's tag slugs, lowercased and sorted. The
author is the publishing WordPress.org account: display name, profile URL and
the profile's username; the plugin directory wraps the name in a link that the
adapter unwraps, and the theme directory is asked for `extended_author` so it
sends the account object rather than a bare nicename.

The themes API includes `download_link` only when the `downloadlink` field is
requested, and reports update times to the second in GMT as
`last_updated_time`; the adapter prefers that over the date-only `last_updated`.
A few themes have purely numeric versions, which the API encodes as JSON
numbers; the adapter keeps those digits verbatim so they still match the
download URL and SVN directory.

The source owns upstream access; the host owns filtering, ordering, S3 and resume.
Local builds use the workspace SDK. Before extracting this directory, publish and
pin an SDK release, then update the module path and command self-imports. The host
continues to connect through the same registry identities and Connect URLs.
