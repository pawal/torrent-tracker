package store

import (
	"path/filepath"
	"testing"
	"time"
)

// base is the fixed clock every fixture is dated from, so a test can say
// "an hour later" and mean it.
var base = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

// testStore opens an empty database that goes away with the test.
func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// must fails the test on any error, for the calls whose only interesting
// outcome is that they worked at all.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// mustAdd registers a tracker as of base.
func mustAdd(t *testing.T, s *Store, name string) Tracker {
	t.Helper()
	tr, _, err := s.AddTracker(t.Context(), name, "test", base)
	if err != nil {
		t.Fatalf("AddTracker(%q): %v", name, err)
	}
	return tr
}

// adds turns addresses into the plan actions that first record them. A CIDR
// would need Prefix set, so these are addresses only.
func adds(ips ...string) []Action {
	out := make([]Action, 0, len(ips))
	for _, ip := range ips {
		out = append(out, Action{IP: ip, Family: Family(ip), Kind: ActionAdd})
	}
	return out
}

// apply persists one collection pass over a tracker.
func apply(t *testing.T, s *Store, trackerID int64, at time.Time, actions ...Action) {
	t.Helper()
	must(t, s.ApplyPlan(t.Context(), trackerID, Plan{Status: StatusOK, Actions: actions}, at))
}

// resolves points a tracker at addresses as of when, creating it if need be.
func resolves(t *testing.T, s *Store, name string, when time.Time, ips ...string) Tracker {
	t.Helper()
	tr, _, err := s.AddTracker(t.Context(), name, "test", when)
	if err != nil {
		t.Fatal(err)
	}
	must(t, s.ApplyPlan(t.Context(), tr.ID, Plan{
		Status: StatusOK, StatusChanged: true, Actions: adds(ips...),
	}, when))
	return tr
}

// withEndpoint adds a tracker with one announce endpoint, which is the least a
// probing pass needs, and returns both ids.
func withEndpoint(t *testing.T, s *Store, name string, at time.Time, scheme string, port int) (trackerID, endpointID int64) {
	t.Helper()
	tr, _, err := s.AddTracker(t.Context(), name, "test", at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEndpoint(t.Context(), tr.ID, scheme, port, "/announce", at); err != nil {
		t.Fatal(err)
	}
	eps, err := s.EndpointsFor(t.Context(), tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) == 0 {
		t.Fatalf("%s: no endpoint was recorded", name)
	}
	return tr.ID, eps[0].ID
}

// newTrackerWithEndpoint is withEndpoint on the conventional UDP port.
func newTrackerWithEndpoint(t *testing.T, s *Store, name string) (trackerID, endpointID int64) {
	t.Helper()
	return withEndpoint(t, s, name, base, "udp", 6969)
}

// newListTracker builds what a client list needs from a name: when it was
// added, one announce endpoint, and the addresses it resolves to.
func newListTracker(t *testing.T, s *Store, name string, added time.Time, scheme string, port int, ips ...string) (trackerID, endpointID int64) {
	t.Helper()
	trackerID, endpointID = withEndpoint(t, s, name, added, scheme, port)
	must(t, s.ApplyPlan(t.Context(), trackerID, Plan{
		Status: StatusOK, StatusChanged: true, Actions: adds(ips...),
	}, added))
	return trackerID, endpointID
}

// verdict is one probe result standing since a given time. probeRun stamps the
// endpoint on it, so a test names the endpoint once.
func verdict(ip string, result ProbeResult, since time.Time) Probe {
	return Probe{
		IP: ip, Family: Family(ip),
		Result: result, Since: since, CheckedAt: since,
	}
}

// probeRun records one probing pass over an endpoint. Whatever a verdict
// replaces is archived, so successive calls with a moving Since build the same
// closed intervals a real pass would leave behind.
func probeRun(t *testing.T, s *Store, endpointID int64, at time.Time, probes ...Probe) {
	t.Helper()
	for i := range probes {
		probes[i].EndpointID = endpointID
	}
	if err := s.PutProbes(t.Context(), []int64{endpointID}, probes, at); err != nil {
		t.Fatalf("PutProbes: %v", err)
	}
}

// changeKinds lists a tracker's change types, newest first.
func changeKinds(t *testing.T, s *Store, trackerID int64) []string {
	t.Helper()
	changes, err := s.ChangesFor(t.Context(), trackerID, 200)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Type)
	}
	return out
}

// countKind counts one kind of entry in a tracker's feed.
func countKind(t *testing.T, s *Store, trackerID int64, kind string) int {
	t.Helper()
	n := 0
	for _, got := range changeKinds(t, s, trackerID) {
		if got == kind {
			n++
		}
	}
	return n
}

// activeOne asserts a tracker has exactly one open address interval.
func activeOne(t *testing.T, s *Store, trackerID int64) IPRecord {
	t.Helper()
	got, err := s.ActiveRecords(t.Context(), trackerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d active records, want 1: %+v", len(got), got)
	}
	return got[0]
}
