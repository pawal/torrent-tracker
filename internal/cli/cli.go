// Package cli implements the trackerd subcommands.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/pawal/torrent-tracker/internal/api"
	"github.com/pawal/torrent-tracker/internal/collector"
	"github.com/pawal/torrent-tracker/internal/resolver"
	"github.com/pawal/torrent-tracker/internal/store"
	"github.com/pawal/torrent-tracker/internal/trackerlist"
	"github.com/pawal/torrent-tracker/web"
)

const usage = `trackerd - track the IP addresses of BitTorrent trackers over time

usage: trackerd [--db PATH] <command> [flags]

commands:
  serve      run the collector and the HTTP API
  poll       run a single collection pass, then probe, and exit
  enrich     look up AS, RIR and location for observed addresses
  probe      check which trackers still answer the tracker protocol
  reach      list trackers by whether they answer
  list       list known trackers
  add        add tracker names or announce URLs
  rm         remove a tracker (history is kept unless --purge)
  import     import announce URLs from a file or a published list
  changes    print the recent change feed
  networks   summarise the networks the trackers live in
  parked     list names that now resolve only to parking addresses
  shared     list addresses more than one tracker name resolves to
  control    list or set the control names parking is judged against
  sources    list the built-in public tracker lists

Run "trackerd <command> -h" for command flags.
`

// Main is the process entry point. It returns the exit code.
func Main(args []string) int {
	global := flag.NewFlagSet("trackerd", flag.ContinueOnError)
	global.SetOutput(os.Stderr)
	dbPath := global.String("db", defaultDB(), "path to the SQLite database")
	verbose := global.Bool("v", false, "verbose logging")
	global.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		fmt.Fprintln(os.Stderr, "\nglobal flags:")
		global.PrintDefaults()
	}

	if err := global.Parse(args); err != nil {
		return 2
	}
	rest := global.Args()
	if len(rest) == 0 {
		global.Usage()
		return 2
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, cmdArgs := rest[0], rest[1:]
	run, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		global.Usage()
		return 2
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()

	if err := run(ctx, st, log, cmdArgs); err != nil {
		if errors.Is(err, context.Canceled) {
			return 0
		}
		if errors.Is(err, flag.ErrHelp) {
			return 2
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

type commandFunc func(context.Context, *store.Store, *slog.Logger, []string) error

var commands map[string]commandFunc

func init() {
	// Assigned in init to avoid an initialisation cycle via cmdServe.
	commands = map[string]commandFunc{
		"serve":    cmdServe,
		"poll":     cmdPoll,
		"enrich":   cmdEnrich,
		"probe":    cmdProbe,
		"reach":    cmdReach,
		"list":     cmdList,
		"add":      cmdAdd,
		"rm":       cmdRemove,
		"import":   cmdImport,
		"changes":  cmdChanges,
		"networks": cmdNetworks,
		"sources":  cmdSources,
		"parked":   cmdParked,
		"shared":   cmdShared,
		"control":  cmdControl,
	}
}

func defaultDB() string {
	if v := os.Getenv("TRACKERD_DB"); v != "" {
		return v
	}
	return "trackers.db"
}

// resolverFlags are shared by serve and poll.
type resolverFlags struct {
	servers   string
	timeout   time.Duration
	retries   int
	workers   int
	threshold int
	rollAfter int
	steady    int
	retention time.Duration
	backoff   int
	retire    time.Duration
}

func (rf *resolverFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&rf.servers, "resolver", "", "comma-separated resolvers (default: system resolvers)")
	fs.DurationVar(&rf.timeout, "timeout", 5*time.Second, "per-query timeout")
	fs.IntVar(&rf.retries, "retries", 1, "extra attempts per resolver after a timeout")
	fs.IntVar(&rf.workers, "workers", 8, "concurrent lookups")
	fs.IntVar(&rf.threshold, "miss-threshold", 2,
		"consecutive absences before an address is retired (raise to suppress round-robin churn)")
	fs.IntVar(&rf.rollAfter, "roll-after", 3,
		"changes before a family is tracked by prefix instead of by address (-1 to keep every address)")
	fs.IntVar(&rf.steady, "steady-after", 6,
		"unchanged runs before a rolling family goes back to per-address tracking")
	fs.DurationVar(&rf.retention, "lookup-retention", 90*24*time.Hour,
		"how long the per-pass lookup log is kept")
	fs.IntVar(&rf.backoff, "backoff-after", 24,
		"consecutive passes without an address before a name is only retried daily (-1 to keep every name hourly)")
	fs.DurationVar(&rf.retire, "retire-after", 30*24*time.Hour,
		"how long a name that has never resolved, or never answered a probe, is kept in collection, history intact (-1 to keep it forever)")
}

// splitList turns a comma-separated flag value into a slice, dropping blanks.
func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (rf *resolverFlags) resolver() (*resolver.DNS, error) {
	return resolver.New(splitList(rf.servers), rf.timeout, rf.retries)
}

func (rf *resolverFlags) collector(st *store.Store, log *slog.Logger) (*collector.Collector, error) {
	res, err := rf.resolver()
	if err != nil {
		return nil, err
	}
	log.Debug("resolver configured", "servers", res.Servers)
	return &collector.Collector{
		Store:         st,
		Resolver:      res,
		Log:           log,
		Concurrency:   rf.workers,
		MissThreshold: rf.threshold,
		RollAfter:     rf.rollAfter,
		SteadyAfter:   rf.steady,
		Retention:     rf.retention,
		BackoffAfter:  rf.backoff,
		RetireAfter:   rf.retire,
	}, nil
}

func cmdServe(ctx context.Context, st *store.Store, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	interval := fs.Duration("interval", time.Hour, "collection interval")
	noCollect := fs.Bool("no-collect", false, "serve the API without running the collector")
	baseURL := fs.String("base-url", "", "public origin for canonical links and the sitemap (default: from the request)")
	var (
		rf resolverFlags
		ef enrichFlags
		pf probeFlags
	)
	rf.register(fs)
	ef.register(fs, true)
	pf.register(fs, true)
	pf.registerInterval(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	dist, err := web.Dist()
	if err != nil {
		return fmt.Errorf("load embedded frontend: %w", err)
	}
	srv := &api.Server{Store: st, Log: log, Static: dist, BaseURL: *baseURL}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	if !*noCollect {
		c, err := rf.collector(st, log)
		if err != nil {
			return err
		}

		if ef.enabled {
			res, err := rf.resolver()
			if err != nil {
				return err
			}
			enricher, closeAll, err := ef.build(st, res, log)
			if err != nil {
				return err
			}
			defer closeAll()
			// Runs straight after each collection pass, so addresses
			// discovered this round are annotated before anyone looks.
			c.AfterRun = func(ctx context.Context) {
				if _, err := enricher.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
					log.Error("enrichment run failed", "err", err)
				}
			}
		}

		if pf.enabled {
			res, err := rf.resolver()
			if err != nil {
				return err
			}
			// Its own loop, not AfterRun: probing is heavier than resolving
			// and belongs on a slower clock.
			p := pf.build(st, res, log)
			go func() {
				if err := p.Run(ctx, pf.interval); err != nil && !errors.Is(err, context.Canceled) {
					errCh <- err
				}
			}()
		}

		go func() {
			if err := c.Run(ctx, *interval); err != nil && !errors.Is(err, context.Canceled) {
				errCh <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		shutdown(httpSrv)
		return err
	}
	shutdown(httpSrv)
	return nil
}

func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

func cmdPoll(ctx context.Context, st *store.Store, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("poll", flag.ContinueOnError)
	var (
		rf resolverFlags
		pf probeFlags
	)
	rf.register(fs)
	pf.register(fs, true)
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := rf.collector(st, log)
	if err != nil {
		return err
	}
	sum, err := c.RunOnce(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d trackers, %d ok, %d failed, %d changes in %s\n",
		sum.Trackers, sum.OK, sum.Errors, sum.Changes, sum.Duration.Round(time.Millisecond))
	if sum.Skipped > 0 || sum.Retired > 0 {
		fmt.Printf("%d backed off to a daily lookup, %d retired for never resolving\n",
			sum.Skipped, sum.Retired)
	}

	if !pf.enabled {
		return nil
	}
	// After collection, so the probe works from the addresses this pass found.
	res, err := rf.resolver()
	if err != nil {
		return err
	}
	fmt.Println()
	return runProbePass(ctx, st, log, pf, res)
}

func cmdList(ctx context.Context, st *store.Store, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	all := fs.Bool("all", false, "include removed trackers")
	asJSON := fs.Bool("json", false, "output JSON")
	namesOnly := fs.Bool("names", false, "print only hostnames")
	if err := fs.Parse(args); err != nil {
		return err
	}

	views, err := st.ListTrackerViews(ctx, *all)
	if err != nil {
		return err
	}
	switch {
	case *asJSON:
		return writeJSON(os.Stdout, views)
	case *namesOnly:
		for _, v := range views {
			fmt.Println(v.Name)
		}
		return nil
	}

	tw := table("NAME", "STATUS", "CHECKED", "IPV4", "IPV6")
	for _, v := range views {
		status := string(v.LastStatus)
		if status == "" {
			status = "-"
		}
		if !v.Enabled {
			status += " (removed)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			v.Name, status, ago(v.LastCheckedAt),
			joinOrDash(v.IPv4), joinOrDash(v.IPv6))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d trackers\n", len(views))
	return nil
}

func cmdAdd(ctx context.Context, st *store.Store, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	source := fs.String("source", "manual", "note where this tracker came from")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("add: need at least one tracker name or announce URL")
	}

	now := time.Now().UTC()
	var added, existing int
	for _, raw := range fs.Args() {
		ep, err := trackerlist.ParseEndpoint(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipped %s: %v\n", raw, err)
			continue
		}
		t, created, err := st.AddTracker(ctx, ep.Host, *source, now)
		if err != nil {
			return err
		}
		if created {
			added++
			fmt.Println("added", ep.Host)
		} else {
			existing++
			fmt.Println("already known:", ep.Host)
		}
		// A bare hostname carries no endpoint, so it cannot be probed.
		if !ep.Probeable() {
			continue
		}
		fresh, err := st.AddEndpoint(ctx, t.ID, ep.Scheme, ep.Port, ep.Path, now)
		if err != nil {
			return err
		}
		if fresh {
			fmt.Println("  endpoint", ep.Label())
		}
	}
	fmt.Printf("\n%d added, %d already known\n", added, existing)
	return nil
}

func cmdRemove(ctx context.Context, st *store.Store, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "delete the tracker and all of its history")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("rm: need at least one tracker name")
	}

	for _, raw := range fs.Args() {
		host := hostArg(raw)
		if err := st.RemoveTracker(ctx, host, *purge); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			continue
		}
		if *purge {
			fmt.Println("purged", host)
		} else {
			fmt.Println("removed", host, "(history kept)")
		}
	}
	return nil
}

func cmdImport(ctx context.Context, st *store.Store, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	file := fs.String("file", "", "read announce URLs from this file")
	from := fs.String("url", "", "fetch announce URLs from a URL or a built-in source name")
	dry := fs.Bool("dry-run", false, "report what would be imported without writing")
	onlyEndpoints := fs.Bool("endpoints-only", false,
		"only attach announce endpoints to known trackers, adding and re-enabling nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*file == "") == (*from == "") {
		return errors.New("import: pass exactly one of --file or --url")
	}

	var (
		eps     []trackerlist.Endpoint
		skipped []string
		source  string
		err     error
	)
	if *file != "" {
		source = "file:" + *file
		eps, skipped, err = trackerlist.ParseEndpointsFile(*file)
	} else {
		source = *from
		if _, ok := trackerlist.Sources[*from]; !ok {
			source = "url:" + *from
		}
		eps, skipped, err = trackerlist.FetchEndpoints(ctx, *from)
	}
	if err != nil {
		return err
	}

	hosts := trackerlist.Hosts(eps)
	fmt.Printf("parsed %d unique hostnames on %d endpoints (%d lines skipped)\n",
		len(hosts), len(eps), len(skipped))
	if *dry {
		for _, ep := range eps {
			fmt.Println(" ", ep)
		}
		return nil
	}

	now := time.Now().UTC()
	var added, newEndpoints, unknown int
	// A zero id marks a hostname the registry does not have and, under
	// --endpoints-only, is not going to get.
	ids := make(map[string]int64, len(hosts))

	for _, ep := range eps {
		id, seen := ids[ep.Host]
		if !seen {
			if *onlyEndpoints {
				t, err := st.TrackerByName(ctx, ep.Host)
				switch {
				case errors.Is(err, store.ErrNotFound):
					unknown++
				case err != nil:
					return err
				default:
					id = t.ID
				}
			} else {
				t, created, err := st.AddTracker(ctx, ep.Host, source, now)
				if err != nil {
					return err
				}
				if created {
					added++
				}
				id = t.ID
			}
			ids[ep.Host] = id
		}
		// Endpoints we cannot speak to are not worth recording.
		if id == 0 || !ep.Probeable() {
			continue
		}
		fresh, err := st.AddEndpoint(ctx, id, ep.Scheme, ep.Port, ep.Path, now)
		if err != nil {
			return err
		}
		if fresh {
			newEndpoints++
		}
	}

	if *onlyEndpoints {
		fmt.Printf("%d new announce endpoints; %d of %d hostnames are not in the registry and were left alone\n",
			newEndpoints, unknown, len(hosts))
		return nil
	}
	fmt.Printf("%d new, %d already known, %d new announce endpoints\n",
		added, len(hosts)-added, newEndpoints)
	return nil
}

func cmdChanges(ctx context.Context, st *store.Store, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("changes", flag.ContinueOnError)
	limit := fs.Int("n", 50, "how many changes to show")
	since := fs.Duration("since", 0, "only changes within this duration (e.g. 24h)")
	asJSON := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var from time.Time
	if *since > 0 {
		from = time.Now().UTC().Add(-*since)
	}
	changes, err := st.RecentChanges(ctx, from, *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(os.Stdout, changes)
	}
	for _, c := range changes {
		fmt.Printf("%s  %s\n", c.ObservedAt.Format(time.RFC3339), describe(c))
	}
	if len(changes) == 0 {
		fmt.Println("no changes recorded")
	}
	return nil
}

// describe renders a change in the +/- style of the original Perl report.
func describe(c store.Change) string {
	switch c.Type {
	case store.ChangeIPAdded:
		return fmt.Sprintf("+ %s %s", c.Tracker, c.IP)
	case store.ChangeIPRemoved:
		return fmt.Sprintf("- %s %s", c.Tracker, c.IP)
	case store.ChangeStatusChanged:
		return fmt.Sprintf("! %s %s", c.Tracker, c.Detail)
	case store.ChangeTrackerAdded:
		return fmt.Sprintf("* %s added (%s)", c.Tracker, c.Detail)
	case store.ChangeTrackerRetired:
		return fmt.Sprintf("* %s retired (%s)", c.Tracker, c.Detail)
	default:
		return fmt.Sprintf("? %s %s %s", c.Tracker, c.Type, c.Detail)
	}
}

func cmdSources(_ context.Context, _ *store.Store, _ *slog.Logger, _ []string) error {
	names := make([]string, 0, len(trackerlist.Sources))
	for k := range trackerlist.Sources {
		names = append(names, k)
	}
	sort.Strings(names)

	tw := table("NAME", "URL")
	for _, n := range names {
		fmt.Fprintf(tw, "%s\t%s\n", n, trackerlist.Sources[n])
	}
	return tw.Flush()
}

// table is the aligned-column writer every listing prints through.
func table(header ...string) *tabwriter.Writer {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	return tw
}

// hostArg reads a tracker name from the command line, accepting a full announce
// URL as well as a bare hostname. Anything unparseable is passed through so the
// store can be the one to say it does not know the name.
func hostArg(raw string) string {
	if host, err := trackerlist.Host(raw); err == nil {
		return host
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func joinOrDash(v []string) string {
	if len(v) == 0 {
		return "-"
	}
	return strings.Join(v, ",")
}

func ago(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func cmdParked(ctx context.Context, st *store.Store, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("parked", flag.ContinueOnError)
	disable := fs.Bool("disable", false, "remove every parked tracker, keeping its history")
	asJSON := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	parked, err := st.ListParked(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(os.Stdout, parked)
	}
	if len(parked) == 0 {
		fmt.Println("no parked trackers")
		return nil
	}

	tw := table("NAME", "STATUS", "CHECKED")
	for _, t := range parked {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", t.Name, orDash(string(t.LastStatus)), ago(t.LastCheckedAt))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if !*disable {
		fmt.Printf("\n%d parked, still collecting. Use --disable to remove them.\n", len(parked))
		return nil
	}
	for _, t := range parked {
		if err := st.RemoveTracker(ctx, t.Name, false); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			continue
		}
	}
	fmt.Printf("\n%d removed, history kept\n", len(parked))
	return nil
}

func cmdShared(ctx context.Context, st *store.Store, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("shared", flag.ContinueOnError)
	limit := fs.Int("n", 50, "how many addresses to show")
	window := fs.Duration("since", 48*time.Hour, "how recently a name must have been on the address")
	asJSON := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	shared, err := st.SharedAddresses(ctx, time.Now().UTC().Add(-*window), *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(os.Stdout, shared)
	}
	if len(shared) == 0 {
		fmt.Println("no address is shared by more than one tracker")
		return nil
	}

	tw := table("ADDRESS", "NAMES", "STILL", "NETWORK", "TRACKERS")
	for _, a := range shared {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", a.IP, len(a.Trackers),
			yesNo(a.Active), orDash(network(a.Network)), strings.Join(a.Trackers, ", "))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d shared addresses. One host is one operator and one outage; "+
		"one CDN edge is only one front end.\n", len(shared))
	return nil
}

// network renders an origin AS the way the listings do.
func network(n store.NetworkRef) string {
	if n.ASN == 0 {
		return ""
	}
	if n.Holder == "" {
		return fmt.Sprintf("AS%d", n.ASN)
	}
	return fmt.Sprintf("AS%d %s", n.ASN, n.Holder)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func cmdControl(ctx context.Context, st *store.Store, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("control", flag.ContinueOnError)
	unset := fs.Bool("unset", false, "turn the names back into ordinary trackers")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() == 0 {
		controls, err := st.ListControls(ctx)
		if err != nil {
			return err
		}
		if len(controls) == 0 {
			fmt.Println("no control names")
			return nil
		}
		for _, t := range controls {
			fmt.Println(t.Name)
		}
		return nil
	}

	for _, raw := range fs.Args() {
		host := hostArg(raw)
		if err := st.SetControl(ctx, host, !*unset); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			continue
		}
		if *unset {
			fmt.Println("no longer a control name:", host)
		} else {
			fmt.Println("control name:", host)
		}
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
