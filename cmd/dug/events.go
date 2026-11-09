package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/rezen/dug/engine"
	"github.com/rezen/dug/events"
	"github.com/rezen/dug/sdk/registry"
)

// eventSettings configures the background event subscribers. With neither a
// database nor a webhook no bus is created and publishing costs nothing.
type eventSettings struct {
	db, webhook string
	batch       int
	flush       time.Duration
	buffer      int
}

func (s *eventSettings) register(flags *flag.FlagSet) {
	flags.StringVar(&s.db, "events-db", "", "Record discovery and download events in this SQLite file, written in the background")
	flags.StringVar(&s.webhook, "events-webhook", "", "POST batches of discovery and download events to this URL in the background (HMAC secret: DUG_EVENTS_WEBHOOK_SECRET)")
	flags.IntVar(&s.batch, "events-batch-size", 500, "Events per database write or webhook request")
	flags.DurationVar(&s.flush, "events-flush-interval", 2*time.Second, "Deliver a partial event batch after this long")
	flags.IntVar(&s.buffer, "events-buffer", 10000, "Queued events per subscriber; beyond it events are dropped rather than slowing the run")
}

func (s *eventSettings) validate() error {
	if s.batch < 1 || s.flush <= 0 || s.buffer < 1 {
		return misuse("events batch size, flush interval and buffer must be positive")
	}
	return nil
}

// start subscribes the configured sinks. It returns a nil bus, which discards
// events, when none is configured.
func (s *eventSettings) start(client *http.Client, logger *slog.Logger) (*events.Bus, error) {
	if s.db == "" && s.webhook == "" {
		return nil, nil
	}
	bus := events.New(func(name string, err error, lost int) {
		logger.Error("event delivery failed", "subscriber", name, "events", lost, "error", err.Error())
	})
	opts := events.SubscribeOptions{BatchSize: s.batch, FlushInterval: s.flush, Buffer: s.buffer}
	if s.db != "" {
		sink, err := events.OpenSQLite(s.db)
		if err != nil {
			return nil, err
		}
		if err := bus.Subscribe("sqlite", sink, opts); err != nil {
			sink.Close()
			return nil, err
		}
	}
	if s.webhook != "" {
		sink, err := events.NewWebhook(s.webhook, os.Getenv("DUG_EVENTS_WEBHOOK_SECRET"), client)
		if err != nil {
			bus.Close(context.Background())
			return nil, usageError{err}
		}
		if err := bus.Subscribe("webhook", sink, opts); err != nil {
			bus.Close(context.Background())
			return nil, err
		}
	}
	return bus, nil
}

// closeBus drains queued events before the process exits and reports losses.
// It uses its own deadline because the run's context may already be cancelled.
func closeBus(bus *events.Bus, logger *slog.Logger) {
	if bus == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := bus.Close(ctx); err != nil {
		logger.Error("closing event subscribers", "error", err.Error())
	}
	for _, stats := range bus.Stats() {
		if stats.Dropped > 0 || stats.Failed > 0 {
			logger.Warn("events lost", "subscriber", stats.Name, "dropped", stats.Dropped, "failed", stats.Failed, "delivered", stats.Delivered)
		}
	}
}

// publishDiscovery wraps a listing callback so each entry is also published.
// Publishing never blocks, so listing speed is unchanged.
func publishDiscovery(bus *events.Bus, next func(registry.Reference) error) func(registry.Reference) error {
	return func(ref registry.Reference) error {
		if next != nil {
			if err := next(ref); err != nil {
				return err
			}
		}
		bus.Publish(events.Event{Kind: events.Discovered, Reference: ref})
		return nil
	}
}

// publishDownload maps an engine event onto the bus.
func publishDownload(bus *events.Bus, event engine.Event) {
	kind := events.Downloaded
	switch event.Status {
	case "skipped":
		kind = events.Skipped
	case "failed":
		kind = events.Failed
	}
	out := events.Event{Kind: kind, Reference: event.Reference, Key: event.Key}
	if event.Error != nil {
		out.Error = event.Error.Error()
	}
	bus.Publish(out)
}
