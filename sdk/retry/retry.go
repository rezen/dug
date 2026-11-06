package retry

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

// Permanent marks err as not worth retrying. Do returns it at once, and plugin
// servers map it to a non-retryable RPC code so hosts stop retrying too.
func Permanent(err error) error { return permanent{err} }

// IsPermanent reports whether err, or an error it wraps, was marked Permanent.
func IsPermanent(err error) bool {
	var p permanent
	return errors.As(err, &p)
}

func Do(ctx context.Context, attempts int, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		err = fn()
		if err == nil || IsPermanent(err) || ctx.Err() != nil {
			return err
		}
		if i+1 < attempts {
			delay := min(30*time.Second, 250*time.Millisecond*time.Duration(1<<min(i, 7)))
			if err := Wait(ctx, delay+time.Duration(rand.Int64N(int64(delay/2)+1))); err != nil {
				return err
			}
		}
	}
	return err
}

func Wait(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
