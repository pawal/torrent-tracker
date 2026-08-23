package collector

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/pawal/torrent-tracker/internal/prober"
	"github.com/pawal/torrent-tracker/internal/resolver"
	"github.com/pawal/torrent-tracker/internal/store"
)

// trackerNamed reads a tracker back, for the assertions about what a pass
// stored on it.
func trackerNamed(t *testing.T, st *store.Store, name string) store.Tracker {
	t.Helper()
	got, err := st.TrackerByName(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// publishes scripts a name that resolves and answers the given TXT records.
func publishes(fake *fakeResolver, name string, txts ...string) {
	fake.set(name, resolver.TypeA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"1.2.3.4"},
	})
	fake.set(name, resolver.TypeTXT, resolver.Result{Status: store.StatusOK, TXT: txts})
}

func TestCollectRecordsPreferences(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := context.Background()
	addTracker(t, st, "tracker.example.com")
	publishes(fake, "tracker.example.com", "v=spf1 -all", "BITTORRENT UDP:6969 TCP:80")

	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	got := trackerNamed(t, st, "tracker.example.com")
	if got.BEP34 != "BITTORRENT UDP:6969 TCP:80" {
		t.Errorf("stored %q, want the record verbatim", got.BEP34)
	}
	if got.BEP34Denies {
		t.Error("a record naming two endpoints is not a denial")
	}

	// A UDP preference names an endpoint outright, so it can be adopted. A TCP
	// one names a port without saying whether it is HTTP or HTTPS, and a wrong
	// guess would record a dead endpoint for a live tracker.
	eps, err := st.EndpointsFor(ctx, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].Scheme != "udp" || eps[0].Port != 6969 {
		t.Fatalf("endpoints = %v, want only udp:6969", eps)
	}
	if eps[0].Path != "/announce" {
		t.Errorf("path = %q, want /announce", eps[0].Path)
	}
}

func TestCollectHonoursDenial(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := context.Background()
	tr := addTracker(t, st, "denies.example.com")
	if _, err := st.AddEndpoint(ctx, tr.ID, "udp", 6969, "/announce", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	eps, err := st.EndpointsFor(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Hour)
	if err := st.PutProbes(ctx, []int64{eps[0].ID}, []store.Probe{{
		EndpointID: eps[0].ID, IP: "1.2.3.4", Family: 4,
		Result: store.ProbeLive, Since: now, CheckedAt: now,
	}}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetReach(ctx, tr.ID, store.ReachLive, "", now); err != nil {
		t.Fatal(err)
	}

	publishes(fake, "denies.example.com", "BITTORRENT")
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	got := trackerNamed(t, st, "denies.example.com")
	if !got.BEP34Denies {
		t.Fatal("a bare BITTORRENT record did not register as a denial")
	}
	// We were told there is no tracker here, so we stop measuring one. Saying
	// "live" from a probe we will never repeat would be a lie by omission.
	if got.Reach != store.ReachUnknown {
		t.Errorf("reach = %q, want unknown once probing stops", got.Reach)
	}
	open, err := st.ProbesFor(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("%d probe verdicts still open, want none", len(open))
	}
	// The history is the point of the project: what was measured stays.
	closed, err := st.ProbeHistoryFor(ctx, tr.ID, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 {
		t.Errorf("history = %v, want the hour that was measured", closed)
	}
	// The name itself survives. Deleting on the strength of a DNS record we
	// might have misparsed would throw away years of history.
	if !got.Enabled {
		t.Error("the tracker was disabled; a denial is not a deletion")
	}
}

func TestCollectKeepsRecordWhenTheQueryFails(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := context.Background()
	tr := addTracker(t, st, "tracker.example.com")
	if _, err := st.SetBEP34(ctx, tr.ID, "BITTORRENT UDP:6969", false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	fake.set("tracker.example.com", resolver.TypeA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"1.2.3.4"},
	})
	fake.set("tracker.example.com", resolver.TypeTXT, resolver.Result{Status: store.StatusServFail})
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// The same rule that stops a SERVFAIL retiring an address: a query that
	// could not be answered is not an answer of "no record".
	if got := trackerNamed(t, st, "tracker.example.com"); got.BEP34 != "BITTORRENT UDP:6969" {
		t.Errorf("record = %q after a SERVFAIL, want it left alone", got.BEP34)
	}
}

func TestCollectClearsWithdrawnRecord(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := context.Background()
	tr := addTracker(t, st, "tracker.example.com")
	if _, err := st.SetBEP34(ctx, tr.ID, "BITTORRENT", true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// NOERROR with no BITTORRENT record is an authoritative "no preferences",
	// so the denial lifts and the tracker is fair game again.
	publishes(fake, "tracker.example.com", "v=spf1 -all")
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	got := trackerNamed(t, st, "tracker.example.com")
	if got.BEP34 != "" || got.BEP34Denies {
		t.Errorf("record = %q denies=%v, want both cleared", got.BEP34, got.BEP34Denies)
	}
	if countKind(t, st, tr.ID, store.ChangeBEP34Removed) != 1 {
		t.Error("no bep34_removed was recorded; withdrawing a record is news")
	}
}

// advertises stores the record a name publishes, as a collection pass would.
func (f *probeFixture) advertises(t *testing.T, record string) {
	t.Helper()
	if _, err := f.store.SetBEP34(t.Context(), f.tracker.ID, record, false, f.now); err != nil {
		t.Fatal(err)
	}
}

// probed reports whether a pass tried one endpoint on one address.
func (f *probeFixture) probed(key string) bool {
	f.checker.mu.Lock()
	defer f.checker.mu.Unlock()
	return slices.Contains(f.checker.calls, key)
}

// endpointsOf lists a tracker's endpoints as "scheme:port", retired ones
// marked, so a test can state the whole set in one line.
func endpointsOf(t *testing.T, st *store.Store, trackerID int64) []string {
	t.Helper()
	eps, err := st.EndpointsFor(t.Context(), trackerID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(eps))
	for _, ep := range eps {
		label := ep.Label()
		if ep.RetiredAt != nil {
			label += " (retired)"
		}
		out = append(out, label)
	}
	return out
}

// refused is a port that rejected the connection outright.
func refused() prober.Result {
	return prober.Result{State: prober.Unreachable, Reason: "connection refused"}
}

// A TCP preference names a port without naming a scheme, which is why they went
// unused. Trying both is not a guess.
func TestProberAdoptsAdvertisedTCPPort(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4"}, endpoint{"udp", 6969})
	f.advertises(t, "BITTORRENT UDP:6969 TCP:8080")
	f.checker.results["udp:6969 1.2.3.4"] = live()
	f.checker.results["https:8080 1.2.3.4"] = live()

	f.run(t)

	want := []string{"https:8080", "udp:6969"}
	if got := endpointsOf(t, f.store, f.tracker.ID); !slices.Equal(got, want) {
		t.Fatalf("endpoints = %v, want %v", got, want)
	}
	if !f.probed("http:8080 1.2.3.4") {
		t.Error("only one scheme was tried; the record does not say which it is")
	}

	// Adopted means probed from now on, like any other endpoint.
	f.now = f.now.Add(time.Hour)
	f.run(t)
	probes, err := f.store.ProbesFor(t.Context(), f.tracker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 2 {
		t.Errorf("got %d probes, want one per endpoint", len(probes))
	}
}

// Both schemes failing is an answer of its own: adopt neither.
func TestProberAdoptsNothingWhenNeitherSchemeAnswers(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4"}, endpoint{"udp", 6969})
	f.advertises(t, "BITTORRENT UDP:6969 TCP:80")
	f.checker.results["udp:6969 1.2.3.4"] = live()
	f.checker.results["http:80 1.2.3.4"] = notATracker()

	f.run(t)

	want := []string{"udp:6969"}
	if got := endpointsOf(t, f.store, f.tracker.ID); !slices.Equal(got, want) {
		t.Errorf("endpoints = %v, want %v: neither scheme answered", got, want)
	}
}

// A port that already answers is settled, and probing the other scheme every
// pass would only add load.
func TestProberLeavesASatisfiedPreferenceAlone(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4"}, endpoint{"https", 443})
	f.advertises(t, "BITTORRENT TCP:443")
	f.checker.results["https:443 1.2.3.4"] = live()

	f.run(t)

	if f.probed("http:443 1.2.3.4") {
		t.Error("the other scheme was probed although the port answers")
	}
}

// The atrack.pow7.com shape: an imported http:80 endpoint on a port the host
// advertises and refuses. One refused port read as half a tracker being down.
func TestProberRetiresARefusedPreference(t *testing.T) {
	f := newProbeFixture(t, "atrack.pow7.com", []string{"1.2.3.4"},
		endpoint{"udp", 6969}, endpoint{"http", 80})
	f.advertises(t, "BITTORRENT UDP:6969 TCP:80")
	f.checker.results["udp:6969 1.2.3.4"] = live()
	f.checker.results["http:80 1.2.3.4"] = refused()
	f.checker.results["https:80 1.2.3.4"] = refused()

	f.run(t)
	f.now = f.now.Add(time.Hour) // the second miss settles http:80 as dead
	f.run(t)

	want := []string{"http:80 (retired)", "udp:6969"}
	if got := endpointsOf(t, f.store, f.tracker.ID); !slices.Equal(got, want) {
		t.Fatalf("endpoints = %v, want %v", got, want)
	}
	if got := f.reach(t); got != store.ReachLive {
		t.Errorf("reach = %q, want live: what is left of the tracker answers", got)
	}
	history, err := f.store.ProbeHistoryFor(t.Context(), f.tracker.ID, f.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 {
		t.Error("retiring the endpoint threw its history away")
	}

	// The port is still advertised, so it is still tried, and an answer brings
	// the endpoint back.
	f.checker.results["http:80 1.2.3.4"] = live()
	f.now = f.now.Add(time.Hour)
	f.run(t)

	want = []string{"http:80", "udp:6969"}
	if got := endpointsOf(t, f.store, f.tracker.ID); !slices.Equal(got, want) {
		t.Errorf("endpoints = %v, want %v: the port answers again", got, want)
	}
}

// Retiring the only endpoint would take the name out of probing, and a name
// with nothing to probe reads unknown, which says less than dead.
func TestProberKeepsTheLastEndpoint(t *testing.T) {
	f := newProbeFixture(t, "parked.example.com", []string{"1.2.3.4"}, endpoint{"http", 80})
	f.advertises(t, "BITTORRENT TCP:80")
	f.checker.results["http:80 1.2.3.4"] = notATracker()

	f.run(t)

	want := []string{"http:80"}
	if got := endpointsOf(t, f.store, f.tracker.ID); !slices.Equal(got, want) {
		t.Fatalf("endpoints = %v, want %v", got, want)
	}
	if got := f.reach(t); got != store.ReachDead {
		t.Errorf("reach = %q, want dead", got)
	}
}

func TestCollectAdoptsAdvertisedEndpointOnce(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := context.Background()
	tr := addTracker(t, st, "tracker.example.com")
	publishes(fake, "tracker.example.com", "BITTORRENT UDP:6969")

	for range 3 {
		if _, err := c.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Adding an endpoint is idempotent, so a record that never changes must not
	// multiply the work every pass.
	eps, err := st.EndpointsFor(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 {
		t.Errorf("got %d endpoints after three passes, want 1", len(eps))
	}
	if n := countKind(t, st, tr.ID, store.ChangeBEP34Added); n != 1 {
		t.Errorf("recorded the record %d times, want once", n)
	}
}
