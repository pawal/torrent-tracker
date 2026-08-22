package collector

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

// pool runs fn over items, at most limit at a time, and returns the results in
// item order. A cancelled pass stops handing out work, so items never started
// are absent from the results rather than reported as zero values.
func pool[T, R any](ctx context.Context, limit int, items []T, fn func(T) R) []R {
	var (
		wg   sync.WaitGroup
		sem  = make(chan struct{}, limit)
		out  = make([]R, len(items))
		done = make([]bool, len(items))
	)
	for i, item := range items {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			out[i], done[i] = fn(item), true
		}()
	}
	wg.Wait()

	kept := make([]R, 0, len(items))
	for i, ok := range done {
		if ok {
			kept = append(kept, out[i])
		}
	}
	return kept
}

// every runs fn immediately, then once per interval until ctx is cancelled.
// Each wait is jittered by up to 10%, so the collection and probing loops do
// not settle into lockstep with each other.
func every(ctx context.Context, interval time.Duration, fn func()) error {
	for {
		fn()
		timer := time.NewTimer(interval + time.Duration(rand.Int64N(int64(interval/10)+1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// orDefault is the shape of every optional knob on the collectors: an unset
// field is zero, so the default stands.
func orDefault[T int | time.Duration](v, def T) T {
	if v > 0 {
		return v
	}
	return def
}

func nowOr(f func() time.Time) time.Time {
	if f != nil {
		return f()
	}
	return time.Now().UTC()
}

func logOr(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.Default()
}
