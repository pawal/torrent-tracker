package store

import (
	"testing"
	"time"
)

func kinds(t *testing.T, s *Store, trackerID int64) []string {
	t.Helper()
	changes, err := s.ChangesFor(t.Context(), trackerID, 50)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Type)
	}
	return out
}

func TestSetBEP34RecordsEveryTransition(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "tracker.example.com")

	steps := []struct {
		record string
		denies bool
		want   bool
		kind   string
	}{
		{"BITTORRENT UDP:6969", false, true, ChangeBEP34Added},
		// Seeing the same record again is not news, however many passes run.
		{"BITTORRENT UDP:6969", false, false, ""},
		{"BITTORRENT UDP:6969 TCP:80", false, true, ChangeBEP34Changed},
		// Withdrawing a record matters as much as publishing one: the host has
		// stopped saying where its tracker is.
		{"", false, true, ChangeBEP34Removed},
	}
	for _, step := range steps {
		changed, err := s.SetBEP34(ctx, tr.ID, step.record, step.denies, base)
		if err != nil {
			t.Fatal(err)
		}
		if changed != step.want {
			t.Errorf("SetBEP34(%q) reported changed=%v, want %v", step.record, changed, step.want)
		}
	}

	got := kinds(t, s, tr.ID)
	want := []string{ChangeBEP34Removed, ChangeBEP34Changed, ChangeBEP34Added, ChangeTrackerAdded}
	if len(got) != len(want) {
		t.Fatalf("feed = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("feed = %v, want %v", got, want)
			break
		}
	}
}

func TestSetBEP34StoresTheDenial(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	tr := mustAdd(t, s, "tracker.example.com")

	if _, err := s.SetBEP34(ctx, tr.ID, "BITTORRENT", true, base); err != nil {
		t.Fatal(err)
	}
	got, err := s.TrackerByName(ctx, "tracker.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.BEP34 != "BITTORRENT" || !got.BEP34Denies {
		t.Errorf("stored %q denies=%v, want the record and a denial", got.BEP34, got.BEP34Denies)
	}
}

func TestProbeTargetsSkipsDenyingHosts(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	willing, _ := newTrackerWithEndpoint(t, s, "willing.example.com")
	denying, _ := newTrackerWithEndpoint(t, s, "denying.example.com")
	for _, id := range []int64{willing, denying} {
		if err := s.ApplyPlan(ctx, id, Plan{
			Status: StatusOK, StatusChanged: true,
			Actions: []Action{{IP: "1.2.3.4", Family: 4, Kind: ActionAdd}},
		}, base); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetBEP34(ctx, denying, "BITTORRENT", true, base); err != nil {
		t.Fatal(err)
	}

	// The record is the one opt-out the protocol gives an operator. Probing
	// anyway because the name still resolves would be ignoring it.
	targets, err := s.ProbeTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Tracker.Name != "willing.example.com" {
		names := []string{}
		for _, tg := range targets {
			names = append(names, tg.Tracker.Name)
		}
		t.Errorf("probing %v, want only willing.example.com", names)
	}
}

func TestClearProbesClosesTheIntervals(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	trackerID, endpointID := newTrackerWithEndpoint(t, s, "tracker.example.com")
	probeRun(t, s, endpointID, base, verdict(endpointID, "1.2.3.4", ProbeLive, base, base))

	if err := s.ClearProbes(ctx, trackerID, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Nothing is being measured now, so nothing may claim to be. The hour that
	// was measured stays on record as a closed interval.
	open, err := s.ProbesFor(ctx, trackerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("%d probes still open, want 0", len(open))
	}
	closed, err := s.ProbeHistoryFor(ctx, trackerID, base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0].Result != ProbeLive {
		t.Errorf("history = %v, want the measured hour kept", closed)
	}
	if len(closed) == 1 && !closed[0].Until.Equal(base.Add(time.Hour)) {
		t.Errorf("interval ends %v, want it closed at the moment probing stopped", closed[0].Until)
	}
}
