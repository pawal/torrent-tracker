package store

import (
	"testing"
	"time"
)

func TestListEndpointsBuildsAnnounceURLs(t *testing.T) {
	s := testStore(t)
	_, endpointID := newListTracker(t, s, "tracker.example.com", base, "udp", 6969, "1.2.3.4", "2001:db8::1")
	probeRun(t, s, endpointID, base, verdict("1.2.3.4", ProbeLive, base))

	got, err := s.ListEndpoints(t.Context(), base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	e := got[0]
	// The scheme, port and path the importer kept are exactly what a client
	// needs back; a bare hostname would be useless to paste.
	if e.URL != "udp://tracker.example.com:6969/announce" {
		t.Errorf("url = %q", e.URL)
	}
	if !e.V4 || !e.V6 {
		t.Errorf("families = v4:%v v6:%v, want both", e.V4, e.V6)
	}
	if e.Added != base {
		t.Errorf("added = %v, want %v", e.Added, base)
	}
	if e.Uptime.Share() != 1 {
		t.Errorf("uptime = %v, want 1", e.Uptime.Share())
	}
}

func TestListEndpointsReportsFamiliesSeparately(t *testing.T) {
	s := testStore(t)
	newListTracker(t, s, "v4.example.com", base, "udp", 6969, "1.2.3.4")
	newListTracker(t, s, "v6.example.com", base, "udp", 6969, "2001:db8::1")

	got, err := s.ListEndpoints(t.Context(), base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ListEntry{}
	for _, e := range got {
		byName[e.Tracker] = e
	}
	// A caller on a v4-only network cannot use a v6-only tracker, so the two
	// have to be distinguishable before any filtering happens.
	if v4 := byName["v4.example.com"]; !v4.V4 || v4.V6 {
		t.Errorf("v4-only name reports v4:%v v6:%v", v4.V4, v4.V6)
	}
	if v6 := byName["v6.example.com"]; v6.V4 || !v6.V6 {
		t.Errorf("v6-only name reports v4:%v v6:%v", v6.V4, v6.V6)
	}
}

func TestListEndpointsExcludesNonTrackers(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	newListTracker(t, s, "good.example.com", base, "udp", 6969, "1.2.3.4")
	parkedID, _ := newListTracker(t, s, "parked.example.com", base, "udp", 6969, "1.2.3.5")
	newListTracker(t, s, "gone.example.com", base, "udp", 6969, "1.2.3.6")
	newListTracker(t, s, "canary.example.com", base, "udp", 6969, "1.2.3.7")
	denyID, _ := newListTracker(t, s, "denies.example.com", base, "udp", 6969, "1.2.3.8")

	// A parked domain resolves perfectly and answers nothing, so it would sail
	// through any DNS-based filter. It must never reach a client.
	if _, err := s.SetParked(ctx, parkedID, true, "parking address", base); err != nil {
		t.Fatal(err)
	}
	// Handing out a URL for a host that published "no trackers here" would be
	// the loudest possible way of ignoring it.
	if _, err := s.SetBEP34(ctx, denyID, "BITTORRENT", true, base); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTracker(ctx, "gone.example.com", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetControl(ctx, "canary.example.com", true); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListEndpoints(ctx, base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Tracker != "good.example.com" {
		names := []string{}
		for _, e := range got {
			names = append(names, e.Tracker)
		}
		t.Errorf("listed %v, want only good.example.com", names)
	}
}

func TestListEndpointsOrdersByUptime(t *testing.T) {
	s := testStore(t)
	end := base.Add(24 * time.Hour)
	_, worst := newListTracker(t, s, "a-worst.example.com", base, "udp", 6969, "1.2.3.4")
	_, best := newListTracker(t, s, "z-best.example.com", base, "udp", 6969, "1.2.3.5")

	probeRun(t, s, worst, base, verdict("1.2.3.4", ProbeDead, base))
	probeRun(t, s, best, base, verdict("1.2.3.5", ProbeLive, base))

	got, err := s.ListEndpoints(t.Context(), base, end)
	if err != nil {
		t.Fatal(err)
	}
	// Best first, whatever the names sort like: a truncated list should keep
	// the trackers worth having.
	if len(got) != 2 || got[0].Tracker != "z-best.example.com" {
		t.Errorf("order = %q, %q; want the working tracker first", got[0].Tracker, got[1].Tracker)
	}
}
