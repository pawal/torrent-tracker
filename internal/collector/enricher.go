package collector

import (
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/pawal/torrent-tracker/internal/enrich"
	"github.com/pawal/torrent-tracker/internal/store"
)

// Enricher fills in the network placement of observed addresses.
type Enricher struct {
	Store    *store.Store
	Provider enrich.Provider
	Log      *slog.Logger

	// MaxAge is how long placement data stays fresh. Defaults to 30 days:
	// prefixes change hands slowly, and the registries are rate-limited.
	MaxAge time.Duration
	// BatchLimit caps addresses per run so a first run over a large registry
	// does not hammer the sources. Defaults to 250.
	BatchLimit int
	// Concurrency bounds simultaneous lookups. Defaults to 4, deliberately
	// lower than DNS collection because RDAP is rate-limited.
	Concurrency int
	// Now is overridable for tests.
	Now func() time.Time
}

// EnrichSummary reports what one enrichment pass did.
type EnrichSummary struct {
	Considered int
	Enriched   int
	Failed     int
	Duration   time.Duration
}

func (e *Enricher) now() time.Time        { return nowOr(e.Now) }
func (e *Enricher) log() *slog.Logger     { return logOr(e.Log) }
func (e *Enricher) maxAge() time.Duration { return orDefault(e.MaxAge, 30*24*time.Hour) }
func (e *Enricher) batchLimit() int       { return orDefault(e.BatchLimit, 250) }
func (e *Enricher) concurrency() int      { return orDefault(e.Concurrency, 4) }

// RunOnce enriches active addresses that are new or stale.
func (e *Enricher) RunOnce(ctx context.Context) (EnrichSummary, error) {
	start := e.now()

	pending, err := e.Store.IPsNeedingEnrichment(ctx, e.maxAge(), start, e.batchLimit())
	if err != nil {
		return EnrichSummary{}, err
	}
	sum := EnrichSummary{Considered: len(pending)}
	if len(pending) == 0 {
		return sum, nil
	}

	for _, ok := range pool(ctx, e.concurrency(), pending, func(rec store.IPRecord) bool {
		return e.enrichOne(ctx, rec)
	}) {
		if ok {
			sum.Enriched++
		} else {
			sum.Failed++
		}
	}

	sum.Duration = e.now().Sub(start)
	e.log().Info("enrichment finished",
		"considered", sum.Considered, "enriched", sum.Enriched, "failed", sum.Failed,
		"duration", sum.Duration.Round(time.Millisecond))
	return sum, nil
}

// enrichOne looks up and stores one address. A failed lookup is still written,
// with the error recorded, so it ages out of the pending set rather than being
// retried on every single pass.
func (e *Enricher) enrichOne(ctx context.Context, rec store.IPRecord) bool {
	addr, err := netip.ParseAddr(rec.IP)
	if err != nil {
		e.log().Warn("skipping unparseable address", "ip", rec.IP, "err", err)
		return false
	}

	info, lookupErr := e.Provider.Lookup(ctx, addr)
	stored := toStoreInfo(rec, info)
	if lookupErr != nil {
		stored.Error = lookupErr.Error()
		e.log().Debug("enrichment failed", "ip", rec.IP, "err", lookupErr)
	}

	if err := e.Store.PutIPInfo(ctx, stored, e.now()); err != nil {
		e.log().Error("storing enrichment failed", "ip", rec.IP, "err", err)
		return false
	}
	return lookupErr == nil
}

func toStoreInfo(rec store.IPRecord, info enrich.Info) store.IPInfo {
	return store.IPInfo{
		IP:          rec.IP,
		Family:      rec.Family,
		ASN:         info.ASN,
		ASName:      info.ASName,
		Prefix:      info.Prefix,
		RIR:         info.RIR,
		Country:     info.Country,
		Allocated:   info.Allocated,
		NetworkName: info.NetworkName,
		Org:         info.Org,
		City:        info.City,
		Latitude:    info.Latitude,
		Longitude:   info.Longitude,
		Sources:     strings.Join(dedupe(info.Sources), ","),
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
