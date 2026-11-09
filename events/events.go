// Package events is an in-process publish/subscribe bus for what dug finds and
// downloads. Publishing never blocks the caller: every subscriber owns a
// bounded queue and a goroutine that delivers events in batches, so a slow
// database or webhook cannot slow discovery or downloads. When a queue is full
// the event is dropped for that subscriber and counted, because the inventory
// and manifests in S3 remain the durable record.
package events

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rezen/dug/sdk/registry"
)

// Kind says what happened to a reference.
type Kind string

const (
	// Discovered: stage one emitted the reference. Key is empty.
	Discovered Kind = "discovered"
	// Downloaded: every blob and the manifest were stored. Key is the manifest.
	Downloaded Kind = "downloaded"
	// Skipped: a current manifest already existed. Key is the manifest.
	Skipped Kind = "skipped"
	// Failed: the resource could not be archived. Error says why.
	Failed Kind = "failed"
)

// Event is one thing that happened to one reference.
type Event struct {
	Kind      Kind               `json:"kind"`
	Time      time.Time          `json:"time"`
	Reference registry.Reference `json:"reference"`
	Key       string             `json:"key,omitempty"`
	Error     string             `json:"error,omitempty"`
}

// Handler receives batches of events. Handle returning an error reports the
// whole batch as failed; retries belong inside the handler. If a Handler is
// also an io.Closer it is closed after its queue has drained.
type Handler interface {
	Handle(ctx context.Context, batch []Event) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, batch []Event) error

func (f HandlerFunc) Handle(ctx context.Context, batch []Event) error { return f(ctx, batch) }

// SubscribeOptions tunes one subscriber. Zero values take the defaults.
type SubscribeOptions struct {
	// Kinds limits delivery; empty means every kind.
	Kinds []Kind
	// BatchSize is the most events per Handle call (default 500).
	BatchSize int
	// FlushInterval delivers a partial batch after this long (default 2s).
	FlushInterval time.Duration
	// Buffer is the queue depth before events are dropped (default 10000).
	Buffer int
}

// Stats counts one subscriber's events.
type Stats struct {
	Name      string
	Delivered int64 // events in batches whose Handle returned nil
	Failed    int64 // events in batches whose Handle returned an error
	Dropped   int64 // events refused because the queue was full
}

type subscriber struct {
	name     string
	handler  Handler
	kinds    map[Kind]bool
	queue    chan Event
	size     int
	interval time.Duration
	done     chan struct{}

	delivered, failed, dropped atomic.Int64
}

// Bus fans events out to subscribers. The zero value is not usable; call New.
// A nil *Bus is valid and discards everything, so callers need no checks.
type Bus struct {
	onError func(subscriber string, err error, lost int)

	mu     sync.RWMutex
	closed bool
	subs   []*subscriber

	ctx    context.Context
	cancel context.CancelFunc
}

// New returns a Bus. onError, if set, is called from subscriber goroutines
// when a batch could not be delivered; it must be concurrency safe.
func New(onError func(subscriber string, err error, lost int)) *Bus {
	ctx, cancel := context.WithCancel(context.Background())
	return &Bus{onError: onError, ctx: ctx, cancel: cancel}
}

// Subscribe registers a handler and starts its delivery goroutine.
func (b *Bus) Subscribe(name string, handler Handler, opts SubscribeOptions) error {
	if b == nil {
		return errors.New("nil bus")
	}
	if name == "" || handler == nil {
		return errors.New("subscriber name and handler are required")
	}
	if opts.BatchSize < 0 || opts.FlushInterval < 0 || opts.Buffer < 0 {
		return errors.New("batch size, flush interval and buffer must be nonnegative")
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = 500
	}
	if opts.FlushInterval == 0 {
		opts.FlushInterval = 2 * time.Second
	}
	if opts.Buffer == 0 {
		opts.Buffer = 10000
	}
	s := &subscriber{name: name, handler: handler, queue: make(chan Event, opts.Buffer), size: opts.BatchSize, interval: opts.FlushInterval, done: make(chan struct{})}
	if len(opts.Kinds) > 0 {
		s.kinds = make(map[Kind]bool, len(opts.Kinds))
		for _, kind := range opts.Kinds {
			s.kinds[kind] = true
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("bus is closed")
	}
	for _, other := range b.subs {
		if other.name == name {
			return fmt.Errorf("duplicate subscriber %q", name)
		}
	}
	b.subs = append(b.subs, s)
	go b.run(s)
	return nil
}

// Publish queues the event for every interested subscriber and returns at
// once. It is safe for concurrent use. The reference and its maps and slices
// must not be mutated afterwards.
func (b *Bus) Publish(event Event) {
	if b == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for _, s := range b.subs {
		if s.kinds != nil && !s.kinds[event.Kind] {
			continue
		}
		select {
		case s.queue <- event:
		default:
			s.dropped.Add(1)
		}
	}
}

// Stats reports every subscriber's counters. It may be called at any time.
func (b *Bus) Stats() []Stats {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Stats, len(b.subs))
	for i, s := range b.subs {
		out[i] = Stats{Name: s.name, Delivered: s.delivered.Load(), Failed: s.failed.Load(), Dropped: s.dropped.Load()}
	}
	return out
}

// Close stops accepting events, delivers what is queued, and closes handlers
// that are io.Closers. If ctx ends first, in-flight deliveries are cancelled and
// ctx's error is returned; undelivered events are lost.
func (b *Bus) Close(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	subs := b.subs
	for _, s := range subs {
		close(s.queue)
	}
	b.mu.Unlock()
	var errs []error
	for _, s := range subs {
		select {
		case <-s.done:
		case <-ctx.Done():
			b.cancel()
			<-s.done
			errs = append(errs, fmt.Errorf("subscriber %s: %w", s.name, ctx.Err()))
		}
		if closer, ok := s.handler.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				errs = append(errs, fmt.Errorf("subscriber %s: close: %w", s.name, err))
			}
		}
	}
	b.cancel()
	return errors.Join(errs...)
}

func (b *Bus) run(s *subscriber) {
	defer close(s.done)
	batch := make([]Event, 0, s.size)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.handler.Handle(b.ctx, batch); err != nil {
			s.failed.Add(int64(len(batch)))
			if b.onError != nil {
				b.onError(s.name, err, len(batch))
			}
		} else {
			s.delivered.Add(int64(len(batch)))
		}
		// Handlers must not retain the slice, so reuse is safe.
		batch = batch[:0]
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case event, ok := <-s.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, event)
			if len(batch) >= s.size {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}
