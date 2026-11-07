package pool

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestOrderedKeepsOrderAndBoundsConcurrency(t *testing.T) {
	items := make([]int, 40)
	for i := range items {
		items[i] = i
	}
	var inflight, peak atomic.Int32
	var got []int
	err := Ordered(context.Background(), 4, items, func(_ context.Context, n int) int {
		cur := inflight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(time.Duration(40-n) * 200 * time.Microsecond) // later items finish first
		inflight.Add(-1)
		return n
	}, func(n int) error { got = append(got, n); return nil })
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range got {
		if n != i {
			t.Fatalf("result %d = %d, want input order", i, n)
		}
	}
	if len(got) != len(items) {
		t.Fatalf("got %d results, want %d", len(got), len(items))
	}
	if p := peak.Load(); p < 2 || p > 4 {
		t.Fatalf("peak concurrency %d, want overlap capped at 4", p)
	}
}

func TestOrderedStopsOnYieldError(t *testing.T) {
	boom := errors.New("boom")
	var started atomic.Int32
	err := Ordered(context.Background(), 2, make([]int, 1000), func(ctx context.Context, _ int) int {
		started.Add(1)
		return 0
	}, func(int) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if n := started.Load(); n > 20 {
		t.Fatalf("%d lookups started after the first yield failed", n)
	}
}

func TestStreamOverlapsSourceWithWork(t *testing.T) {
	n := 0
	var got []int
	err := Stream(context.Background(), 3, func() (int, bool) {
		if n == 10 {
			return 0, false
		}
		n++
		return n, true
	}, func(_ context.Context, v int) int { return v * 2 }, func(v int) error { got = append(got, v); return nil })
	if err != nil || len(got) != 10 || got[9] != 20 {
		t.Fatalf("got %v, err %v", got, err)
	}
}

func TestOrderedHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Ordered(ctx, 2, []int{1, 2, 3}, func(ctx context.Context, n int) int { <-ctx.Done(); return n }, func(int) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestWalkFeedsPoolAndReportsWalkError(t *testing.T) {
	boom := errors.New("walk failed")
	var got []int
	err := Walk(context.Background(), 3, func(ctx context.Context, push func(int) error) error {
		for i := 1; i <= 5; i++ {
			if err := push(i); err != nil {
				return err
			}
		}
		return boom
	}, func(_ context.Context, n int) int { return n }, func(n int) error { got = append(got, n); return nil })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the walk error", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %v, want the five items pushed before the walk failed", got)
	}
}

func TestWalkYieldErrorStopsWalk(t *testing.T) {
	boom := errors.New("boom")
	var pushed atomic.Int32
	err := Walk(context.Background(), 2, func(ctx context.Context, push func(int) error) error {
		for i := 0; ; i++ {
			if err := push(i); err != nil {
				return err
			}
			pushed.Add(1)
		}
	}, func(_ context.Context, n int) int { return n }, func(int) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the yield error, not the walk's cancellation", err)
	}
	if n := pushed.Load(); n > 20 {
		t.Fatalf("walk pushed %d items after the first yield failed", n)
	}
}

func TestCollectKeepsValuesBeforeError(t *testing.T) {
	boom := errors.New("boom")
	c := Collect(func(add func(int) error) error {
		add(1)
		add(2)
		return boom
	})
	var got []int
	if err := c.Emit(func(v int) error { got = append(got, v); return nil }); !errors.Is(err, boom) || len(got) != 2 {
		t.Fatalf("got %v, err %v", got, err)
	}
}
