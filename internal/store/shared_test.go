package store

import (
	"testing"
	"time"
)

// resolves points a tracker at addresses as of when, creating it if need be.
func resolves(t *testing.T, s *Store, name string, when time.Time, ips ...string) Tracker {
	t.Helper()
	tr, _, err := s.AddTracker(t.Context(), name, "test", when)
	if err != nil {
		t.Fatal(err)
	}
	actions := make([]Action, 0, len(ips))
	for _, ip := range ips {
		actions = append(actions, Action{IP: ip, Family: Family(ip), Kind: ActionAdd})
	}
	if err := s.ApplyPlan(t.Context(), tr.ID, Plan{
		Status: StatusOK, StatusChanged: true, Actions: actions,
	}, when); err != nil {
		t.Fatal(err)
	}
	return tr
}

func sharedNames(shared []SharedAddress, ip string) []string {
	for _, a := range shared {
		if a.IP == ip {
			return a.Trackers
		}
	}
	return nil
}

func TestSharedAddressesFindsNamesOnOneHost(t *testing.T) {
	s := testStore(t)
	resolves(t, s, "a.example.com", base, "1.2.3.4", "2001:db8::1")
	resolves(t, s, "b.example.com", base, "1.2.3.4")
	resolves(t, s, "alone.example.com", base, "9.9.9.9")

	got, err := s.SharedAddresses(t.Context(), base.Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	// Two names answering on one address are one operator and one outage,
	// whatever the names suggest. An address only one name uses says nothing.
	if len(got) != 1 || got[0].IP != "1.2.3.4" {
		t.Fatalf("shared = %+v, want only 1.2.3.4", got)
	}
	if names := got[0].Trackers; len(names) != 2 || names[0] != "a.example.com" || names[1] != "b.example.com" {
		t.Errorf("names = %v, want both trackers", names)
	}
	if !got[0].Active {
		t.Error("both names resolve there now, so the cluster is current")
	}
}

func TestSharedAddressesIgnoresPrefixRecords(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	for _, name := range []string{"cdn-a.example.com", "cdn-b.example.com"} {
		tr, _, err := s.AddTracker(ctx, name, "test", base)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ApplyPlan(ctx, tr.ID, Plan{
			Status: StatusOK, StatusChanged: true,
			Actions: []Action{{IP: "2600:9000:2094::/48", Family: 6, Kind: ActionAdd, Prefix: true}},
		}, base); err != nil {
			t.Fatal(err)
		}
	}

	// Two rolling names inside one CDN prefix are not two names on one host.
	// Reading a prefix record as an address would cluster half of Cloudflare.
	got, err := s.SharedAddresses(ctx, base.Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("shared = %+v, want nothing: a shared prefix is not a shared host", got)
	}
}

func TestSharedAddressesSkipsNonTrackers(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	resolves(t, s, "live.example.com", base, "1.2.3.4")
	parked := resolves(t, s, "parked.example.com", base, "1.2.3.4")
	resolves(t, s, "gone.example.com", base, "1.2.3.4")
	resolves(t, s, "canary.example.com", base, "1.2.3.4")

	// The parking cluster is the control name's business, and it would swamp
	// this listing with names already flagged for it.
	if _, err := s.SetParked(ctx, parked.ID, true, "parking address", base); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTracker(ctx, "gone.example.com", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetControl(ctx, "canary.example.com", true); err != nil {
		t.Fatal(err)
	}

	got, err := s.SharedAddresses(ctx, base.Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	// One tracker left standing on the address, so there is no cluster at all.
	if len(got) != 0 {
		t.Errorf("shared = %v, want nothing once the non-trackers are out",
			sharedNames(got, "1.2.3.4"))
	}
}

func TestSharedAddressesCoversRotation(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	a := resolves(t, s, "a.example.com", base, "1.2.3.4")
	resolves(t, s, "b.example.com", base, "1.2.3.4")

	// A host handing out a rotating subset drops an address from one name and
	// not the other. They are still the same host, which is why the window
	// looks back rather than only at what resolves this minute.
	if err := s.ApplyPlan(ctx, a.ID, Plan{
		Status:  StatusOK,
		Actions: []Action{{IP: "1.2.3.4", Family: 4, Kind: ActionRemove}},
	}, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, err := s.SharedAddresses(ctx, base.Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(sharedNames(got, "1.2.3.4")) != 2 {
		t.Fatalf("shared = %+v, want both names inside the window", got)
	}
	// Only one of them is there now, so the cluster is history, not news.
	if got[0].Active {
		t.Error("reported as current with one name gone")
	}

	// Outside the window the departed name no longer counts, and one name is
	// not a cluster.
	stale, err := s.SharedAddresses(ctx, base.Add(2*time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Errorf("shared = %+v, want nothing once the sighting ages out", stale)
	}
}

func TestSharedAddressesCarryTheirNetwork(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	resolves(t, s, "a.example.com", base, "1.2.3.4")
	resolves(t, s, "b.example.com", base, "1.2.3.4")
	if err := s.PutIPInfo(ctx, IPInfo{
		IP: "1.2.3.4", Family: 4, ASN: 13335, ASName: "CLOUDFLARENET",
		Country: "US", RIR: "arin", Prefix: "1.2.3.0/24",
	}, base); err != nil {
		t.Fatal(err)
	}

	got, err := s.SharedAddresses(ctx, base.Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("shared = %+v, want one cluster", got)
	}
	// Without the network, a CDN edge and a shared host read identically, and
	// only one of them means one operator.
	if got[0].Network.ASN != 13335 || got[0].Network.Holder == "" {
		t.Errorf("network = %+v, want the origin AS attached", got[0].Network)
	}
}
