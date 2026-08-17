package collector

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pawal/torrent-tracker/internal/prober"
	"github.com/pawal/torrent-tracker/internal/store"
)

// fakeChecker answers from a table keyed by "scheme:port ip", so a test can
// make one address of a name work and another fail. Anything not listed is
// treated as silent, which is the common case for a dead tracker.
//
// The mutex is not decoration: the real prober probes a tracker's endpoints and
// addresses concurrently, so several goroutines land here at once. results is
// written before a pass starts and only read during one, so it needs no guard;
// the call log does, and so does anything a test hooks in through before.
type fakeChecker struct {
	results map[string]prober.Result
	// before runs at the start of each probe, for tests that need to observe
	// how many probes overlap.
	before func(key string)

	mu    sync.Mutex
	calls []string
}

func (f *fakeChecker) Probe(_ context.Context, t prober.Target) prober.Result {
	key := t.Scheme + ":" + strconv.Itoa(t.Port) + " " + t.IP
	if f.before != nil {
		f.before(key)
	}

	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()

	if r, ok := f.results[key]; ok {
		return r
	}
	return prober.Result{State: prober.Dead, Reason: "timed out"}
}

func live() prober.Result {
	return prober.Result{State: prober.Live, RTT: 12 * time.Millisecond}
}

// probeFixture builds a tracker with the given endpoints and active addresses,
// ready for a probing pass.
type probeFixture struct {
	store   *store.Store
	tracker store.Tracker
	checker *fakeChecker
	prober  *Prober
	now     time.Time
}

func newProbeFixture(t *testing.T, name string, addrs []string, endpoints [][2]any) *probeFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	st, err := store.Open(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	tr, _, err := st.AddTracker(ctx, name, "test", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range endpoints {
		if _, err := st.AddEndpoint(ctx, tr.ID, ep[0].(string), ep[1].(int), "/announce", now); err != nil {
			t.Fatal(err)
		}
	}

	// Give the tracker its addresses the way a collection pass would.
	var actions []store.Action
	for _, ip := range addrs {
		actions = append(actions, store.Action{Kind: store.ActionAdd, IP: ip, Family: store.Family(ip)})
	}
	if err := st.ApplyPlan(ctx, tr.ID, store.Plan{Status: store.StatusOK, Actions: actions}, now); err != nil {
		t.Fatal(err)
	}

	checker := &fakeChecker{results: map[string]prober.Result{}}
	f := &probeFixture{store: st, tracker: tr, checker: checker, now: now}
	f.prober = &Prober{
		Store:         st,
		Probe:         checker,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		MissThreshold: 2,
		Now:           func() time.Time { return f.now },
	}
	return f
}

func (f *probeFixture) run(t *testing.T) ProbeSummary {
	t.Helper()
	sum, err := f.prober.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func (f *probeFixture) reach(t *testing.T) store.Reach {
	t.Helper()
	tr, err := f.store.TrackerByName(context.Background(), f.tracker.Name)
	if err != nil {
		t.Fatal(err)
	}
	return tr.Reach
}

func (f *probeFixture) changeTypes(t *testing.T) []string {
	t.Helper()
	changes, err := f.store.ChangesFor(context.Background(), f.tracker.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, c := range changes {
		switch c.Type {
		case store.ChangeTrackerUp, store.ChangeTrackerDown, store.ChangeTrackerPartial:
			kinds = append(kinds, c.Type)
		}
	}
	return kinds
}

func TestProberEveryAddressAnswers(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4", "1.2.3.5"},
		[][2]any{{"udp", 6969}})
	f.checker.results["udp:6969 1.2.3.4"] = live()
	f.checker.results["udp:6969 1.2.3.5"] = live()

	sum := f.run(t)
	if sum.Live != 1 || sum.Probes != 2 {
		t.Errorf("summary = %d live, %d probes; want 1 and 2", sum.Live, sum.Probes)
	}
	if got := f.reach(t); got != store.ReachLive {
		t.Errorf("reach = %q, want live", got)
	}
}

// The payoff for probing per address rather than per name: a stale record left
// in DNS shows up as partial instead of flapping between live and dead. It
// takes two passes, because one silent round is not yet proof.
func TestProberOneStaleAddressIsPartial(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4", "1.2.3.5"},
		[][2]any{{"udp", 6969}})
	f.checker.results["udp:6969 1.2.3.4"] = live()

	f.run(t)
	if got := f.reach(t); got != store.ReachLive {
		t.Fatalf("after one pass reach = %q, want live (the miss is unconfirmed)", got)
	}

	f.now = f.now.Add(time.Hour)
	f.run(t)
	if got := f.reach(t); got != store.ReachPartial {
		t.Errorf("reach = %q, want partial", got)
	}

	want := []string{store.ChangeTrackerPartial, store.ChangeTrackerUp}
	if kinds := f.changeTypes(t); len(kinds) != 2 || kinds[0] != want[0] || kinds[1] != want[1] {
		t.Errorf("changes = %v, want %v (newest first)", kinds, want)
	}
}

// Endpoints on the same host disagree often enough to matter: udp works while
// the https vhost has been retired.
func TestProberEndpointsDisagree(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4"},
		[][2]any{{"udp", 6969}, {"https", 443}})
	f.checker.results["udp:6969 1.2.3.4"] = live()

	f.run(t)
	f.now = f.now.Add(time.Hour)
	f.run(t)

	if got := f.reach(t); got != store.ReachPartial {
		t.Errorf("reach = %q, want partial", got)
	}

	probes, err := f.store.ProbesFor(context.Background(), f.tracker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 2 {
		t.Fatalf("got %d probes, want one per endpoint", len(probes))
	}
}

// One dropped packet is not death. The tracker stays live until it misses as
// often as the threshold allows, mirroring how addresses are retired.
func TestProberMissThreshold(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4"},
		[][2]any{{"udp", 6969}})
	f.checker.results["udp:6969 1.2.3.4"] = live()
	f.run(t)

	// First failure: still believed live, on probation.
	delete(f.checker.results, "udp:6969 1.2.3.4")
	f.now = f.now.Add(time.Hour)
	f.run(t)
	if got := f.reach(t); got != store.ReachLive {
		t.Fatalf("after one miss reach = %q, want live (threshold is 2)", got)
	}

	// Second consecutive failure crosses the threshold.
	f.now = f.now.Add(time.Hour)
	f.run(t)
	if got := f.reach(t); got != store.ReachDead {
		t.Fatalf("after two misses reach = %q, want dead", got)
	}

	want := []string{store.ChangeTrackerDown, store.ChangeTrackerUp}
	if kinds := f.changeTypes(t); len(kinds) != 2 || kinds[0] != want[0] || kinds[1] != want[1] {
		t.Errorf("changes = %v, want %v (newest first)", kinds, want)
	}
}

// A single answer resets the count, so an endpoint that drops one packet every
// other round never accumulates its way to dead.
func TestProberRecoveryResetsMisses(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4"},
		[][2]any{{"udp", 6969}})
	f.checker.results["udp:6969 1.2.3.4"] = live()
	f.run(t)

	for i := 0; i < 3; i++ {
		delete(f.checker.results, "udp:6969 1.2.3.4")
		f.now = f.now.Add(time.Hour)
		f.run(t)

		f.checker.results["udp:6969 1.2.3.4"] = live()
		f.now = f.now.Add(time.Hour)
		f.run(t)
	}

	if got := f.reach(t); got != store.ReachLive {
		t.Errorf("reach = %q, want live", got)
	}
	if kinds := f.changeTypes(t); len(kinds) != 1 {
		t.Errorf("changes = %v, want only the original tracker_up", kinds)
	}
}

// Being throttled teaches us nothing, so a tracker known to be live stays
// live rather than sliding to unknown on the CDN's say-so.
func TestProberUnknownKeepsTheLastVerdict(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4"},
		[][2]any{{"https", 443}})
	f.checker.results["https:443 1.2.3.4"] = live()
	f.run(t)

	f.checker.results["https:443 1.2.3.4"] = prober.Result{
		State: prober.Unknown, Reason: "throttled (HTTP 429)",
	}
	f.now = f.now.Add(time.Hour)
	f.run(t)

	if got := f.reach(t); got != store.ReachLive {
		t.Errorf("reach = %q, want live", got)
	}
	if kinds := f.changeTypes(t); len(kinds) != 1 {
		t.Errorf("changes = %v, want only the original tracker_up", kinds)
	}

	// The verdict is retained but visibly ageing.
	probes, err := f.store.ProbesFor(context.Background(), f.tracker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if probes[0].Reason != "throttled (HTTP 429)" {
		t.Errorf("reason = %q, want the throttling noted", probes[0].Reason)
	}
	if !probes[0].CheckedAt.After(probes[0].Since) {
		t.Error("checked_at should have moved past since, showing the verdict ageing")
	}
}

// A name that resolves to nothing has nothing to probe. Calling that dead
// would report a resolver outage as every tracker dying at once.
func TestProberNoAddressesIsUnknown(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", nil, [][2]any{{"udp", 6969}})

	sum := f.run(t)
	if sum.Probes != 0 {
		t.Errorf("made %d probes against a name with no addresses", sum.Probes)
	}
	if got := f.reach(t); got != store.ReachUnknown {
		t.Errorf("reach = %q, want unknown", got)
	}
	if kinds := f.changeTypes(t); len(kinds) != 0 {
		t.Errorf("changes = %v, want none", kinds)
	}
}

// Probing per address is what makes a stale record visible, but it multiplies
// the work per name; the fan-out is what keeps that from stretching a pass. The
// upper bound matters as much as the overlap: a name behind a CDN that sees all
// its addresses hit at once is a name that throttles us.
func TestProberFanoutOverlapsProbesUpToTheBound(t *testing.T) {
	addrs := []string{"1.2.3.4", "1.2.3.5", "1.2.3.6", "1.2.3.7", "1.2.3.8", "1.2.3.9"}
	f := newProbeFixture(t, "tracker.example.com", addrs, [][2]any{{"udp", 6969}})
	for _, ip := range addrs {
		f.checker.results["udp:6969 "+ip] = live()
	}
	f.prober.Fanout = 3

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	f.checker.before = func(string) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()

		// Hold the slot long enough for the rest to pile up behind it.
		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
	}

	if sum := f.run(t); sum.Probes != len(addrs) {
		t.Errorf("made %d probes, want %d", sum.Probes, len(addrs))
	}
	// Two rather than three: the assertion is that probes overlap, not that the
	// scheduler starts them promptly on a loaded machine.
	if peak < 2 {
		t.Errorf("peak probes in flight = %d, want them to overlap", peak)
	}
	if peak > f.prober.Fanout {
		t.Errorf("peak probes in flight = %d, want at most the fan-out of %d", peak, f.prober.Fanout)
	}
}

// Running the probes concurrently must not shuffle their verdicts. Each pair
// keeps its own result, or an endpoint inherits another address's reason and the
// change feed names the wrong thing as broken.
func TestProberFanoutKeepsVerdictsWithTheirTarget(t *testing.T) {
	addrs := []string{"1.2.3.4", "1.2.3.5", "1.2.3.6", "1.2.3.7"}
	f := newProbeFixture(t, "tracker.example.com", addrs,
		[][2]any{{"udp", 6969}, {"https", 443}})
	// Only udp, and only on half the addresses: eight probes, two answers.
	answering := map[string]bool{"udp:6969 1.2.3.5": true, "udp:6969 1.2.3.7": true}
	for key := range answering {
		f.checker.results[key] = live()
	}

	f.run(t)

	probes, err := f.store.ProbesFor(context.Background(), f.tracker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != len(addrs)*2 {
		t.Fatalf("got %d probes, want one per endpoint and address", len(probes))
	}

	endpoints, err := f.store.EndpointsFor(context.Background(), f.tracker.ID)
	if err != nil {
		t.Fatal(err)
	}
	scheme := map[int64]string{}
	for _, ep := range endpoints {
		scheme[ep.ID] = ep.Scheme + ":" + strconv.Itoa(ep.Port)
	}

	for _, p := range probes {
		key := scheme[p.EndpointID] + " " + p.IP
		// A first silent round is unknown rather than dead, so live is the only
		// verdict worth asserting on: it is the one that cannot be inherited.
		if got := p.Result == store.ProbeLive; got != answering[key] {
			t.Errorf("%s: live = %v, want %v (reason %q)", key, got, answering[key], p.Reason)
		}
	}
}

func TestSummarise(t *testing.T) {
	endpoints := []store.Endpoint{
		{ID: 1, Scheme: "udp", Port: 6969},
		{ID: 2, Scheme: "https", Port: 443},
	}
	now := time.Now().UTC()
	probes := []store.Probe{
		{EndpointID: 1, IP: "1.2.3.4", Result: store.ProbeLive, Since: now},
		{EndpointID: 2, IP: "1.2.3.4", Result: store.ProbeDead, Reason: "timed out", Since: now},
	}

	got := summarise(endpoints, probes)
	for _, want := range []string{"1 of 2 endpoints answer", "udp:6969 live", "https:443 dead (timed out)"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q is missing %q", got, want)
		}
	}
}

func TestSummariseNamesThePartialAddresses(t *testing.T) {
	endpoints := []store.Endpoint{{ID: 1, Scheme: "udp", Port: 6969}}
	now := time.Now().UTC()
	probes := []store.Probe{
		{EndpointID: 1, IP: "1.2.3.4", Result: store.ProbeLive, Since: now},
		{EndpointID: 1, IP: "1.2.3.5", Result: store.ProbeDead, Since: now},
		{EndpointID: 1, IP: "1.2.3.6", Result: store.ProbeLive, Since: now},
	}

	if got := summarise(endpoints, probes); !strings.Contains(got, "live on 2 of 3 addresses") {
		t.Errorf("summary = %q, want the address count spelled out", got)
	}
}
