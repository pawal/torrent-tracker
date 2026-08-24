package store

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/pawal/torrent-tracker/internal/prober"
)

func TestRollUp(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name   string
		probes []Probe
		want   Reach
	}{
		{"nothing probed", nil, ReachUnknown},
		{"single answer", []Probe{verdict("1.2.3.4", ProbeLive, now)}, ReachLive},
		{"every address answers", []Probe{
			verdict("1.2.3.4", ProbeLive, now),
			verdict("1.2.3.5", ProbeLive, now),
		}, ReachLive},
		{"nothing answers", []Probe{
			verdict("1.2.3.4", ProbeDead, now),
			verdict("1.2.3.5", ProbeDead, now),
		}, ReachDead},
		// The case a per-hostname probe cannot see: one stale address left in
		// DNS while the rest of the name serves fine.
		{"one address of several is broken", []Probe{
			verdict("1.2.3.4", ProbeLive, now),
			verdict("1.2.3.5", ProbeDead, now),
		}, ReachPartial},
		// An unknown neither proves life nor death, so it must not drag a
		// working tracker down to partial.
		{"unknown abstains", []Probe{
			verdict("1.2.3.4", ProbeLive, now),
			verdict("1.2.3.5", ProbeUnknown, now),
		}, ReachLive},
		{"only unknowns", []Probe{verdict("1.2.3.4", ProbeUnknown, now)}, ReachUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RollUp(tt.probes); got != tt.want {
				t.Errorf("RollUp() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReachChange(t *testing.T) {
	tests := []struct {
		name       string
		prev, next Reach
		want       string
	}{
		{"first measurement finds it alive", "", ReachLive, ChangeTrackerUp},
		{"first measurement finds it dead", "", ReachDead, ChangeTrackerDown},
		{"came back", ReachDead, ReachLive, ChangeTrackerUp},
		{"came back on some addresses", ReachDead, ReachPartial, ChangeTrackerUp},
		{"went away", ReachLive, ReachDead, ChangeTrackerDown},
		{"degraded", ReachLive, ReachPartial, ChangeTrackerPartial},
		{"recovered fully", ReachPartial, ReachLive, ChangeTrackerUp},
		{"partly gone, then wholly", ReachPartial, ReachDead, ChangeTrackerDown},
		{"no movement", ReachLive, ReachLive, ""},
		// Failing to probe says nothing, so it must never be reported as the
		// tracker changing. Same rule as a failed DNS query not retiring an
		// address.
		{"lost the ability to probe", ReachLive, ReachUnknown, ""},
		{"still cannot probe", ReachUnknown, ReachUnknown, ""},
		{"finally probed", ReachUnknown, ReachLive, ChangeTrackerUp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReachChange(tt.prev, tt.next); got != tt.want {
				t.Errorf("ReachChange(%q, %q) = %q, want %q", tt.prev, tt.next, got, tt.want)
			}
		})
	}
}

func TestAddEndpointIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	trackerID, _ := newTrackerWithEndpoint(t, s, "tracker.example.com")

	// Re-importing the same list must not multiply the endpoints.
	fresh, err := s.AddEndpoint(ctx, trackerID, "udp", 6969, "/announce", now)
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Error("re-adding the same endpoint reported it as new")
	}

	// A different port on the same host is a genuinely different endpoint.
	fresh, err = s.AddEndpoint(ctx, trackerID, "https", 443, "/announce", now)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Error("adding https:443 alongside udp:6969 reported it as already known")
	}

	eps, err := s.EndpointsFor(ctx, trackerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 {
		t.Errorf("got %d endpoints, want 2", len(eps))
	}
}

// A retired endpoint stops being probed and stops being offered to clients,
// but what was measured on it stays.
func TestRetireEndpoint(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()

	trackerID, endpointID := newListTracker(t, s, "tracker.example.com", base, "http", 80, "1.2.3.4")
	probeRun(t, s, endpointID, base, verdict("1.2.3.4", ProbeDead, base))

	must(t, s.RetireEndpoint(ctx, endpointID, base.Add(time.Hour)))

	targets, err := s.ProbeTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Errorf("still probing %d targets, want none: the only endpoint was retired", len(targets))
	}
	entries, err := s.ListEndpoints(ctx, base, base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("client list offers %v, want nothing", entries)
	}
	history, err := s.ProbeHistoryFor(ctx, trackerID, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Errorf("history = %v, want the interval that was measured", history)
	}

	// A later answer on an advertised port brings it back.
	fresh, err := s.AdoptEndpoint(ctx, trackerID, "http", 80, "/announce", base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Error("reviving a retired endpoint did not report as news")
	}
	eps, err := s.EndpointsFor(ctx, trackerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].RetiredAt != nil {
		t.Errorf("endpoints = %+v, want the one endpoint back in service", eps)
	}
}

func TestPutProbesRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")

	dead := verdict("1.2.3.5", ProbeDead, now)
	dead.Reason = "timed out"
	probeRun(t, s, endpointID, now, verdict("1.2.3.4", ProbeLive, now), dead)

	got, err := s.ProbesFor(ctx, trackerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d probes, want 2", len(got))
	}
	if got[0].Result != ProbeLive || got[1].Result != ProbeDead {
		t.Errorf("results = %q, %q; want live, dead", got[0].Result, got[1].Result)
	}
	if got[1].Reason != "timed out" {
		t.Errorf("reason = %q, want %q", got[1].Reason, "timed out")
	}
}

// An address that stops resolving has no present state to report, so its probe
// row goes away rather than lingering as a stale verdict.
func TestPutProbesPrunesAddressesNoLongerProbed(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")

	probeRun(t, s, endpointID, now,
		verdict("1.2.3.4", ProbeLive, now),
		verdict("1.2.3.5", ProbeLive, now))

	// The second address is gone from DNS, so this round only probes the first.
	probeRun(t, s, endpointID, now, verdict("1.2.3.4", ProbeLive, now.Add(time.Hour)))

	got, err := s.ProbesFor(ctx, trackerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d probes, want 1", len(got))
	}
	if got[0].IP != "1.2.3.4" {
		t.Errorf("surviving probe is for %s, want 1.2.3.4", got[0].IP)
	}
}

func TestSetReachAppendsChanges(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	trackerID, _ := newTrackerWithEndpoint(t, s, "tracker.example.com")

	prev, changed, err := s.SetReach(ctx, trackerID, ReachLive, "1 of 1 endpoints answer", now)
	if err != nil {
		t.Fatal(err)
	}
	if prev != "" || !changed {
		t.Errorf("first measurement: prev = %q, changed = %v; want \"\", true", prev, changed)
	}

	// Same verdict again is not news.
	if _, changed, err = s.SetReach(ctx, trackerID, ReachLive, "still fine", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	} else if changed {
		t.Error("an unchanged verdict reported a change")
	}

	if _, changed, err = s.SetReach(ctx, trackerID, ReachDead, "0 of 1 endpoints answer", now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	} else if !changed {
		t.Error("live to dead did not report a change")
	}

	// Newest first: down, then the original up. The repeat in between wrote
	// nothing.
	if got := reachKinds(t, s, trackerID); !slices.Equal(got, []string{ChangeTrackerDown, ChangeTrackerUp}) {
		t.Errorf("reachability changes = %v, want down then up", got)
	}
}

// reachKinds keeps only the reachability entries of a tracker's feed, newest
// first, which is what a probing test is ever asserting on.
func reachKinds(t *testing.T, s *Store, trackerID int64) []string {
	t.Helper()
	var out []string
	for _, kind := range changeKinds(t, s, trackerID) {
		switch kind {
		case ChangeTrackerUp, ChangeTrackerDown, ChangeTrackerPartial:
			out = append(out, kind)
		}
	}
	return out
}

// Going unknown is not a change worth recording: it means the probe could not
// be made, which says nothing about the tracker.
func TestSetReachIgnoresUnknown(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	trackerID, _ := newTrackerWithEndpoint(t, s, "tracker.example.com")
	if _, _, err := s.SetReach(ctx, trackerID, ReachLive, "up", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetReach(ctx, trackerID, ReachUnknown, "nothing to probe", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	changes, err := s.ChangesFor(ctx, trackerID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Type == ChangeTrackerDown {
			t.Error("failing to probe was recorded as the tracker going down")
		}
	}
}

func TestReachSummaryCountsNeverProbedAsUnknown(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	live, _ := newTrackerWithEndpoint(t, s, "live.example.com")
	newTrackerWithEndpoint(t, s, "unprobed.example.com")
	if _, _, err := s.SetReach(ctx, live, ReachLive, "up", now); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReachSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got[ReachLive] != 1 {
		t.Errorf("live = %d, want 1", got[ReachLive])
	}
	if got[ReachUnknown] != 1 {
		t.Errorf("unknown = %d, want 1 (the never-probed name)", got[ReachUnknown])
	}
}

func TestSoftwareStats(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	const opentracker = "no info_hash parameter supplied"
	for _, tc := range []struct {
		name string
		sig  string
	}{
		{"a.example.com", opentracker},
		{"b.example.com", opentracker},
		{"c.example.com", "missing info_hash"},
		{"udponly.example.com", ""}, // UDP discloses nothing
	} {
		_, endpointID := newTrackerWithEndpoint(t, s, tc.name)
		p := verdict("1.2.3.4", ProbeLive, now)
		p.Signature = tc.sig
		probeRun(t, s, endpointID, now, p)
	}

	got, err := s.SoftwareStats(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	// Most common first, and the unfingerprintable tracker is absent rather
	// than lumped into an empty bucket.
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	if got[0].Signature != opentracker || got[0].Trackers != 2 {
		t.Errorf("row 0 = %+v, want %q on 2 trackers", got[0], opentracker)
	}
	if got[1].Trackers != 1 {
		t.Errorf("row 1 = %+v, want 1 tracker", got[1])
	}

	cov, err := s.ProbeCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Fingerprinted != 3 || cov.Trackers != 4 {
		t.Errorf("coverage = %d fingerprinted of %d, want 3 of 4",
			cov.Fingerprinted, cov.Trackers)
	}
	// Only opentracker's wording can be put a name to; "missing info_hash"
	// groups replies that look alike and names nobody.
	if cov.Named != 2 {
		t.Errorf("named = %d, want 2", cov.Named)
	}
	if got[0].Name != "opentracker" || got[1].Name != "" {
		t.Errorf("names = %q and %q, want opentracker and none", got[0].Name, got[1].Name)
	}
}

// The live registry reads "identified" as any fingerprint at all, which is
// mostly the CDN in front of the tracker: the top signature is a failure text
// 24 trackers share, and Server is cloudflare on 88 live probes. Neither names
// software, and only a header the tracker itself wrote does.
func TestProbeCoverageSeparatesNamedSoftwareFromFingerprints(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	for _, tc := range []struct {
		name, sig, server string
		kind              prober.Kind
	}{
		{"named.example.com", "no info_hash parameter supplied", "cloudflare", prober.KindFailure},
		{"generic.example.com", "Your client forgot to send your torrent's info_hash", "cloudflare", prober.KindFailure},
		{"header.example.com", "files", "Ocelot 1.0", prober.KindShape},
	} {
		_, endpointID := newTrackerWithEndpoint(t, s, tc.name)
		p := verdict("1.2.3.4", ProbeLive, now)
		p.Signature, p.Kind, p.Server = tc.sig, tc.kind, tc.server
		probeRun(t, s, endpointID, now, p)
	}

	cov, err := s.ProbeCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Fingerprinted != 3 {
		t.Errorf("fingerprinted = %d, want 3", cov.Fingerprinted)
	}
	if cov.Named != 2 {
		t.Errorf("named = %d, want 2: the generic failure text names nobody", cov.Named)
	}
}

// The live registry had one implementation spread across ten rows, split by keys
// that follow the peers a tracker happens to have rather than the software it
// runs: "peers6" shows up only when there was an IPv6 peer to report, and one
// tracker answered with "peers6" and no "peers" at all. Grouping has to see past
// that, or the chart invents diversity and a tracker drifts between rows from
// one pass to the next without anything having changed.
func TestSoftwareStatsGroupsShapesPastConditionalKeys(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	put := func(name, sig string, kind prober.Kind) {
		t.Helper()
		_, endpointID := newTrackerWithEndpoint(t, s, name)
		p := verdict("1.2.3.4", ProbeLive, now)
		p.Signature, p.Kind = sig, kind
		probeRun(t, s, endpointID, now, p)
	}

	// Verbatim from tracker.evilbit.de, where each of these was its own row.
	shapes := []string{
		"complete,downloaded,incomplete,interval,min interval,peers",
		"complete,downloaded,incomplete,interval,min interval,peers,peers6",
		"complete,incomplete,interval,min interval,peers",
		"complete,incomplete,interval,peers,peers6",
		"complete,downloaded,incomplete,interval,min interval,peers6",
		"complete,external ip,incomplete,interval,peers,peers6",
		"complete,incomplete,interval,min interval,peers,warning message",
		"complete,incomplete,interval,peers",
	}
	for i, sig := range shapes {
		put(fmt.Sprintf("shape%d.example.com", i), sig, prober.KindShape)
	}
	// A genuinely different shape, and a failure text, which is a literal from
	// somebody's source and must never be folded into anything.
	put("sparse.example.com", "interval,peers", prober.KindShape)
	put("literal.example.com", "no info_hash parameter supplied", prober.KindFailure)

	got, err := s.SoftwareStats(ctx, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(got), got)
	}
	if got[0].Signature != "complete,incomplete,interval" || got[0].Trackers != len(shapes) {
		t.Errorf("row 0 = %q on %d trackers, want the announce shape on %d",
			got[0].Signature, got[0].Trackers, len(shapes))
	}
	// The raw signatures are kept so the fold can be inspected rather than taken
	// on trust.
	if len(got[0].Variants) != len(shapes) {
		t.Errorf("row 0 lists %d variants, want all %d that were folded in: %v",
			len(got[0].Variants), len(shapes), got[0].Variants)
	}
	rest := map[string]prober.Kind{got[1].Signature: got[1].Kind, got[2].Signature: got[2].Kind}
	if rest["interval"] != prober.KindShape {
		t.Errorf("rows = %+v, want the sparse shape kept apart as \"interval\"", got)
	}
	if rest["no info_hash parameter supplied"] != prober.KindFailure {
		t.Errorf("rows = %+v, want the failure text untouched", got)
	}
}

// Rows probed before the kind column existed have no kind, so there is nothing
// to say whether their signature is a literal or a shape. They group by the raw
// signature, exactly as they did before, until the next pass rewrites them.
func TestSoftwareStatsLeavesUnclassifiedRowsAlone(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	const sig = "complete,downloaded,incomplete,interval,min interval,peers"
	_, endpointID := newTrackerWithEndpoint(t, s, "legacy.example.com")
	p := verdict("1.2.3.4", ProbeLive, now)
	p.Signature = sig
	probeRun(t, s, endpointID, now, p)

	got, err := s.SoftwareStats(ctx, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != sig {
		t.Errorf("stats = %+v, want the raw signature %q", got, sig)
	}
	if got[0].Variants != nil {
		t.Errorf("variants = %v, want none: nothing was folded", got[0].Variants)
	}
}

// Trackers added as bare hostnames have nothing to speak to and must not be
// mistaken for silent ones.
func TestProbeTargetsSkipsTrackersWithoutEndpoints(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	newTrackerWithEndpoint(t, s, "withendpoint.example.com")
	if _, _, err := s.AddTracker(ctx, "bare.example.com", "test", now); err != nil {
		t.Fatal(err)
	}

	targets, err := s.ProbeTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(targets))
	}
	if targets[0].Tracker.Name != "withendpoint.example.com" {
		t.Errorf("target = %q, want withendpoint.example.com", targets[0].Tracker.Name)
	}

	cov, err := s.ProbeCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Trackers != 2 || cov.WithEndpoints != 1 {
		t.Errorf("coverage = %d trackers, %d with endpoints; want 2 and 1",
			cov.Trackers, cov.WithEndpoints)
	}
}

// Missing an endpoint and never having resolved are separate reasons to read
// unknown, and the second one no endpoint can fix.
func TestProbeCoverageCountsNamesThatNeverResolved(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	trackerID, _ := newTrackerWithEndpoint(t, s, "resolves.example.com")
	must(t, s.ApplyPlan(ctx, trackerID, Plan{Status: StatusOK, Actions: adds("1.2.3.4")}, now))
	newTrackerWithEndpoint(t, s, "noaddress.example.com")
	if _, _, err := s.AddTracker(ctx, "bare.example.com", "test", now); err != nil {
		t.Fatal(err)
	}

	cov, err := s.ProbeCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov.NeverResolved != 2 {
		t.Errorf("never_resolved = %d, want 2", cov.NeverResolved)
	}
}

// The probes table keeps only the open interval, so the moment a verdict is
// replaced the previous stretch has to land in probe_history or the tracker's
// past is gone. Since moving is the signal, exactly as merge produces it.
func TestPutProbesArchivesReplacedVerdicts(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	t0 := time.Now().UTC().Add(-4 * time.Hour)
	t1 := t0.Add(2 * time.Hour)

	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")

	probeRun(t, s, endpointID, t0, verdict("1.2.3.4", ProbeLive, t0))
	dead := verdict("1.2.3.4", ProbeDead, t1)
	dead.Reason = "connection refused"
	probeRun(t, s, endpointID, t1, dead)

	got, err := s.ProbeHistoryFor(ctx, trackerID, t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d closed intervals, want 1", len(got))
	}
	if got[0].Result != ProbeLive {
		t.Errorf("archived result = %q, want live", got[0].Result)
	}
	// The closed interval must abut the open one: the UI draws them as one
	// unbroken axis, and any gap would read as "not probed".
	if !got[0].Since.Equal(t0) || !got[0].Until.Equal(t1) {
		t.Errorf("interval = %s → %s, want %s → %s", got[0].Since, got[0].Until, t0, t1)
	}

	open, err := s.ProbesFor(ctx, trackerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Result != ProbeDead || !open[0].Since.Equal(t1) {
		t.Errorf("open probe = %+v, want dead since %s", open, t1)
	}
}

// A tracker that answers for a month is one interval, not a row per round.
// merge holds Since still while the verdict stands, so nothing is archived.
func TestPutProbesArchivesNothingWhileVerdictStands(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	start := time.Now().UTC().Add(-6 * time.Hour)

	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")

	for i := range 4 {
		p := verdict("1.2.3.4", ProbeLive, start)
		p.CheckedAt = start.Add(time.Duration(i) * time.Hour)
		probeRun(t, s, endpointID, p.CheckedAt, p)
	}

	got, err := s.ProbeHistoryFor(ctx, trackerID, start.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d closed intervals, want none", len(got))
	}
}

// An address dropped from DNS loses its probe row, so its interval has to be
// closed at the pass that dropped it rather than left dangling.
func TestPutProbesClosesHistoryForDroppedAddress(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	t0 := time.Now().UTC().Add(-3 * time.Hour)
	t1 := t0.Add(time.Hour)

	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")

	probeRun(t, s, endpointID, t0,
		verdict("1.2.3.4", ProbeLive, t0),
		verdict("1.2.3.5", ProbeLive, t0))
	survivor := verdict("1.2.3.4", ProbeLive, t0)
	survivor.CheckedAt = t1
	probeRun(t, s, endpointID, t1, survivor)

	got, err := s.ProbeHistoryFor(ctx, trackerID, t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d closed intervals, want 1", len(got))
	}
	if got[0].IP != "1.2.3.5" || !got[0].Until.Equal(t1) {
		t.Errorf("closed %s until %s, want 1.2.3.5 until %s", got[0].IP, got[0].Until, t1)
	}
}

// The window is what the page draws, so an interval that ended before it must
// not be shipped, and retention must be able to delete it outright.
func TestProbeHistoryWindowAndPruning(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()

	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")

	// Four verdicts, so three intervals close: one entirely older than a
	// month, one straddling the left edge, and one inside the window.
	steps := []struct {
		result ProbeResult
		at     time.Time
	}{
		{ProbeLive, now.AddDate(0, 0, -100)},
		{ProbeDead, now.AddDate(0, 0, -90)},
		{ProbeLive, now.AddDate(0, 0, -1)},
		{ProbeDead, now},
	}
	for _, st := range steps {
		probeRun(t, s, endpointID, st.at, verdict("1.2.3.4", st.result, st.at))
	}

	month, err := s.ProbeHistoryFor(ctx, trackerID, now.AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	// The dead stretch began 90 days ago but only ended yesterday, so it is
	// inside the month even though its start is not.
	if len(month) != 2 {
		t.Fatalf("got %d intervals in a month window, want 2", len(month))
	}
	if month[0].Result != ProbeDead || month[1].Result != ProbeLive {
		t.Errorf("window results = %q, %q; want dead, live", month[0].Result, month[1].Result)
	}

	deleted, err := s.PruneProbeHistory(ctx, now.AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("pruned %d rows, want 1", deleted)
	}
	left, err := s.ProbeHistoryFor(ctx, trackerID, now.AddDate(0, 0, -365))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Errorf("%d intervals survived the prune, want 2", len(left))
	}
}
