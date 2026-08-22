package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPutAndGetIPInfo(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()

	in := IPInfo{
		IP: "1.2.3.4", Family: 4, ASN: 13335, ASName: "CLOUDFLARENET",
		Prefix: "1.2.3.0/24", RIR: "arin", Country: "US", Allocated: "2011-11-01",
		NetworkName: "CF-NET", Org: "Cloudflare, Inc.", City: "London",
		Latitude: 51.5, Longitude: -0.1, Sources: "cymru,rdap",
	}
	must(t, s.PutIPInfo(ctx, in, base))

	got, err := s.IPInfoFor(ctx, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if got.ASN != in.ASN || got.RIR != in.RIR || got.Org != in.Org || got.City != in.City {
		t.Errorf("got %+v, want %+v", got, in)
	}
	if got.Latitude != 51.5 || got.Longitude != -0.1 {
		t.Errorf("coordinates lost: %v %v", got.Latitude, got.Longitude)
	}
	if !got.FetchedAt.Equal(base) {
		t.Errorf("FetchedAt = %v, want %v", got.FetchedAt, base)
	}

	if _, err := s.IPInfoFor(ctx, "9.9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPutIPInfoUpserts(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()

	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.2.3.4", Family: 4, ASN: 1, Country: "US"}, base))
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.2.3.4", Family: 4, ASN: 1, Country: "NL"}, base.Add(time.Hour)))

	got, err := s.IPInfoFor(ctx, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if got.Country != "NL" {
		t.Errorf("Country = %q, want the updated NL", got.Country)
	}
	if !got.FetchedAt.Equal(base.Add(time.Hour)) {
		t.Errorf("FetchedAt was not refreshed: %v", got.FetchedAt)
	}
}

func TestIPInfoHolder(t *testing.T) {
	if got := (IPInfo{Org: "O", ASName: "A", NetworkName: "N"}).Holder(); got != "O" {
		t.Errorf("got %q, want the org", got)
	}
	if got := (IPInfo{ASName: "A", NetworkName: "N"}).Holder(); got != "A" {
		t.Errorf("got %q, want the AS name", got)
	}
	if got := (IPInfo{NetworkName: "N"}).Holder(); got != "N" {
		t.Errorf("got %q, want the network name", got)
	}
	if got := (IPInfo{}).Holder(); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// An address changing origin AS is a real event and belongs in the feed.
func TestPutIPInfoRecordsASNChange(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")
	apply(t, s, tr.ID, base, adds("1.2.3.4")...)

	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.2.3.4", Family: 4, ASN: 24940, ASName: "HETZNER-AS"}, base))
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.2.3.4", Family: 4, ASN: 13335, ASName: "CLOUDFLARENET"}, base.Add(time.Hour)))

	changes, err := s.ChangesFor(ctx, tr.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var found *Change
	for i := range changes {
		if changes[i].Type == ChangeASNChanged {
			found = &changes[i]
		}
	}
	if found == nil {
		t.Fatalf("no asn_changed event in %+v", changes)
	}
	if found.IP != "1.2.3.4" {
		t.Errorf("event IP = %q", found.IP)
	}
	for _, want := range []string{"AS24940", "HETZNER-AS", "AS13335", "CLOUDFLARENET"} {
		if !strings.Contains(found.Detail, want) {
			t.Errorf("detail %q is missing %q", found.Detail, want)
		}
	}
}

// The first observation is not a change, and neither is a failed re-lookup.
func TestPutIPInfoASNChangeEdgeCases(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")
	apply(t, s, tr.ID, base, adds("1.2.3.4")...)

	// First write: no event.
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.2.3.4", Family: 4, ASN: 100}, base))
	if n := countKind(t, s, tr.ID, ChangeASNChanged); n != 0 {
		t.Errorf("first write produced %d events, want 0", n)
	}

	// Same AS again: no event.
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.2.3.4", Family: 4, ASN: 100}, base.Add(time.Hour)))
	if n := countKind(t, s, tr.ID, ChangeASNChanged); n != 0 {
		t.Errorf("unchanged AS produced %d events, want 0", n)
	}

	// Lookup failed and reported no AS: not a move, so no event.
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.2.3.4", Family: 4, ASN: 0, Error: "servfail"}, base.Add(2*time.Hour)))
	if n := countKind(t, s, tr.ID, ChangeASNChanged); n != 0 {
		t.Errorf("a failed lookup produced %d events, want 0", n)
	}

	// Recovering from unknown to a real AS is also not a move.
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.2.3.4", Family: 4, ASN: 100}, base.Add(3*time.Hour)))
	if n := countKind(t, s, tr.ID, ChangeASNChanged); n != 0 {
		t.Errorf("recovery produced %d events, want 0", n)
	}
}

// An address shared by several trackers should notify all of them.
func TestPutIPInfoASNChangeFansOut(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	a := mustAdd(t, s, "a.example.com")
	b := mustAdd(t, s, "b.example.com")
	apply(t, s, a.ID, base, adds("1.2.3.4")...)
	apply(t, s, b.ID, base, adds("1.2.3.4")...)

	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.2.3.4", Family: 4, ASN: 1}, base))
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.2.3.4", Family: 4, ASN: 2}, base.Add(time.Hour)))

	if n := countKind(t, s, a.ID, ChangeASNChanged); n != 1 {
		t.Errorf("tracker a got %d events, want 1", n)
	}
	if n := countKind(t, s, b.ID, ChangeASNChanged); n != 1 {
		t.Errorf("tracker b got %d events, want 1", n)
	}
}

func TestIPsNeedingEnrichment(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")

	apply(t, s, tr.ID, base, adds("1.1.1.1")...)
	apply(t, s, tr.ID, base, adds("2.2.2.2")...)
	apply(t, s, tr.ID, base, adds("3.3.3.3")...)

	now := base.Add(48 * time.Hour)

	// Nothing enriched yet: all three are pending.
	pending, err := s.IPsNeedingEnrichment(ctx, 24*time.Hour, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("got %d pending, want 3", len(pending))
	}

	// Fresh data drops out; stale data stays in.
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.1.1.1", Family: 4, ASN: 1}, now.Add(-time.Hour)))
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "2.2.2.2", Family: 4, ASN: 2}, now.Add(-72*time.Hour)))

	pending, err = s.IPsNeedingEnrichment(ctx, 24*time.Hour, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range pending {
		got[p.IP] = true
	}
	if got["1.1.1.1"] {
		t.Error("freshly enriched address is still pending")
	}
	if !got["2.2.2.2"] || !got["3.3.3.3"] {
		t.Errorf("pending = %v, want the stale and the unseen address", got)
	}

	// A retired address is not worth looking up.
	must(t, s.ApplyPlan(ctx, tr.ID, Plan{Status: StatusOK, Actions: []Action{
		{IP: "3.3.3.3", Family: 4, Kind: ActionRemove},
	}}, now))
	pending, _ = s.IPsNeedingEnrichment(ctx, 24*time.Hour, now, 100)
	for _, p := range pending {
		if p.IP == "3.3.3.3" {
			t.Error("inactive address should not be pending")
		}
	}

	// The limit is honoured.
	limited, _ := s.IPsNeedingEnrichment(ctx, 0, now, 1)
	if len(limited) != 1 {
		t.Errorf("got %d with limit 1", len(limited))
	}
}

func TestNetworkSummaries(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	a := mustAdd(t, s, "a.example.com")
	b := mustAdd(t, s, "b.example.com")

	apply(t, s, a.ID, base, adds("1.1.1.1")...)
	apply(t, s, a.ID, base, adds("1.1.1.2")...)
	apply(t, s, b.ID, base, adds("2.2.2.2")...)

	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.1.1.1", Family: 4, ASN: 13335, Org: "Cloudflare", RIR: "arin", Country: "US"}, base))
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.1.1.2", Family: 4, ASN: 13335, Org: "Cloudflare", RIR: "arin", Country: "US"}, base))
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "2.2.2.2", Family: 4, ASN: 24940, Org: "Hetzner", RIR: "ripencc", Country: "DE"}, base))

	nets, err := s.TopNetworks(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 2 {
		t.Fatalf("got %d networks, want 2: %+v", len(nets), nets)
	}
	if nets[0].Key != "AS13335" || nets[0].IPs != 2 || nets[0].Trackers != 1 {
		t.Errorf("top network = %+v", nets[0])
	}
	if nets[0].Label != "Cloudflare" {
		t.Errorf("label = %q", nets[0].Label)
	}

	rirs, err := s.ByRIR(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rirs) != 2 {
		t.Errorf("got %d RIRs, want 2: %+v", len(rirs), rirs)
	}

	countries, err := s.ByCountry(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(countries) != 2 {
		t.Errorf("got %d countries, want 2: %+v", len(countries), countries)
	}

	cov, err := s.Coverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov.ActiveIPs != 3 || cov.Enriched != 3 || cov.WithASN != 3 {
		t.Errorf("coverage = %+v", cov)
	}
}

// A retired tracker keeps its addresses on record, so the rollups used to count
// names that no page will list: on the live registry that put 34 trackers into
// the country totals, 27 of them under US alone. Clicking a country to list its
// trackers made the gap visible, since the count and the list have to agree.
func TestNetworkSummariesCountOnlyListedTrackers(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	live := mustAdd(t, s, "live.example.com")
	gone := mustAdd(t, s, "gone.example.com")

	apply(t, s, live.ID, base, adds("1.1.1.1")...)
	apply(t, s, gone.ID, base, adds("1.1.1.2")...)
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.1.1.1", Family: 4, ASN: 13335, Org: "Cloudflare", RIR: "arin", Country: "US"}, base))
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.1.1.2", Family: 4, ASN: 13335, Org: "Cloudflare", RIR: "arin", Country: "US"}, base))

	must(t, s.RemoveTracker(ctx, "gone.example.com", false))

	listed, err := s.ListTrackerViews(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("got %d listed trackers, want 1", len(listed))
	}

	countries, err := s.ByCountry(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(countries) != 1 || countries[0].Key != "US" {
		t.Fatalf("countries = %+v, want one US row", countries)
	}
	if countries[0].Trackers != 1 {
		t.Errorf("US covers %d trackers, want 1: the retired name is still counted",
			countries[0].Trackers)
	}
	// The address itself is still on record and still enriched; it is the
	// tracker count that has to match what the list shows.
	if countries[0].IPs != 1 {
		t.Errorf("US covers %d addresses, want 1", countries[0].IPs)
	}

	nets, err := s.TopNetworks(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 1 || nets[0].Trackers != 1 {
		t.Errorf("networks = %+v, want AS13335 on 1 tracker", nets)
	}

	rirs, err := s.ByRIR(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rirs) != 1 || rirs[0].Trackers != 1 {
		t.Errorf("rirs = %+v, want arin on 1 tracker", rirs)
	}
}

func TestCoverageWithUnenrichedAddresses(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")
	apply(t, s, tr.ID, base, adds("1.1.1.1")...)
	apply(t, s, tr.ID, base, adds("2.2.2.2")...)

	// One enriched but with no AS determined.
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.1.1.1", Family: 4, Error: "no origin"}, base))

	cov, err := s.Coverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov.ActiveIPs != 2 || cov.Enriched != 1 || cov.WithASN != 0 {
		t.Errorf("coverage = %+v, want 2 active, 1 enriched, 0 with an AS", cov)
	}
}

func TestTrackerNetworks(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")
	apply(t, s, tr.ID, base, adds("1.1.1.1")...)
	apply(t, s, tr.ID, base, adds("1.1.1.2")...)
	apply(t, s, tr.ID, base, adds("2.2.2.2")...)

	// Two addresses in one AS, one in another: two distinct networks.
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.1.1.1", Family: 4, ASN: 13335, Org: "Cloudflare", RIR: "arin", Country: "US"}, base))
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.1.1.2", Family: 4, ASN: 13335, Org: "Cloudflare", RIR: "arin", Country: "US"}, base))
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "2.2.2.2", Family: 4, ASN: 24940, Org: "Hetzner", RIR: "ripencc", Country: "DE"}, base))

	nets, err := s.TrackerNetworks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	refs := nets[tr.ID]
	if len(refs) != 2 {
		t.Fatalf("got %d networks for the tracker, want 2 distinct: %+v", len(refs), refs)
	}
}

func TestListTrackerViewsIncludesNetworks(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "a.example.com")
	mustAdd(t, s, "bare.example.com")
	apply(t, s, tr.ID, base, adds("1.1.1.1")...)
	must(t, s.PutIPInfo(ctx, IPInfo{IP: "1.1.1.1", Family: 4, ASN: 13335, Org: "Cloudflare"}, base))

	views, err := s.ListTrackerViews(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		if v.Networks == nil {
			t.Errorf("%s: Networks is nil, want an empty slice for JSON", v.Name)
		}
		if v.Name == "a.example.com" {
			if len(v.Networks) != 1 || v.Networks[0].ASN != 13335 {
				t.Errorf("networks = %+v", v.Networks)
			}
		}
	}
}
