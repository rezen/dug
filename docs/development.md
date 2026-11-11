# Development and testing

Run `just` to list tasks. Recipes run from the repository root, even when invoked
from a source directory. Existing environment variables are inherited; service
and CLI recipes forward arguments unchanged. The `local` recipe supplies its own
S3 configuration as described above.

```sh
just build                       # all executables
just check                       # race tests, vet, and module boundaries
just test sources/firefox        # one module; omit it for all modules
just vet sdk
just fmt
just generate
just s3                          # foreground rclone S3 server over data/
just s3-fake --listen 127.0.0.1:9000 # foreground test backend (alternative)
just plugin wordpress            # foreground Connect plugin (another terminal)
just dug --help
just local sync wordpress --option slugs=akismet # automatic local S3
```

`just dug` accepts the same flags as the binary, including quoted filters:

```sh
just dug download wordpress --inventory INVENTORY_KEY \
  --where 'downloads > 20 AND updated >= 2025-01-01' --limit 100
```

`just e2e-wordpress`, `just e2e-wordpress-themes`, `just e2e-firefox`, and `just e2e-jenkins` are explicit
live-network jobs; they are not part of `just check`. CI uses `just check` and `just build`.

`just check` visits every module; root `go test ./...` covers only the host.
Standalone builds with `GOWORK=off` also verify source dependency boundaries.
Tests use local HTTP/S3 fixtures and require loopback sockets, but no external
credentials. They cover plugin pagination, interrupted downloads, integrity failures, concurrency,
resume, Connect streams and cancellation, plugin authentication, revision drift,
signed S3 requests, multipart completion, abort cleanup, complete-inventory
validation, disk merge sorting, filters and limits, WordPress plugin and theme
ratings/version history, persistent GoFakeS3 multipart storage, and Chrome sitemap/update/CRX transfers.

Protocol references: [WordPress plugin API](https://developer.wordpress.org/reference/functions/plugins_api/),
[WordPress theme API](https://developer.wordpress.org/reference/functions/themes_api/),
[AWS Go SDK endpoint configuration](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html).

Chrome references: [public sitemap](https://chromewebstore.google.com/sitemap),
[update manifest protocol](https://developer.chrome.com/docs/extensions/how-to/distribute/host-on-linux).

## Live end-to-end testing

For an automated run that starts and stops its own local services:

```sh
just e2e-wordpress
```

The script builds and starts the WordPress plugin and GoFakeS3 on loopback ports
17881 and 19000, lists the full public catalog, ranks by total downloads, filters
`downloads > 20 AND rating >= 0`, and downloads the top 100 current releases.
It runs the same job again to verify skipping, then reads every ZIP back from S3,
checks SHA-256/size and ZIP CRCs, and verifies that stored metadata includes ratings
and downloads. It stops its servers on exit and retains the database and reports
under `.local/e2e-wordpress/` (override with `DUG_E2E_DIR`). Real upstream network
access is required; the ordinary test suite uses fixtures.

The run on 2026-09-25 listed **73,556** plugins, downloaded and verified **100 ZIPs**
(**597,995,413 bytes**), and skipped all 100 on the second run, with zero failures.
This is evidence from one live run, not a throughput benchmark. Reports include
`verification.json`, `download.json`, `resume.json`, and `top100.json`. To inspect
the retained bucket again, start `dug-s3local` with
`--data .local/e2e-wordpress/s3.db --bucket dug-e2e`.

`just e2e-wordpress-themes` runs the same job against the theme directory with
`dug-wordpress-themes` and keeps its results under `.local/e2e-wordpress-themes/`.
The run on 2026-11-06 listed **15,389** themes, downloaded and verified **100 ZIPs**
(**294,521,803 bytes**), and skipped all 100 on the second run, with zero failures.

### Firefox

`just e2e-firefox` downloads two versions of uBlock Origin and Dark Reader,
verifies S3 bytes and archive CRCs, and checks resume. The verified live run saved
four XPIs totaling 11,003,663 bytes under `.local/e2e-firefox/xpis/`, with a persistent
S3 database and `verification.json` alongside them.
