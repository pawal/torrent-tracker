// Package collector resolves the known tracker names on a schedule and turns
// the answers into address history and change-feed entries.
package collector

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/pawal/torrent-tracker/internal/bep34"
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
	// SteadyAfter is how many unchanged runs switch it back. Defaults to 6.
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

func (c *Collector) now() time.Time    { return nowOr(c.Now) }
func (c *Collector) log() *slog.Logger { return logOr(c.Log) }
func (c *Collector) concurrency() int  { return orDefault(c.Concurrency, 8) }

func (c *Collector) retention() time.Duration { return orDefault(c.Retention, 90*24*time.Hour) }

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

	type outcome struct {
		name string
		plan store.Plan
		err  error
	}
	results := pool(ctx, c.concurrency(), trackers, func(t store.Tracker) outcome {
		plan, err := c.collectOne(ctx, t, prefixes, parking)
		return outcome{t.Name, plan, err}
	})
	for _, got := range results {
		if got.err != nil {
			sum.Errors++
			c.log().Error("collect failed", "tracker", got.name, "err", got.err)
			continue
		}
		if got.plan.Status == store.StatusOK {
			sum.OK++
		} else {
			sum.Errors++
		}
		sum.Changes += got.plan.Changes()
	}

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
	if err := c.checkBEP34(ctx, t); err != nil {
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

// checkBEP34 records the tracker preferences a name publishes in DNS and adopts
// the UDP endpoints it advertises. A query that failed leaves the stored record
// alone, the rule that keeps a broken resolver from erasing history.
func (c *Collector) checkBEP34(ctx context.Context, t store.Tracker) error {
	res := c.Resolver.Lookup(ctx, t.Name, resolver.TypeTXT)
	if !res.Status.Resolved() {
		return nil
	}
	rec := bep34.Parse(res.TXT)
	changed, err := c.Store.SetBEP34(ctx, t.ID, rec.Raw, rec.Denies(), c.now())
	if err != nil {
		return err
	}
	if rec.Denies() {
		if changed {
			c.log().Info("tracker denies BitTorrent traffic", "tracker", t.Name, "record", rec.Raw)
			return c.stopProbing(ctx, t)
		}
		return nil
	}
	if changed {
		c.log().Info("tracker preferences changed", "tracker", t.Name, "record", rec.Raw)
	}
	// A UDP preference names an endpoint outright. A TCP one names a port
	// without saying whether it speaks HTTP or HTTPS, and guessing wrong would
	// record a dead endpoint for a live tracker, so those are kept in the
	// record and not adopted.
	for _, p := range rec.Prefs {
		if p.Proto != "udp" {
			continue
		}
		fresh, err := c.Store.AddEndpoint(ctx, t.ID, "udp", p.Port, "/announce", c.now())
		if err != nil {
			return err
		}
		if fresh {
			c.log().Info("adopted endpoint advertised in DNS", "tracker", t.Name, "port", p.Port)
		}
	}
	return nil
}

// stopProbing closes out a tracker we have been told not to contact. The
// history stays; what ends is the claim to be measuring it now.
func (c *Collector) stopProbing(ctx context.Context, t store.Tracker) error {
	if err := c.Store.ClearProbes(ctx, t.ID, c.now()); err != nil {
		return err
	}
	_, _, err := c.Store.SetReach(ctx, t.ID, store.ReachUnknown, "", c.now())
	return err
}

// Run collects immediately, then every interval until ctx is cancelled.
func (c *Collector) Run(ctx context.Context, interval time.Duration) error {
	return every(ctx, orDefault(interval, time.Hour), func() {
		if _, err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
			c.log().Error("collection run failed", "err", err)
		}
		if c.AfterRun != nil && ctx.Err() == nil {
			c.AfterRun(ctx)
		}
	})
}
