package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/pawal/torrent-tracker/internal/collector"
	"github.com/pawal/torrent-tracker/internal/enrich"
	"github.com/pawal/torrent-tracker/internal/resolver"
	"github.com/pawal/torrent-tracker/internal/store"
)

// enrichFlags are shared by the enrich and serve commands.
type enrichFlags struct {
	enabled  bool
	cymru    bool
	rdap     bool
	geoipDB  string
	maxAge   time.Duration
	batch    int
	workers  int
	rdapWait time.Duration
}

func (ef *enrichFlags) register(fs *flag.FlagSet, withEnable bool) {
	if withEnable {
		fs.BoolVar(&ef.enabled, "enrich", true, "look up AS/RIR/location for new addresses after each pass")
	}
	fs.BoolVar(&ef.cymru, "cymru", true, "use the Team Cymru DNS service for AS, RIR and country")
	fs.BoolVar(&ef.rdap, "rdap", true, "query the RIRs over RDAP for network name and holder")
	fs.StringVar(&ef.geoipDB, "geoip-db", os.Getenv("TRACKERD_GEOIP_DB"),
		"path to a MaxMind/DB-IP .mmdb file for city-level geolocation")
	fs.DurationVar(&ef.maxAge, "enrich-max-age", 30*24*time.Hour, "how long placement data stays fresh")
	fs.IntVar(&ef.batch, "enrich-batch", 250, "maximum addresses to enrich per pass")
	fs.IntVar(&ef.workers, "enrich-workers", 4, "concurrent enrichment lookups")
	fs.DurationVar(&ef.rdapWait, "rdap-interval", time.Second, "minimum delay between RDAP requests")
}

// build assembles the provider chain and the enricher. The returned closer
// releases the GeoIP database, if one was opened.
func (ef *enrichFlags) build(st *store.Store, res resolver.Resolver, log *slog.Logger) (*collector.Enricher, func(), error) {
	var (
		providers []enrich.Provider
		closers   []func()
	)

	if ef.cymru {
		providers = append(providers, enrich.Timeout{
			Provider: &enrich.Cymru{Resolver: res},
			Limit:    20 * time.Second,
		})
	}
	if ef.rdap {
		providers = append(providers, enrich.Timeout{
			Provider: &enrich.RDAP{MinInterval: ef.rdapWait},
			Limit:    45 * time.Second,
		})
	}
	if ef.geoipDB != "" {
		mm, err := enrich.OpenMaxMind(ef.geoipDB)
		if err != nil {
			return nil, nil, err
		}
		closers = append(closers, func() { mm.Close() })
		providers = append(providers, mm)
		log.Debug("geoip database opened", "path", ef.geoipDB)
	}

	if len(providers) == 0 {
		return nil, nil, fmt.Errorf("no enrichment providers enabled")
	}

	closeAll := func() {
		for _, c := range closers {
			c()
		}
	}
	return &collector.Enricher{
		Store:       st,
		Provider:    &enrich.Chain{Providers: providers, Log: log},
		Log:         log,
		MaxAge:      ef.maxAge,
		BatchLimit:  ef.batch,
		Concurrency: ef.workers,
	}, closeAll, nil
}

func cmdEnrich(ctx context.Context, st *store.Store, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("enrich", flag.ContinueOnError)
	all := fs.Bool("all", false, "re-enrich every address, ignoring --enrich-max-age")
	var (
		ef enrichFlags
		rf resolverFlags
	)
	ef.register(fs, false)
	rf.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *all {
		ef.maxAge = 0
	}

	res, err := rf.resolver()
	if err != nil {
		return err
	}

	enricher, closeAll, err := ef.build(st, res, log)
	if err != nil {
		return err
	}
	defer closeAll()

	sum, err := enricher.RunOnce(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d addresses considered, %d enriched, %d failed in %s\n",
		sum.Considered, sum.Enriched, sum.Failed, sum.Duration.Round(time.Millisecond))

	cov, err := st.Coverage(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("coverage: %d/%d active addresses enriched, %d with an origin AS\n",
		cov.Enriched, cov.ActiveIPs, cov.WithASN)
	return nil
}

func cmdNetworks(ctx context.Context, st *store.Store, _ *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("networks", flag.ContinueOnError)
	limit := fs.Int("n", 20, "how many networks to show")
	asJSON := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	networks, err := st.TopNetworks(ctx, *limit)
	if err != nil {
		return err
	}
	rirs, err := st.ByRIR(ctx)
	if err != nil {
		return err
	}
	countries, err := st.ByCountry(ctx, *limit)
	if err != nil {
		return err
	}

	if *asJSON {
		return writeJSON(os.Stdout, map[string]any{
			"networks": networks, "rirs": rirs, "countries": countries,
		})
	}

	section := func(title, keyHeader string, rows []store.NetworkStat, withLabel bool) {
		fmt.Printf("\n%s\n", title)
		header := []string{keyHeader, "TRACKERS", "ADDRESSES"}
		if withLabel {
			header = []string{keyHeader, "HOLDER", "TRACKERS", "ADDRESSES"}
		}
		tw := table(header...)
		for _, r := range rows {
			if withLabel {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\n", r.Key, truncate(r.Label, 44), r.Trackers, r.IPs)
			} else {
				fmt.Fprintf(tw, "%s\t%d\t%d\n", r.Key, r.Trackers, r.IPs)
			}
		}
		tw.Flush()
	}

	cov, err := st.Coverage(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d/%d active addresses enriched, %d with an origin AS\n",
		cov.Enriched, cov.ActiveIPs, cov.WithASN)
	if cov.Enriched == 0 {
		fmt.Println("\nNothing to summarise yet. Run \"trackerd enrich\" first.")
		return nil
	}

	section("Top networks", "AS", networks, true)
	section("By RIR", "RIR", rirs, false)
	section("By country", "CC", countries, false)
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
