package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pawal/torrent-tracker/internal/prober"
)

func probeAt(endpointID int64, ip string, result ProbeResult, now time.Time) Probe {
	return Probe{
		EndpointID: endpointID, IP: ip, Family: Family(ip),
		Result: result, Since: now, CheckedAt: now,
	}
}

func TestRollUp(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name   string
		probes []Probe
		want   Reach
	}{
		{"nothing probed", nil, ReachUnknown},
		{"single answer", []Probe{probeAt(1, "1.2.3.4", ProbeLive, now)}, ReachLive},
		{"every address answers", []Probe{
			probeAt(1, "1.2.3.4", ProbeLive, now),
			probeAt(1, "1.2.3.5", ProbeLive, now),
		}, ReachLive},
		{"nothing answers", []Probe{
			probeAt(1, "1.2.3.4", ProbeDead, now),
			probeAt(2, "1.2.3.4", ProbeDead, now),
		}, ReachDead},
		// The case a per-hostname probe cannot see: one stale address left in
		// DNS while the rest of the name serves fine.
		{"one address of several is broken", []Probe{
			probeAt(1, "1.2.3.4", ProbeLive, now),
			probeAt(1, "1.2.3.5", ProbeDead, now),
		}, ReachPartial},
		// An unknown neither proves life nor death, so it must not drag a
		// working tracker down to partial.
		{"unknown abstains", []Probe{
			probeAt(1, "1.2.3.4", ProbeLive, now),
			probeAt(1, "1.2.3.5", ProbeUnknown, now),
		}, ReachLive},
		{"only unknowns", []Probe{probeAt(1, "1.2.3.4", ProbeUnknown, now)}, ReachUnknown},
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

// newTrackerWithEndpoint sets up the minimum a probe pass needs: a tracker
// with one announce endpoint.
func newTrackerWithEndpoint(t *testing.T, s *Store, name string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	tr, _, err := s.AddTracker(ctx, name, "test", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEndpoint(ctx, tr.ID, "udp", 6969, "/announce", now); err != nil {
		t.Fatal(err)
	}
	eps, err := s.EndpointsFor(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(eps))
	}
	return tr.ID, eps[0].ID
}

func TestAddEndpointIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
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

func TestPutProbesRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")

	want := []Probe{
		probeAt(endpointID, "1.2.3.4", ProbeLive, now),
		probeAt(endpointID, "1.2.3.5", ProbeDead, now),
	}
	want[1].Reason = "timed out"
	if err := s.PutProbes(ctx, []int64{endpointID}, want, now); err != nil {
		t.Fatal(err)
	}

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
	ctx := context.Background()
	now := time.Now().UTC()

	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")

	first := []Probe{
		probeAt(endpointID, "1.2.3.4", ProbeLive, now),
		probeAt(endpointID, "1.2.3.5", ProbeLive, now),
	}
	if err := s.PutProbes(ctx, []int64{endpointID}, first, now); err != nil {
		t.Fatal(err)
	}

	// The second address is gone from DNS, so this round only probes the first.
	second := []Probe{probeAt(endpointID, "1.2.3.4", ProbeLive, now.Add(time.Hour))}
	if err := s.PutProbes(ctx, []int64{endpointID}, second, now); err != nil {
		t.Fatal(err)
	}

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
	ctx := context.Background()
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

	changes, err := s.ChangesFor(ctx, trackerID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, c := range changes {
		if c.Type == ChangeTrackerUp || c.Type == ChangeTrackerDown || c.Type == ChangeTrackerPartial {
			kinds = append(kinds, c.Type)
		}
	}
	// Newest first: down, then the original up. The repeat in between wrote
	// nothing.
	want := []string{ChangeTrackerDown, ChangeTrackerUp}
	if len(kinds) != len(want) {
		t.Fatalf("reachability changes = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("change[%d] = %q, want %q", i, kinds[i], want[i])
		}
	}
}

// Going unknown is not a change worth recording: it means the probe could not
// be made, which says nothing about the tracker.
func TestSetReachIgnoresUnknown(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
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
	ctx := context.Background()
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
	ctx := context.Background()
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
		p := probeAt(endpointID, "1.2.3.4", ProbeLive, now)
		p.Signature = tc.sig
		if err := s.PutProbes(ctx, []int64{endpointID}, []Probe{p}, now); err != nil {
			t.Fatal(err)
		}
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
	if cov.Identified != 3 || cov.Trackers != 4 {
		t.Errorf("coverage = %d identified of %d, want 3 of 4", cov.Identified, cov.Trackers)
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
	ctx := context.Background()
	now := time.Now().UTC()

	put := func(name, sig string, kind prober.Kind) {
		t.Helper()
		_, endpointID := newTrackerWithEndpoint(t, s, name)
		p := probeAt(endpointID, "1.2.3.4", ProbeLive, now)
		p.Signature, p.Kind = sig, kind
		if err := s.PutProbes(ctx, []int64{endpointID}, []Probe{p}, now); err != nil {
			t.Fatal(err)
		}
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
	ctx := context.Background()
	now := time.Now().UTC()

	const sig = "complete,downloaded,incomplete,interval,min interval,peers"
	_, endpointID := newTrackerWithEndpoint(t, s, "legacy.example.com")
	p := probeAt(endpointID, "1.2.3.4", ProbeLive, now)
	p.Signature = sig
	if err := s.PutProbes(ctx, []int64{endpointID}, []Probe{p}, now); err != nil {
		t.Fatal(err)
	}

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
	ctx := context.Background()
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

// The probes table keeps only the open interval, so the moment a verdict is
// replaced the previous stretch has to land in probe_history or the tracker's
// past is gone. Since moving is the signal, exactly as merge produces it.
func TestPutProbesArchivesReplacedVerdicts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	t0 := time.Now().UTC().Add(-4 * time.Hour)
	t1 := t0.Add(2 * time.Hour)

	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")

	if err := s.PutProbes(ctx, []int64{endpointID},
		[]Probe{probeAt(endpointID, "1.2.3.4", ProbeLive, t0)}, t0); err != nil {
		t.Fatal(err)
	}
	dead := probeAt(endpointID, "1.2.3.4", ProbeDead, t1)
	dead.Reason = "connection refused"
	if err := s.PutProbes(ctx, []int64{endpointID}, []Probe{dead}, t1); err != nil {
		t.Fatal(err)
	}

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
	ctx := context.Background()
	start := time.Now().UTC().Add(-6 * time.Hour)

	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")

	for i := range 4 {
		p := probeAt(endpointID, "1.2.3.4", ProbeLive, start)
		p.CheckedAt = start.Add(time.Duration(i) * time.Hour)
		if err := s.PutProbes(ctx, []int64{endpointID}, []Probe{p}, p.CheckedAt); err != nil {
			t.Fatal(err)
		}
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
	ctx := context.Background()
	t0 := time.Now().UTC().Add(-3 * time.Hour)
	t1 := t0.Add(time.Hour)

	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")

	first := []Probe{
		probeAt(endpointID, "1.2.3.4", ProbeLive, t0),
		probeAt(endpointID, "1.2.3.5", ProbeLive, t0),
	}
	if err := s.PutProbes(ctx, []int64{endpointID}, first, t0); err != nil {
		t.Fatal(err)
	}
	survivor := probeAt(endpointID, "1.2.3.4", ProbeLive, t0)
	survivor.CheckedAt = t1
	if err := s.PutProbes(ctx, []int64{endpointID}, []Probe{survivor}, t1); err != nil {
		t.Fatal(err)
	}

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
	ctx := context.Background()
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
		if err := s.PutProbes(ctx, []int64{endpointID},
			[]Probe{probeAt(endpointID, "1.2.3.4", st.result, st.at)}, st.at); err != nil {
			t.Fatal(err)
		}
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
