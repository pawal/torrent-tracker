package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pawal/torrent-tracker/internal/resolver"
	"github.com/pawal/torrent-tracker/internal/store"
)

// fakeResolver serves scripted answers keyed by "name/TYPE" and counts calls.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string]resolver.Result
	calls   int
}

func newFake() *fakeResolver {
	return &fakeResolver{answers: map[string]resolver.Result{}}
}

func (f *fakeResolver) set(name string, rr resolver.RRType, res resolver.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[name+"/"+rr.String()] = res
}

func (f *fakeResolver) Lookup(_ context.Context, name string, rr resolver.RRType) resolver.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if res, ok := f.answers[name+"/"+rr.String()]; ok {
		return res
	}
	// Unscripted names simply have no records of that type.
	return resolver.Result{Status: store.StatusNoData}
}

func (f *fakeResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func testCollector(t *testing.T) (*Collector, *store.Store, *fakeResolver) {
	t.Helper()
	st := testStore(t)
	fake := newFake()
	c := &Collector{
		Store:         st,
		Resolver:      fake,
		Log:           discard(),
		Concurrency:   4,
		MissThreshold: 1,
	}
	return c, st, fake
}

func TestRunOnceRecordsAddresses(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := context.Background()

	tr := addTracker(t, st, "a.example.com")
	fake.set("a.example.com", resolver.TypeA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"1.2.3.4"},
	})
	fake.set("a.example.com", resolver.TypeAAAA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"2001:db8::1"},
	})

	sum, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Trackers != 1 || sum.OK != 1 || sum.Errors != 0 {
		t.Errorf("summary = %+v, want 1 tracker, 1 ok", sum)
	}

	records, err := st.ActiveRecords(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d active records, want 2: %+v", len(records), records)
	}

	got, _ := st.TrackerByName(ctx, "a.example.com")
	if got.LastStatus != store.StatusOK {
		t.Errorf("last_status = %q, want ok", got.LastStatus)
	}
	if got.LastCheckedAt == nil {
		t.Error("last_checked_at was not set")
	}
}

func TestRunOnceIsIdempotent(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := context.Background()

	tr := addTracker(t, st, "a.example.com")
	fake.set("a.example.com", resolver.TypeA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"1.2.3.4"},
	})

	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	first, _ := st.ChangesFor(ctx, tr.ID, 100)

	// A second identical pass must not manufacture changes.
	sum, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Changes != 0 {
		t.Errorf("second run reported %d changes, want 0", sum.Changes)
	}
	second, _ := st.ChangesFor(ctx, tr.ID, 100)
	if len(second) != len(first) {
		t.Errorf("change count grew from %d to %d on an unchanged run", len(first), len(second))
	}
}

// The whole point of the tool: an address change becomes history.
func TestRunOnceDetectsReaddressing(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := context.Background()

	tr := addTracker(t, st, "a.example.com")
	fake.set("a.example.com", resolver.TypeA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"1.2.3.4"},
	})
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// The tracker moves.
	fake.set("a.example.com", resolver.TypeA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"9.9.9.9"},
	})
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	active, _ := st.ActiveRecords(ctx, tr.ID)
	if len(active) != 1 || active[0].IP != "9.9.9.9" {
		t.Fatalf("active = %+v, want only 9.9.9.9", active)
	}
	all, _ := st.RecordsFor(ctx, tr.ID)
	if len(all) != 2 {
		t.Fatalf("got %d intervals, want 2 (the old address is history)", len(all))
	}

	added := countKind(t, st, tr.ID, store.ChangeIPAdded)
	removed := countKind(t, st, tr.ID, store.ChangeIPRemoved)
	if added != 2 || removed != 1 {
		t.Errorf("added=%d removed=%d, want 2 and 1", added, removed)
	}
}

// A resolver outage must not wipe the history.
func TestRunOnceSurvivesResolverOutage(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := context.Background()

	tr := addTracker(t, st, "a.example.com")
	fake.set("a.example.com", resolver.TypeA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"1.2.3.4"},
	})
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// Everything starts failing.
	fake.set("a.example.com", resolver.TypeA, resolver.Result{Status: store.StatusServFail, Err: "SERVFAIL"})
	fake.set("a.example.com", resolver.TypeAAAA, resolver.Result{Status: store.StatusServFail, Err: "SERVFAIL"})

	sum, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Errors != 1 {
		t.Errorf("errors = %d, want 1", sum.Errors)
	}

	active, _ := st.ActiveRecords(ctx, tr.ID)
	if len(active) != 1 || active[0].IP != "1.2.3.4" {
		t.Errorf("active = %+v, want the address preserved through the outage", active)
	}

	// And it recovers cleanly, without a spurious re-add.
	fake.set("a.example.com", resolver.TypeA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"1.2.3.4"},
	})
	fake.set("a.example.com", resolver.TypeAAAA, resolver.Result{Status: store.StatusNoData})
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	all, _ := st.RecordsFor(ctx, tr.ID)
	if len(all) != 1 {
		t.Errorf("got %d intervals, want 1 unbroken interval across the outage", len(all))
	}
}

func TestRunOnceNXDomain(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := context.Background()

	tr := addTracker(t, st, "dead.example.com")
	fake.set("dead.example.com", resolver.TypeA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"1.2.3.4"},
	})
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// The domain lapses.
	fake.set("dead.example.com", resolver.TypeA, resolver.Result{Status: store.StatusNXDomain})
	fake.set("dead.example.com", resolver.TypeAAAA, resolver.Result{Status: store.StatusNXDomain})
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	got, _ := st.TrackerByName(ctx, "dead.example.com")
	if got.LastStatus != store.StatusNXDomain {
		t.Errorf("last_status = %q, want nxdomain", got.LastStatus)
	}
	if active, _ := st.ActiveRecords(ctx, tr.ID); len(active) != 0 {
		t.Errorf("active = %+v, want none after NXDOMAIN", active)
	}
	if all, _ := st.RecordsFor(ctx, tr.ID); len(all) != 1 {
		t.Errorf("the historical interval should survive: %+v", all)
	}
}

func TestRunOnceSkipsDisabledTrackers(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := context.Background()

	addTracker(t, st, "a.example.com")
	addTracker(t, st, "b.example.com")
	if err := st.RemoveTracker(ctx, "b.example.com", false); err != nil {
		t.Fatal(err)
	}

	sum, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Trackers != 1 {
		t.Errorf("polled %d trackers, want 1 (the disabled one must be skipped)", sum.Trackers)
	}
	// A, AAAA and the BEP 34 TXT record, for the one enabled tracker.
	if got := fake.callCount(); got != 3 {
		t.Errorf("made %d lookups, want 3", got)
	}
}

func TestRunOnceRecordsRun(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := context.Background()

	addTracker(t, st, "a.example.com")
	addTracker(t, st, "b.example.com")
	fake.set("a.example.com", resolver.TypeA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"1.2.3.4"},
	})
	fake.set("b.example.com", resolver.TypeA, resolver.Result{Status: store.StatusServFail})
	fake.set("b.example.com", resolver.TypeAAAA, resolver.Result{Status: store.StatusServFail})

	sum, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}

	runs, err := st.RecentRuns(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	r := runs[0]
	if r.FinishedAt == nil {
		t.Error("run was not closed")
	}
	if r.TrackerCount != 2 || r.OKCount != 1 || r.ErrorCount != 1 {
		t.Errorf("run = %+v, want 2 trackers, 1 ok, 1 error", r)
	}
	if r.ChangeCount != sum.Changes {
		t.Errorf("run change_count = %d, summary said %d", r.ChangeCount, sum.Changes)
	}
}

func TestRunOnceEmptyRegistry(t *testing.T) {
	c, _, _ := testCollector(t)
	sum, err := c.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("an empty registry should not be an error: %v", err)
	}
	if sum.Trackers != 0 {
		t.Errorf("trackers = %d, want 0", sum.Trackers)
	}
}

func TestRunOnceConcurrent(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := context.Background()

	const n = 25
	for i := 0; i < n; i++ {
		name := string(rune('a'+i%26)) + "-many.example.com"
		addTracker(t, st, name)
		fake.set(name, resolver.TypeA, resolver.Result{
			Status: store.StatusOK, Addrs: []string{"1.2.3.4"},
		})
	}

	sum, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatalf("concurrent run failed (SQLite contention?): %v", err)
	}
	if sum.Trackers != n || sum.OK != n {
		t.Errorf("summary = %+v, want %d trackers all ok", sum, n)
	}

	views, _ := st.ListTrackerViews(ctx, false)
	for _, v := range views {
		if len(v.IPv4) != 1 {
			t.Errorf("%s: ipv4 = %v, want one address", v.Name, v.IPv4)
		}
	}
}

func TestRunOnceMissThreshold(t *testing.T) {
	c, st, fake := testCollector(t)
	c.MissThreshold = 3
	ctx := context.Background()

	tr := addTracker(t, st, "flappy.example.com")
	fake.set("flappy.example.com", resolver.TypeA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"1.1.1.1", "2.2.2.2"},
	})
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// 2.2.2.2 drops out of the rotation.
	fake.set("flappy.example.com", resolver.TypeA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"1.1.1.1"},
	})

	for i := 1; i <= 2; i++ {
		if _, err := c.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		active, _ := st.ActiveRecords(ctx, tr.ID)
		if len(active) != 2 {
			t.Fatalf("after %d misses got %d active, want 2 still held", i, len(active))
		}
	}

	// The third consecutive miss retires it.
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	active, _ := st.ActiveRecords(ctx, tr.ID)
	if len(active) != 1 || active[0].IP != "1.1.1.1" {
		t.Errorf("active = %+v, want only 1.1.1.1 after the threshold is reached", active)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	c, st, fake := testCollector(t)
	addTracker(t, st, "a.example.com")
	fake.set("a.example.com", resolver.TypeA, resolver.Result{
		Status: store.StatusOK, Addrs: []string{"1.2.3.4"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, time.Hour) }()

	// Give the immediate first pass a moment, then stop.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop when the context was cancelled")
	}

	// The initial pass should still have happened.
	if runs, _ := st.RecentRuns(context.Background(), 5); len(runs) == 0 {
		t.Error("Run did not collect before waiting for the first tick")
	}
}

func TestCollectorDefaults(t *testing.T) {
	c := &Collector{}
	if c.concurrency() != 8 {
		t.Errorf("default concurrency = %d, want 8", c.concurrency())
	}
	if c.log() == nil {
		t.Error("log() must never return nil")
	}
	if c.now().IsZero() {
		t.Error("now() must return a real time")
	}

	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return fixed }
	if !c.now().Equal(fixed) {
		t.Error("Now override was ignored")
	}
}

// --- parked names ------------------------------------------------------
//
// Expired tracker domains get bought up and pointed at a parking host, where
// they keep answering and so keep looking alive. A control name is one known
// never to have been a tracker, so whatever it resolves to is a parking
// answer, and any name resolving only to those is parked rather than alive.

func TestParkedDetectedByControlName(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := t.Context()

	canary := addTracker(t, st, "0123456789nonexistent.com")
	if err := st.SetControl(ctx, canary.Name, true); err != nil {
		t.Fatal(err)
	}
	fake.set(canary.Name, resolver.TypeA, ok("34.66.57.33"))

	parked := addTracker(t, st, "tracker.dead.example")
	fake.set(parked.Name, resolver.TypeA, ok("34.66.57.33"))

	alive := addTracker(t, st, "tracker.alive.example")
	fake.set(alive.Name, resolver.TypeA, ok("1.2.3.4"))

	// Sharing the parking host is only damning if that is all there is: a
	// tracker that also answers with an address of its own is still alive.
	mixed := addTracker(t, st, "tracker.mixed.example")
	fake.set(mixed.Name, resolver.TypeA, ok("34.66.57.33", "5.6.7.8"))

	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	list, err := st.ListParked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range list {
		got[tr.Name] = true
	}
	if !got[parked.Name] {
		t.Errorf("%s resolves only to the parking address but was not flagged", parked.Name)
	}
	for _, name := range []string{alive.Name, mixed.Name, canary.Name} {
		if got[name] {
			t.Errorf("%s should not be flagged as parked", name)
		}
	}
}

func TestControlNamesAreNotTrackers(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := t.Context()

	canary := addTracker(t, st, "0123456789nonexistent.com")
	if err := st.SetControl(ctx, canary.Name, true); err != nil {
		t.Fatal(err)
	}
	fake.set(canary.Name, resolver.TypeA, ok("34.66.57.33"))
	addTracker(t, st, "tracker.real.example")

	sum, err := c.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The control name is still resolved, it just is not counted or listed.
	if sum.Trackers != 1 {
		t.Errorf("run covered %d trackers, want 1 with the control excluded", sum.Trackers)
	}
	stats, err := st.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Trackers != 1 {
		t.Errorf("stats count %d trackers, want 1", stats.Trackers)
	}
}

func TestParkedFlagClearsWhenTheNameComesBack(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := t.Context()

	canary := addTracker(t, st, "canary.example")
	if err := st.SetControl(ctx, canary.Name, true); err != nil {
		t.Fatal(err)
	}
	fake.set(canary.Name, resolver.TypeA, ok("34.66.57.33"))

	tr := addTracker(t, st, "tracker.revived.example")
	fake.set(tr.Name, resolver.TypeA, ok("34.66.57.33"))
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	fake.set(tr.Name, resolver.TypeA, ok("9.9.9.9"))
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	list, err := st.ListParked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("still parked after answering with its own address: %+v", list)
	}
}

// The end-to-end path for rolling: three runs with a changed IPv6 set, then
// the individual addresses stop being written and a single prefix record
// stands in for them.
func TestRunOnceCollapsesRollingFamilyToPrefix(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := t.Context()
	c.RollAfter = 3

	tr := addTracker(t, st, "cdn.example")
	fake.set(tr.Name, resolver.TypeA, ok("65.9.46.42"))

	// A rolling family collapses onto the prefix enrichment found, so the
	// addresses have to be annotated before any of this can happen.
	edges := []string{
		"2600:9000:2094:1400::1", "2600:9000:2094:3c00::1",
		"2600:9000:2094:5c00::1", "2600:9000:2094:7600::1",
	}
	for _, ip := range edges {
		enrichedInto(t, st, ip, "2600:9000:2094::/48")
	}

	// The first three runs are still per-address: detection costs three runs
	// of churn before it can tell a roller from an ordinary address change.
	for i, ip := range edges[:3] {
		fake.set(tr.Name, resolver.TypeAAAA, ok(ip))
		if _, err := c.RunOnce(ctx); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	beforeCollapse, err := st.ChangesFor(ctx, tr.ID, 100)
	if err != nil {
		t.Fatal(err)
	}

	fake.set(tr.Name, resolver.TypeAAAA, ok(edges[3]))
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	records, err := st.ActiveRecords(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	var v6 []store.IPRecord
	for _, r := range records {
		if r.Family == 6 {
			v6 = append(v6, r)
		}
	}
	if len(v6) != 1 {
		t.Fatalf("%d active IPv6 records, want 1 prefix record: %+v", len(v6), v6)
	}
	if !v6[0].IsPrefix || v6[0].IP != "2600:9000:2094::/48" {
		t.Errorf("active IPv6 record = %+v, want the /48 marked as a prefix", v6[0])
	}

	// The IPv4 side never churned, so it is untouched by any of this.
	for _, r := range records {
		if r.Family == 4 && (r.IsPrefix || r.IP != "65.9.46.42") {
			t.Errorf("IPv4 record = %+v, want the address kept as-is", r)
		}
	}

	// The collapse itself closes the old address quietly: all it should add to
	// the feed is the prefix and the mode change, never an ip_removed.
	changes, err := st.ChangesFor(ctx, tr.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, ch := range changes[:len(changes)-len(beforeCollapse)] {
		counts[ch.Type]++
	}
	if counts[store.ChangeIPRemoved] != 0 {
		t.Errorf("the collapse emitted %d ip_removed entries, want none", counts[store.ChangeIPRemoved])
	}
	if counts[store.ChangePrefixAdded] != 1 || counts[store.ChangeIPsRolling] != 1 {
		t.Errorf("collapse change types = %v, want one prefix_added and one ips_rolling", counts)
	}

	// And once rolling, a fresh set inside the same prefix is not news at all.
	before := len(changes)
	fake.set(tr.Name, resolver.TypeAAAA, ok("2600:9000:2094:ff00::9"))
	if _, err := c.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if changes, err = st.ChangesFor(ctx, tr.ID, 100); err != nil {
		t.Fatal(err)
	}
	if len(changes) != before {
		t.Errorf("churn inside the prefix added %d change entries, want 0", len(changes)-before)
	}
}

// The case the whole feature exists for: a CDN hands out addresses that have
// never been seen before, so none of them is in ip_info when the pass runs.
// Only the addresses from earlier runs have been enriched, which is all the
// enrichment pass can ever know. Collapsing therefore cannot depend on looking
// the address up; it depends on the address falling inside a prefix already
// known from a sibling.
func TestRollingCollapsesWithoutEnrichingEveryAddress(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := t.Context()
	c.RollAfter = 3

	tr := addTracker(t, st, "cdn.example")
	fake.set(tr.Name, resolver.TypeA, ok("65.9.46.42"))

	enrichedInto(t, st, "2600:9000:2094:0::1", "2600:9000:2094::/48")

	for _, ip := range []string{
		"2600:9000:2094:1400::1", "2600:9000:2094:3c00::1",
		"2600:9000:2094:5c00::1", "2600:9000:2094:7600::1",
	} {
		fake.set(tr.Name, resolver.TypeAAAA, ok(ip))
		if _, err := c.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	records, err := st.ActiveRecords(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.Family == 6 {
			if !r.IsPrefix || r.IP != "2600:9000:2094::/48" {
				t.Errorf("active IPv6 record = %+v, want the /48 it sits inside", r)
			}
			return
		}
	}
	t.Errorf("no active IPv6 record at all: %+v", records)
}

// An address outside every known prefix must not be forced into one.
func TestRollingIgnoresUnrelatedPrefixes(t *testing.T) {
	c, st, fake := testCollector(t)
	ctx := t.Context()
	c.RollAfter = 2

	tr := addTracker(t, st, "elsewhere.example")
	enrichedInto(t, st, "2600:9000:2094:0::1", "2600:9000:2094::/48")

	for _, ip := range []string{"2001:db8:1::1", "2001:db8:2::1", "2001:db8:3::1"} {
		fake.set(tr.Name, resolver.TypeAAAA, ok(ip))
		if _, err := c.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	records, err := st.ActiveRecords(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.IsPrefix {
			t.Errorf("collapsed onto an unrelated prefix: %+v", r)
		}
	}
}
