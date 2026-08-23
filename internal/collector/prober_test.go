package collector

import (
	"context"
	"slices"
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
	return prober.Result{State: prober.Unreachable, Reason: "timed out"}
}

func live() prober.Result {
	return prober.Result{State: prober.Live, RTT: 12 * time.Millisecond}
}

// notATracker is a server answering cleanly with something that is not a
// tracker reply, as opposed to the silence the fake defaults to.
func notATracker() prober.Result {
	return prober.Result{State: prober.Dead, Reason: "HTTP 400, HTML not a tracker reply"}
}

// probeFixture builds a tracker with the given endpoints and active addresses,
// ready for a probing pass.
type probeFixture struct {
	store   *store.Store
	tracker store.Tracker
	checker *fakeChecker
	prober  *Prober
	now     time.Time
	addrs   []string
}

// endpoint is one announce endpoint a fixture should offer.
type endpoint struct {
	scheme string
	port   int
}

func newProbeFixture(t *testing.T, name string, addrs []string, endpoints ...endpoint) *probeFixture {
	t.Helper()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	st := testStore(t)
	tr := addTracker(t, st, name)
	for _, ep := range endpoints {
		if _, err := st.AddEndpoint(t.Context(), tr.ID, ep.scheme, ep.port, "/announce", now); err != nil {
			t.Fatal(err)
		}
	}
	resolvesTo(t, st, tr.ID, now, addrs...)

	checker := &fakeChecker{results: map[string]prober.Result{}}
	f := &probeFixture{store: st, tracker: tr, checker: checker, now: now, addrs: addrs}
	f.prober = &Prober{
		Store:         st,
		Probe:         checker,
		Log:           discard(),
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

// resolvesTo swaps the tracker's addresses for a new set, the way a CDN handing
// out a fresh pool looks to a collection pass. Addresses in both sets stay.
func (f *probeFixture) resolvesTo(t *testing.T, addrs ...string) {
	t.Helper()
	fresh := map[string]bool{}
	for _, ip := range addrs {
		fresh[ip] = true
	}
	var actions []store.Action
	for _, ip := range f.addrs {
		if !fresh[ip] {
			actions = append(actions, store.Action{IP: ip, Family: store.Family(ip), Kind: store.ActionRemove})
		}
		delete(fresh, ip)
	}
	for _, ip := range addrs {
		if fresh[ip] {
			actions = append(actions, store.Action{IP: ip, Family: store.Family(ip), Kind: store.ActionAdd})
		}
	}
	err := f.store.ApplyPlan(t.Context(), f.tracker.ID,
		store.Plan{Status: store.StatusOK, Actions: actions}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	f.addrs = addrs
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
	return changeKinds(t, f.store, f.tracker.ID,
		store.ChangeTrackerUp, store.ChangeTrackerDown, store.ChangeTrackerPartial)
}

func TestProberEveryAddressAnswers(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4", "1.2.3.5"},
		endpoint{"udp", 6969})
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
		endpoint{"udp", 6969})
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
	if kinds := f.changeTypes(t); !slices.Equal(kinds, want) {
		t.Errorf("changes = %v, want %v (newest first)", kinds, want)
	}
}

// Endpoints on the same host disagree often enough to matter: udp works while
// the https vhost has been retired.
func TestProberEndpointsDisagree(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4"},
		endpoint{"udp", 6969}, endpoint{"https", 443})
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
		endpoint{"udp", 6969})
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
	if kinds := f.changeTypes(t); !slices.Equal(kinds, want) {
		t.Errorf("changes = %v, want %v (newest first)", kinds, want)
	}
}

// A single answer resets the count, so an endpoint that drops one packet every
// other round never accumulates its way to dead.
func TestProberRecoveryResetsMisses(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4"},
		endpoint{"udp", 6969})
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

// A name behind a CDN can hand out addresses never probed before every round.
// Counting misses per address alone, each round starts over at 1, and the name
// stays unmeasurable forever however long it has been failing.
func TestProberChurningAddressesStillReachAVerdict(t *testing.T) {
	f := newProbeFixture(t, "churner.example.com", []string{"1.2.3.4", "1.2.3.5"},
		endpoint{"udp", 6969})

	f.run(t)
	if got := f.reach(t); got != store.ReachUnknown {
		t.Fatalf("after one pass reach = %q, want unknown (one silent round is not proof)", got)
	}

	f.now = f.now.Add(time.Hour)
	f.resolvesTo(t, "5.6.7.8", "5.6.7.9")
	f.run(t)

	if got := f.reach(t); got != store.ReachDead {
		t.Errorf("reach = %q, want dead within the %d-round threshold", got, f.prober.MissThreshold)
	}
}

// The streak belongs to the endpoint, but only an endpoint answering nowhere
// has one. An address that answers keeps its neighbours' churn from reading as
// the whole name dying.
func TestProberChurnKeepsAnsweringEndpointPartial(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com",
		[]string{"1.2.3.4", "1.2.3.5", "1.2.3.6", "1.2.3.7"}, endpoint{"udp", 6969})
	f.checker.results["udp:6969 1.2.3.4"] = live()
	f.run(t)

	// 1.2.3.4 keeps answering and 1.2.3.5 keeps failing, while the last two are
	// replaced by addresses never probed before.
	for _, fresh := range [][]string{{"5.6.7.8", "5.6.7.9"}, {"5.6.8.0", "5.6.8.1"}} {
		f.now = f.now.Add(time.Hour)
		f.resolvesTo(t, append([]string{"1.2.3.4", "1.2.3.5"}, fresh...)...)
		f.run(t)

		if got := f.reach(t); got != store.ReachPartial {
			t.Fatalf("reach = %q, want partial: one address still answers", got)
		}
	}
}

// The 004430.xyz shape: an endpoint answering every other round with a clean
// HTTP 400 read live for six days at uptime 1.0, because the grace path
// treated a reply as though it were a dropped packet.
func TestProberNotATrackerReplyBreaksUptime(t *testing.T) {
	f := newProbeFixture(t, "004430.xyz", []string{"1.2.3.4"}, endpoint{"https", 443})
	from := f.now.Add(-time.Hour)

	for i := range 6 {
		f.checker.results["https:443 1.2.3.4"] = live()
		if i%2 == 1 {
			f.checker.results["https:443 1.2.3.4"] = notATracker()
		}
		f.run(t)
		f.now = f.now.Add(6 * time.Hour)
	}

	win, err := f.store.AvailabilityOver(context.Background(), from, f.now)
	if err != nil {
		t.Fatal(err)
	}
	a := win.Trackers[f.tracker.ID]
	if !a.Known() {
		t.Fatal("nothing measured over the window")
	}
	if a.Share() >= 1 {
		t.Errorf("uptime = %.2f over rounds that half failed, want less than 1", a.Share())
	}
	if a.Misses < 3 {
		t.Errorf("misses = %d, want the three failed rounds counted", a.Misses)
	}
}

// The other half of the split: a timeout may be the network, so it keeps its
// grace. The attempt is still counted, or a live interval that failed half the
// time reads exactly like a clean one.
func TestProberGracedFailureIsStillCounted(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4"},
		endpoint{"udp", 6969})
	f.checker.results["udp:6969 1.2.3.4"] = live()
	f.run(t)

	delete(f.checker.results, "udp:6969 1.2.3.4") // silence, not an answer
	f.now = f.now.Add(time.Hour)
	f.run(t)

	probes, err := f.store.ProbesFor(context.Background(), f.tracker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if probes[0].Result != store.ProbeLive {
		t.Fatalf("result = %q, want live: one timeout is not proof", probes[0].Result)
	}
	if probes[0].Misses != 1 {
		t.Errorf("misses = %d, want the graced round counted", probes[0].Misses)
	}
	if !probes[0].Since.Before(probes[0].CheckedAt) {
		t.Error("the live interval should have held across the miss")
	}
}

// Being throttled teaches us nothing, so a tracker known to be live stays
// live rather than sliding to unknown on the CDN's say-so.
func TestProberUnknownKeepsTheLastVerdict(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4"},
		endpoint{"https", 443})
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
	f := newProbeFixture(t, "tracker.example.com", nil, endpoint{"udp", 6969})

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
	f := newProbeFixture(t, "tracker.example.com", addrs, endpoint{"udp", 6969})
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
		endpoint{"udp", 6969}, endpoint{"https", 443})
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

// A pass that flips a verdict has to leave the previous stretch behind, or the
// tracker page can only ever show the present. Two passes with the checker
// switched off in between is the smallest way to see that.
func TestProberRecordsHistoryAcrossPasses(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4"},
		endpoint{"udp", 6969})
	f.checker.results["udp:6969 1.2.3.4"] = live()
	f.run(t)

	// Silence, twice, because one missed round is not yet death.
	delete(f.checker.results, "udp:6969 1.2.3.4")
	f.now = f.now.Add(6 * time.Hour)
	f.run(t)
	f.now = f.now.Add(6 * time.Hour)
	f.run(t)

	got, err := f.store.ProbeHistoryFor(context.Background(), f.tracker.ID,
		f.now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d closed intervals, want 1: %+v", len(got), got)
	}
	if got[0].Result != store.ProbeLive {
		t.Errorf("archived result = %q, want live", got[0].Result)
	}
	// The live stretch has to cover the whole time it was believed live,
	// including the round that missed but had not yet crossed the threshold.
	if want := 12 * time.Hour; got[0].Until.Sub(got[0].Since) != want {
		t.Errorf("live interval lasted %s, want %s", got[0].Until.Sub(got[0].Since), want)
	}
}

// Retention is what keeps the table bounded, and the probing loop is the only
// thing that runs often enough to apply it.
func TestProberPrunesHistoryPastRetention(t *testing.T) {
	f := newProbeFixture(t, "tracker.example.com", []string{"1.2.3.4"},
		endpoint{"udp", 6969})
	f.prober.Retention = 24 * time.Hour
	f.checker.results["udp:6969 1.2.3.4"] = live()
	f.run(t)

	// Go dead, closing the live interval, then jump past the retention window
	// and probe once more so the loop gets a chance to sweep.
	delete(f.checker.results, "udp:6969 1.2.3.4")
	f.now = f.now.Add(time.Hour)
	f.run(t)
	f.now = f.now.Add(time.Hour)
	f.run(t)

	before, err := f.store.ProbeHistoryFor(context.Background(), f.tracker.ID, f.now.AddDate(-1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("got %d closed intervals before the sweep, want 1", len(before))
	}

	f.now = f.now.Add(48 * time.Hour)
	f.run(t)

	after, err := f.store.ProbeHistoryFor(context.Background(), f.tracker.ID, f.now.AddDate(-1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, iv := range after {
		if iv.Until.Before(f.now.Add(-24 * time.Hour)) {
			t.Errorf("interval ending %s survived a 24h retention", iv.Until)
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
