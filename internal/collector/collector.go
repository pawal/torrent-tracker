// Package collector resolves the known tracker names on a schedule and turns
// the answers into address history and change-feed entries.
package collector

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net/netip"
	"sort"
	"strings"
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
	// RollAfter is how many changed runs switch a family to prefix tracking.
	// Defaults to 3; negative keeps every address.
	RollAfter int
	// SteadyAfter is how many unchanged runs switch it back. Defaults to 3.
	SteadyAfter int
	// Retention is how long the per-pass lookup log is kept. Defaults to 90
	// days, three times the month the UI draws.
	Retention time.Duration
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

func (c *Collector) retention() time.Duration {
	if c.Retention > 0 {
		return c.Retention
	}
	return 90 * 24 * time.Hour
}

func (c *Collector) rollAfter() int {
	switch {
	case c.RollAfter < 0:
		return 0 // rolling detection off
	case c.RollAfter == 0:
		return 3
	}
	return c.RollAfter
}

// RunOnce resolves every enabled tracker once and persists the outcome.
func (c *Collector) RunOnce(ctx context.Context) (Summary, error) {
	start := c.now()

	trackers, err := c.Store.ListTrackers(ctx, false)
	if err != nil {
		return Summary{}, err
	}

	// Control names first: what they resolve to is a parking answer.
	parking, err := c.resolveControls(ctx)
	if err != nil {
		return Summary{}, err
	}

	prefixes, err := c.prefixIndex(ctx)
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

			plan, err := c.collectOne(ctx, t, prefixes, parking)

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

	if n, err := c.Store.PruneLookups(ctx, c.now().Add(-c.retention())); err != nil {
		c.log().Error("prune lookups", "err", err)
	} else if n > 0 {
		c.log().Info("pruned lookups", "rows", n)
	}

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
func (c *Collector) collectOne(ctx context.Context, t store.Tracker,
	prefixes *prefixIndex, parking map[string]bool,
) (store.Plan, error) {
	prev, err := c.Store.ActiveRecords(ctx, t.ID)
	if err != nil {
		return store.Plan{}, err
	}
	states, err := c.Store.FamilyStates(ctx, t.ID)
	if err != nil {
		return store.Plan{}, err
	}

	obs := c.resolve(ctx, t.Name)

	plan := Diff(prev, states, t.LastStatus, obs, Options{
		MissThreshold: c.MissThreshold,
		RollAfter:     c.rollAfter(),
		SteadyAfter:   c.SteadyAfter,
		PrefixFor:     prefixes.lookup,
	})
	plan.Duration = time.Duration(obs.Duration()) * time.Millisecond

	if err := c.Store.ApplyPlan(ctx, t.ID, plan, c.now()); err != nil {
		return plan, err
	}
	if err := c.markParked(ctx, t, obs, parking); err != nil {
		return plan, err
	}
	if plan.Changes() > 0 {
		c.log().Info("tracker changed", "tracker", t.Name, "status", plan.Status, "changes", plan.Changes())
	}
	return plan, nil
}

// prefixIndex resolves an address to its prefix: exactly where enrichment
// knows the address, by containment where the address is too new to be known.
type prefixIndex struct {
	exact map[string]string
	// known is sorted most specific first.
	known []netip.Prefix
}

func (c *Collector) prefixIndex(ctx context.Context) (*prefixIndex, error) {
	exact, err := c.Store.PrefixMap(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := c.Store.KnownPrefixes(ctx)
	if err != nil {
		return nil, err
	}

	idx := &prefixIndex{exact: exact}
	for _, p := range raw {
		if parsed, err := netip.ParsePrefix(p); err == nil {
			idx.known = append(idx.known, parsed)
		}
	}
	sort.Slice(idx.known, func(i, j int) bool {
		return idx.known[i].Bits() > idx.known[j].Bits()
	})
	return idx, nil
}

func (p *prefixIndex) lookup(ip string) string {
	if p == nil {
		return ""
	}
	if prefix, ok := p.exact[ip]; ok {
		return prefix
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	for _, prefix := range p.known {
		if prefix.Contains(addr) {
			return prefix.String()
		}
	}
	return ""
}

func (c *Collector) resolve(ctx context.Context, name string) Observation {
	return Observation{
		A:    c.Resolver.Lookup(ctx, name, resolver.TypeA),
		AAAA: c.Resolver.Lookup(ctx, name, resolver.TypeAAAA),
	}
}

// resolveControls returns the addresses the control names answer with.
func (c *Collector) resolveControls(ctx context.Context) (map[string]bool, error) {
	controls, err := c.Store.ListControls(ctx)
	if err != nil {
		return nil, err
	}
	parking := map[string]bool{}
	for _, t := range controls {
		if ctx.Err() != nil {
			break
		}
		obs := c.resolve(ctx, t.Name)
		for _, r := range []resolver.Result{obs.A, obs.AAAA} {
			for _, ip := range r.Addrs {
				parking[ip] = true
			}
		}
	}
	if len(parking) > 0 {
		c.log().Debug("parking addresses", "controls", len(controls), "addresses", len(parking))
	}
	return parking, nil
}

// markParked flags a tracker whose every answer is a parking address. It
// judges the observation, not the records, so prefix rows do not hide it.
func (c *Collector) markParked(ctx context.Context, t store.Tracker, obs Observation, parking map[string]bool) error {
	if len(parking) == 0 {
		return nil
	}
	addrs := append(append([]string{}, obs.A.Addrs...), obs.AAAA.Addrs...)
	parked := len(addrs) > 0
	for _, ip := range addrs {
		if !parking[ip] {
			parked = false
			break
		}
	}
	// A name that did not resolve says nothing either way.
	if !parked && len(addrs) == 0 {
		return nil
	}

	detail := ""
	if parked {
		detail = fmt.Sprintf("resolves only to parking addresses: %s", strings.Join(addrs, " "))
	}
	changed, err := c.Store.SetParked(ctx, t.ID, parked, detail, c.now())
	if err != nil {
		return err
	}
	if changed && parked {
		c.log().Info("tracker parked", "tracker", t.Name, "addresses", len(addrs))
	}
	return nil
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
