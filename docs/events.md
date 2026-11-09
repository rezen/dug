# Events

dug publishes an event for every reference it discovers and for every download
outcome. Subscribers receive them in batches on their own goroutines, so a slow
database or webhook never slows discovery or downloads.

| Kind         | Published when                                   | Fields set          |
|--------------|--------------------------------------------------|---------------------|
| `discovered` | stage one emits a reference (`list`, `sync`)     | reference           |
| `downloaded` | every blob and the manifest are stored           | reference, key      |
| `skipped`    | a current manifest already exists                | reference, key      |
| `failed`     | the resource could not be archived               | reference, error    |

## Built-in subscribers

- `--events-db FILE` stores events in the SQLite table `events` through gorm,
  one transaction per batch. The reference is kept as JSON without its `raw`
  upstream record. The driver is pure Go, so no cgo is needed.
- `--events-webhook URL` POSTs `{"events": [...]}` per batch, retrying network
  errors, 429 and 5xx. With `DUG_EVENTS_WEBHOOK_SECRET` set, the body is signed
  in `X-Dug-Signature: sha256=<hex hmac>`.

Both honor `--events-batch-size`, `--events-flush-interval` and
`--events-buffer`, and can be set in `dug.yaml`.

## Delivery guarantees

Publishing never blocks. If a subscriber falls more than `--events-buffer`
events behind, new events are dropped for it and counted; the run ends with an
"events lost" warning. Failed batches are logged and counted too. S3 (the
inventory and manifests) stays the durable record. Pending events are flushed
when the command exits, for up to 30 seconds.

## Adding a subscriber

Implement `events.Handler` (`Handle(ctx, []Event) error`; also `io.Closer` if it
holds resources) and register it with `bus.Subscribe(name, handler,
events.SubscribeOptions{Kinds: ...})`. Handlers must not retain the batch slice.
