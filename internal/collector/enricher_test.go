package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pawal/torrent-tracker/internal/enrich"
	"github.com/pawal/torrent-tracker/internal/store"
)

// fakeProvider serves canned enrichment keyed by address.
type fakeProvider struct {
	mu    sync.Mutex
	byIP  map[string]enrich.Info
	err   error
	calls int
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Lookup(_ context.Context, ip netip.Addr) (enrich.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return enrich.Info{}, f.err
	}
	return f.byIP[ip.String()], nil
}

func testEnricher(t *testing.T) (*Enricher, *store.Store, *fakeProvider) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "enrich.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	p := &fakeProvider{byIP: map[string]enrich.Info{}}
	return &Enricher{
		Store:       st,
		Provider:    p,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Concurrency: 4,
	}, st, p
}

func seedTrackerIP(t *testing.T, st *store.Store, name, ip string, family int) store.Tracker {
	t.Helper()
	ctx := context.Background()
	tr, _, err := st.AddTracker(ctx, name, "test", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	err = st.ApplyPlan(ctx, tr.ID, store.Plan{Status: store.StatusOK, Actions: []store.Action{
		{IP: ip, Family: family, Kind: store.ActionAdd},
	}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestEnricherStoresPlacement(t *testing.T) {
	e, st, p := testEnricher(t)
	ctx := context.Background()
	seedTrackerIP(t, st, "a.example.com", "1.2.3.4", 4)

	p.byIP["1.2.3.4"] = enrich.Info{
		ASN: 13335, ASName: "CLOUDFLARENET", RIR: "arin", Country: "US",
		Org: "Cloudflare, Inc.", Sources: []string{"cymru", "rdap"},
	}

	sum, err := e.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Considered != 1 || sum.Enriched != 1 || sum.Failed != 0 {
		t.Errorf("summary = %+v", sum)
	}

	got, err := st.IPInfoFor(ctx, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if got.ASN != 13335 || got.RIR != "arin" || got.Org != "Cloudflare, Inc." {
		t.Errorf("stored %+v", got)
	}
	if got.Sources != "cymru,rdap" {
		t.Errorf("Sources = %q", got.Sources)
	}
}

// Enriched addresses should not be looked up again until they go stale.
func TestEnricherSkipsFreshAddresses(t *testing.T) {
	e, st, p := testEnricher(t)
	ctx := context.Background()
	seedTrackerIP(t, st, "a.example.com", "1.2.3.4", 4)
	p.byIP["1.2.3.4"] = enrich.Info{ASN: 1}

	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	first := p.calls

	sum, err := e.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Considered != 0 {
		t.Errorf("second run considered %d addresses, want 0", sum.Considered)
	}
	if p.calls != first {
		t.Errorf("provider called again: %d -> %d", first, p.calls)
	}
}

// A failed lookup still writes a row, so it ages out instead of being retried
// on every single pass.
func TestEnricherRecordsFailures(t *testing.T) {
	e, st, p := testEnricher(t)
	ctx := context.Background()
	seedTrackerIP(t, st, "a.example.com", "1.2.3.4", 4)
	p.err = errors.New("registry unreachable")

	sum, err := e.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 1 || sum.Enriched != 0 {
		t.Errorf("summary = %+v", sum)
	}

	got, err := st.IPInfoFor(ctx, "1.2.3.4")
	if err != nil {
		t.Fatalf("no row written for a failed lookup: %v", err)
	}
	if got.Error == "" {
		t.Error("the failure reason was not recorded")
	}

	// And it is not immediately retried.
	if sum, _ := e.RunOnce(ctx); sum.Considered != 0 {
		t.Errorf("failed address was retried straight away")
	}
}

func TestEnricherRespectsBatchLimit(t *testing.T) {
	e, st, p := testEnricher(t)
	e.BatchLimit = 2
	ctx := context.Background()

	for _, ip := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"} {
		seedTrackerIP(t, st, ip+".example.com", ip, 4)
		p.byIP[ip] = enrich.Info{ASN: 1}
	}

	sum, err := e.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Considered != 2 {
		t.Errorf("considered %d, want the batch limit of 2", sum.Considered)
	}
}

func TestEnricherEmptyQueue(t *testing.T) {
	e, _, p := testEnricher(t)
	sum, err := e.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Considered != 0 || p.calls != 0 {
		t.Errorf("summary = %+v, calls = %d", sum, p.calls)
	}
}

// An unknown address still yields a row, so we do not re-query it constantly.
func TestEnricherHandlesUnknownAddress(t *testing.T) {
	e, st, _ := testEnricher(t)
	ctx := context.Background()
	seedTrackerIP(t, st, "a.example.com", "192.0.2.1", 4)

	sum, err := e.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Enriched != 1 {
		t.Errorf("summary = %+v", sum)
	}
	got, err := st.IPInfoFor(ctx, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ASN != 0 {
		t.Errorf("ASN = %d, want 0", got.ASN)
	}
}

func TestEnricherConcurrent(t *testing.T) {
	e, st, p := testEnricher(t)
	ctx := context.Background()

	const n = 30
	for i := 0; i < n; i++ {
		ip := netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)}).String()
		seedTrackerIP(t, st, ip+".example.com", ip, 4)
		p.byIP[ip] = enrich.Info{ASN: 64500 + i, RIR: "arin"}
	}

	sum, err := e.RunOnce(ctx)
	if err != nil {
		t.Fatalf("concurrent enrichment failed: %v", err)
	}
	if sum.Enriched != n {
		t.Errorf("enriched %d, want %d (failed %d)", sum.Enriched, n, sum.Failed)
	}

	cov, err := st.Coverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov.WithASN != n {
		t.Errorf("coverage with_asn = %d, want %d", cov.WithASN, n)
	}
}

func TestEnricherDefaults(t *testing.T) {
	e := &Enricher{}
	if e.maxAge() != 30*24*time.Hour {
		t.Errorf("maxAge = %v", e.maxAge())
	}
	if e.batchLimit() != 250 {
		t.Errorf("batchLimit = %d", e.batchLimit())
	}
	if e.concurrency() != 4 {
		t.Errorf("concurrency = %d", e.concurrency())
	}
	if e.log() == nil || e.now().IsZero() {
		t.Error("log/now defaults are broken")
	}
}

func TestDedupeSources(t *testing.T) {
	got := dedupe([]string{"cymru", "rdap", "cymru", "", "maxmind"})
	want := []string{"cymru", "rdap", "maxmind"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}
