package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/pawal/torrent-tracker/internal/collector"
	"github.com/pawal/torrent-tracker/internal/prober"
	"github.com/pawal/torrent-tracker/internal/resolver"
	"github.com/pawal/torrent-tracker/internal/store"
)

// probeFlags are shared by the probe, poll and serve commands.
type probeFlags struct {
	enabled   bool
	interval  time.Duration
	timeout   time.Duration
	workers   int
	fanout    int
	threshold int
	sample    int
}

func (pf *probeFlags) register(fs *flag.FlagSet, withEnable bool) {
	if withEnable {
		fs.BoolVar(&pf.enabled, "probe", true, "check whether tracked names still answer the tracker protocol")
	}
	fs.DurationVar(&pf.timeout, "probe-timeout", 5*time.Second, "per-probe timeout")
	fs.IntVar(&pf.workers, "probe-workers", 8, "trackers probed concurrently")
	fs.IntVar(&pf.fanout, "probe-fanout", 4, "probes in flight per tracker")
	fs.IntVar(&pf.threshold, "probe-miss-threshold", 2,
		"consecutive failures before an endpoint is called dead")
	fs.IntVar(&pf.sample, "probe-sample", 2, "addresses probed per rolling family")
}

// registerInterval adds the scheduling flag, which only the long-running
// serve command has any use for. Slower than collection on purpose: a probe is
// several requests per tracker and far more visible than a DNS query.
func (pf *probeFlags) registerInterval(fs *flag.FlagSet) {
	fs.DurationVar(&pf.interval, "probe-interval", 6*time.Hour, "how often to probe")
}

func (pf *probeFlags) build(st *store.Store, res resolver.Resolver, log *slog.Logger) *collector.Prober {
	return &collector.Prober{
		Store:           st,
		Probe:           &prober.Prober{Timeout: pf.timeout},
		Resolver:        res,
		Log:             log,
		Concurrency:     pf.workers,
		Fanout:          pf.fanout,
		MissThreshold:   pf.threshold,
		SamplePerFamily: pf.sample,
	}
}

func cmdProbe(ctx context.Context, st *store.Store, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "output JSON")
	var (
		pf probeFlags
		rf resolverFlags
	)
	pf.register(fs, false)
	rf.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	res, err := rf.resolver()
	if err != nil {
		return err
	}
	if *asJSON {
		sum, err := pf.build(st, res, log).RunOnce(ctx)
		if err != nil {
			return err
		}
		return writeJSON(os.Stdout, sum)
	}
	return runProbePass(ctx, st, log, pf, res)
}

// runProbePass probes once and prints the tally, or says why it could not.
// Shared by probe and poll.
func runProbePass(ctx context.Context, st *store.Store, log *slog.Logger,
	pf probeFlags, res resolver.Resolver,
) error {
	cov, err := st.ProbeCoverage(ctx)
	if err != nil {
		return err
	}
	if cov.Endpoints == 0 {
		fmt.Println("no announce endpoints on record, nothing to probe.")
		fmt.Println(`Re-run "trackerd import" to harvest them from a tracker list.`)
		return nil
	}

	sum, err := pf.build(st, res, log).RunOnce(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d trackers probed on %d endpoints, %d probes in %s\n",
		sum.Trackers, cov.Endpoints, sum.Probes, sum.Duration.Round(time.Millisecond))
	fmt.Printf("%d live, %d partial, %d dead, %d unknown, %d changes\n",
		sum.Live, sum.Partial, sum.Dead, sum.Unknown, sum.Changes)
	if cov.WithEndpoints < cov.Trackers {
		fmt.Printf("\n%d of %d trackers have no announce endpoint and were skipped.\n",
			cov.Trackers-cov.WithEndpoints, cov.Trackers)
	}
	return nil
}

func cmdReach(ctx context.Context, st *store.Store, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("reach", flag.ContinueOnError)
	only := fs.String("state", "", "show only trackers in this state (live, partial, dead, unknown)")
	asJSON := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	trackers, err := st.ListTrackers(ctx, false)
	if err != nil {
		return err
	}
	filtered := trackers[:0]
	for _, t := range trackers {
		reach := t.Reach
		if reach == "" {
			reach = store.ReachUnknown
		}
		if *only == "" || string(reach) == *only {
			filtered = append(filtered, t)
		}
	}
	if *asJSON {
		return writeJSON(os.Stdout, filtered)
	}

	summary, err := st.ReachSummary(ctx)
	if err != nil {
		return err
	}
	states := make([]string, 0, len(summary))
	for r := range summary {
		states = append(states, string(r))
	}
	sort.Strings(states)

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tREACH\tDNS\tPROBED")
	for _, t := range filtered {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			t.Name, orDash(string(t.Reach)), orDash(string(t.LastStatus)), ago(t.ReachCheckedAt))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Println()
	for _, s := range states {
		fmt.Printf("%s: %d  ", s, summary[store.Reach(s)])
	}
	fmt.Println()
	return nil
}
