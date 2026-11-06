# Chrome registry plugin

Independent Go module for Chrome Web Store CRX entries. Its only Dug dependency
is `github.com/rezen/dug/sdk`.

```sh
go test ./...
go build -o dug-chrome ./cmd/dug-chrome
./dug-chrome --listen 127.0.0.1:7782
```

Options: `ids`, `listing_concurrency` (1–128), `browser_version`, and
`details` (default true). Without IDs it uses the public sitemap, which can
include themes and does not guarantee exhaustive coverage. Discovery pins a
compatible version, CRX URL, SHA-256 and size. Historical version depth is not
supported.

The update service publishes no listing facts, so with `details` on the
adapter also fetches each item's Web Store page in English. Its embedded item
record gives the exact user count (the `installs` fact), rating average
(`rating` 0–100 and `rating_average` 0–5 metadata), rating count
(`num_ratings`), title and category path; the visible text supplies rounded
fallbacks plus the category name and the "Offered by" developer, which becomes
the author. The category path becomes tags such as `privacy`. A 404 or the
bare store shell, as served for removed items, yields no facts rather than an
error; other HTTP failures stop the listing like any metadata failure.

The host owns storage and transfer scheduling. Local builds use the workspace SDK.
Before extracting this directory, publish and pin an SDK release, then update the
module path and command self-import; the Connect protocol stays unchanged.
