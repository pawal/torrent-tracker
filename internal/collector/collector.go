// Package collector resolves the known tracker names on a schedule and turns
// the answers into address history and change-feed entries.
package collector

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/pawal/torrent-tracker/internal/resolver"
	"github.com/pawal/torrent-tracker/internal/store"
)

// Collector runs collection passes over the tracker registry.
type Collector struct {
	Store    *store.Store
	Resolver resolver.Resolver
	Log      *slog.Logger

	// Concurrency bounds simultaneous lookups. Defaults to 8.
	Concurrency int
	// MissThreshold is the consecutive absences needed to retire an address.
	MissThreshold int
	// AfterRun, if set, is called after each pass inside Run. Enrichment hangs
	// off this so freshly discovered addresses are annotated straight away.
	AfterRun func(context.Context)
	// Now is overridable for tests.
	Now func() time.Time
}

// Summary reports what one pass did.
type Summary struct {
	RunID    int64
	Trackers int
	OK       int
	Errors   int
	Changes  int
	Duration time.Duration
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

func (c *Collector) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *Collector) concurrency() int {
	if c.Concurrency > 0 {
		return c.Concurrency
	}
	return 8
}

// RunOnce resolves every enabled tracker once and persists the outcome.
func (c *Collector) RunOnce(ctx context.Context) (Summary, error) {
	start := c.now()

	trackers, err := c.Store.ListTrackers(ctx, false)
	if err != nil {
		return Summary{}, err
	}

	runID, err := c.Store.StartRun(ctx, start, len(trackers))
	if err != nil {
		return Summary{}, err
	}
	sum := Summary{RunID: runID, Trackers: len(trackers)}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, c.concurrency())
	)

	for _, t := range trackers {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(t store.Tracker) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			plan, err := c.collectOne(ctx, t)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				sum.Errors++
				c.log().Error("collect failed", "tracker", t.Name, "err", err)
				return
			}
			if plan.Status == store.StatusOK {
				sum.OK++
			} else {
				sum.Errors++
			}
			sum.Changes += plan.Changes()
		}(t)
	}
	wg.Wait()

	end := c.now()
	sum.Duration = end.Sub(start)
	if err := c.Store.FinishRun(ctx, runID, end, sum.OK, sum.Errors, sum.Changes); err != nil {
		return sum, err
	}
	c.log().Info("collection finished",
		"trackers", sum.Trackers, "ok", sum.OK, "errors", sum.Errors,
		"changes", sum.Changes, "duration", sum.Duration.Round(time.Millisecond))
	return sum, nil
}

// collectOne resolves and persists a single tracker.
func (c *Collector) collectOne(ctx context.Context, t store.Tracker) (store.Plan, error) {
	prev, err := c.Store.ActiveRecords(ctx, t.ID)
	if err != nil {
		return store.Plan{}, err
	}

	obs := Observation{
		A:    c.Resolver.Lookup(ctx, t.Name, resolver.TypeA),
		AAAA: c.Resolver.Lookup(ctx, t.Name, resolver.TypeAAAA),
	}

	plan := Diff(prev, t.LastStatus, obs, c.MissThreshold)
	plan.Duration = time.Duration(obs.Duration()) * time.Millisecond

	if err := c.Store.ApplyPlan(ctx, t.ID, plan, c.now()); err != nil {
		return plan, err
	}
	if plan.Changes() > 0 {
		c.log().Info("tracker changed", "tracker", t.Name, "status", plan.Status, "changes", plan.Changes())
	}
	return plan, nil
}

// Run collects immediately, then every interval until ctx is cancelled. Each
// wait is jittered by up to 10% to avoid hammering the resolver in lockstep.
func (c *Collector) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Hour
	}
	for {
		if _, err := c.RunOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.log().Error("collection run failed", "err", err)
		}
		if c.AfterRun != nil && ctx.Err() == nil {
			c.AfterRun(ctx)
		}

		jitter := time.Duration(rand.Int63n(int64(interval/10) + 1))
		timer := time.NewTimer(interval + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
