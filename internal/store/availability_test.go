package store

import (
	"testing"
	"time"
)

// verdict is one probe result whose Since can predate the pass that recorded
// it, which is how a verdict that has stood for a while looks on disk.
func verdict(endpointID int64, ip string, result ProbeResult, since, checked time.Time) Probe {
	return Probe{
		EndpointID: endpointID, IP: ip, Family: Family(ip),
		Result: result, Since: since, CheckedAt: checked,
	}
}

// probeRun records one probing pass over an endpoint. Whatever a verdict
// replaces is archived, so successive calls with a moving Since build the same
// closed intervals a real pass would leave behind.
func probeRun(t *testing.T, s *Store, endpointID int64, at time.Time, probes ...Probe) {
	t.Helper()
	if err := s.PutProbes(t.Context(), []int64{endpointID}, probes, at); err != nil {
		t.Fatalf("PutProbes: %v", err)
	}
}

func TestMergeAndCover(t *testing.T) {
	h := func(n int) time.Time { return base.Add(time.Duration(n) * time.Hour) }
	tests := []struct {
		name string
		ivs  []interval
		want time.Duration
	}{
		{"nothing measured", nil, 0},
		{"one stretch", []interval{{h(0), h(2)}}, 2 * time.Hour},
		{"disjoint stretches add up", []interval{{h(0), h(1)}, {h(3), h(4)}}, 2 * time.Hour},
		// Two addresses answering at the same time are one hour of uptime, not
		// two. This is the whole difference between a union and a sum, and it
		// is why a name with four addresses cannot score 400%.
		{"overlap counts once", []interval{{h(0), h(2)}, {h(1), h(3)}}, 3 * time.Hour},
		{"nested counts once", []interval{{h(0), h(4)}, {h(1), h(2)}}, 4 * time.Hour},
		{"abutting stretches merge", []interval{{h(0), h(1)}, {h(1), h(2)}}, 2 * time.Hour},
		{"input order does not matter", []interval{{h(3), h(4)}, {h(0), h(1)}}, 2 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cover(merge(tt.ivs)); got != tt.want {
				t.Errorf("cover(merge()) = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAvailabilityOverUnionsAddresses(t *testing.T) {
	s := testStore(t)
	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")
	end := base.Add(24 * time.Hour)

	// One address answers all day while another never does. A client needs one
	// of them to work, not both, so this is a full day of uptime rather than
	// the half an average over the two lanes would report.
	probeRun(t, s, endpointID, base,
		verdict(endpointID, "1.2.3.4", ProbeLive, base, base),
		verdict(endpointID, "1.2.3.5", ProbeDead, base, base))

	win, err := s.AvailabilityOver(t.Context(), base.Add(-time.Hour), end)
	if err != nil {
		t.Fatal(err)
	}
	got := win.Trackers[trackerID]
	if got.Measured != 24*time.Hour {
		t.Errorf("measured = %v, want 24h", got.Measured)
	}
	if got.Share() != 1 {
		t.Errorf("share = %v, want 1", got.Share())
	}
}

func TestAvailabilityOverAbstainsFromUnknown(t *testing.T) {
	s := testStore(t)
	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")
	half := base.Add(12 * time.Hour)

	// Twelve hours answering, then twelve nobody could measure. The unmeasured
	// half must not read as downtime: the same abstention rule that keeps a
	// failed probe from retiring a tracker.
	probeRun(t, s, endpointID, base, verdict(endpointID, "1.2.3.4", ProbeLive, base, base))
	probeRun(t, s, endpointID, half, verdict(endpointID, "1.2.3.4", ProbeUnknown, half, half))

	win, err := s.AvailabilityOver(t.Context(), base, base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := win.Trackers[trackerID]
	if got.Measured != 12*time.Hour {
		t.Errorf("measured = %v, want 12h: unknown time is not measured time", got.Measured)
	}
	if got.Share() != 1 {
		t.Errorf("share = %v, want 1", got.Share())
	}
}

func TestAvailabilityOverIgnoresNeverProbed(t *testing.T) {
	s := testStore(t)
	trackerID, _ := newTrackerWithEndpoint(t, s, "tracker.example.com")

	// A name nothing has ever spoken to has no uptime at all, which is not the
	// same as an uptime of zero: absent from the map, so a caller can tell.
	win, err := s.AvailabilityOver(t.Context(), base, base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := win.Trackers[trackerID]; ok {
		t.Errorf("unprobed tracker has availability %+v, want none", a)
	}
	if (Availability{}).Known() {
		t.Error("an empty Availability reports itself as measured")
	}
}

func TestAvailabilityOverClipsToWindow(t *testing.T) {
	s := testStore(t)
	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")
	old, went := base.Add(-48*time.Hour), base.Add(-12*time.Hour)

	// Up for a day and a half, then down for the last twelve hours. Only what
	// the 24-hour window covers counts, so the long healthy stretch before it
	// cannot prop up the score.
	probeRun(t, s, endpointID, old, verdict(endpointID, "1.2.3.4", ProbeLive, old, old))
	probeRun(t, s, endpointID, went, verdict(endpointID, "1.2.3.4", ProbeDead, went, went))

	win, err := s.AvailabilityOver(t.Context(), base.Add(-24*time.Hour), base)
	if err != nil {
		t.Fatal(err)
	}
	got := win.Trackers[trackerID]
	if got.Measured != 24*time.Hour || got.Live != 12*time.Hour {
		t.Errorf("got %v of %v, want 12h of 24h", got.Live, got.Measured)
	}
	if got.Share() != 0.5 {
		t.Errorf("share = %v, want 0.5", got.Share())
	}
}

func TestAvailabilityOverSeparatesEndpoints(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	trackerID, udpID := newTrackerWithEndpoint(t, s, "tracker.example.com")
	if _, err := s.AddEndpoint(ctx, trackerID, "https", 443, "/announce", base); err != nil {
		t.Fatal(err)
	}
	eps, err := s.EndpointsFor(ctx, trackerID)
	if err != nil {
		t.Fatal(err)
	}
	var httpsID int64
	for _, e := range eps {
		if e.Scheme == "https" {
			httpsID = e.ID
		}
	}

	// udp:6969 answers and https:443 does not. The name is up either way, but
	// the per-scheme lists need the endpoints kept apart.
	probeRun(t, s, udpID, base, verdict(udpID, "1.2.3.4", ProbeLive, base, base))
	probeRun(t, s, httpsID, base, verdict(httpsID, "1.2.3.4", ProbeDead, base, base))

	win, err := s.AvailabilityOver(ctx, base, base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := win.Trackers[trackerID].Share(); got != 1 {
		t.Errorf("tracker share = %v, want 1: one working endpoint is enough", got)
	}
	if got := win.Endpoints[udpID].Share(); got != 1 {
		t.Errorf("udp share = %v, want 1", got)
	}
	dead := win.Endpoints[httpsID]
	if dead.Share() != 0 || !dead.Known() {
		t.Errorf("https = %+v, want a measured 0", dead)
	}
}

func TestAvailabilityOverSkipsDisabledAndControl(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	liveID, liveEndpoint := newTrackerWithEndpoint(t, s, "live.example.com")
	goneID, goneEndpoint := newTrackerWithEndpoint(t, s, "gone.example.com")
	canaryID, canaryEndpoint := newTrackerWithEndpoint(t, s, "canary.example.com")

	for _, e := range []int64{liveEndpoint, goneEndpoint, canaryEndpoint} {
		probeRun(t, s, e, base, verdict(e, "1.2.3.4", ProbeLive, base, base))
	}
	// Removing keeps the history, so only the query's filter stands between a
	// disabled name and a list a client would paste.
	if err := s.RemoveTracker(ctx, "gone.example.com", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetControl(ctx, "canary.example.com", true); err != nil {
		t.Fatal(err)
	}

	win, err := s.AvailabilityOver(ctx, base, base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := win.Trackers[liveID]; !ok {
		t.Error("the enabled tracker is missing")
	}
	if _, ok := win.Trackers[goneID]; ok {
		t.Error("a disabled tracker has uptime")
	}
	if _, ok := win.Trackers[canaryID]; ok {
		t.Error("a control name has uptime; it is not a tracker")
	}
}

func TestAvailabilityOverEmptyWindow(t *testing.T) {
	s := testStore(t)
	_, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")
	probeRun(t, s, endpointID, base, verdict(endpointID, "1.2.3.4", ProbeLive, base, base))

	// A window that ends before it starts divides by nothing rather than
	// panicking or reporting the whole history.
	win, err := s.AvailabilityOver(t.Context(), base, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(win.Trackers) != 0 {
		t.Errorf("got %d trackers over an empty window, want 0", len(win.Trackers))
	}
}

func TestAvailabilityOverDatesTheCurrentStretch(t *testing.T) {
	s := testStore(t)
	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")
	swap, end := base.Add(5*time.Hour), base.Add(10*time.Hour)

	// One address answers for five hours and stops; the other takes over at the
	// same moment. The tracker never stopped answering, so the stretch runs back
	// to the first address — which is what the earliest live probe's own Since
	// would get wrong, reporting five hours instead of ten.
	probeRun(t, s, endpointID, base,
		verdict(endpointID, "1.2.3.4", ProbeLive, base, base),
		verdict(endpointID, "1.2.3.5", ProbeDead, base, base))
	probeRun(t, s, endpointID, swap,
		verdict(endpointID, "1.2.3.4", ProbeDead, swap, swap),
		verdict(endpointID, "1.2.3.5", ProbeLive, swap, swap))

	got, err := s.AvailabilityOver(t.Context(), base.Add(-time.Hour), end)
	if err != nil {
		t.Fatal(err)
	}
	state := got.Trackers[trackerID]
	if !state.Answering {
		t.Fatal("reads as silent while an address is answering")
	}
	if !state.Since.Equal(base) {
		t.Errorf("answering since %v, want %v", state.Since, base)
	}
	if state.Clipped {
		t.Error("the stretch started inside the window, so it is not a lower bound")
	}
}

func TestAvailabilityOverDatesSilence(t *testing.T) {
	s := testStore(t)
	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")
	went, end := base.Add(6*time.Hour), base.Add(10*time.Hour)

	probeRun(t, s, endpointID, base, verdict(endpointID, "1.2.3.4", ProbeLive, base, base))
	probeRun(t, s, endpointID, went, verdict(endpointID, "1.2.3.4", ProbeDead, went, went))

	got, err := s.AvailabilityOver(t.Context(), base.Add(-time.Hour), end)
	if err != nil {
		t.Fatal(err)
	}
	state := got.Trackers[trackerID]
	// Silence is dated from the last answer, not from the first probe.
	if state.Answering || !state.Since.Equal(went) {
		t.Errorf("answering=%v since %v, want silent since %v", state.Answering, state.Since, went)
	}
}

func TestAvailabilityOverMarksAStretchOlderThanTheWindow(t *testing.T) {
	s := testStore(t)
	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")
	old, end := base.Add(-48*time.Hour), base

	probeRun(t, s, endpointID, old, verdict(endpointID, "1.2.3.4", ProbeLive, old, old))

	got, err := s.AvailabilityOver(t.Context(), base.Add(-24*time.Hour), end)
	if err != nil {
		t.Fatal(err)
	}
	state := got.Trackers[trackerID]
	// It has answered longer than the window can see, so the duration is a
	// lower bound and has to say so rather than claim exactly 24 hours.
	if !state.Answering || !state.Clipped {
		t.Errorf("answering=%v clipped=%v, want an answering stretch marked as clipped",
			state.Answering, state.Clipped)
	}
}

func TestAvailabilityOverHasNoStretchOnceProbingStops(t *testing.T) {
	s := testStore(t)
	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")
	stopped, end := base.Add(4*time.Hour), base.Add(10*time.Hour)

	probeRun(t, s, endpointID, base, verdict(endpointID, "1.2.3.4", ProbeLive, base, base))
	// The addresses went away, or the host asked not to be probed. Either way
	// the last verdict describes the past.
	if err := s.ClearProbes(t.Context(), trackerID, stopped); err != nil {
		t.Fatal(err)
	}

	got, err := s.AvailabilityOver(t.Context(), base, end)
	if err != nil {
		t.Fatal(err)
	}
	state := got.Trackers[trackerID]
	if state.Measured != 4*time.Hour {
		t.Errorf("measured = %v, want the 4h that was measured", state.Measured)
	}
	// "Answering for six hours" would be a claim about time nobody watched.
	if !state.Since.IsZero() {
		t.Errorf("dated a stretch to %v with nothing being measured now", state.Since)
	}
}
