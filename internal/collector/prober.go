package collector

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/pawal/torrent-tracker/internal/prober"
	"github.com/pawal/torrent-tracker/internal/resolver"
	"github.com/pawal/torrent-tracker/internal/store"
)

// Checker is the seam over the network, so tests need no sockets.
type Checker interface {
	Probe(ctx context.Context, t prober.Target) prober.Result
}

// Prober checks whether the tracked names still answer the tracker protocol.
// It works per (endpoint, address), the same grain as enrichment, so a name
// serving on three of its four addresses is visible as exactly that.
type Prober struct {
	Store    *store.Store
	Probe    Checker
	Resolver resolver.Resolver
	Log      *slog.Logger

	// Concurrency bounds simultaneous trackers. Defaults to 8. Endpoints
	// within one tracker are probed in series to stay polite.
	Concurrency int
	// MissThreshold is the consecutive failures needed to call an endpoint
	// dead. Defaults to 2.
	MissThreshold int
	// SamplePerFamily caps how many addresses of a rolling family are probed.
	// Defaults to 2; the rest are interchangeable and gone by next round.
	SamplePerFamily int
	// Now is overridable for tests.
	Now func() time.Time
}

// ProbeSummary reports what one probing pass did.
type ProbeSummary struct {
	Trackers int
	Probes   int
	Live     int
	Partial  int
	Dead     int
	Unknown  int
	Changes  int
	Duration time.Duration
}

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

func (p *Prober) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func (p *Prober) concurrency() int {
	if p.Concurrency > 0 {
		return p.Concurrency
	}
	return 8
}

func (p *Prober) missThreshold() int {
	if p.MissThreshold > 0 {
		return p.MissThreshold
	}
	return 2
}

func (p *Prober) samplePerFamily() int {
	if p.SamplePerFamily > 0 {
		return p.SamplePerFamily
	}
	return 2
}

func (p *Prober) probe() Checker {
	if p.Probe != nil {
		return p.Probe
	}
	return &prober.Prober{}
}

// RunOnce probes every enabled tracker that has an announce endpoint.
func (p *Prober) RunOnce(ctx context.Context) (ProbeSummary, error) {
	start := p.now()

	targets, err := p.Store.ProbeTargets(ctx)
	if err != nil {
		return ProbeSummary{}, err
	}
	sum := ProbeSummary{Trackers: len(targets)}
	if len(targets) == 0 {
		return sum, nil
	}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, p.concurrency())
	)

	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(t store.ProbeTarget) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			reach, n, changed, err := p.probeOne(ctx, t)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if ctx.Err() == nil {
					p.log().Error("probe failed", "tracker", t.Tracker.Name, "err", err)
				}
				sum.Unknown++
				return
			}
			sum.Probes += n
			switch reach {
			case store.ReachLive:
				sum.Live++
			case store.ReachPartial:
				sum.Partial++
			case store.ReachDead:
				sum.Dead++
			default:
				sum.Unknown++
			}
			if changed {
				sum.Changes++
			}
		}(t)
	}
	wg.Wait()

	sum.Duration = p.now().Sub(start)
	p.log().Info("probing finished",
		"trackers", sum.Trackers, "probes", sum.Probes, "live", sum.Live,
		"partial", sum.Partial, "dead", sum.Dead, "unknown", sum.Unknown,
		"changes", sum.Changes, "duration", sum.Duration.Round(time.Millisecond))
	return sum, nil
}

// Run probes immediately, then every interval until ctx is cancelled. The
// wait is jittered like collection so the two do not march in lockstep.
func (p *Prober) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	for {
		if _, err := p.RunOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.log().Error("probing run failed", "err", err)
		}

		jitter := time.Duration(rand.Int64N(int64(interval/10) + 1))
		timer := time.NewTimer(interval + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// probeOne probes every endpoint of one tracker on every address it has, then
// rolls the results up to a single verdict for the name.
func (p *Prober) probeOne(ctx context.Context, t store.ProbeTarget) (store.Reach, int, bool, error) {
	previous, err := p.Store.ProbesFor(ctx, t.Tracker.ID)
	if err != nil {
		return "", 0, false, err
	}
	prev := make(map[probeKey]store.Probe, len(previous))
	for _, pr := range previous {
		prev[probeKey{pr.EndpointID, pr.IP}] = pr
	}

	addrs := p.addresses(ctx, t)
	now := p.now()
	threshold := p.missThreshold()

	ids := make([]int64, 0, len(t.Endpoints))
	probes := make([]store.Probe, 0, len(t.Endpoints)*len(addrs))
	for _, ep := range t.Endpoints {
		ids = append(ids, ep.ID)
		for _, ip := range addrs {
			if ctx.Err() != nil {
				return "", 0, false, ctx.Err()
			}
			res := p.probe().Probe(ctx, prober.Target{
				Host: t.Tracker.Name, IP: ip, Scheme: ep.Scheme, Port: ep.Port, Path: ep.Path,
			})
			key := probeKey{ep.ID, ip}
			before, seen := prev[key]
			probes = append(probes, merge(key, ip, res, before, seen, now, threshold))
		}
	}

	if err := p.Store.PutProbes(ctx, ids, probes); err != nil {
		return "", 0, false, err
	}
	reach := store.RollUp(probes)
	_, changed, err := p.Store.SetReach(ctx, t.Tracker.ID, reach, summarise(t.Endpoints, probes), now)
	if err != nil {
		return reach, len(probes), false, err
	}
	if changed {
		p.log().Info("reachability changed", "tracker", t.Tracker.Name, "reach", reach)
	}
	return reach, len(probes), changed, nil
}

type probeKey struct {
	endpointID int64
	ip         string
}

// addresses lists what to probe: the tracked addresses, plus a sample from
// each rolling family, whose records hold a prefix rather than addresses.
func (p *Prober) addresses(ctx context.Context, t store.ProbeTarget) []string {
	addrs := append([]string{}, t.Addrs...)
	if p.Resolver == nil {
		return addrs
	}
	for _, family := range t.Rolling {
		rr := resolver.TypeA
		if family == 6 {
			rr = resolver.TypeAAAA
		}
		got := p.Resolver.Lookup(ctx, t.Tracker.Name, rr).Addrs
		if len(got) > p.samplePerFamily() {
			got = got[:p.samplePerFamily()]
		}
		addrs = append(addrs, got...)
	}
	return addrs
}

// merge folds a fresh attempt into the stored verdict. One failure is not
// death: the threshold mirrors the rule that keeps a single absent A record
// from retiring an address.
func merge(key probeKey, ip string, res prober.Result, before store.Probe, seen bool,
	now time.Time, threshold int,
) store.Probe {
	out := store.Probe{
		EndpointID: key.endpointID,
		IP:         ip,
		Family:     store.Family(ip),
		RTTms:      int(res.RTT.Milliseconds()),
		Since:      now,
		CheckedAt:  now,
		Signature:  res.Signature,
		Server:     res.Server,
	}
	// A silent round says nothing about the software, so the last thing it did
	// disclose is kept rather than blanked.
	if out.Signature == "" && seen {
		out.Signature, out.Server = before.Signature, before.Server
	}

	switch res.State {
	case prober.Live:
		out.Result = store.ProbeLive
	case prober.Unknown:
		// The probe told us nothing, so keep believing the last thing it did
		// tell us. CheckedAt still moves, so the verdict visibly ages.
		out.Result = store.ProbeUnknown
		out.Reason = res.Reason
		if seen {
			out.Result = before.Result
			out.MissCount = before.MissCount
		}
	default:
		out.Reason = res.Reason
		out.MissCount = 1
		if seen {
			out.MissCount = before.MissCount + 1
		}
		switch {
		case out.MissCount >= threshold:
			out.Result = store.ProbeDead
		case seen && before.Result == store.ProbeLive:
			// Believed live until it misses again.
			out.Result = store.ProbeLive
		default:
			out.Result = store.ProbeUnknown
		}
	}

	if seen && before.Result == out.Result {
		out.Since = before.Since
	}
	return out
}

// summarise renders the probe outcome for a change-feed entry, one clause per
// endpoint. Per-address transitions stay out of the feed: a rolling family
// would flood it.
func summarise(endpoints []store.Endpoint, probes []store.Probe) string {
	if len(probes) == 0 {
		return "nothing to probe"
	}

	byEndpoint := map[int64][]store.Probe{}
	for _, p := range probes {
		byEndpoint[p.EndpointID] = append(byEndpoint[p.EndpointID], p)
	}

	var (
		parts     []string
		answering int
		total     int
	)
	for _, ep := range endpoints {
		got := byEndpoint[ep.ID]
		if len(got) == 0 {
			continue
		}
		total++
		reach := store.RollUp(got)
		if reach.Answers() {
			answering++
		}
		parts = append(parts, ep.Label()+" "+describeEndpoint(reach, got))
	}
	if total == 0 {
		return "nothing to probe"
	}
	return fmt.Sprintf("%d of %d endpoints answer: %s", answering, total, strings.Join(parts, ", "))
}

func describeEndpoint(reach store.Reach, probes []store.Probe) string {
	switch reach {
	case store.ReachPartial:
		live := 0
		for _, p := range probes {
			if p.Result == store.ProbeLive {
				live++
			}
		}
		return fmt.Sprintf("live on %d of %d addresses", live, len(probes))
	case store.ReachDead:
		for _, p := range probes {
			if p.Reason != "" {
				return "dead (" + p.Reason + ")"
			}
		}
		return "dead"
	}
	return string(reach)
}
