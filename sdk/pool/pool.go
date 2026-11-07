// Package pool runs bounded concurrent work while keeping results in input
// order, so a listing that fans out lookups stays deterministic and resumable.
package pool

import "context"

// Ordered runs work over items with at most workers in flight and yields
// the results in item order. A slow item delays yielding but never idles the
// other workers. It stops at the first yield error or cancellation, and the
// work functions see a context that is cancelled by then.
func Ordered[T, R any](ctx context.Context, workers int, items []T, work func(context.Context, T) R, yield func(R) error) error {
	i := 0
	next := func() (T, bool) {
		if i >= len(items) {
			var zero T
			return zero, false
		}
		i++
		return items[i-1], true
	}
	return Stream(ctx, workers, next, work, yield)
}

// Stream is Ordered for a source of unknown length. next is called from one
// goroutine and returns false when the source is exhausted; it may block, and
// lookups already started keep running while it does, which lets a paged walk
// overlap with the work it feeds. A next that can fail should record its error
// and return false.
func Stream[T, R any](ctx context.Context, workers int, next func() (T, bool), work func(context.Context, T) R, yield func(R) error) error {
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	futures := make(chan chan R, workers)
	slots := make(chan struct{}, workers)
	go func() {
		defer close(futures)
		for {
			item, ok := next()
			if !ok {
				return
			}
			future := make(chan R, 1)
			select {
			case futures <- future:
			case <-ctx.Done():
				return
			}
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() {
				defer func() { <-slots }()
				future <- work(ctx, item)
			}()
		}
	}()
	for future := range futures {
		select {
		case result := <-future:
			if err := yield(result); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}

// Walk is Stream for a serial walk that discovers the items, such as paged
// search results. walk runs on its own goroutine and calls push for each item;
// push blocks while every worker is busy, so the walk never runs far ahead, and
// it returns an error once the run is over, which walk should return promptly.
// An error from yield or the pool wins over the walk's, and a failed walk
// ends the run after the lookups already started are yielded.
func Walk[T, R any](ctx context.Context, workers int, walk func(ctx context.Context, push func(T) error) error, work func(context.Context, T) R, yield func(R) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	items := make(chan T)
	walked := make(chan error, 1)
	go func() {
		defer close(items)
		walked <- walk(ctx, func(item T) error {
			select {
			case items <- item:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	err := Stream(ctx, workers, func() (T, bool) { item, ok := <-items; return item, ok }, work, yield)
	cancel()
	if walkErr := <-walked; err == nil {
		err = walkErr
	}
	return err
}

// Collected is what a worker gathered for one item: the values it produced, in
// order, and the error that ended it. Values before the error are kept, so a
// failure part way through an item still emits what preceded it.
type Collected[V any] struct {
	Values []V
	Err    error
}

// Collect runs fn, which reports values through add instead of emitting them,
// so it can run on a worker while the caller emits in order from yield.
func Collect[V any](fn func(add func(V) error) error) Collected[V] {
	var c Collected[V]
	c.Err = fn(func(v V) error {
		c.Values = append(c.Values, v)
		return nil
	})
	return c
}

// Emit passes the values to emit in order and then returns the collected error.
func (c Collected[V]) Emit(emit func(V) error) error {
	for _, v := range c.Values {
		if err := emit(v); err != nil {
			return err
		}
	}
	return c.Err
}
