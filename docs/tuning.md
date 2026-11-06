# Concurrency and resource bounds

| Option | Default | Controls |
| --- | ---: | --- |
| `--concurrency` | 16 | Active resource workers, including metadata resolution |
| `--downloads` | 32 | Global blob transfers, from local staging through S3 upload |
| `--upload-concurrency` | 4 | Parallel multipart upload requests per blob |
| `--part-size` | 16777216 | Multipart part size in bytes, minimum 5 MiB |
| `--queue-size` | 64 | Ordered download queue buffer |
| `--attempts` | 4 | Attempts per resolution/transfer and per HTTP request |
| `--timeout` | 30m | Deadline per resolution or full blob-transfer attempt |
| `--max-blob-bytes` | 4294967296 | Per-blob disk/size ceiling (4 GiB) |
| `--temp-dir` | OS default | Temporary staging directory, preferably on SSD |
| `--limit` | 0 | list: first discovery entries; download/sync: matching versions after filtering and ordering |
| `--list-limit` | 0 | Optional sync inventory limit, independent of download limit |
| `--max-inventory-bytes` | 17179869184 | Serialized inventory ceiling (16 GiB) |
| `--sort-buffer-bytes` | 16777216 | Approximate encoded entries per sorting run |

Plugin options use repeatable `--option key=value` flags and are validated by the plugin; see [sources](sources.md). `--attempts` controls host transfer/RPC retries.

HTTP clients are created by `sdk/services.NewDugHttpClient`. Defaults are 128
connections per host, 256 idle connections globally, a 30-second dial timeout,
15-second TLS handshake timeout, 60-second response-header timeout, and 90-second
idle timeout. Environment proxies and HTTP/2 are enabled; automatic compression
is disabled for archive transfers. `WithConnections`, `WithTimeout`, and
`WithTransportOptions` customize these settings. Whole-request timeouts default
to zero so request contexts control long-running downloads and plugin streams.
The host applies `--timeout` to its S3 client. Injected clients remain supported;
nil clients use a shared Dug client and reuse its connection pool.

Each source plugin caches its upstream GET responses on disk
(`bartventer/httpcache`, `fscache` store) and serves them for 1 hour without
contacting the registry, overriding the upstream's own freshness headers; expired
entries are revalidated with ETag/Last-Modified. Override with the plugin flags
`--cache-ttl 10m` (`0` disables) and `--cache-dir PATH`, or the environment
variables `DUG_HTTP_CACHE_TTL` and `DUG_HTTP_CACHE_DIR` (flags win). The default
directory is `<user cache dir>/dug-http`. The host's own S3 and download clients
are not cached; opt a client in with `services.WithCache`.

Transfers reuse HTTP connections and run concurrently. Large blobs are staged to
temporary files while hashing, then uploaded with parallel S3 parts. Seekable files
allow parallel uploads to read file sections without allocating a full buffer
for each multipart part. At most
`downloads × max-blob-bytes` bytes of staging space can be in use, plus one byte
per active download for size-limit detection. Actual usage follows blob sizes.
Files are removed on success, failure, and graceful cancellation. Abrupt process
termination can leave `dug-blob-*` files in the temp directory.

The maximum number of simultaneous S3 part requests is approximately
`downloads × upload-concurrency`, plus metadata operations. `--concurrency` also
controls metadata requests; `--downloads` is not a global HTTP request limit.
Each WordPress release has one ZIP, so simultaneous plugin transfers are bounded
by the smaller of `--concurrency` and `--downloads`.
Start with defaults and increase while measuring network, disk, provider limits,
and S3 request costs. No production throughput benchmark is claimed.

HTTP 429/408/5xx responses use retries with jitter and `Retry-After`; permanent
HTTP failures stop retrying within the plugin and reach the host as non-retryable
`failed_precondition` errors. Other Connect stream errors propagate to the host,
which may retry the blob according to its policy.
A failed body transfer restarts that blob. The
AWS client also applies its own protocol retries, so total request attempts may
exceed `--attempts`. There is no byte-range resume, request-per-second throttle,
or distributed worker queue in this version.
